package claude

import (
	"context"

	"github.com/elucid/gateway-platform/pkg/contracts"
)

// Quota reads the optional, verified Claude quota endpoint. The fixture
// response is normalized by the shared provider boundary into QuotaInfo.
func (p *Provider) Quota(ctx context.Context, credential contracts.Credential) (contracts.QuotaInfo, error) {
	endpoint, err := p.Config.QuotaURL()
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	if endpoint == "" {
		return contracts.QuotaInfo{}, nil
	}
	headers, err := p.Config.InferenceHeaders(credential.AccessToken)
	if err != nil {
		return contracts.QuotaInfo{}, err
	}
	return p.HTTP.FetchQuota(ctx, endpoint, headers)
}
