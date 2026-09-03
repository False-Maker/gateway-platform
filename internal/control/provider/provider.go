package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

// Provider is the control-plane boundary for channel-specific OAuth and quota
// behavior. Implementations stay isolated by provider package and are exposed
// through the registry.
type Provider interface {
	Kind() string
	Authorize(context.Context, contracts.ImportRequest) (contracts.TokenBundle, error)
	Refresh(context.Context, contracts.Credential) (contracts.TokenBundle, error)
	Revoke(context.Context, contracts.Credential) error
	Profile(contracts.Account) contracts.UpstreamProfile
	Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error)
}

// OAuthRefresher is an optional control-plane capability. TokenBundle is kept
// out of gateway leases because it may contain a refresh token.
type OAuthRefresher interface {
	RefreshOAuth(context.Context, contracts.TokenBundle) (contracts.TokenBundle, error)
}

type OAuthRevoker interface {
	RevokeOAuth(context.Context, contracts.TokenBundle) error
}

type AuthModeProvider interface {
	AuthMode() string
}

// ImportProfileProvider lets providers derive a public upstream profile from
// validated import metadata without putting routing metadata in credentials.
type ImportProfileProvider interface {
	ProfileForImport(contracts.Account, contracts.ImportRequest) (contracts.UpstreamProfile, error)
}

var ErrOAuthRefreshUnsupported = errors.New("provider does not support OAuth refresh")

// RefreshOAuth dispatches refresh-token rotation without widening the P0
// Provider contract or putting a refresh token in contracts.Credential.
func RefreshOAuth(ctx context.Context, p Provider, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	refresher, ok := p.(OAuthRefresher)
	if !ok {
		return contracts.TokenBundle{}, ErrOAuthRefreshUnsupported
	}
	if strings.TrimSpace(current.RefreshToken) == "" {
		return contracts.TokenBundle{}, fmt.Errorf("%w: missing refresh_token", contracts.ErrInvalidContract)
	}
	return refresher.RefreshOAuth(ctx, current)
}

func RevokeOAuth(ctx context.Context, p Provider, current contracts.TokenBundle) error {
	revoker, ok := p.(OAuthRevoker)
	if !ok {
		return contracts.ErrUnsupportedCapability
	}
	if strings.TrimSpace(current.RefreshToken) == "" && strings.TrimSpace(current.AccessToken) == "" {
		return fmt.Errorf("%w: missing revoke token", contracts.ErrInvalidContract)
	}
	return revoker.RevokeOAuth(ctx, current)
}

var registry = struct {
	sync.RWMutex
	items  map[string]Provider
	byAuth map[string]map[string]Provider
}{items: make(map[string]Provider), byAuth: make(map[string]map[string]Provider)}

func Register(p Provider) {
	if p == nil || p.Kind() == "" {
		return
	}
	registry.Lock()
	defer registry.Unlock()
	mode := ""
	if advertised, ok := p.(AuthModeProvider); ok {
		mode, _ = canonicalAuthMode(advertised.AuthMode())
		if mode != "" {
			if registry.byAuth[p.Kind()] == nil {
				registry.byAuth[p.Kind()] = make(map[string]Provider)
			}
			registry.byAuth[p.Kind()][mode] = p
		}
	}
	if _, exists := registry.items[p.Kind()]; !exists || mode == AuthModeOAuth || mode == "" {
		registry.items[p.Kind()] = p
	}
}

func Get(kind string) (Provider, bool) {
	registry.RLock()
	defer registry.RUnlock()
	p, ok := registry.items[kind]
	return p, ok
}

// GetForAuth resolves providers that expose both OAuth and API-key profiles
// without changing the stable Provider interface or provider kind.
func GetForAuth(kind, authMode string) (Provider, bool) {
	mode, ok := canonicalAuthMode(authMode)
	if !ok {
		return nil, false
	}
	registry.RLock()
	defer registry.RUnlock()
	if modes := registry.byAuth[kind]; modes != nil {
		if p, exists := modes[mode]; exists {
			return p, true
		}
	}
	p, exists := registry.items[kind]
	if !exists {
		return nil, false
	}
	if advertised, typed := p.(AuthModeProvider); typed {
		providerMode, valid := canonicalAuthMode(advertised.AuthMode())
		if !valid || providerMode != mode {
			return nil, false
		}
	}
	return p, true
}

func canonicalAuthMode(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case AuthModeAPIKey, "static":
		return AuthModeAPIKey, true
	case AuthModeOAuth, "pkce", "device", "cli":
		return AuthModeOAuth, true
	default:
		return "", false
	}
}
