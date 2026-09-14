package control

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"os"
	"strings"
)

// B5: the console's identity model.
//
// §6.4.1 constrains how gateway talks to its clients; it says nothing about
// who may call control. 总览 §6.5 reserves the console itself for P6, and the
// P0 decision that does apply here is the one in §3: control and gateway share
// only the snapshot and the Release stream, and there is no synchronous RPC
// between them. The console therefore must not become a second channel into
// the tenant auth surface -- a tenant token is a client credential, and using
// it to reach the wallet and the pricing table would hand every tenant the
// platform's revenue and the ability to top itself up.
//
// So the console has its own credentials, configured out of band and compared
// in constant time. That keeps the boundary the design already drew -- tenant
// identity travels by snapshot, operator identity by config -- and it means the
// console can be exposed on a separate address that a tenant token has no way
// to reach.
//
// There are two levels, not one (B5.1). B5 shipped a single token, so every
// caller who could read the reconciliation could also top up a wallet, insert a
// price and disable an account. Reads are the common case and writes are the
// dangerous one, so they get different credentials.
//
// Three consequences worth stating because they are choices, not oversights:
//   * Levels are not identities. The audit trail (billing_resolutions.operator,
//     account_actions.operator) records what the caller claimed via the
//     X-Operator header, which is attribution for a human-readable log, not an
//     authenticated identity. An operator directory is a real access-control
//     system, and this is still not that -- two capability levels is a
//     blast-radius control, not authentication.
//   * An unset read-write token disables the console entirely rather than
//     leaving it open. A missing environment variable must not be the
//     difference between "no console" and "unauthenticated console".
//   * Write authorization is enforced by the route table, not by each handler
//     remembering to check. A handler added later is read-only until someone
//     lists it as a write, which fails in the safe direction.

// ConsoleTokenEnv names the environment variable holding the read-write
// operator token. It has no default: control without it simply does not serve
// the console.
const ConsoleTokenEnv = "GATEWAY_CONSOLE_TOKEN"

// ConsoleReadOnlyTokenEnv names an optional second token that may read but not
// write.
//
// B5 shipped one token for every caller, so anyone who could look at the
// reconciliation could also top up a wallet and insert a price. That is a wide
// blast radius for the common case, which is looking. Two levels is the
// smallest split that removes it: reads need either token, writes need the
// read-write one. It stays env-configured rather than becoming an operator
// table, so `X-Operator` is still attribution and not an authenticated
// identity -- see the note above; that has not changed.
const ConsoleReadOnlyTokenEnv = "GATEWAY_CONSOLE_READONLY_TOKEN"

// ConsoleAddrEnv names the environment variable holding the console's listen
// address. It defaults to loopback so an operator has to opt in to exposing it,
// and it deliberately is not the metrics address (metrics have no auth at all
// and must not gain one by being merged with this).
const ConsoleAddrEnv = "GATEWAY_CONSOLE_ADDR"

const defaultConsoleAddr = "127.0.0.1:9092"

// operatorClaimHeader is where the caller says who they are, for the audit row.
const operatorClaimHeader = "X-Operator"

// ConsoleAuth verifies the operator token. A zero-token ConsoleAuth rejects
// everything, which is what makes an unconfigured console fail closed.
type ConsoleAuth struct {
	// Token is the read-write operator credential. Empty disables the console.
	Token string
	// ReadOnlyToken may read but never write. Optional; empty means there is
	// no read-only level and every caller uses Token.
	ReadOnlyToken string
}

// consoleAccess is what a presented credential is allowed to do.
type consoleAccess int

const (
	consoleDenied consoleAccess = iota
	consoleReadOnly
	consoleReadWrite
)

// Enabled reports whether a read-write token was configured at all.
//
// A read-only token alone does not enable the console. Booting with only the
// reader configured is far more likely to be a misconfiguration than an
// intent to run a permanently read-only control plane, and failing closed on
// it costs an operator one environment variable rather than costing them a
// console they believed was writable.
func (a ConsoleAuth) Enabled() bool { return strings.TrimSpace(a.Token) != "" }

// Validate rejects a configuration whose two tokens are the same string.
//
// That case has no safe interpretation. Treating the value as read-write hands
// writes to everyone the operator handed a "reader"; treating it as read-only
// silently makes every write path 403 with no explanation. Refusing to start
// is the only reading that cannot mislead someone, and it costs one
// environment variable to fix.
func (a ConsoleAuth) Validate() error {
	readOnly := strings.TrimSpace(a.ReadOnlyToken)
	if readOnly != "" && readOnly == strings.TrimSpace(a.Token) {
		return errors.New(ConsoleReadOnlyTokenEnv + " must differ from " + ConsoleTokenEnv +
			": identical values would grant writes to every read-only holder")
	}
	return nil
}

// access classifies the request's bearer token. It returns consoleDenied for a
// missing, malformed or wrong token, and never reports which of those it was.
func (a ConsoleAuth) access(r *http.Request) consoleAccess {
	if !a.Enabled() {
		return consoleDenied
	}
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	prefix := "bearer "
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return consoleDenied
	}
	presented := strings.TrimSpace(raw[len(prefix):])
	if presented == "" {
		return consoleDenied
	}
	// Constant time so a wrong token cannot be recovered a byte at a time.
	// Both comparisons always run: returning early on the first match would
	// make the read-write token distinguishable by response timing.
	readWrite := subtle.ConstantTimeCompare([]byte(presented), []byte(strings.TrimSpace(a.Token))) == 1
	readOnly := false
	if configured := strings.TrimSpace(a.ReadOnlyToken); configured != "" {
		readOnly = subtle.ConstantTimeCompare([]byte(presented), []byte(configured)) == 1
	}
	switch {
	case readWrite:
		return consoleReadWrite
	case readOnly:
		return consoleReadOnly
	default:
		return consoleDenied
	}
}

// authorize reports whether the request may proceed at all.
func (a ConsoleAuth) authorize(r *http.Request) bool { return a.access(r) != consoleDenied }

// operator reports who the caller claims to be, for the audit trail only. It is
// never used to make an authorization decision -- authorize has already run.
func operator(r *http.Request) string {
	who := strings.TrimSpace(r.Header.Get(operatorClaimHeader))
	if who == "" {
		return "unknown"
	}
	if len(who) > 200 {
		who = who[:200]
	}
	return who
}

// ConsoleAddress returns the configured listen address and whether the console
// is enabled at all.
func ConsoleAddress() (string, bool) {
	if strings.TrimSpace(os.Getenv(ConsoleTokenEnv)) == "" {
		return "", false
	}
	address := strings.TrimSpace(os.Getenv(ConsoleAddrEnv))
	if address == "" {
		address = defaultConsoleAddr
	}
	return address, true
}
