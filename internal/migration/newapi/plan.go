package newapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/elucid/gateway-platform/internal/control/provider"
	"github.com/elucid/gateway-platform/internal/control/provider/claude"
	"github.com/elucid/gateway-platform/internal/control/provider/codex"
	"github.com/elucid/gateway-platform/internal/control/provider/gemini"
	"github.com/elucid/gateway-platform/internal/control/provider/grok"
	"github.com/elucid/gateway-platform/pkg/contracts"
)

// BuildPlan is pure: it never contacts a provider and never writes either
// database. Secrets are retained only in the in-memory plan for Apply.
func BuildPlan(snapshot SourceSnapshot) (Plan, error) {
	if snapshot.CapturedAt.IsZero() {
		snapshot.CapturedAt = time.Now().UTC()
	}
	plan := Plan{SourceSystem: SourceSystem, Snapshot: snapshot}
	plan.Summary = Summary{
		SourceSystem: SourceSystem, SourceSnapshotAt: snapshot.CapturedAt,
		ProviderCounts: map[string]int{}, StatusCounts: map[string]int{},
		UnknownChannelTypes: map[string]int{}, RejectionReasons: map[string]int{},
		GroupCounts: map[string]int{}, SchemaWarnings: append([]string(nil), snapshot.SchemaWarnings...),
	}
	seen := make(map[string]bool)
	seenKeyDigests := make(map[string]bool)
	for _, channel := range snapshot.Channels {
		providerName, authMode, profileFactory, ok := channelProvider(channel.Type)
		if !ok {
			reason := fmt.Sprintf("unsupported channel type %d", channel.Type)
			plan.Summary.UnknownChannelTypes[strconv.Itoa(channel.Type)]++
			plan.addRejected(fmt.Sprintf("channel:%d", channel.ID), "account", reason, channelRawSummary(channel))
			continue
		}
		status, statusErr := mapStatus(channel.Status)
		if statusErr != nil {
			plan.addRejected(fmt.Sprintf("channel:%d", channel.ID), "account", statusErr.Error(), channelRawSummary(channel))
			continue
		}
		keys, keyErr := parseChannelKeys(channel.Key, providerName)
		if keyErr != nil {
			plan.addRejected(fmt.Sprintf("channel:%d", channel.ID), "account", keyErr.Error(), channelRawSummary(channel))
			continue
		}
		groups := splitGroups(channel.Group)
		keyStatuses := parseKeyStatuses(channel.ChannelInfo)
		for keyIndex, rawKey := range keys {
			keyDigest := digestString(rawKey)
			if seenKeyDigests[keyDigest] {
				plan.Summary.DuplicateSourceKeys++
			}
			seenKeyDigests[keyDigest] = true
			bundle, credKind, err := convertCredential(providerName, authMode, rawKey)
			if err != nil {
				plan.addRejected(fmt.Sprintf("channel:%d#key:%d", channel.ID, keyIndex), "account", err.Error(), channelRawSummary(channel))
				continue
			}
			if keyStatus, exists := keyStatuses[keyIndex]; exists {
				if _, statusErr := mapStatus(keyStatus); statusErr != nil {
					for _, group := range groups {
						sourceID := fmt.Sprintf("channel:%d#key:%d", channel.ID, keyIndex)
						if len(groups) > 1 {
							sourceID += "#group:" + group
						}
						plan.addRejected(sourceID, "account", fmt.Sprintf("unsupported per-key status %d", keyStatus), channelRawSummary(channel))
					}
					continue
				}
			}
			for _, group := range groups {
				sourceID := fmt.Sprintf("channel:%d", channel.ID)
				if len(keys) > 1 {
					sourceID += fmt.Sprintf("#key:%d", keyIndex)
				}
				if len(groups) > 1 {
					sourceID += "#group:" + group
				}
				if seen[sourceID] {
					plan.Summary.DuplicateSourceKeys++
					continue
				}
				seen[sourceID] = true
				accountID := importedAccountID(SourceSystem, sourceID)
				account := contracts.Account{ID: accountID, Provider: providerName, Platform: providerName, Group: group, Status: status, FenceEpoch: 1, Limits: contracts.AccountLimits{}.WithDefaults(), Quota: contracts.QuotaInfo{}}
				account.Credential = contracts.Credential{Kind: credKind, AccessToken: bundle.AccessToken, ExpiresAt: bundle.ExpiresAt}
				account.Profile = profileFactory(account)
				if channel.BaseURL != "" {
					account.Profile.BaseURL = channel.BaseURL
				}
				if account.Profile.Metadata == nil {
					account.Profile.Metadata = map[string]string{}
				}
				proxy := parseProxy(channel.Setting)
				if proxy != "" {
					account.Profile.Proxy = proxy
					account.Profile.Metadata["source_proxy"] = proxy
				}
				capabilities, manualReview := parseModels(channel.Models)
				account.Capabilities = capabilities
				conversion := map[string]any{"source_channel_id": channel.ID, "key_index": keyIndex, "auth_mode": authMode}
				if manualReview {
					conversion["manual_review"] = true
				}
				if keyStatus, ok := keyStatuses[keyIndex]; ok {
					account.Status, _ = mapStatus(keyStatus)
					conversion["per_key_status"] = keyStatus
				}
				modelMapping := json.RawMessage(`{}`)
				if strings.TrimSpace(channel.ModelMapping) != "" {
					if !json.Valid([]byte(channel.ModelMapping)) {
						conversion["manual_review"] = true
						conversion["model_mapping_invalid"] = true
					} else {
						modelMapping = json.RawMessage(channel.ModelMapping)
					}
				}
				raw := channelRawSummary(channel)
				raw["key_index"] = keyIndex
				raw["key_count"] = len(keys)
				raw["group"] = group
				planned := PlannedAccount{SourceID: sourceID, Account: account, Bundle: bundle, Proxy: proxy, ModelMapping: modelMapping, RawSummary: raw, Conversion: conversion}
				plan.Accounts = append(plan.Accounts, planned)
				plan.addImportedRecord(sourceID, "account", accountID, raw, conversion)
				plan.Summary.ProviderCounts[providerName]++
				plan.Summary.StatusCounts[account.Status]++
				plan.Summary.GroupCounts[group]++
			}
		}
	}
	plan.addSourceRecords(snapshot)
	plan.Summary.AccountCount = len(plan.Accounts)
	plan.Summary.RecordCount = len(plan.Records)
	plan.Summary.SourceDigest = digestJSON(struct {
		Channels []SourceChannel
		Users    []SourceUser
		Tokens   []SourceToken
		Quota    []SourceQuota
		Groups   []SourceGroup
	}{snapshot.Channels, snapshot.Users, snapshot.Tokens, snapshot.Quota, snapshot.Groups})
	return plan, nil
}

