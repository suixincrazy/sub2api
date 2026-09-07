package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAIOverloadSchedule_FallbackAndImmediateRecovery(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, session := range []string{"", "overload-session"} {
			t.Run(advanced+"/"+session, func(t *testing.T) {
				groupID := int64(2)
				accounts := []Account{
					{ID: 16, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Priority: 0, GroupIDs: []int64{groupID}},
					{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 10, GroupIDs: []int64{groupID}},
				}
				svc := &OpenAIGatewayService{
					accountRepo:      schedulerTestOpenAIAccountRepo{accounts: accounts},
					cache:            &schedulerTestGatewayCache{},
					cfg:              newSchedulerTestSubscriptionPriorityConfig(),
					rateLimitService: newOpenAIAdvancedSchedulerRateLimitService(advanced),
				}
				pick := func(want int64) {
					t.Helper()
					selection, _, err := svc.SelectAccountWithScheduler(context.Background(), &groupID, "", session, "gpt-5.5", nil, OpenAIUpstreamTransportAny, false)
					require.NoError(t, err)
					require.NotNil(t, selection)
					require.Equal(t, want, selection.Account.ID)
					if selection.ReleaseFunc != nil {
						selection.ReleaseFunc()
					}
				}
				overload := errors.New("stream disconnected before completion: Our servers are currently overloaded. Please try again later.")
				pick(16)
				for i := 0; i < 5; i++ {
					svc.ObserveOpenAIAccountOverloadResult(&groupID, &accounts[0], "gpt-5.5", false, overload)
					pick(16)
				}
				svc.ObserveOpenAIAccountOverloadResult(&groupID, &accounts[0], "gpt-5.5", false, overload)
				pick(1)
				svc.ObserveOpenAIAccountOverloadResult(&groupID, &accounts[1], "gpt-5.5", true, nil)
				pick(16)
			})
		}
	}
}

func newOverloadScheduleFixture(advanced string, batch bool) (*OpenAIGatewayService, []Account, *int64) {
	groupID := int64(2)
	accounts := []Account{
		{ID: 16, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 0, Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "upstream-high", "other": "other-high"}}},
		{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Priority: 10, Concurrency: 1, GroupIDs: []int64{groupID}, Credentials: map[string]any{"model_mapping": map[string]any{"alias": "upstream-low", "other": "other-low"}}},
	}
	cfg := newSchedulerTestSubscriptionPriorityConfig()
	cfg.Gateway.Scheduling.LoadBatchEnabled = batch
	svc := &OpenAIGatewayService{
		accountRepo:        schedulerTestOpenAIAccountRepo{accounts: accounts},
		cache:              &schedulerTestGatewayCache{},
		cfg:                cfg,
		rateLimitService:   newOpenAIAdvancedSchedulerRateLimitService(advanced, "true"),
		concurrencyService: NewConcurrencyService(schedulerTestConcurrencyCache{}),
	}
	return svc, accounts, &groupID
}

func overloadTestPick(t *testing.T, svc *OpenAIGatewayService, groupID *int64, model, session string) int64 {
	t.Helper()
	selection, _, err := svc.SelectAccountWithScheduler(context.Background(), groupID, "", session, model, nil, OpenAIUpstreamTransportAny, false)
	require.NoError(t, err)
	require.NotNil(t, selection)
	if selection.ReleaseFunc != nil {
		selection.ReleaseFunc()
	}
	return selection.Account.ID
}

func overloadTestTrip(svc *OpenAIGatewayService, groupID *int64, account *Account, model string) {
	for i := 0; i < 6; i++ {
		svc.ObserveOpenAIAccountOverloadResult(groupID, account, model, false, ErrOpenAIUpstreamOverloaded)
		svc.ReportOpenAIAccountScheduleResult(account, model, false, nil, ErrOpenAIUpstreamOverloaded)
	}
}

