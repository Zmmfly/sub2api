-- A single B-tree serves both sort directions. The time key lets PostgreSQL
-- check the usage window during the index scan before fetching the log row.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_usage_logs_traffic_sort
    ON usage_logs ((COALESCE(usage_log_total_traffic_bytes(traffic), 0)), id, created_at);