func channelProvider(kind int) (string, string, func(contracts.Account) contracts.UpstreamProfile, bool) {
	switch kind {
	case 14:
		p := claude.NewAPIKey(nil)
		return provider.KindClaude, provider.AuthModeAPIKey, p.Profile, true
	case 24:
		p := gemini.NewAPIKey(nil)
		return provider.KindGemini, provider.AuthModeAPIKey, p.Profile, true
	case 48:
		p := grok.NewAPIKey(nil)
		return provider.KindGrok, provider.AuthModeAPIKey, p.Profile, true
	case 57:
		p := codex.NewOAuth(nil)
		return provider.KindCodex, provider.AuthModeOAuth, p.Profile, true
	default:
		return "", "", nil, false
	}
}

func mapStatus(status int) (string, error) {
	switch status {
	case 1:
		return "active", nil
	case 2, 3:
		return "disabled", nil
	default:
		return "", fmt.Errorf("unsupported channel status %d", status)
	}
}

func parseChannelKeys(raw, providerName string) ([]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("channel key is empty")
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil || len(values) == 0 {
			return nil, errors.New("channel key JSON array is invalid")
		}
		out := make([]string, 0, len(values))
		for _, value := range values {
			if len(bytesTrim(value)) == 0 || string(bytesTrim(value)) == "null" {
				return nil, errors.New("channel key array contains an empty value")
			}
			var stringValue string
			if json.Unmarshal(value, &stringValue) == nil {
				if strings.TrimSpace(stringValue) == "" {
					return nil, errors.New("channel key array contains an empty value")
				}
				out = append(out, stringValue)
			} else {
				out = append(out, string(value))
			}
		}
		return out, nil
	}
	parts := strings.Split(strings.Trim(trimmed, "\n"), "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return nil, errors.New("channel key contains an empty line")
		}
		out = append(out, strings.TrimSpace(part))
	}
	if providerName == provider.KindCodex && len(out) != 1 {
		return nil, errors.New("codex OAuth key must be a single JSON object")
	}
	return out, nil
}

