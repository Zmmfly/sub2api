package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Stored in ops_metrics_hourly.traffic. These are additive sufficient statistics,
// not averages of per-request ratios. A NULL column means the bucket predates
// traffic aggregation and must not be mistaken for an observed empty bucket.
type opsTrafficAggregate struct {
	RequestCount int64                   `json:"request_count"`
	Upstream     opsTrafficLinkAggregate `json:"upstream"`
	Downstream   opsTrafficLinkAggregate `json:"downstream"`
}

type opsTrafficLinkAggregate struct {
	OriginalBytes       *int64 `json:"original_bytes"`
	ActualBytes         *int64 `json:"actual_bytes"`
	OriginalSampleCount int64  `json:"original_sample_count"`
	ActualSampleCount   int64  `json:"actual_sample_count"`
	PairedSampleCount   int64  `json:"paired_sample_count"`
	PairedOriginalBytes *int64 `json:"paired_original_bytes"`
	PairedActualBytes   *int64 `json:"paired_actual_bytes"`
}

func (s *opsTrafficAggregate) add(other opsTrafficAggregate) {
	s.RequestCount += other.RequestCount
	s.Upstream.add(other.Upstream)
	s.Downstream.add(other.Downstream)
}

func (s *opsTrafficLinkAggregate) add(other opsTrafficLinkAggregate) {
	addNullableTrafficBytes(&s.OriginalBytes, other.OriginalBytes)
	addNullableTrafficBytes(&s.ActualBytes, other.ActualBytes)
	addNullableTrafficBytes(&s.PairedOriginalBytes, other.PairedOriginalBytes)
	addNullableTrafficBytes(&s.PairedActualBytes, other.PairedActualBytes)
	s.OriginalSampleCount += other.OriginalSampleCount
	s.ActualSampleCount += other.ActualSampleCount
	s.PairedSampleCount += other.PairedSampleCount
}

func addNullableTrafficBytes(dst **int64, src *int64) {
	if src == nil {
		return
	}
	value := *src
	if *dst != nil {
		value += **dst
	}
	*dst = &value
}

func (s opsTrafficAggregate) summary() *service.OpsTrafficSummary {
	return &service.OpsTrafficSummary{
		Source: "http_requests", RequestCount: s.RequestCount,
		Upstream: s.Upstream.summary(), Downstream: s.Downstream.summary(),
	}
}

func (s opsTrafficLinkAggregate) summary() service.OpsTrafficLink {
	out := service.OpsTrafficLink{
		OriginalBytes: s.OriginalBytes, ActualBytes: s.ActualBytes,
		OriginalSampleCount: s.OriginalSampleCount, ActualSampleCount: s.ActualSampleCount,
		PairedSampleCount: s.PairedSampleCount,
	}
	if s.PairedSampleCount > 0 && s.PairedOriginalBytes != nil && *s.PairedOriginalBytes > 0 && s.PairedActualBytes != nil {
		percent := (1 - float64(*s.PairedActualBytes)/float64(*s.PairedOriginalBytes)) * 100
		out.CompressionPercent = &percent
	}
	return out
}

// opsTrafficExtractSQL returns one scalar projection per measured byte field.
// Missing directions, missing fields and JSON null all stay SQL NULL. Do not
// coalesce these values to zero: zero is an actual measured empty body.
func opsTrafficExtractSQL(alias string) string {
	fields := make([]string, 0, 8)
	for _, side := range []string{"upstream", "downstream"} {
		for _, direction := range []string{"request", "response"} {
			for _, kind := range []string{"original", "actual"} {
				fields = append(fields, fmt.Sprintf("(%s.traffic #>> '{%s,%s,%s_bytes}')::bigint AS traffic_%s_%s_%s",
					alias, side, direction, kind, side, direction, kind))
			}
		}
	}
	return strings.Join(fields, ",\n    ")
}

