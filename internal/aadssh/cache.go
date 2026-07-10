package aadssh

import (
	"context"
	"os"

	"github.com/AzureAD/microsoft-authentication-library-for-go/apps/cache"
)

// fileTokenCache is a minimal file-backed implementation of
// cache.ExportReplace so that MSAL refresh tokens persist across separate
// `aztunnel arc aad-cert` invocations, enabling silent (non-interactive)
// certificate renewal.
type fileTokenCache struct {
	path string
}

func newFileTokenCache(path string) *fileTokenCache {
	return &fileTokenCache{path: path}
}

// Replace loads the cached token data into MSAL's in-memory cache. A missing
// file is treated as an empty cache.
func (c *fileTokenCache) Replace(ctx context.Context, unmarshal cache.Unmarshaler, _ cache.ReplaceHints) error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return unmarshal.Unmarshal(data)
}

// Export writes MSAL's in-memory cache back to disk with 0600 permissions. The
// write is atomic so a concurrent invocation never reads a truncated cache.
func (c *fileTokenCache) Export(ctx context.Context, marshal cache.Marshaler, _ cache.ExportHints) error {
	data, err := marshal.Marshal()
	if err != nil {
		return err
	}
	return writeFileAtomic(c.path, data, 0o600)
}
