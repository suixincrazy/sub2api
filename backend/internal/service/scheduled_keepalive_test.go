//go:build unit

package service

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type keepalivePlanRepoStub struct {
	ScheduledTestPlanRepository
	mu    sync.Mutex
	plans map[int64]*ScheduledTestPlan
}

func (r *keepalivePlanRepoStub) Create(_ context.Context, p *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	return p, nil
}
func (r *keepalivePlanRepoStub) Update(_ context.Context, p *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plans != nil {
		copy := *p
		r.plans[p.ID] = &copy
	}
	return p, nil
}
func (r *keepalivePlanRepoStub) GetByID(_ context.Context, id int64) (*ScheduledTestPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.plans[id]
	if p == nil {
		return nil, errors.New("not found")
	}
	copy := *p
	return &copy, nil
}
func (r *keepalivePlanRepoStub) ListEnabled(_ context.Context) ([]*ScheduledTestPlan, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var plans []*ScheduledTestPlan
	for _, p := range r.plans {
		if p.Enabled {
			copy := *p
			plans = append(plans, &copy)
		}
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ID < plans[j].ID })
	return plans, nil
}
func (r *keepalivePlanRepoStub) UpdateAfterRun(_ context.Context, plan *ScheduledTestPlan, lastRun, nextRun time.Time, status string, failures int) (bool, error) {
	p := plan
	r.mu.Lock()
	defer r.mu.Unlock()
	current := r.plans[p.ID]
	if current == nil || !current.Enabled || !current.UpdatedAt.Equal(p.UpdatedAt) {
		return false, nil
	}
	current.LastRunAt = &lastRun
	current.NextRunAt = &nextRun
	current.LastStatus = status
	current.ConsecutiveFailures = failures
	return true, nil
}
func (r *keepalivePlanRepoStub) due(id int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().Add(-time.Second)
	r.plans[id].NextRunAt = &now
}

type keepaliveResultRepoStub struct {
	ScheduledTestResultRepository
	mu           sync.Mutex
	results      []*ScheduledTestResult
	beforeCreate func()
}

func (r *keepaliveResultRepoStub) Create(_ context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error) {
	if r.beforeCreate != nil {
		r.beforeCreate()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	copy := *result
	r.results = append(r.results, &copy)
	return &copy, nil
}
func (r *keepaliveResultRepoStub) PruneOldResults(_ context.Context, _ int64, keep int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.results) > keep {
		r.results = r.results[len(r.results)-keep:]
	}
	return nil
}
func (r *keepaliveResultRepoStub) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.results)
}

func newKeepaliveTestRunner(t *testing.T, count int) (*ScheduledTestRunnerService, *keepalivePlanRepoStub, *keepaliveResultRepoStub) {
	t.Helper()
	repo := &keepalivePlanRepoStub{plans: map[int64]*ScheduledTestPlan{}}
	for i := 1; i <= count; i++ {
		repo.plans[int64(i)] = &ScheduledTestPlan{ID: int64(i), AccountID: int64(i), Enabled: true, ModelID: "gpt-6-astra", MaxResults: 100, ProbeIntervalSeconds: 2, KeepaliveIntervalSeconds: 60, KeepaliveMaxIntervalSeconds: 90, UpdatedAt: time.Now()}
	}
	results := &keepaliveResultRepoStub{}
	runner := NewScheduledTestRunnerService(repo, NewScheduledTestService(repo, results), &AccountTestService{}, nil, nil)
	t.Cleanup(func() { runner.cancel(); runner.workers.Wait() })
	return runner, repo, results
}

func waitKeepaliveIdle(t *testing.T, runner *ScheduledTestRunnerService) {
	t.Helper()
	require.Eventually(t, func() bool { runner.mu.Lock(); defer runner.mu.Unlock(); return len(runner.running) == 0 }, time.Second, time.Millisecond)
}

