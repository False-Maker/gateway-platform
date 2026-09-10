package control

import (
	"strings"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestCheckMoneyRejectsValuesTheColumnCannotStoreExactly(t *testing.T) {
	for _, ok := range []contracts.Decimal{"0", "-1", "0.000000000001", "12.5", "3e-12", "1E6"} {
		if err := checkMoney("amount", ok); err != nil {
			t.Errorf("checkMoney(%q) = %v, want nil", ok, err)
		}
	}
	// 13 decimal places would be rounded by NUMERIC(38,12); a silently
	// rounded amount is a money bug, so it must be refused up front.
	for _, bad := range []contracts.Decimal{"0.0000000000001", "1.5e-13", "", "abc", "1,5"} {
		if err := checkMoney("amount", bad); err == nil {
			t.Errorf("checkMoney(%q) accepted an unstorable amount", bad)
		}
	}
}

func TestMoneyPlacesAccountsForExponent(t *testing.T) {
	cases := map[contracts.Decimal]int{
		"1":        0,
		"1.25":     2,
		"1.25e2":   0,
		"1.25e-2":  4,
		"1e-12":    12,
		"0.5E+1":   0,
		"-0.00001": 5,
	}
	for amount, want := range cases {
		if got := moneyPlaces(amount); got != want {
			t.Errorf("moneyPlaces(%q) = %d, want %d", amount, got, want)
		}
	}
}

func TestCheckMovementKindEnforcesSignPerKind(t *testing.T) {
	cases := []struct {
		kind   string
		amount contracts.Decimal
		wantOK bool
	}{
		{"topup", "10", true},
		{"topup", "-10", false},
		{"topup", "0", false},
		{"debit", "-0.5", true},
		{"debit", "0.5", false},
		{"debit", "0", false},
		{"adjustment", "-3", true},
		{"adjustment", "3", true},
		{"adjustment", "0", false},
		{"refund", "1", false},
		{"", "1", false},
	}
	for _, testCase := range cases {
		err := checkMovementKind(testCase.kind, testCase.amount)
		if (err == nil) != testCase.wantOK {
			t.Errorf("checkMovementKind(%q, %q) = %v", testCase.kind, testCase.amount, err)
		}
	}
	if err := checkMovementKind("topup", "not-a-number"); err == nil || !strings.Contains(err.Error(), "not a decimal") {
		t.Errorf("non-decimal amount error = %v", err)
	}
}
