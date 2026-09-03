package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestParseQuotaFixtureValidatesCanonicalItems(t *testing.T) {
	got, err := ParseQuotaFixture([]byte(`{"items":[{"scope":"account","unit":"token","limit":100,"remaining":75,"reset_at":"2026-09-01T12:00:00Z"},{"scope":"model","model":"gpt-5","unit":"request","remaining":3}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) != 2 || got.Items[0].Limit == nil || *got.Items[0].Limit != 100 || got.Items[1].Model != "gpt-5" {
		t.Fatalf("quota = %#v", got)
	}
	if got.Items[0].ResetAt == nil || !got.Items[0].ResetAt.Equal(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset_at = %#v", got.Items[0].ResetAt)
	}
}

func TestParseQuotaFixtureAllowsMissingQuotaAndRejectsInvalidItems(t *testing.T) {
	for _, body := range []string{`{}`, `{"items":[]}`} {
		got, err := ParseQuotaFixture([]byte(body))
		if err != nil || len(got.Items) != 0 {
			t.Fatalf("missing quota body %s: %#v %v", body, got, err)
		}
	}
	for _, body := range []string{
		`{"items":[{"scope":"tenant","unit":"token"}]}`,
		`{"items":[{"scope":"model","unit":"token"}]}`,
		`{"items":[{"scope":"account","model":"gpt-5","unit":"token"}]}`,
		`{"items":[{"scope":"account","unit":"second"}]}`,
		`{"items":[{"scope":"account","unit":"token","limit":1,"remaining":2}]}`,
	} {
		if _, err := ParseQuotaFixture([]byte(body)); !errors.Is(err, contracts.ErrInvalidContract) {
			t.Fatalf("body %s: got %v, want invalid contract", body, err)
		}
	}
	if _, err := ParseQuotaFixture([]byte(`{"items":[]} {}`)); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("trailing JSON: got %v", err)
	}
}

func TestFetchQuotaBoundsResponseAndClassifiesStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/quota" || r.Header.Get("Authorization") != "Bearer access-1" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("request path=%s headers=%#v", r.URL.Path, r.Header)
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_token","detail":"secret"}`)
	}))
	profile := CodexChatGPTProfile()
	profile.APIBaseURL = server.URL
	profile.QuotaPath = "/quota"
	headers, err := profile.InferenceHeaders("access-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = (HTTPClient{}).FetchQuota(context.Background(), server.URL+"/quota", headers)
	server.Close()
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) || ErrorClass(err) != contracts.ErrorAuthInvalid || httpErr.Code != "invalid_token" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("quota error=%v class=%s", err, ErrorClass(err))
	}

	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxQuotaResponseBytes+1))
	}))
	defer large.Close()
	if _, err := (HTTPClient{}).FetchQuota(context.Background(), large.URL, headers); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("large quota response error = %v", err)
	}
}

func TestQuotaURLIsOptionalAndValidatesPath(t *testing.T) {
	profile := CodexAPIKeyProfile()
	endpoint, err := profile.QuotaURL()
	if err != nil || endpoint != "" {
		t.Fatalf("empty quota endpoint = %q err=%v", endpoint, err)
	}
	profile.QuotaPath = "quota"
	if _, err := profile.QuotaURL(); err == nil {
		t.Fatal("quota path without slash was accepted")
	}
}
