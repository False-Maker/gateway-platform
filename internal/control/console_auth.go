package control

import (
	"crypto/subtle"
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
// So the console has its own credential: one operator token, configured out of
// band, compared in constant time. That keeps the boundary the design already
// drew -- tenant identity travels by snapshot, operator identity by config --
// and it means the console can be exposed on a separate address that a tenant
// token has no way to reach.
//
// Two consequences worth stating because they are choices, not oversights:
//   * There is one operator identity, not an operator table. The audit trail
//     (billing_resolutions.operator) records what the caller claimed via the
//     X-Operator header, which is attribution for a human-readable log, not an
//     authenticated identity. An operator directory is a real access-control
//     system, and B5 is not that.
//   * An unset token disables the console entirely rather than leaving it open.
//     A missing environment variable must not be the difference between "no
//     console" and "unauthenticated console".

// ConsoleTokenEnv names the environment variable holding the operator token.
// It has no default: control without it simply does not serve the console.
const ConsoleTokenEnv = "GATEWAY_CONSOLE_TOKEN"

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
	// Token is the operator credential. Empty disables the console.
	Token string
}

// Enabled reports whether a token was configured at all.
func (a ConsoleAuth) Enabled() bool { return strings.TrimSpace(a.Token) != "" }

// authorize checks the request's bearer token. It returns false for a missing,
// malformed or wrong token, and never reports which of those it was.
func (a ConsoleAuth) authorize(r *http.Request) bool {
	if !a.Enabled() {
		return false
	}
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	prefix := "bearer "
	if len(raw) < len(prefix) || !strings.EqualFold(raw[:len(prefix)], prefix) {
		return false
	}
	presented := strings.TrimSpace(raw[len(prefix):])
	if presented == "" {
		return false
	}
	// Constant time so a wrong token cannot be recovered a byte at a time.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(strings.TrimSpace(a.Token))) == 1
}

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
