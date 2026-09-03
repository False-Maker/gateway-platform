package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

func TestRefreshOAuthUsesControlPlaneTokenBundle(t *testing.T) {
	p := &oauthProviderStub{}
	got, err := RefreshOAuth(context.Background(), p, contracts.TokenBundle{RefreshToken: "refresh-token"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.called || got.AccessToken != "new-access-token" {
		t.Fatalf("OAuth refresher was not called: %#v", got)
	}
}

func TestRefreshOAuthRejectsMissingCapabilityOrToken(t *testing.T) {
	if _, err := RefreshOAuth(context.Background(), providerStub{}, contracts.TokenBundle{RefreshToken: "refresh-token"}); !errors.Is(err, ErrOAuthRefreshUnsupported) {
		t.Fatalf("got %v, want %v", err, ErrOAuthRefreshUnsupported)
	}
	if _, err := RefreshOAuth(context.Background(), &oauthProviderStub{}, contracts.TokenBundle{}); !errors.Is(err, contracts.ErrInvalidContract) {
		t.Fatalf("got %v, want invalid contract", err)
	}
}

type providerStub struct{}

func (providerStub) Kind() string { return "stub" }
func (providerStub) Authorize(context.Context, contracts.ImportRequest) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, nil
}
func (providerStub) Refresh(context.Context, contracts.Credential) (contracts.TokenBundle, error) {
	return contracts.TokenBundle{}, nil
}
func (providerStub) Revoke(context.Context, contracts.Credential) error { return nil }
func (providerStub) Profile(contracts.Account) contracts.UpstreamProfile {
	return contracts.UpstreamProfile{}
}
func (providerStub) Quota(context.Context, contracts.Credential) (contracts.QuotaInfo, error) {
	return contracts.QuotaInfo{}, nil
}

type oauthProviderStub struct {
	providerStub
	called bool
}

func (p *oauthProviderStub) RefreshOAuth(_ context.Context, current contracts.TokenBundle) (contracts.TokenBundle, error) {
	p.called = true
	return contracts.TokenBundle{AccessToken: "new-access-token", RefreshToken: current.RefreshToken}, nil
}
