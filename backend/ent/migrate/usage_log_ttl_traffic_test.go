package migrate

import (
	"testing"

	"entgo.io/ent/schema/field"
	"github.com/stretchr/testify/require"
)

// Adding TTL columns and the fork's traffic JSON must not leave generated
// foreign keys or indexes pointing at a neighboring column.
func TestUsageLogTTLAndTrafficSchemaReferences(t *testing.T) {
	for _, name := range []string{"cache_creation_30m_tokens", "cache_creation_12h_tokens", "traffic"} {
		found := false
		for _, column := range UsageLogsColumns {
			if column.Name != name {
				continue
			}
			found = true
			if name == "traffic" {
				require.Equal(t, field.TypeJSON, column.Type)
				require.True(t, column.Nullable)
				require.Nil(t, column.Default, "historical traffic must remain unknown")
			} else {
				require.Equal(t, field.TypeInt, column.Type)
				require.Equal(t, 0, column.Default)
			}
		}
		require.True(t, found, "missing column %s", name)
	}
	for symbol, column := range map[string]string{
		"usage_logs_api_keys_usage_logs":           "api_key_id",
		"usage_logs_accounts_usage_logs":           "account_id",
		"usage_logs_groups_usage_logs":             "group_id",
		"usage_logs_users_usage_logs":              "user_id",
		"usage_logs_user_subscriptions_usage_logs": "subscription_id",
	} {
		fk := findForeignKeyBySymbol(t, UsageLogsTable, symbol)
		require.Len(t, fk.Columns, 1)
		require.Equal(t, column, fk.Columns[0].Name, symbol)
	}
	for name, expected := range map[string][]string{
		"usagelog_user_id":               {"user_id"},
		"usagelog_api_key_id":            {"api_key_id"},
		"usagelog_account_id":            {"account_id"},
		"usagelog_group_id":              {"group_id"},
		"usagelog_subscription_id":       {"subscription_id"},
		"usagelog_created_at":            {"created_at"},
		"usagelog_user_id_created_at":    {"user_id", "created_at"},
		"usagelog_api_key_id_created_at": {"api_key_id", "created_at"},
		"usagelog_group_id_created_at":   {"group_id", "created_at"},
	} {
		index := findIndexByName(t, UsageLogsTable, name)
		actual := make([]string, len(index.Columns))
		for i, column := range index.Columns {
			actual[i] = column.Name
		}
		require.Equal(t, expected, actual, name)
	}
}
