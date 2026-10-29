package service

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 已通过鉴权、仍在处理中的请求（典型是长流式）不会再走中间件，
// 认证缓存失效只能拦住新请求。这里按认证缓存键登记在途请求，
// 缓存失效时回源复核一次：Key 被停用/删除或所属用户被停用，就撤销这些请求。
const apiKeyInflightRevalidateTimeout = 5 * time.Second

type apiKeyInflightEntry struct {
	key          string
	revokers     map[uint64]func()
	revalidating bool
	pending      bool
}

type apiKeyInflightRegistry struct {
	mu      sync.Mutex
	nextID  uint64
	entries map[string]*apiKeyInflightEntry
}

// TrackInflightRequest 登记一个已通过鉴权的请求；revoke 在 Key 被撤销时调用（至多一次）。
// 返回的 release 必须在请求结束时调用。
func (s *APIKeyService) TrackInflightRequest(key string, revoke func()) (release func()) {
	if s == nil || key == "" || revoke == nil {
		return func() {}
	}
	cacheKey := s.authCacheKey(key)
	r := &s.inflight
	r.mu.Lock()
	if r.entries == nil {
		r.entries = make(map[string]*apiKeyInflightEntry)
	}
	entry := r.entries[cacheKey]
	if entry == nil {
		entry = &apiKeyInflightEntry{key: key, revokers: make(map[uint64]func())}
		r.entries[cacheKey] = entry
	}
	r.nextID++
	id := r.nextID
	var once sync.Once
	entry.revokers[id] = func() { once.Do(revoke) }
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		e := r.entries[cacheKey]
		if e == nil {
			return
		}
		delete(e.revokers, id)
		if len(e.revokers) == 0 && !e.revalidating {
			delete(r.entries, cacheKey)
		}
	}
}

// revalidateInflight 在认证缓存失效后异步复核在途请求。同一键的并发失效合并成
// 「正在复核 + 再复核一次」，避免每次失效都打一遍 DB。
func (s *APIKeyService) revalidateInflight(cacheKey string) {
	if s == nil {
		return
	}
	r := &s.inflight
	r.mu.Lock()
	entry := r.entries[cacheKey]
	if entry == nil || len(entry.revokers) == 0 {
		r.mu.Unlock()
		return
	}
	if entry.revalidating {
		entry.pending = true
		r.mu.Unlock()
		return
	}
	entry.revalidating = true
	key := entry.key
	r.mu.Unlock()

	go s.runInflightRevalidation(cacheKey, entry, key)
}

func (s *APIKeyService) runInflightRevalidation(cacheKey string, entry *apiKeyInflightEntry, key string) {
	r := &s.inflight
	for {
		revoked := s.apiKeyRevokedForInflight(key)

		r.mu.Lock()
		var revokers []func()
		if revoked {
			for id, fn := range entry.revokers {
				revokers = append(revokers, fn)
				delete(entry.revokers, id)
			}
		}
		again := entry.pending && len(entry.revokers) > 0
		entry.pending = false
		if !again {
			entry.revalidating = false
			if len(entry.revokers) == 0 && r.entries[cacheKey] == entry {
				delete(r.entries, cacheKey)
			}
		}
		r.mu.Unlock()

		for _, fn := range revokers {
			fn()
		}
		if !again {
			return
		}
	}
}

// apiKeyRevokedForInflight 与鉴权中间件的基础拦截保持一致：
// 未知/停用状态拦截，expired 与 quota_exhausted 留给计费阶段，不中断在途请求。
// 查询失败（非「不存在」）时保守放行，避免 DB 抖动误杀请求。
func (s *APIKeyService) apiKeyRevokedForInflight(key string) bool {
	if s.apiKeyRepo == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), apiKeyInflightRevalidateTimeout)
	defer cancel()
	apiKey, err := s.apiKeyRepo.GetByKeyForAuth(ctx, key)
	if err != nil {
		return errors.Is(err, ErrAPIKeyNotFound)
	}
	if apiKey == nil {
		return true
	}
	if !apiKey.IsActive() &&
		apiKey.Status != StatusAPIKeyExpired &&
		apiKey.Status != StatusAPIKeyQuotaExhausted {
		return true
	}
	if apiKey.User != nil && !apiKey.User.IsActive() {
		return true
	}
	return false
}
