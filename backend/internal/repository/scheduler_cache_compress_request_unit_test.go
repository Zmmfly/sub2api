//go:build unit

package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/reqcompress"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestSchedulerCachePreservesCompressRequestEnabled pins the account snapshot's
// explicit field list: the full and metadata payloads must both carry the
// per-account request compression switch, and decoding either one must refresh
// the process-lifetime negotiation registry.
func TestSchedulerCachePreservesCompressRequestEnabled(t *testing.T) {
	const accountID int64 = 9003
	account := service.Account{
		ID:                     accountID,
		Name:                   "compress-request-snapshot",
		Platform:               service.PlatformOpenAI,
		Type:                   service.AccountTypeAPIKey,
		Status:                 service.StatusActive,
		Schedulable:            true,
		CompressRequestEnabled: true,
		UpdatedAt:              time.Now().UTC(),
	}
	t.Cleanup(func() { reqcompress.SyncAccount(accountID, false, time.Time{}) })

	full, meta, err := marshalSchedulerCacheAccount(account)
	require.NoError(t, err)

	for name, payload := range map[string][]byte{"full": full, "metadata": meta} {
		decoded, decodeErr := decodeCachedAccount(payload)
		require.NoError(t, decodeErr, name)
		require.True(t, decoded.CompressRequestEnabled, name)
		require.True(t, reqcompress.Enabled(accountID), name)
	}

	// A lagging payload written before the admin switched the account off must
	// not re-enable it.
	stale := account
	stale.UpdatedAt = account.UpdatedAt.Add(-time.Minute)
	_, staleMeta, err := marshalSchedulerCacheAccount(stale)
	require.NoError(t, err)
	reqcompress.SyncAccount(accountID, false, account.UpdatedAt)
	_, err = decodeCachedAccount(staleMeta)
	require.NoError(t, err)
	require.False(t, reqcompress.Enabled(accountID))
}
