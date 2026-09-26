package service

import (
	"context"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const (
	scheduledTestDefaultMaxWorkers = 10
	keepaliveRequestTimeout        = 30 * time.Second
)

type keepaliveRun struct {
	plan   *ScheduledTestPlan
	cancel context.CancelFunc
}

// ScheduledTestRunnerService maintains persistent probe/keepalive schedules.
// A single dispatcher owns admission; slow requests never block other accounts.
type ScheduledTestRunnerService struct {
	planRepo     ScheduledTestPlanRepository
	scheduledSvc *ScheduledTestService
	runTest      func(context.Context, int64, string) (*ScheduledTestResult, error)
	rateLimitSvc *RateLimitService

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	running   map[int64]*keepaliveRun
	mu        sync.Mutex
	workers   sync.WaitGroup
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewScheduledTestRunnerService(
	planRepo ScheduledTestPlanRepository,
	scheduledSvc *ScheduledTestService,
	accountTestSvc *AccountTestService,
	rateLimitSvc *RateLimitService,
	_ *config.Config,
) *ScheduledTestRunnerService {
	ctx, cancel := context.WithCancel(context.Background())
	return &ScheduledTestRunnerService{
		planRepo: planRepo, scheduledSvc: scheduledSvc,
		runTest:      accountTestSvc.RunKeepaliveBackground,
		rateLimitSvc: rateLimitSvc, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), running: make(map[int64]*keepaliveRun),
	}
}

func (s *ScheduledTestRunnerService) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		go func() {
			defer close(s.done)
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				s.runScheduled()
				select {
				case <-s.ctx.Done():
					s.workers.Wait()
					return
				case <-ticker.C:
				}
			}
		}()
		logger.LegacyPrintf("service.scheduled_test_runner", "[KeepaliveRunner] started (tick=1s, workers=%d)", scheduledTestDefaultMaxWorkers)
	})
}

func (s *ScheduledTestRunnerService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.cancel()
		// Also makes Stop safe when startup never reached Start.
		s.Start()
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
			logger.LegacyPrintf("service.scheduled_test_runner", "[KeepaliveRunner] shutdown timed out")
		}
	})
}

func (s *ScheduledTestRunnerService) runScheduled() {
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	plans, err := s.planRepo.ListEnabled(ctx)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[KeepaliveRunner] list plans: %v", err)
		return
	}
	enabled := make(map[int64]*ScheduledTestPlan, len(plans))
	for _, p := range plans {
		enabled[p.ID] = p
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	busyAccounts := make(map[int64]bool)
	for id, run := range s.running {
		current := enabled[id]
		if current == nil || !current.UpdatedAt.Equal(run.plan.UpdatedAt) {
			run.cancel()
		}
		// A cancelled transport still owns its slot until it actually exits.
		busyAccounts[run.plan.AccountID] = true
	}
	now := time.Now()
	for _, plan := range plans {
		if len(s.running) >= scheduledTestDefaultMaxWorkers || s.ctx.Err() != nil {
			break
		}
		if s.running[plan.ID] != nil || busyAccounts[plan.AccountID] {
			continue
		}
		if plan.NextRunAt != nil && plan.NextRunAt.After(now) {
			continue
		}
		runCtx, runCancel := context.WithCancel(s.ctx)
		run := &keepaliveRun{plan: plan, cancel: runCancel}
		s.running[plan.ID] = run
		busyAccounts[plan.AccountID] = true
		s.workers.Add(1)
		go func() {
			defer s.workers.Done()
			defer runCancel()
			defer func() {
				s.mu.Lock()
				delete(s.running, run.plan.ID)
				s.mu.Unlock()
			}()
			s.runOnePlan(runCtx, run.plan)
		}()
	}
}

func (s *ScheduledTestRunnerService) runOnePlan(ctx context.Context, plan *ScheduledTestPlan) {
	// Recheck after admission: a previous request may have finished while the
	// dispatcher was loading its snapshot, or the administrator paused the plan.
	checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
	current, err := s.planRepo.GetByID(checkCtx, plan.ID)
	checkCancel()
	if err != nil || current == nil || !current.Enabled || !current.UpdatedAt.Equal(plan.UpdatedAt) ||
		(current.NextRunAt != nil && current.NextRunAt.After(time.Now())) {
		return
	}
	plan = current
	startedAt := time.Now()
	requestCtx, cancel := context.WithTimeout(ctx, keepaliveRequestTimeout)
	result, err := s.runTest(requestCtx, plan.AccountID, plan.ModelID)
	cancel()
	// Pause/delete/edit/shutdown cancels the run. It must not revive the old plan.
	if ctx.Err() != nil {
		return
	}
	if err != nil || result == nil {
		result = &ScheduledTestResult{Status: "failed", StartedAt: startedAt, FinishedAt: time.Now()}
		result.LatencyMs = result.FinishedAt.Sub(startedAt).Milliseconds()
		result.ErrorMessage = "keepalive returned no result"
		if err != nil {
			result.ErrorMessage = err.Error()
		}
	}

	s.scheduledSvc.planMu.Lock()
	defer s.scheduledSvc.planMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	delay, failures := nextKeepaliveDelay(plan, result)
	nextRun := time.Now().Add(delay)
	saveCtx, saveCancel := context.WithTimeout(ctx, 5*time.Second)
	defer saveCancel()
	applied, err := s.planRepo.UpdateAfterRun(saveCtx, plan, startedAt, nextRun, result.Status, failures)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[KeepaliveRunner] plan=%d update schedule: %v", plan.ID, err)
		return
	}
	if !applied {
		return
	}
	if err := s.scheduledSvc.SaveResult(saveCtx, plan.ID, plan.MaxResults, result); err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[KeepaliveRunner] plan=%d save result: %v", plan.ID, err)
	}
	if result.Status == "success" && plan.AutoRecover {
		s.tryRecoverAccount(saveCtx, plan.AccountID, plan.ID)
	}
}

func nextKeepaliveDelay(plan *ScheduledTestPlan, result *ScheduledTestResult) (time.Duration, int) {
	if result.Status == "success" {
		minimum := max(1, plan.KeepaliveIntervalSeconds)
		maximum := max(minimum, plan.KeepaliveMaxIntervalSeconds)
		return time.Duration(minimum+rand.IntN(maximum-minimum+1)) * time.Second, 0
	}
	failures := min(plan.ConsecutiveFailures+1, 30)
	delay := time.Duration(max(1, plan.ProbeIntervalSeconds)) * time.Second
	if result.HTTPStatus == http.StatusTooManyRequests {
		if backoff := time.Duration(min(1<<min(failures, 6), 60)) * time.Second; backoff > delay {
			delay = backoff
		}
	}
	if result.RetryAfter > delay {
		delay = result.RetryAfter
	}
	return delay, failures
}

// tryRecoverAccount attempts to recover an account from recoverable runtime state.
func (s *ScheduledTestRunnerService) tryRecoverAccount(ctx context.Context, accountID int64, planID int64) {
	if s.rateLimitSvc == nil {
		return
	}

	recovery, err := s.rateLimitSvc.RecoverAccountAfterSuccessfulTest(ctx, accountID)
	if err != nil {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover failed: %v", planID, err)
		return
	}
	if recovery == nil {
		return
	}

	if recovery.ClearedError {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d recovered from error status", planID, accountID)
	}
	if recovery.ClearedRateLimit {
		logger.LegacyPrintf("service.scheduled_test_runner", "[ScheduledTestRunner] plan=%d auto-recover: account=%d cleared rate-limit/runtime state", planID, accountID)
	}
}
