package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/codexhistory"
)

const SettingKeyCodexHistoryFilterEnabled = "codex_history_filter_enabled"
const codexHistoryFilterSettingsTTL = 30 * time.Second

type CodexHistoryFilterStats struct {
	Requests              int64      `json:"requests"`
	FilteredRequests      int64      `json:"filtered_requests"`
	RemovedReasoningItems int64      `json:"removed_reasoning_items"`
	RemovedItemIDs        int64      `json:"removed_item_ids"`
	BlockedRequests       int64      `json:"blocked_requests"`
	UpstreamErrors        int64      `json:"upstream_errors"`
	UpstreamHTTPErrors    int64      `json:"upstream_http_errors"`
	LastFilteredAt        *time.Time `json:"last_filtered_at"`
	LastUpstreamStatus    *int       `json:"last_upstream_status"`
}

type CodexHistoryFilterStatus struct {
	Enabled       bool                    `json:"enabled"`
	FilterVersion int                     `json:"filter_version"`
	Stats         CodexHistoryFilterStats `json:"stats"`
}

type codexHistoryFilterRuntime struct {
	settingsMu sync.Mutex
	enabled    bool
	expiresAt  time.Time
	statsMu    sync.Mutex
	stats      CodexHistoryFilterStats
}

func (s *SettingService) CodexHistoryFilterEnabled(ctx context.Context) (bool, error) {
	if s == nil || s.settingRepo == nil {
		return false, nil
	}
	runtime := &s.codexHistoryFilter
	runtime.settingsMu.Lock()
	defer runtime.settingsMu.Unlock()
	if time.Now().Before(runtime.expiresAt) {
		return runtime.enabled, nil
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	value, err := s.settingRepo.GetValue(dbCtx, SettingKeyCodexHistoryFilterEnabled)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return false, fmt.Errorf("read Codex history filter setting: %w", err)
	}
	enabled := false
	if err == nil && value != "" {
		enabled, err = strconv.ParseBool(value)
		if err != nil {
			return false, errors.New("invalid Codex history filter setting")
		}
	}
	runtime.enabled, runtime.expiresAt = enabled, time.Now().Add(codexHistoryFilterSettingsTTL)
	return enabled, nil
}

func (s *SettingService) SetCodexHistoryFilterEnabled(ctx context.Context, enabled bool) error {
	if s == nil || s.settingRepo == nil {
		return errors.New("settings repository is unavailable")
	}
	runtime := &s.codexHistoryFilter
	runtime.settingsMu.Lock()
	defer runtime.settingsMu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyCodexHistoryFilterEnabled, strconv.FormatBool(enabled)); err != nil {
		return fmt.Errorf("save Codex history filter setting: %w", err)
	}
	runtime.enabled, runtime.expiresAt = enabled, time.Now().Add(codexHistoryFilterSettingsTTL)
	return nil
}

func (s *SettingService) GetCodexHistoryFilterStatus(ctx context.Context) (*CodexHistoryFilterStatus, error) {
	enabled, err := s.CodexHistoryFilterEnabled(ctx)
	if err != nil {
		return nil, err
	}
	status := &CodexHistoryFilterStatus{Enabled: enabled, FilterVersion: codexhistory.Version}
	if s != nil {
		s.codexHistoryFilter.statsMu.Lock()
		status.Stats = s.codexHistoryFilter.stats
		s.codexHistoryFilter.statsMu.Unlock()
	}
	return status, nil
}

func (s *SettingService) RecordCodexHistoryFiltered(reasoningItems, itemIDs int) {
	runtime := &s.codexHistoryFilter
	runtime.statsMu.Lock()
	defer runtime.statsMu.Unlock()
	now := time.Now().UTC()
	runtime.stats.Requests++
	runtime.stats.FilteredRequests++
	runtime.stats.RemovedReasoningItems += int64(reasoningItems)
	runtime.stats.RemovedItemIDs += int64(itemIDs)
	runtime.stats.LastFilteredAt = &now
}

func (s *SettingService) RecordCodexHistoryBlocked() {
	s.codexHistoryFilter.statsMu.Lock()
	s.codexHistoryFilter.stats.BlockedRequests++
	s.codexHistoryFilter.statsMu.Unlock()
}

func (s *SettingService) RecordCodexHistoryResponse(status int, transportError bool) {
	runtime := &s.codexHistoryFilter
	runtime.statsMu.Lock()
	defer runtime.statsMu.Unlock()
	runtime.stats.LastUpstreamStatus = &status
	if status >= 400 {
		runtime.stats.UpstreamHTTPErrors++
	}
	if transportError {
		runtime.stats.UpstreamErrors++
	}
}
