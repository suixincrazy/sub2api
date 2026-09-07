package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ErrOpenAIUpstreamOverloaded carries a WS terminal overload without retaining its payload.
var ErrOpenAIUpstreamOverloaded = errors.New("OpenAI servers are currently overloaded")

const (
	openAIOverloadThreshold  = 6
	openAIOverloadStateTTL   = 10 * time.Minute
	openAIOverloadMaxEntries = 4096
)

type openAIOverloadKey struct {
	groupID   int64
	model     string
	accountID int64
}

type openAIOverloadEntry struct {
	failures  int
	priority  int
	recovered bool
	expiresAt time.Time
}

type openAIAccountOverloadState struct {
	mu      sync.Mutex
	entries map[openAIOverloadKey]openAIOverloadEntry
}

func openAIOverloadModel(model string) string {
	return strings.TrimSpace(model)
}

func isOpenAIObservedOverload(err error) bool {
	if err == nil {
		return false
	}
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		return isOpenAIRequestScopedCapacityShed(failover.ClientMessage, failover.ResponseBody)
	}
	var streamErr *sseStreamErrorEventError
	if errors.As(err, &streamErr) {
		return isOpenAIRequestScopedCapacityShed("", []byte(streamErr.RawData))
	}
	return isOpenAICapacityShedMessage(err.Error())
}

// ObserveOpenAIAccountOverloadResult observes one completed forwarding attempt.
// model is the routing model before account-specific mapping.
func (s *OpenAIGatewayService) ObserveOpenAIAccountOverloadResult(groupID *int64, account *Account, model string, success bool, observedErr error) {
	if s == nil || account == nil || account.Platform != PlatformOpenAI || account.ID <= 0 || openAIOverloadModel(model) == "" {
		return
	}
	s.observeOpenAIAccountOverloadResult(groupID, account, model, success, observedErr, time.Now())
}

func (s *OpenAIGatewayService) observeOpenAIAccountOverloadResult(groupID *int64, account *Account, model string, success bool, observedErr error, now time.Time) {
	state := &s.openaiOverload
	state.mu.Lock()
	defer state.mu.Unlock()
	key := openAIOverloadKey{derefGroupID(groupID), openAIOverloadModel(model), account.ID}
	for k, entry := range state.entries {
		if !now.Before(entry.expiresAt) {
			delete(state.entries, k)
		} else if k.groupID == key.groupID && k.model == key.model && entry.failures >= openAIOverloadThreshold {
			// Active fallback traffic keeps demotion alive until a success;
			// the TTL only discards idle routing state, not a slow fallback.
			entry.expiresAt = now.Add(openAIOverloadStateTTL)
			state.entries[k] = entry
		}
	}
	entry := state.entries[key]
	if success && observedErr == nil {
		// A peer at the same priority must not immediately re-admit a bad peer.
		// Keep recovered priority preference briefly so existing fallback sticky
		// sessions and weighted routing also return to the primary tier.
		if entry.recovered {
			entry.failures = 0
			state.entries[key] = entry
		} else {
			delete(state.entries, key)
		}
		for k, failed := range state.entries {
			if k.groupID == key.groupID && k.model == key.model && failed.failures >= openAIOverloadThreshold && failed.priority < account.Priority {
				failed.failures = 0
				failed.recovered = true
				failed.expiresAt = now.Add(openAIOverloadStateTTL)
				state.entries[k] = failed
				slog.Info("openai_overload_recovered", "group_id", key.groupID, "model", key.model, "account_id", k.accountID, "fallback_account_id", account.ID)
			}
		}
		return
	}
	if !isOpenAIObservedOverload(observedErr) {
		// Cancellation and a non-success terminal without an error are not an
		// observed upstream outcome and must not erase an overload streak.
		if observedErr == nil || errors.Is(observedErr, context.Canceled) {
			return
		}
		if entry.failures < openAIOverloadThreshold && entry.failures > 0 {
			entry.failures = 0
			state.entries[key] = entry
		}
		return
	}
	if state.entries == nil {
		state.entries = make(map[openAIOverloadKey]openAIOverloadEntry)
	}
	if _, exists := state.entries[key]; !exists && len(state.entries) >= openAIOverloadMaxEntries {
		var oldestKey openAIOverloadKey
		var oldest time.Time
		for k, e := range state.entries {
			if oldest.IsZero() || e.expiresAt.Before(oldest) {
				oldestKey, oldest = k, e.expiresAt
			}
		}
		delete(state.entries, oldestKey)
	}
	entry.priority = account.Priority
	if entry.failures < openAIOverloadThreshold {
		entry.failures++
		entry.expiresAt = now.Add(openAIOverloadStateTTL)
		if entry.failures == openAIOverloadThreshold {
			entry.recovered = false
			slog.Warn("openai_overload_demoted", "group_id", key.groupID, "model", key.model, "account_id", key.accountID, "failure_count", entry.failures)
		}
	}
	state.entries[key] = entry
}

