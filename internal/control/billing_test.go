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

// A20: the integer-digit ceiling of NUMERIC(38,12). Before this, "1e30" passed
// every Go guard -- moneyPlaces reports 0 fractional digits once the exponent
// is applied -- and only failed inside PostgreSQL.
func TestMoneyIntegerDigitsAndCeiling(t *testing.T) {
	for amount, want := range map[string]int{
		"0":                          0,
		"0.5":                        0,
		"-0.5":                       0,
		"7":                          1,
		"-7":                         1,
		"123.45":                     3,
		"1.5e3":                      4, // 1500
		"1500e-2":                    2, // 15
		"1e30":                       31,
		"007":                        1,
		"99999999999999999999999999": 26, // exactly the ceiling
	} {
		if got := moneyIntegerDigits(contracts.Decimal(amount)); got != want {
			t.Errorf("moneyIntegerDigits(%s) = %d, want %d", amount, got, want)
		}
	}

	// At the ceiling it is storable; one digit past it is not.
	atCeiling := contracts.Decimal("99999999999999999999999999")
	if err := checkMoney("amount", atCeiling); err != nil {
		t.Errorf("26 integer digits must be accepted: %v", err)
	}
	past := contracts.Decimal("1e30")
	if err := checkMoney("amount", past); err == nil {
		t.Error("1e30 must be rejected before it reaches PostgreSQL")
	} else if !strings.Contains(err.Error(), "integer digits") {
		t.Errorf("rejection %q does not name the problem", err)
	}
	// The scale rule must still hold, unchanged.
	if err := checkMoney("amount", contracts.Decimal("0.0000000000001")); err == nil {
		t.Error("13 decimal places must still be rejected")
	}
}