func convertCredential(providerName, authMode, raw string) (contracts.TokenBundle, string, error) {
	if providerName != provider.KindCodex {
		if strings.TrimSpace(raw) == "" {
			return contracts.TokenBundle{}, "", errors.New("static API key is empty")
		}
		return contracts.TokenBundle{AccessToken: raw}, "static", nil
	}
	if authMode != provider.AuthModeOAuth {
		return contracts.TokenBundle{}, "", errors.New("codex channel is not OAuth")
	}
	var key struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
		LastRefresh  string `json:"last_refresh"`
		Email        string `json:"email"`
		Expired      string `json:"expired"`
	}
	if err := json.Unmarshal([]byte(raw), &key); err != nil {
		return contracts.TokenBundle{}, "", errors.New("codex OAuth key JSON is invalid")
	}
	if strings.TrimSpace(key.AccessToken) == "" && strings.TrimSpace(key.RefreshToken) == "" {
		return contracts.TokenBundle{}, "", errors.New("codex OAuth key has no access or refresh token")
	}
	bundle := contracts.TokenBundle{AccessToken: key.AccessToken, RefreshToken: key.RefreshToken, IDToken: key.IDToken, AccountID: key.AccountID, Email: key.Email, Metadata: map[string]string{}}
	if key.LastRefresh != "" {
		bundle.Metadata["last_refresh"] = key.LastRefresh
	}
	if key.Expired != "" {
		if parsed, err := time.Parse(time.RFC3339, key.Expired); err == nil {
			bundle.ExpiresAt = parsed
		} else {
			bundle.Metadata["expired_raw"] = key.Expired
		}
	}
	if len(bundle.Metadata) == 0 {
		bundle.Metadata = nil
	}
	return bundle, "oauth", nil
}

func splitGroups(raw string) []string {
	seen := map[string]bool{}
	var groups []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !seen[part] {
			seen[part] = true
			groups = append(groups, part)
		}
	}
	if len(groups) == 0 {
		return []string{"default"}
	}
	return groups
}

func parseProxy(setting string) string {
	if strings.TrimSpace(setting) == "" {
		return ""
	}
	var value struct {
		Proxy string `json:"proxy"`
	}
	if json.Unmarshal([]byte(setting), &value) == nil {
		return strings.TrimSpace(value.Proxy)
	}
	return ""
}

func parseModels(raw string) ([]string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, false
	}
	if strings.HasPrefix(trimmed, "[") {
		var values []string
		if err := json.Unmarshal([]byte(trimmed), &values); err != nil {
			return nil, true
		}
		return cleanStrings(values), false
	}
	if strings.HasPrefix(trimmed, "{") {
		return nil, true
	}
	return cleanStrings(strings.Split(trimmed, ",")), false
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func parseKeyStatuses(raw string) map[int]int {
	var value struct {
		MultiKeyStatusList map[string]int `json:"multi_key_status_list"`
	}
	if json.Unmarshal([]byte(raw), &value) != nil {
		return nil
	}
	out := map[int]int{}
	for index, status := range value.MultiKeyStatusList {
		if parsed, err := strconv.Atoi(index); err == nil {
			out[parsed] = status
		}
	}
	return out
}

func channelRawSummary(channel SourceChannel) map[string]any {
	return map[string]any{
		"channel_id":           channel.ID,
		"channel_type":         channel.Type,
		"status":               channel.Status,
		"name":                 channel.Name,
		"base_url":             channel.BaseURL,
		"models":               channel.Models,
		"group":                channel.Group,
		"key_sha256":           digestString(channel.Key),
		"setting_proxy":        parseProxy(channel.Setting),
		"model_mapping_sha256": digestString(channel.ModelMapping),
		"used_quota_raw":       channel.UsedQuota,
		"balance_raw":          channel.Balance,
	}
}

