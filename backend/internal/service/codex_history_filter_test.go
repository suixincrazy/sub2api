package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type failingCodexHistorySettingRepo struct {
	*panelRateLimitSettingRepo
}

func (r *failingCodexHistorySettingRepo) Set(context.Context, string, string) error {
	return errors.New("database unavailable")
}

func TestCodexHistorySettingsPersistAndToggleImmediately(t *testing.T) {
	ctx := context.Background()
	repo := &panelRateLimitSettingRepo{}
	svc := NewSettingService(repo, nil)
	status, err := svc.GetCodexHistoryFilterStatus(ctx)
	require.NoError(t, err)
	require.False(t, status.Enabled)
	for _, enabled := range []bool{true, true, false, true} {
		require.NoError(t, svc.SetCodexHistoryFilterEnabled(ctx, enabled))
		status, err = svc.GetCodexHistoryFilterStatus(ctx)
		require.NoError(t, err)
		require.Equal(t, enabled, status.Enabled)
		status, err = NewSettingService(repo, nil).GetCodexHistoryFilterStatus(ctx)
		require.NoError(t, err)
		require.Equal(t, enabled, status.Enabled, "a new process must recover the saved setting")
	}
	svc.RecordCodexHistoryFiltered(2, 3)
	status, err = NewSettingService(repo, nil).GetCodexHistoryFilterStatus(ctx)
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.Zero(t, status.Stats.FilteredRequests, "runtime counters reset on restart")
}

func TestCodexHistorySettingsErrorsDoNotDisableProtection(t *testing.T) {
	ctx := context.Background()
	repo := &panelRateLimitSettingRepo{values: map[string]string{SettingKeyCodexHistoryFilterEnabled: "true"}}
	svc := NewSettingService(&failingCodexHistorySettingRepo{repo}, nil)
	enabled, err := svc.CodexHistoryFilterEnabled(ctx)
	require.NoError(t, err)
	require.True(t, enabled)
	require.Error(t, svc.SetCodexHistoryFilterEnabled(ctx, false))
	enabled, err = svc.CodexHistoryFilterEnabled(ctx)
	require.NoError(t, err)
	require.True(t, enabled)
	svc.codexHistoryFilter.expiresAt = time.Time{}
	repo.getValueErr = errors.New("offline")
	_, err = svc.CodexHistoryFilterEnabled(ctx)
	require.Error(t, err)
	repo.getValueErr = nil
	repo.values[SettingKeyCodexHistoryFilterEnabled] = "invalid"
	_, err = svc.CodexHistoryFilterEnabled(ctx)
	require.Error(t, err)
}

func TestCodexHistoryStatsConcurrentAndSnapshotsImmutable(t *testing.T) {
	svc := NewSettingService(nil, nil)
	var workers sync.WaitGroup
	for i := 0; i < 100; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			svc.RecordCodexHistoryFiltered(2, 3)
			svc.RecordCodexHistoryBlocked()
			svc.RecordCodexHistoryResponse(429, true)
		}()
	}
	workers.Wait()
	status, err := svc.GetCodexHistoryFilterStatus(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 100, status.Stats.Requests)
	require.EqualValues(t, 100, status.Stats.FilteredRequests)
	require.EqualValues(t, 200, status.Stats.RemovedReasoningItems)
	require.EqualValues(t, 300, status.Stats.RemovedItemIDs)
	require.EqualValues(t, 100, status.Stats.BlockedRequests)
	require.EqualValues(t, 100, status.Stats.UpstreamErrors)
	require.EqualValues(t, 100, status.Stats.UpstreamHTTPErrors)
	require.Equal(t, 429, *status.Stats.LastUpstreamStatus)
	require.NotNil(t, status.Stats.LastFilteredAt)
	svc.RecordCodexHistoryResponse(200, false)
	require.Equal(t, 429, *status.Stats.LastUpstreamStatus)
}
