package contracts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuotaDecimalRoundTripPreservesPrecision(t *testing.T) {
	quota := QuotaInfo{Items: []QuotaItem{{Scope: "model", Model: "gemini", Unit: "fraction", RemainingFraction: ptr(Decimal("0.99833333")), Precision: &QuotaPrecision{Remaining: ptr(Decimal("2028.68"))}}}}
	payload, err := json.Marshal(quota)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "0.99833333") || !strings.Contains(string(payload), "2028.68") {
		t.Fatalf("precision was changed: %s", payload)
	}
	var decoded QuotaInfo
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if got := string(*decoded.Items[0].RemainingFraction); got != "0.99833333" || string(*decoded.Items[0].Precision.Remaining) != "2028.68" {
		t.Fatalf("decoded quota=%#v", decoded)
	}
}

func TestQuotaDecimalRejectsInvalidJSONValue(t *testing.T) {
	if _, err := json.Marshal(QuotaInfo{Items: []QuotaItem{{Scope: "account", Unit: "credit", RemainingExact: ptr(Decimal("not-a-number"))}}}); err == nil {
		t.Fatal("invalid decimal was accepted")
	}
}

func ptr[T any](value T) *T { return &value }
