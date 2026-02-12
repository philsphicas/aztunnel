package relay

import (
	"context"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

// mockTokenCredential implements azcore.TokenCredential for testing.
type mockTokenCredential struct {
	token     string
	lastScope string
}

func (m *mockTokenCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(opts.Scopes) > 0 {
		m.lastScope = opts.Scopes[0]
	}
	return azcore.AccessToken{
		Token:     m.token,
		ExpiresOn: time.Now().Add(time.Hour),
	}, nil
}
