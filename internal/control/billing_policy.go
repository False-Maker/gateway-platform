package control

import (
	"fmt"
	"sort"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

// B4.3: what the billing job does with usage of each trust level.
//
// The rule this file exists to enforce is that there is no implicit default.
// Every combination of (UsageSource, Partial, succeeded) has an entry in
// usageDispositions below, chosen deliberately; a combination that is not in
// the table is held for a human rather than guessed at. "Zero tokens means
// nothing was used" is exactly the implicit behaviour B4.0 forbade, so a
// `missing` row is never quietly settled at zero.

type billingDisposition string

const (
	// dispositionBill: the token counts are trustworthy, charge for them.
	dispositionBill billingDisposition = "bill"
	// dispositionHold: we cannot decide without a human. The row is parked in
	// `held`, is not charged, and is not written off either.
	dispositionHold billingDisposition = "hold"
	// dispositionNotBillable: decided, deliberately, to be worth zero. Not a
	// missing decision -- an actual one, recorded so it can be counted.
	dispositionNotBillable billingDisposition = "not_billable"
)

// usageClass is the full key of the policy table. `succeeded` is part of the
// key because the same UsageSource means very different things on a request
// that served a response and one that returned an error.
type usageClass struct {
	source    string
	partial   bool
	succeeded bool
}

// usageDispositions is the policy, written out in full. It is data rather than
// a chain of ifs so that the exhaustiveness can be asserted in a test: see
// TestEveryUsageClassHasAnExplicitDisposition.
var usageDispositions = map[usageClass]billingDisposition{
	// Upstream reported the numbers on a request we served: charge for them.
	// A truncated stream still consumed the tokens upstream reported -- the
	// truncation changed what the client received, not what was spent.
	{contracts.UsageSourceUpstream, false, true}: dispositionBill,
	{contracts.UsageSourceUpstream, true, true}:  dispositionBill,

	// Upstream reported numbers but the request failed. We absorb this cost
	// rather than charge a tenant for an error they could not use. Rare in
	// practice: a 429 or 5xx usually carries no usage block at all.
	{contracts.UsageSourceUpstream, false, false}: dispositionNotBillable,
	{contracts.UsageSourceUpstream, true, false}:  dispositionNotBillable,

	// Nothing in this platform produces `estimated` today (gateway emits only
	// `upstream` or `missing`). If a producer ever appears, its output must be
	// looked at by a person before it moves money in either direction --
	// neither charging an estimate nor writing one off is safe by default.
	{contracts.UsageSourceEstimated, false, true}:  dispositionHold,
	{contracts.UsageSourceEstimated, true, true}:   dispositionHold,
	{contracts.UsageSourceEstimated, false, false}: dispositionHold,
	{contracts.UsageSourceEstimated, true, false}:  dispositionHold,

	// We served a response and lost the token counts. This is the revenue leak
	// the DoD wants surfaced: hold it, alert on it, and never invent a number
	// to fill the gap (AGENTS.md §2 forbids fabricating quota).
	{contracts.UsageSourceMissing, false, true}: dispositionHold,
	{contracts.UsageSourceMissing, true, true}:  dispositionHold,

	// A failed request with no usage is the honest zero: the reconciler's
	// synthesised 504 rows land here, and so does every network error.
	{contracts.UsageSourceMissing, false, false}: dispositionNotBillable,
	{contracts.UsageSourceMissing, true, false}:  dispositionNotBillable,
}

// dispositionFor resolves the policy. An unrecognised class -- a UsageSource
// added to the contract without being added here -- is held, never billed and
// never written off, so the omission shows up as a growing held bucket instead
// of as silent over- or under-charging.
func dispositionFor(class usageClass) (billingDisposition, bool) {
	disposition, known := usageDispositions[class]
	if !known {
		return dispositionHold, false
	}
	return disposition, true
}

// releaseSucceeded says whether the request actually served a response. Only
// contracts.ErrorOK counts: an unrecognised error class is treated as a
// failure, because billing something we cannot classify is the worse mistake.
func releaseSucceeded(statusCode int, errorClass string) bool {
	return errorClass == string(contracts.ErrorOK) && statusCode >= 200 && statusCode < 400
}

// billingStateFor maps a disposition to the terminal usage_ledger state. Rows
// destined for dispositionBill have no state yet -- they still have to be
// priced, and may end up `billed` or `unpriced`.
func billingStateFor(disposition billingDisposition) (string, error) {
	switch disposition {
	case dispositionHold:
		return "held", nil
	case dispositionNotBillable:
		return "not_billable", nil
	default:
		return "", fmt.Errorf("disposition %q has no terminal billing state", disposition)
	}
}

// knownUsageSources is the set the policy table is required to cover: the same
// three values contracts.Release validation accepts. It is hand-listed, so a
// fourth source added to the contract would not fail this list by itself --
// what protects us there is dispositionFor, which holds an unknown class
// instead of guessing, and the held-bucket alert that follows from it.
func knownUsageSources() []string {
	sources := []string{contracts.UsageSourceUpstream, contracts.UsageSourceEstimated, contracts.UsageSourceMissing}
	sort.Strings(sources)
	return sources
}