func (p *Plan) addRejected(sourceID, kind, reason string, raw map[string]any) {
	p.Records = append(p.Records, MigrationRecord{SourceID: sourceID, RecordKind: kind, SourceDigest: digestJSON(raw), RawSummary: raw, Conversion: map[string]any{"rejected": true}, RejectionReason: reason, Status: "rejected"})
	p.Summary.RejectedCount++
	p.Summary.RejectionReasons[reason]++
}

func (p *Plan) addImportedRecord(sourceID, kind, targetID string, raw, conversion map[string]any) {
	p.Records = append(p.Records, MigrationRecord{SourceID: sourceID, RecordKind: kind, SourceDigest: digestJSON(raw), RawSummary: raw, Conversion: conversion, TargetID: targetID, Status: "imported"})
	p.Summary.ImportedCount++
}

func (p *Plan) addSourceRecords(snapshot SourceSnapshot) {
	for _, user := range snapshot.Users {
		raw := map[string]any{"user_id": user.ID, "username": user.Username, "email": user.Email, "status": user.Status, "quota_raw": user.Quota, "used_quota_raw": user.UsedQuota, "group": user.Group}
		p.addImportedRecord(fmt.Sprintf("user:%d", user.ID), "tenant", "tenant-"+strconv.FormatInt(user.ID, 10), raw, map[string]any{"staging_only": true, "default_principal_id": "principal-" + strconv.FormatInt(user.ID, 10)})
	}
	for _, token := range snapshot.Tokens {
		raw := map[string]any{"token_id": token.ID, "user_id": token.UserID, "status": token.Status, "expired_time_raw": token.ExpiredTime, "remain_quota_raw": token.RemainQuota, "group": token.Group, "key_sha256": digestString(token.Key)}
		p.addImportedRecord(fmt.Sprintf("token:%d", token.ID), "token", "token-"+strconv.FormatInt(token.ID, 10), raw, map[string]any{"staging_only": true, "revoked": token.Status != 1})
	}
	for _, quota := range snapshot.Quota {
		raw := map[string]any{"quota_id": quota.ID, "user_id": quota.UserID, "model": quota.Model, "created_at_raw": quota.CreatedAt, "token_used_raw": quota.TokenUsed, "count_raw": quota.Count, "quota_raw": quota.Quota, "group": quota.Group}
		p.addImportedRecord(fmt.Sprintf("quota:%d", quota.ID), "quota", "quota-"+strconv.FormatInt(quota.ID, 10), raw, map[string]any{"staging_only": true, "unit": "new-api-raw"})
	}
	for _, group := range snapshot.Groups {
		raw := map[string]any{"group_id": group.ID, "name": group.Name, "status": group.Status}
		p.addImportedRecord("group:"+group.ID, "group", "group-"+group.ID, raw, map[string]any{"staging_only": true})
	}
	if len(snapshot.Groups) == 0 {
		seen := map[string]bool{}
		for _, channel := range snapshot.Channels {
			for _, group := range splitGroups(channel.Group) {
				if seen[group] {
					continue
				}
				seen[group] = true
				raw := map[string]any{"name": group, "source": "channels.group"}
				p.addImportedRecord("group:"+group, "group", "group-"+group, raw, map[string]any{"staging_only": true})
			}
		}
	}
	p.Summary.QuotaRows = len(snapshot.Quota)
	p.Summary.QuotaRawDigest = digestJSON(snapshot.Quota)
}

func importedAccountID(sourceSystem, sourceID string) string {
	sum := sha256.Sum256([]byte(sourceSystem + "\x00" + sourceID))
	return "account-" + hex.EncodeToString(sum[:16])
}

func digestString(value string) string { return digestJSON(value) }
func digestJSON(value any) string {
	sum := sha256.Sum256(mustJSON(value))
	return hex.EncodeToString(sum[:])
}
func mustJSON(value any) []byte     { encoded, _ := json.Marshal(value); return encoded }
func bytesTrim(value []byte) []byte { return []byte(strings.TrimSpace(string(value))) }