func TestScheduledKeepaliveRetriesAndContinuesAfterRecovery(t *testing.T) {
	runner, repo, results := newKeepaliveTestRunner(t, 1)
	var calls atomic.Int32
	runner.runTest = func(_ context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
		status := "success"
		if calls.Add(1) <= 3 {
			status = "failed"
		}
		return &ScheduledTestResult{Status: status}, nil
	}
	for i := 1; i <= 6; i++ {
		repo.due(1)
		runner.runScheduled()
		waitKeepaliveIdle(t, runner)
		require.Equal(t, int32(i), calls.Load())
		p, err := repo.GetByID(context.Background(), 1)
		require.NoError(t, err)
		if i <= 3 {
			require.Equal(t, "failed", p.LastStatus)
			require.Equal(t, i, p.ConsecutiveFailures)
			require.WithinDuration(t, time.Now().Add(2*time.Second), *p.NextRunAt, time.Second)
		} else {
			require.Equal(t, "success", p.LastStatus)
			require.Zero(t, p.ConsecutiveFailures)
			require.True(t, p.NextRunAt.After(time.Now().Add(59*time.Second)))
			require.True(t, p.NextRunAt.Before(time.Now().Add(91*time.Second)))
		}
		runner.runScheduled()
		waitKeepaliveIdle(t, runner)
		require.Equal(t, int32(i), calls.Load(), "a fresh scan must respect the persisted next request time")
	}
	require.Equal(t, 6, results.count())
}

func TestScheduledKeepalivePauseDeleteEditCancelInFlight(t *testing.T) {
	for _, action := range []string{"pause", "delete", "edit"} {
		t.Run(action, func(t *testing.T) {
			runner, repo, results := newKeepaliveTestRunner(t, 1)
			entered := make(chan struct{})
			cancelled := make(chan struct{})
			runner.runTest = func(ctx context.Context, _ int64, _ string) (*ScheduledTestResult, error) {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				return &ScheduledTestResult{Status: "success"}, nil
			}
			runner.runScheduled()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not start")
			}
			repo.mu.Lock()
			switch action {
			case "pause":
				repo.plans[1].Enabled = false
			case "delete":
				delete(repo.plans, 1)
			case "edit":
				repo.plans[1].UpdatedAt = repo.plans[1].UpdatedAt.Add(time.Second)
			}
			repo.mu.Unlock()
			runner.runScheduled()
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("request was not cancelled")
			}
			waitKeepaliveIdle(t, runner)
			require.Zero(t, results.count(), "cancelled requests must not recover or rewrite a changed plan")
		})
	}
}

func TestScheduledKeepaliveWorkerLimitAndNoOverlappingAccounts(t *testing.T) {
	runner, repo, _ := newKeepaliveTestRunner(t, 12)
	repo.plans[2].AccountID = 1
	entered := make(chan int64, 20)
	runner.runTest = func(ctx context.Context, id int64, _ string) (*ScheduledTestResult, error) {
		entered <- id
		<-ctx.Done()
		return nil, ctx.Err()
	}
	runner.runScheduled()
	ids := map[int64]bool{}
	for i := 0; i < scheduledTestDefaultMaxWorkers; i++ {
		select {
		case id := <-entered:
			require.False(t, ids[id])
			ids[id] = true
		case <-time.After(time.Second):
			t.Fatal("workers did not start")
		}
	}
	runner.runScheduled()
	require.Empty(t, entered, "repeated scans may not admit duplicates or more workers")
	runner.cancel()
	waitKeepaliveIdle(t, runner)
}

func TestScheduledKeepaliveErrorPersistsRetryAndStaleSnapshotDoesNotSend(t *testing.T) {
	runner, repo, results := newKeepaliveTestRunner(t, 1)
	calls := 0
	runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	stale, _ := repo.GetByID(context.Background(), 1)
	runner.runOnePlan(context.Background(), stale)
	runner.runOnePlan(context.Background(), stale)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, results.count())
	p, _ := repo.GetByID(context.Background(), 1)
	require.Equal(t, "failed", p.LastStatus)
	require.NotNil(t, p.NextRunAt)
}

