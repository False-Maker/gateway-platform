package control

import (
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

// The DoD's central requirement: every combination has a decision recorded in
// the table, and no combination falls through to a code default.
func TestEveryUsageClassHasAnExplicitDisposition(t *testing.T) {
	var covered int
	for _, source := range knownUsageSources() {
		for _, partial := range []bool{false, true} {
			for _, succeeded := range []bool{false, true} {
				class := usageClass{source: source, partial: partial, succeeded: succeeded}
				disposition, known := dispositionFor(class)
				if !known {
					t.Errorf("no policy entry for %+v", class)
					continue
				}
				switch disposition {
				case dispositionBill, dispositionHold, dispositionNotBillable:
				default:
					t.Errorf("%+v maps to unknown disposition %q", class, disposition)
				}
				covered++
			}
		}
	}
	if covered != len(usageDispositions) {
		t.Fatalf("policy table has %d entries but only %d are reachable; an entry keys off a source the job never sees", len(usageDispositions), covered)
	}
	if covered != 12 {
		t.Fatalf("expected 3 sources x partial x succeeded = 12 classes, covered %d", covered)
	}
}

func TestDispositionsMatchTheAgreedBillingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class usageClass
		want  billingDisposition
	}{
		{"a served request with upstream usage is charged",
			usageClass{contracts.UsageSourceUpstream, false, true}, dispositionBill},
		{"a truncated stream with upstream usage is still charged",
			usageClass{contracts.UsageSourceUpstream, true, true}, dispositionBill},
		{"a failed request is not charged even when upstream reported usage",
			usageClass{contracts.UsageSourceUpstream, false, false}, dispositionNotBillable},
		{"a served request whose usage was lost is held for a human",
			usageClass{contracts.UsageSourceMissing, false, true}, dispositionHold},
		{"a failed request with no usage is the honest zero",
			usageClass{contracts.UsageSourceMissing, false, false}, dispositionNotBillable},
		{"estimated usage never moves money without a human",
			usageClass{contracts.UsageSourceEstimated, false, true}, dispositionHold},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known := dispositionFor(tc.class)
			if !known {
				t.Fatalf("%+v has no policy entry", tc.class)
			}
			if got != tc.want {
				t.Fatalf("%+v = %q, want %q", tc.class, got, tc.want)
			}
		})
	}
}

// An unknown class must be held: not billed (we would be charging for usage we
// cannot reason about) and not written off (we would be silently losing money).
func TestAnUnknownUsageClassIsHeldRatherThanGuessed(t *testing.T) {
	disposition, known := dispositionFor(usageClass{source: "some-future-source", succeeded: true})
	if known {
		t.Fatal("an invented usage source was reported as a known class")
	}
	if disposition != dispositionHold {
		t.Fatalf("unknown class = %q, want hold", disposition)
	}
}

func TestReleaseSucceededOnlyAcceptsAnExplicitOK(t *testing.T) {
	if !releaseSucceeded(200, string(contracts.ErrorOK)) {
		t.Error("a 200 with error_class=ok is a success")
	}
	for _, tc := range []struct {
		status int
		class  string
	}{
		{500, string(contracts.ErrorUpstream5xx)},
		{429, string(contracts.ErrorRateLimitedKnown)},
		{504, string(contracts.ErrorNetwork)},
		// A status that says success but an error class that does not: trust
		// the error class, because billing an unclassifiable row is worse.
		{200, string(contracts.ErrorNetwork)},
		// An empty error class is not "ok"; it is unrecognised.
		{200, ""},
	} {
		if releaseSucceeded(tc.status, tc.class) {
			t.Errorf("status %d class %q was treated as a success", tc.status, tc.class)
		}
	}
}

func TestOnlyNonBillingDispositionsHaveATerminalState(t *testing.T) {
	for disposition, want := range map[billingDisposition]string{
		dispositionHold:        "held",
		dispositionNotBillable: "not_billable",
	} {
		got, err := billingStateFor(disposition)
		if err != nil {
			t.Fatalf("%q: %v", disposition, err)
		}
		if got != want {
			t.Errorf("%q maps to state %q, want %q", disposition, got, want)
		}
	}
	// `bill` has no terminal state yet: the row still has to be priced, and
	// may land in `billed` or `unpriced`.
	if _, err := billingStateFor(dispositionBill); err == nil {
		t.Error("dispositionBill must not resolve to a terminal state")
	}
}