// opsTrafficAggregateSQL is shared by raw and hourly queries. COALESCE is safe
// only after the explicit observation test, or for counts: an all-unknown byte
// population must yield NULL, whereas a measured zero must remain zero.
func opsTrafficAggregateSQL() string {
	links := make([]string, 0, 2)
	for _, side := range []string{"upstream", "downstream"} {
		reqOriginal := "traffic_" + side + "_request_original"
		reqActual := "traffic_" + side + "_request_actual"
		respOriginal := "traffic_" + side + "_response_original"
		respActual := "traffic_" + side + "_response_actual"
		reqPair := "(" + reqOriginal + " IS NOT NULL AND " + reqActual + " IS NOT NULL)"
		respPair := "(" + respOriginal + " IS NOT NULL AND " + respActual + " IS NOT NULL)"
		sumKnown := func(a, b string) string {
			return fmt.Sprintf("SUM(CASE WHEN %s IS NOT NULL OR %s IS NOT NULL THEN COALESCE(%s, 0)::numeric + COALESCE(%s, 0)::numeric END)", a, b, a, b)
		}
		countKnown := func(a, b string) string {
			return fmt.Sprintf("COALESCE(SUM((%s IS NOT NULL)::int + (%s IS NOT NULL)::int), 0)", a, b)
		}
		sumPaired := func(a, b string) string {
			return fmt.Sprintf("SUM(CASE WHEN %s OR %s THEN (CASE WHEN %s THEN %s::numeric ELSE 0 END) + (CASE WHEN %s THEN %s::numeric ELSE 0 END) END)", reqPair, respPair, reqPair, a, respPair, b)
		}
		links = append(links, fmt.Sprintf(`'%s', jsonb_build_object(
      'original_bytes', %s,
      'actual_bytes', %s,
      'original_sample_count', %s,
      'actual_sample_count', %s,
      'paired_sample_count', COALESCE(SUM(%s::int + %s::int), 0),
      'paired_original_bytes', %s,
      'paired_actual_bytes', %s
    )`, side, sumKnown(reqOriginal, respOriginal), sumKnown(reqActual, respActual),
			countKnown(reqOriginal, respOriginal), countKnown(reqActual, respActual), reqPair, respPair,
			sumPaired(reqOriginal, respOriginal), sumPaired(reqActual, respActual)))
	}
	return "jsonb_build_object('request_count', COUNT(*),\n    " + strings.Join(links, ",\n    ") + ")"
}

// Ledger dimensions are frozen at finalization, not re-derived from mutable
// groups/accounts. Unresolved routing dimensions remain NULL and contribute to
// the global population (or the explicit "unknown" platform sentinel).
func buildTrafficWhere(filter *service.OpsDashboardFilter, start, end time.Time) (string, []any) {
	where := "WHERE te.created_at >= $1 AND te.created_at < $2"
	args := []any{start.UTC(), end.UTC()}
	if filter != nil {
		if filter.GroupID != nil && *filter.GroupID > 0 {
			args = append(args, *filter.GroupID)
			where += fmt.Sprintf(" AND te.group_id = $%d", len(args))
		}
		if platform := strings.TrimSpace(strings.ToLower(filter.Platform)); platform != "" {
			args = append(args, platform)
			if platform == "unknown" {
				where += fmt.Sprintf(" AND (te.platform = $%d OR te.platform IS NULL OR te.platform = '')", len(args))
			} else {
				where += fmt.Sprintf(" AND te.platform = $%d", len(args))
			}
		}
	}
	return where, args
}

type opsTimeRange struct {
	start time.Time
	end   time.Time
}

func restrictOpsMissingRanges(where string, args []any, column string, gaps []opsTimeRange) (string, []any) {
	// The outer start/end already restrict a single contiguous gap.
	if len(gaps) <= 1 {
		return where, args
	}
	clauses := make([]string, 0, len(gaps))
	for _, gap := range gaps {
		idx := len(args) + 1
		clauses = append(clauses, fmt.Sprintf("(%s >= $%d AND %s < $%d)", column, idx, column, idx+1))
		args = append(args, gap.start.UTC(), gap.end.UTC())
	}
	return where + " AND (" + strings.Join(clauses, " OR ") + ")", args
}

func (r *opsRepository) rawTrafficDataExists(ctx context.Context, filter *service.OpsDashboardFilter, start, end time.Time, gaps ...opsTimeRange) (bool, error) {
	where, args := buildTrafficWhere(filter, start, end)
	where, args = restrictOpsMissingRanges(where, args, "te.created_at", gaps)
	var exists bool
	err := r.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM ops_traffic_events te "+where+" LIMIT 1)", args...).Scan(&exists)
	return exists, err
}

func (r *opsRepository) queryHTTPTraffic(ctx context.Context, filter *service.OpsDashboardFilter, start, end time.Time) (opsTrafficAggregate, error) {
	where, args := buildTrafficWhere(filter, start, end)
	query := `WITH traffic_base AS (
  SELECT ` + opsTrafficExtractSQL("te") + `
  FROM ops_traffic_events te
  ` + where + `
)
SELECT ` + opsTrafficAggregateSQL() + ` FROM traffic_base`
	var data []byte
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&data); err != nil {
		return opsTrafficAggregate{}, err
	}
	var stats opsTrafficAggregate
	if err := json.Unmarshal(data, &stats); err != nil {
		return opsTrafficAggregate{}, fmt.Errorf("decode ops traffic aggregate: %w", err)
	}
	return stats, nil
}