func TestScheduledKeepaliveHonorsRateLimitsAndConfiguredIntervals(t *testing.T) {
	p := &ScheduledTestPlan{ProbeIntervalSeconds: 2, KeepaliveIntervalSeconds: 60, KeepaliveMaxIntervalSeconds: 60}
	delay, failures := nextKeepaliveDelay(p, &ScheduledTestResult{Status: "failed", HTTPStatus: 429, RetryAfter: 5 * time.Minute})
	require.Equal(t, 5*time.Minute, delay)
	require.Equal(t, 1, failures)
	p.ConsecutiveFailures = 20
	delay, _ = nextKeepaliveDelay(p, &ScheduledTestResult{Status: "failed", HTTPStatus: 429})
	require.Equal(t, 60*time.Second, delay)
	delay, failures = nextKeepaliveDelay(p, &ScheduledTestResult{Status: "success"})
	require.Equal(t, time.Minute, delay)
	require.Zero(t, failures)
	delay, _ = nextKeepaliveDelay(p, &ScheduledTestResult{Status: "failed", HTTPStatus: 503})
	require.Equal(t, 2*time.Second, delay)
}

func TestScheduledKeepalivePlanDefaultsValidationAndPause(t *testing.T) {
	repo := &keepalivePlanRepoStub{}
	svc := NewScheduledTestService(repo, nil)
	plan, err := svc.CreatePlan(context.Background(), &ScheduledTestPlan{AccountID: 8, Enabled: true})
	require.NoError(t, err)
	require.NotNil(t, plan.NextRunAt)
	require.Equal(t, 2, plan.ProbeIntervalSeconds)
	require.Equal(t, 60, plan.KeepaliveIntervalSeconds)
	require.Equal(t, 90, plan.KeepaliveMaxIntervalSeconds)
	plan.Enabled = false
	plan, err = svc.UpdatePlan(context.Background(), plan)
	require.NoError(t, err)
	require.Nil(t, plan.NextRunAt)
	for _, mutate := range []func(*ScheduledTestPlan){
		func(p *ScheduledTestPlan) { p.ProbeIntervalSeconds = 0 },
		func(p *ScheduledTestPlan) { p.KeepaliveIntervalSeconds = 100; p.KeepaliveMaxIntervalSeconds = 90 },
		func(p *ScheduledTestPlan) { p.KeepaliveMaxIntervalSeconds = 86401 },
		func(p *ScheduledTestPlan) { p.MaxResults = 1001 },
		func(p *ScheduledTestPlan) { p.ModelID = "gpt-image-2" },
	} {
		invalid := *plan
		mutate(&invalid)
		_, err = svc.UpdatePlan(context.Background(), &invalid)
		require.Error(t, err)
	}
}

func TestScheduledKeepaliveSerializesPauseWithCompletion(t *testing.T) {
	runner, repo, results := newKeepaliveTestRunner(t, 1)
	saving := make(chan struct{})
	finishSave := make(chan struct{})
	results.beforeCreate = func() { close(saving); <-finishSave }
	runner.runTest = func(context.Context, int64, string) (*ScheduledTestResult, error) {
		return &ScheduledTestResult{Status: "success"}, nil
	}
	runner.runScheduled()
	select {
	case <-saving:
	case <-time.After(time.Second):
		t.Fatal("result was not being saved")
	}
	// Completion holds the shared mutation lock until recovery is finished.
	acquired := runner.scheduledSvc.planMu.TryLock()
	if acquired {
		runner.scheduledSvc.planMu.Unlock()
	}
	require.False(t, acquired)
	plan, err := repo.GetByID(context.Background(), 1)
	require.NoError(t, err)
	plan.Enabled = false
	paused := make(chan error, 1)
	go func() { _, err := runner.scheduledSvc.UpdatePlan(context.Background(), plan); paused <- err }()
	close(finishSave)
	select {
	case err := <-paused:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("pause did not finish after completion")
	}
	waitKeepaliveIdle(t, runner)
	require.Nil(t, plan.NextRunAt)
	stored, err := repo.GetByID(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, stored.Enabled)
	require.Nil(t, stored.NextRunAt)
}
