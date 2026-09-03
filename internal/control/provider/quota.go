package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

const maxQuotaResponseBytes = 1 << 20

// FetchQuota reads the provider adapter fixture envelope and normalizes it to
// the stable QuotaInfo contract. Real provider response shapes are deliberately
// not inferred here; each provider enables this endpoint only when verified.
func (c HTTPClient) FetchQuota(ctx context.Context, endpoint string, headers http.Header) (contracts.QuotaInfo, error) {
	if strings.TrimSpace(endpoint) == "" {
		return contracts.QuotaInfo{}, fmt.Errorf("%w: quota endpoint is empty", contracts.ErrInvalidContract)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	defer resp.Body.Close()
	body, err := readLimited(resp.Body, maxQuotaResponseBytes)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return contracts.QuotaInfo{}, newHTTPError("quota", resp.StatusCode, body)
	}
	return ParseQuotaFixture(body)
}

// ParseQuotaFixture parses the language-neutral fixture envelope used by the
// Codex and Claude adapters. The envelope intentionally mirrors QuotaInfo so
// Python and Go workers can exchange it without a Go-specific representation.
func ParseQuotaFixture(body []byte) (contracts.QuotaInfo, error) {
	if len(body) == 0 {
		return contracts.QuotaInfo{}, fmt.Errorf("%w: quota response is empty", contracts.ErrInvalidContract)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var quota contracts.QuotaInfo
	if err := decoder.Decode(&quota); err != nil {
		return contracts.QuotaInfo{}, fmt.Errorf("decode quota response: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return contracts.QuotaInfo{}, fmt.Errorf("%w: quota response has trailing data", contracts.ErrInvalidContract)
		}
		return contracts.QuotaInfo{}, fmt.Errorf("decode quota response trailer: %w", err)
	}
	if err := validateQuotaInfo(quota); err != nil {
		return contracts.QuotaInfo{}, err
	}
	return quota, nil
}

func validateQuotaInfo(quota contracts.QuotaInfo) error {
	for i, item := range quota.Items {
		if item.Scope != "account" && item.Scope != "model" {
			return fmt.Errorf("%w: quota item %d has invalid scope", contracts.ErrInvalidContract, i)
		}
		if item.Scope == "model" && strings.TrimSpace(item.Model) == "" {
			return fmt.Errorf("%w: quota item %d model is empty", contracts.ErrInvalidContract, i)
		}
		if item.Scope == "account" && item.Model != "" {
			return fmt.Errorf("%w: quota item %d account scope has model", contracts.ErrInvalidContract, i)
		}
		switch item.Unit {
		case "token", "request", "credit":
		default:
			return fmt.Errorf("%w: quota item %d has invalid unit", contracts.ErrInvalidContract, i)
		}
		if item.Limit != nil && *item.Limit < 0 {
			return fmt.Errorf("%w: quota item %d limit is negative", contracts.ErrInvalidContract, i)
		}
		if item.Remaining != nil && *item.Remaining < 0 {
			return fmt.Errorf("%w: quota item %d remaining is negative", contracts.ErrInvalidContract, i)
		}
		if item.Limit != nil && item.Remaining != nil && *item.Remaining > *item.Limit {
			return fmt.Errorf("%w: quota item %d remaining exceeds limit", contracts.ErrInvalidContract, i)
		}
	}
	return nil
}
