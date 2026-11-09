package repository

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/traffic"
	"github.com/stretchr/testify/require"
)

// Keep the new OpenAI TTL counters distinct from the older cache counters and
// nullable traffic JSON across both parameter preparation and SQL scanning.
func TestUsageLogCacheWriteTTLAndTrafficRoundTrip(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(strconv.FormatBool(observed), func(t *testing.T) {
			var observation *traffic.Usage
			if observed {
				observation = trafficTestUsage()
			}
			log := trafficTestLog(observation)
			log.CacheCreation5mTokens = 11
			log.CacheCreation1hTokens = 23
			log.CacheCreation30mTokens = 37
			log.CacheCreation12hTokens = 53
			log.CacheCreationTokens = 124
			upstreamID, sessionID := "upstream-ttl-traffic", "session-ttl-traffic"
			log.UpstreamRequestID, log.SessionID = &upstreamID, &sessionID
			prepared := prepareUsageLogInsert(log)
			columns := usageInsertColumns()
			require.Len(t, prepared.args, len(columns))
			for name, want := range map[string]int{
				"cache_creation_tokens":     124,
				"cache_creation_5m_tokens":  11,
				"cache_creation_1h_tokens":  23,
				"cache_creation_30m_tokens": 37,
				"cache_creation_12h_tokens": 53,
			} {
				idx := slices.Index(columns, name)
				require.NotEqual(t, -1, idx, "missing column %s", name)
				require.Equal(t, "integer", usageLogInsertArgTypes[idx], name)
				require.Equal(t, want, prepared.args[idx], name)
			}

			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			values := append([]driver.Value{int64(71)}, anySliceToDriverValues(prepared.args)...)
			query := "SELECT " + usageLogSelectColumns + " FROM usage_logs WHERE id = $1"
			mock.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(int64(71)).
				WillReturnRows(sqlmock.NewRows(strings.Split(usageLogSelectColumns, ", ")).AddRow(values...))
			got, err := scanUsageLog(db.QueryRowContext(context.Background(), query, int64(71)))
			require.NoError(t, err)
			require.Equal(t, log.CacheCreationTokens, got.CacheCreationTokens)
			require.Equal(t, log.CacheCreation5mTokens, got.CacheCreation5mTokens)
			require.Equal(t, log.CacheCreation1hTokens, got.CacheCreation1hTokens)
			require.Equal(t, log.CacheCreation30mTokens, got.CacheCreation30mTokens)
			require.Equal(t, log.CacheCreation12hTokens, got.CacheCreation12hTokens)
			require.Equal(t, observation, got.Traffic)
			require.Equal(t, log.UpstreamRequestID, got.UpstreamRequestID)
			require.Equal(t, log.SessionID, got.SessionID)
			require.True(t, got.NativeCompactionV2)
			require.Equal(t, log.CreatedAt, got.CreatedAt)
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func TestUsageLogCacheWriteTTLAndTrafficBatchSnapshot(t *testing.T) {
	columns := usageInsertColumns()
	shortIdx := slices.Index(columns, "cache_creation_30m_tokens")
	longIdx := slices.Index(columns, "cache_creation_12h_tokens")
	trafficIdx := slices.Index(columns, "traffic")
	for _, idx := range []int{shortIdx, longIdx, trafficIdx} {
		require.NotEqual(t, -1, idx)
	}

	observed := trafficTestLog(trafficTestUsage())
	observed.CacheCreation30mTokens, observed.CacheCreation12hTokens = 9, 2390
	first := prepareUsageLogInsert(observed)
	historical := trafficTestLog(nil)
	historical.RequestID = "historical-ttl-traffic"
	second := prepareUsageLogInsert(historical)
	// The queue must own its prepared snapshot even if the original is reused.
	observed.CacheCreation30mTokens, observed.CacheCreation12hTokens = 0, 0
	*observed.Traffic.Downstream.Request.ActualBytes = 999

	keys := []string{usageLogBatchKey(first.requestID, observed.APIKeyID), usageLogBatchKey(second.requestID, historical.APIKeyID)}
	for _, batch := range []bool{false, true} {
		t.Run(strconv.FormatBool(batch), func(t *testing.T) {
			var query string
			var args []any
			offset := 0
			if batch {
				query, args = buildUsageLogBatchInsertQuery(keys, map[string]usageLogInsertPrepared{keys[0]: first, keys[1]: second})
				offset = 1
			} else {
				query, args = buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{first, second})
			}
			width := len(usageLogInsertArgTypes) + offset
			require.Len(t, args, 2*width)
			require.Equal(t, first.args, args[offset:width])
			require.Equal(t, second.args, args[width+offset:])
			require.Equal(t, 9, args[offset+shortIdx])
			require.Equal(t, 2390, args[offset+longIdx])
			require.Equal(t, 0, args[width+offset+shortIdx])
			require.Equal(t, 0, args[width+offset+longIdx])
			require.Nil(t, args[width+offset+trafficIdx])
			var snapshot traffic.Usage
			trafficJSON, ok := args[offset+trafficIdx].(string)
			require.True(t, ok, "observed traffic should be encoded as JSON text")
			require.NoError(t, json.Unmarshal([]byte(trafficJSON), &snapshot))
			require.Equal(t, trafficTestUsage(), &snapshot)
			for row := 0; row < 2; row++ {
				for col, kind := range usageLogInsertArgTypes {
					placeholder := "$" + strconv.Itoa(row*width+offset+col+1) + "::" + kind
					require.Contains(t, query, placeholder)
				}
			}
		})
	}
}