func (s *OpenAIGatewayService) isOpenAIOverloadBlocked(groupID *int64, account *Account, model string) bool {
	if s == nil || account == nil || account.Platform != PlatformOpenAI {
		return false
	}
	state := &s.openaiOverload
	state.mu.Lock()
	defer state.mu.Unlock()
	key := openAIOverloadKey{derefGroupID(groupID), openAIOverloadModel(model), account.ID}
	entry := state.entries[key]
	return entry.failures >= openAIOverloadThreshold && time.Now().Before(entry.expiresAt)
}

// ShouldReselectOpenAIAccountAfterOverload prevents a long-lived WS connection
// from bypassing the scheduler on the next response.create.
func (s *OpenAIGatewayService) ShouldReselectOpenAIAccountAfterOverload(groupID *int64, account *Account, model string) bool {
	return s.isOpenAIOverloadBlocked(groupID, account, model) || s.shouldYieldOpenAIOverloadSticky(groupID, account, model)
}

func (s *OpenAIGatewayService) openAIOverloadRecoveryPriority(groupID *int64, model string) (int, bool) {
	if s == nil {
		return 0, false
	}
	state := &s.openaiOverload
	state.mu.Lock()
	defer state.mu.Unlock()
	priority, found := 0, false
	now := time.Now()
	for key, entry := range state.entries {
		if key.groupID == derefGroupID(groupID) && key.model == openAIOverloadModel(model) && entry.recovered && now.Before(entry.expiresAt) && (!found || entry.priority < priority) {
			priority, found = entry.priority, true
		}
	}
	return priority, found
}

func (s *OpenAIGatewayService) shouldYieldOpenAIOverloadSticky(groupID *int64, account *Account, model string) bool {
	if account == nil || account.Platform != PlatformOpenAI {
		return false
	}
	priority, recovered := s.openAIOverloadRecoveryPriority(groupID, model)
	return recovered && account.Priority > priority
}

func (s *OpenAIGatewayService) clearOpenAIOverloadFallbackBinding(ctx context.Context, groupID *int64, session, model string, excluded map[int64]struct{}) bool {
	if s == nil || s.cache == nil || session == "" || preserveOpenAIGuardianParentBinding(ctx, session) {
		return false
	}
	if _, recovered := s.openAIOverloadRecoveryPriority(groupID, model); !recovered {
		return false
	}
	id, err := s.getStickySessionAccountID(ctx, groupID, session)
	if err != nil || id <= 0 {
		return false
	}
	if _, skip := excluded[id]; skip {
		return false
	}
	account, err := s.getSchedulableAccount(ctx, id)
	if err != nil || !s.shouldYieldOpenAIOverloadSticky(groupID, account, model) {
		return false
	}
	return s.deleteStickySessionAccountID(ctx, groupID, session) == nil
}

func (s *OpenAIGatewayService) preferOpenAIOverloadRecovery(groupID *int64, model string, candidates []*Account) []*Account {
	priority, recovered := s.openAIOverloadRecoveryPriority(groupID, model)
	if !recovered {
		return candidates
	}
	preferred := make([]*Account, 0, len(candidates))
	for _, account := range candidates {
		if account.Platform == PlatformOpenAI && account.Priority <= priority {
			preferred = append(preferred, account)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return candidates
}