func TestOpenAIOverloadSchedule_WeightedAndLegacyBatchRecovery(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("advanced=%s/batch=%v", advanced, batch), func(t *testing.T) {
				svc, accounts, groupID := newOverloadScheduleFixture(advanced, batch)
				overloadTestTrip(svc, groupID, &accounts[0], "alias")
				require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "fallback"))
				svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "alias", true, nil)
				for _, session := range []string{"fallback", "new-session", "fallback"} {
					require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", session))
					svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", true, nil)
				}
				// Once recovered, another six overloads must repeat the fallback cycle.
				overloadTestTrip(svc, groupID, &accounts[0], "alias")
				require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "fallback"))
			})
		}
	}
}

func TestOpenAIOverloadSchedule_SamePriorityRotation(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			svc, accounts, groupID := newOverloadScheduleFixture(advanced, true)
			peer := accounts[0]
			peer.ID = 17
			peer.LastUsedAt = nil
			accounts = append(accounts, peer)
			peer.ID = 18
			newer := time.Now().Add(-20 * time.Second)
			peer.LastUsedAt = &newer
			accounts = append(accounts, peer)
			svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: accounts}
			overloadTestTrip(svc, groupID, &accounts[0], "alias")
			require.Equal(t, int64(17), overloadTestPick(t, svc, groupID, "alias", ""))
			svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[2], "alias", true, nil)
			now := time.Now()
			accounts[2].LastUsedAt = &now
			accounts[3].LastUsedAt = nil
			require.Equal(t, int64(18), overloadTestPick(t, svc, groupID, "alias", ""))
			require.True(t, svc.ShouldReselectOpenAIAccountAfterOverload(groupID, &accounts[0], "alias"))
		})
	}
}

func TestOpenAIOverloadSchedule_IsolationAndReset(t *testing.T) {
	svc, accounts, groupID := newOverloadScheduleFixture("true", true)
	otherGroup := int64(3)
	accounts[0].GroupIDs = append(accounts[0].GroupIDs, otherGroup)
	accounts[1].GroupIDs = append(accounts[1].GroupIDs, otherGroup)
	overloadTestTrip(svc, groupID, &accounts[0], "alias")
	require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "other", ""))
	require.Equal(t, int64(16), overloadTestPick(t, svc, &otherGroup, "alias", ""))
	svc.ObserveOpenAIAccountOverloadResult(&otherGroup, &accounts[1], "alias", true, nil)
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "other", true, nil)
	require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", ""))
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", true, nil)
	for i := 0; i < 5; i++ {
		svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
	}
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", true, nil)
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
	require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", ""))
	for i := 0; i < 10; i++ {
		svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, errors.New("upstream 503 temporarily unavailable"))
	}
	require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", ""))
}

func TestOpenAIOverloadSchedule_OnlyCapacityErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"plain":    {ErrOpenAIUpstreamOverloaded, true},
		"wrapped":  {fmt.Errorf("proxy: %w", ErrOpenAIUpstreamOverloaded), true},
		"http":     {newOpenAIUpstreamFailoverError(503, nil, []byte(`{"error":{"message":"Our servers are currently overloaded. Please try again later."}}`), "", false), true},
		"ws-code":  {&UpstreamFailoverError{ResponseBody: []byte(`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`)}, true},
		"echo":     {&UpstreamFailoverError{StatusCode: 503, ResponseBody: []byte(`{"error":{"message":"invalid request"},"input":"Our servers are currently overloaded"}`)}, false},
		"ordinary": {errors.New("connection reset by peer"), false},
		"auth":     {&UpstreamFailoverError{StatusCode: 401}, false},
		"context":  {errors.New("maximum context length exceeded"), false},
	} {
		t.Run(name, func(t *testing.T) { require.Equal(t, tc.want, isOpenAIObservedOverload(tc.err)) })
	}
}

