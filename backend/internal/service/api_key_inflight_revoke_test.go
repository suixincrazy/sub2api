//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func newInflightTestService(lookup func(ctx context.Context, key string) (*APIKey, error)) *APIKeyService {
	return NewAPIKeyService(&authRepoStub{getByKeyForAuth: lookup}, nil, nil, nil, nil, &authCacheStub{}, &config.Config{})
}

func waitInflightIdle(t *testing.T, svc *APIKeyService) {
	t.Helper()
	require.Eventually(t, func() bool {
		svc.inflight.mu.Lock()
		defer svc.inflight.mu.Unlock()
		for _, e := range svc.inflight.entries {
			if e.revalidating {
				return false
			}
		}
		return true
	}, 2*time.Second, 5*time.Millisecond)
}

func TestInflightRevoke_RevokesOnDisabledDeletedOrUserInactive(t *testing.T) {
	cases := map[string]func(ctx context.Context, key string) (*APIKey, error){
		"key disabled": func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{Key: key, Status: "inactive", User: &User{Status: StatusActive}}, nil
		},
		"key deleted": func(ctx context.Context, key string) (*APIKey, error) {
			return nil, ErrAPIKeyNotFound
		},
		"user disabled": func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{Key: key, Status: StatusActive, User: &User{Status: StatusDisabled}}, nil
		},
	}
	for name, lookup := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newInflightTestService(lookup)
			revoked := make(chan struct{}, 2)
			release := svc.TrackInflightRequest("sk-test", func() { revoked <- struct{}{} })
			defer release()

			svc.InvalidateAuthCacheByKey(context.Background(), "sk-test")
			select {
			case <-revoked:
			case <-time.After(2 * time.Second):
				t.Fatal("in-flight request was not revoked")
			}
			waitInflightIdle(t, svc)
			svc.InvalidateAuthCacheByKey(context.Background(), "sk-test")
			waitInflightIdle(t, svc)
			require.Len(t, revoked, 0, "revoke must fire at most once")
			svc.inflight.mu.Lock()
			require.Empty(t, svc.inflight.entries)
			svc.inflight.mu.Unlock()
		})
	}
}

func TestInflightRevoke_KeepsRequestForBillingStatesAndLookupErrors(t *testing.T) {
	cases := map[string]func(ctx context.Context, key string) (*APIKey, error){
		"active": func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{Key: key, Status: StatusActive, User: &User{Status: StatusActive}}, nil
		},
		"expired": func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{Key: key, Status: StatusAPIKeyExpired, User: &User{Status: StatusActive}}, nil
		},
		"quota exhausted": func(ctx context.Context, key string) (*APIKey, error) {
			return &APIKey{Key: key, Status: StatusAPIKeyQuotaExhausted, User: &User{Status: StatusActive}}, nil
		},
		"db error": func(ctx context.Context, key string) (*APIKey, error) {
			return nil, errors.New("db down")
		},
	}
	for name, lookup := range cases {
		t.Run(name, func(t *testing.T) {
			svc := newInflightTestService(lookup)
			revoked := false
			release := svc.TrackInflightRequest("sk-test", func() { revoked = true })

			svc.InvalidateAuthCacheByKey(context.Background(), "sk-test")
			waitInflightIdle(t, svc)
			require.False(t, revoked)

			release()
			svc.inflight.mu.Lock()
			require.Empty(t, svc.inflight.entries)
			svc.inflight.mu.Unlock()
		})
	}
}

func TestInflightRevoke_NoLookupWithoutInflightRequests(t *testing.T) {
	svc := newInflightTestService(func(ctx context.Context, key string) (*APIKey, error) {
		t.Fatal("lookup must not run when nothing is in flight")
		return nil, nil
	})
	svc.InvalidateAuthCacheByKey(context.Background(), "sk-test")
	release := svc.TrackInflightRequest("sk-other", func() {})
	svc.InvalidateAuthCacheByKey(context.Background(), "sk-test")
	release()
}
