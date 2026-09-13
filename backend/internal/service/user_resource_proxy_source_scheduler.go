package service

import (
	"context"
	"database/sql"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	userProxySourceSchedulerBatchSize = 20
	userProxySourceSyncTimeout        = 3 * time.Minute
)

type dueUserProxySource struct {
	ID      int64
	OwnerID sql.NullInt64
}

func (s *UserResourceService) StartProxySourceScheduler(interval time.Duration) {
	if s == nil || s.db == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	s.proxySourceSchedMu.Lock()
	if s.proxySourceCancel != nil {
		s.proxySourceSchedMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.proxySourceCancel = cancel
	s.proxySourceDone = done
	s.proxySourceSchedMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.runDueProxySourceSyncs(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runDueProxySourceSyncs(ctx)
			}
		}
	}()
}

func (s *UserResourceService) Close() error {
	if s == nil {
		return nil
	}
	s.proxySourceSchedMu.Lock()
	cancel := s.proxySourceCancel
	done := s.proxySourceDone
	s.proxySourceCancel = nil
	s.proxySourceDone = nil
	s.proxySourceSchedMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.LegacyPrintf("service.user_resources", "proxy source scheduler shutdown timed out")
		}
	}
	s.stopProxyQualityWorkers()
	return nil
}

func (s *UserResourceService) runDueProxySourceSyncs(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_user_id
FROM proxy_sources
WHERE deleted_at IS NULL
  AND sync_enabled
  AND (last_synced_at IS NULL OR last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute') <= NOW())
  AND (last_sync_status <> 'syncing' OR updated_at < NOW() - INTERVAL '10 minutes')
ORDER BY COALESCE(last_synced_at, created_at) ASC
LIMIT $1`, userProxySourceSchedulerBatchSize)
	if err != nil {
		logger.LegacyPrintf("service.user_resources", "list due proxy sources failed: %v", err)
		return
	}
	sources := make([]dueUserProxySource, 0, userProxySourceSchedulerBatchSize)
	for rows.Next() {
		var source dueUserProxySource
		if err := rows.Scan(&source.ID, &source.OwnerID); err != nil {
			_ = rows.Close()
			logger.LegacyPrintf("service.user_resources", "scan due proxy source failed: %v", err)
			return
		}
		sources = append(sources, source)
	}
	if err := rows.Close(); err != nil {
		logger.LegacyPrintf("service.user_resources", "close due proxy source rows failed: %v", err)
	}
	if err := rows.Err(); err != nil {
		logger.LegacyPrintf("service.user_resources", "iterate due proxy sources failed: %v", err)
		return
	}

	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		claimed, err := s.claimDueProxySource(ctx, source)
		if err != nil {
			logger.LegacyPrintf("service.user_resources", "claim proxy source id=%d failed: %v", source.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, userProxySourceSyncTimeout)
		syncErr := s.syncDueProxySource(syncCtx, source)
		cancel()
		if syncErr != nil {
			s.markScheduledProxySourceFailure(source, syncErr)
		}
	}
}

func (s *UserResourceService) syncDueProxySource(ctx context.Context, source dueUserProxySource) error {
	return syncDueProxySourceWith(
		ctx,
		source,
		func(ctx context.Context, sourceID int64) error {
			_, err := s.SyncSystemProxySource(ctx, sourceID)
			return err
		},
		func(ctx context.Context, ownerID, sourceID int64) error {
			_, err := s.SyncProxySource(ctx, ownerID, sourceID)
			return err
		},
	)
}

func syncDueProxySourceWith(
	ctx context.Context,
	source dueUserProxySource,
	syncSystem func(context.Context, int64) error,
	syncUser func(context.Context, int64, int64) error,
) error {
	if source.OwnerID.Valid {
		return syncUser(ctx, source.OwnerID.Int64, source.ID)
	}
	return syncSystem(ctx, source.ID)
}

func dueProxySourceOwnerValue(ownerID sql.NullInt64) any {
	if !ownerID.Valid {
		return nil
	}
	return ownerID.Int64
}
func (s *UserResourceService) claimDueProxySource(ctx context.Context, source dueUserProxySource) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_sync_status = 'syncing', last_sync_error = NULL, updated_at = NOW()
WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
  AND sync_enabled
  AND (last_synced_at IS NULL OR last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute') <= NOW())
  AND (last_sync_status <> 'syncing' OR updated_at < NOW() - INTERVAL '10 minutes')`, source.ID, dueProxySourceOwnerValue(source.OwnerID))
	if err != nil {
		return false, err
	}
	return affected(result) == 1, nil
}

func (s *UserResourceService) markScheduledProxySourceFailure(source dueUserProxySource, syncErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_synced_at = NOW(), last_sync_status = 'error', last_sync_error = $1, last_imported_count = 0, updated_at = NOW()
WHERE id = $2 AND owner_user_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL`, safeSyncError(syncErr), source.ID, dueProxySourceOwnerValue(source.OwnerID))
	if err != nil {
		logger.LegacyPrintf("service.user_resources", "mark proxy source id=%d failure failed: %v", source.ID, err)
	}
}