func TestOpenAIOverloadSchedule_ProbeDelayAndConcurrency(t *testing.T) {
	svc, accounts, groupID := newOverloadScheduleFixture("true", false)
	old := time.Now().Add(-3 * time.Minute)
	for i := 0; i < 6; i++ {
		svc.observeOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded, old)
	}
	require.False(t, svc.isOpenAIOverloadBlocked(groupID, &accounts[0], "alias"))
	// Slow or failing fallback traffic must not indefinitely prevent a primary probe.
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "alias", true, nil)
	require.True(t, svc.ShouldReselectOpenAIAccountAfterOverload(groupID, &accounts[1], "alias"))
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
			_ = svc.ShouldReselectOpenAIAccountAfterOverload(groupID, &accounts[0], "alias")
		}()
	}
	wg.Wait()
	require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", ""))
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "alias", true, nil)
	require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", ""))
}

func TestOpenAIOverloadSchedule_ProbeDespiteUnsuccessfulStickyFallback(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			svc, accounts, groupID := newOverloadScheduleFixture(advanced, true)
			overloadTestTrip(svc, groupID, &accounts[0], "alias")
			require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "unsuccessful-fallback"))
			svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "alias", false, errors.New("upstream timeout"))
			require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "unsuccessful-fallback"))
			key := openAIOverloadKey{*groupID, "alias", accounts[0].ID}
			svc.openaiOverload.mu.Lock()
			entry := svc.openaiOverload.entries[key]
			entry.probeAfter = time.Now().Add(-time.Second)
			svc.openaiOverload.entries[key] = entry
			svc.openaiOverload.mu.Unlock()
			require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", "unsuccessful-fallback"))
		})
	}
}

func TestOpenAIOverloadSchedule_IdleExpiry(t *testing.T) {
	svc, accounts, groupID := newOverloadScheduleFixture("true", false)
	// Entirely idle scopes are reclaimed, without a permanent account change.
	idleModel := "idle-model"
	for i := 0; i < 6; i++ {
		svc.observeOpenAIAccountOverloadResult(groupID, &accounts[0], idleModel, false, ErrOpenAIUpstreamOverloaded, time.Now().Add(-openAIOverloadStateTTL-time.Second))
	}
	require.False(t, svc.isOpenAIOverloadBlocked(groupID, &accounts[0], idleModel))
}

func TestOpenAIOverloadSchedule_RecoveryRebindsAndKeepsBusyFallback(t *testing.T) {
	for _, advanced := range []string{"false", "true"} {
		t.Run(advanced, func(t *testing.T) {
			svc, accounts, groupID := newOverloadScheduleFixture(advanced, true)
			overloadTestTrip(svc, groupID, &accounts[0], "alias")
			require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "bound-to-fallback"))
			svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[1], "alias", true, nil)
			svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{acquireResults: map[int64]bool{16: false, 1: true}})
			require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", "bound-to-fallback"))
			svc.concurrencyService = NewConcurrencyService(schedulerTestConcurrencyCache{})
			require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", "bound-to-fallback"))
			// Expiry of recovery metadata must not restore an old fallback binding.
			svc.openaiOverload.entries = nil
			require.Equal(t, int64(16), overloadTestPick(t, svc, groupID, "alias", "bound-to-fallback"))
		})
	}
}

func TestOpenAIOverloadSchedule_CancelDoesNotEraseStreak(t *testing.T) {
	svc, accounts, groupID := newOverloadScheduleFixture("true", false)
	for i := 0; i < 5; i++ {
		svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
	}
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, context.Canceled)
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, nil)
	svc.ObserveOpenAIAccountOverloadResult(groupID, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
	require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", ""))
	for i := 0; i < 6; i++ {
		svc.ObserveOpenAIAccountOverloadResult(nil, &accounts[0], "alias", false, ErrOpenAIUpstreamOverloaded)
	}
	svc.ObserveOpenAIAccountOverloadResult(nil, &accounts[1], "alias", true, nil)
	require.Equal(t, int64(1), overloadTestPick(t, svc, groupID, "alias", ""))
}
