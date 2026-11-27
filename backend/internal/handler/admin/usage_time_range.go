package admin

import (
	"errors"
	"strings"
	"time"
)

// parseUsageTimestampRange accepts an optional pair of RFC3339 bounds for a half-open range.
func parseUsageTimestampRange(rawStart, rawEnd string) (*time.Time, *time.Time, error) {
	rawStart, rawEnd = strings.TrimSpace(rawStart), strings.TrimSpace(rawEnd)
	if rawStart == "" && rawEnd == "" {
		return nil, nil, nil
	}
	if rawStart == "" || rawEnd == "" {
		return nil, nil, errors.New("start_time and end_time are required together")
	}
	start, err := time.Parse(time.RFC3339Nano, rawStart)
	if err != nil {
		return nil, nil, errors.New("invalid start_time format, use RFC3339")
	}
	end, err := time.Parse(time.RFC3339Nano, rawEnd)
	if err != nil {
		return nil, nil, errors.New("invalid end_time format, use RFC3339")
	}
	if !start.Before(end) {
		return nil, nil, errors.New("start_time must be before end_time")
	}
	return &start, &end, nil
}
