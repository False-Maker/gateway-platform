package control

import (
	"math/big"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestLineAmountIsExactAcrossTheFourTokenClasses(t *testing.T) {
	price := ModelPrice{
		ID: "unit", UnitScale: 1000000,
		PriceInput: "3", PriceOutput: "15", PriceCacheRead: "0.3", PriceCacheWrite: "3.75",
	}
	row := billableRow{tokensIn: 1000, tokensOut: 500, cacheReadTokens: 2000, cacheWriteTokens: 400}
	amount, err := lineAmount(row, price)
	if err != nil {
		t.Fatal(err)
	}
	// (1000*3 + 500*15 + 2000*0.3 + 400*3.75) / 1e6 = 12600/1e6 = 0.0126
	if got := amount.FloatString(moneyScale); got != "0.012600000000" {
		t.Fatalf("amount = %s, want 0.012600000000", got)
	}

	// A row with no tokens costs nothing; it is settled, not skipped.
	zero, err := lineAmount(billableRow{}, price)
	if err != nil {
		t.Fatal(err)
	}
	if zero.Sign() != 0 {
		t.Errorf("empty row cost %s", zero.FloatString(moneyScale))
	}
}

func TestLineAmountKeepsPrecisionAFloatWouldLose(t *testing.T) {
	// 0.1 is not representable in binary floating point. One token at a
	// price of 0.1 per token must come out as exactly 0.1, and three of them
	// as exactly 0.3 -- the classic 0.30000000000000004 case.
	price := ModelPrice{ID: "unit", UnitScale: 1, PriceInput: "0.1", PriceOutput: "0", PriceCacheRead: "0", PriceCacheWrite: "0"}
	total := new(big.Rat)
	for index := 0; index < 3; index++ {
		amount, err := lineAmount(billableRow{tokensIn: 1}, price)
		if err != nil {
			t.Fatal(err)
		}
		total.Add(total, amount)
	}
	if got := total.FloatString(moneyScale); got != "0.300000000000" {
		t.Fatalf("three lines of 0.1 = %s", got)
	}
}

func TestLineAmountRejectsUnusablePricesAndCounts(t *testing.T) {
	valid := ModelPrice{ID: "unit", UnitScale: 1000, PriceInput: "1", PriceOutput: "1", PriceCacheRead: "1", PriceCacheWrite: "1"}
	if _, err := lineAmount(billableRow{tokensIn: 1}, ModelPrice{ID: "bad", UnitScale: 0, PriceInput: "1"}); err == nil {
		t.Error("a zero unit_scale was accepted")
	}
	if _, err := lineAmount(billableRow{tokensIn: -1}, valid); err == nil {
		t.Error("a negative token count was accepted")
	}
	broken := valid
	broken.PriceOutput = contracts.Decimal("not-a-price")
	if _, err := lineAmount(billableRow{tokensOut: 1}, broken); err == nil {
		t.Error("an unparseable price was accepted")
	}
	// An unparseable price for a class with zero tokens cannot affect the
	// result, so it must not fail the row either.
	if _, err := lineAmount(billableRow{tokensIn: 1}, broken); err != nil {
		t.Errorf("a bad price for an unused token class failed the row: %v", err)
	}
}

// The UsageSource policy this job used to hard-code as a single narrow
// constant now lives in billing_policy.go and is tested there.
