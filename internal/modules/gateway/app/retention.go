package app

import (
	"context"
	"time"

	"github.com/ai-pipes/mcp-registry/internal/modules/gateway/domain"
)

// PruneResult is what a retention run deleted.
type PruneResult struct {
	Calls     int64
	QuotaDays int64
}

// PruneGatewayHistory deletes call-log rows older than callLogRetentionDays plain 24-hour days
// before now, then the quota counters of days before yesterday (UTC; yesterday and today stay).
// The log goes first: when it fails the counters are not touched.
func PruneGatewayHistory(ctx context.Context, d Deps, callLogRetentionDays int) (PruneResult, error) {
	now := d.Clock.Now()
	cutoff := now.Add(-time.Duration(callLogRetentionDays) * 24 * time.Hour)
	calls, err := d.CallLog.DeleteOlderThan(ctx, cutoff)
	if err != nil {
		return PruneResult{}, err
	}
	days, err := d.Quota.DeleteDaysBefore(ctx, domain.QuotaOldestKept(now))
	if err != nil {
		return PruneResult{}, err
	}
	return PruneResult{Calls: calls, QuotaDays: days}, nil
}
