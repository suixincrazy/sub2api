package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	defaultKeepaliveProbeSeconds = 2
	defaultKeepaliveMinSeconds   = 60
	defaultKeepaliveMaxSeconds   = 90
	maxKeepaliveIntervalSeconds  = 86400
)

// ScheduledTestService provides CRUD operations for scheduled test plans and results.
type ScheduledTestService struct {
	planRepo   ScheduledTestPlanRepository
	resultRepo ScheduledTestResultRepository
	// Serialize plan mutations with completion/recovery so pause is definitive.
	planMu sync.Mutex
}

// NewScheduledTestService creates a new ScheduledTestService.
func NewScheduledTestService(
	planRepo ScheduledTestPlanRepository,
	resultRepo ScheduledTestResultRepository,
) *ScheduledTestService {
	return &ScheduledTestService{
		planRepo:   planRepo,
		resultRepo: resultRepo,
	}
}

// CreatePlan starts enabled keepalive plans immediately.
func (s *ScheduledTestService) CreatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	if plan.ProbeIntervalSeconds == 0 {
		plan.ProbeIntervalSeconds = defaultKeepaliveProbeSeconds
	}
	if plan.KeepaliveIntervalSeconds == 0 {
		plan.KeepaliveIntervalSeconds = defaultKeepaliveMinSeconds
	}
	if plan.KeepaliveMaxIntervalSeconds == 0 {
		plan.KeepaliveMaxIntervalSeconds = max(defaultKeepaliveMaxSeconds, plan.KeepaliveIntervalSeconds)
	}
	if plan.MaxResults == 0 {
		plan.MaxResults = 100
	}
	if err := validateKeepalivePlan(plan); err != nil {
		return nil, err
	}
	setKeepaliveNextRun(plan)
	return s.planRepo.Create(ctx, plan)
}

// GetPlan retrieves a plan by ID.
func (s *ScheduledTestService) GetPlan(ctx context.Context, id int64) (*ScheduledTestPlan, error) {
	return s.planRepo.GetByID(ctx, id)
}

// ListPlansByAccount returns all plans for a given account.
func (s *ScheduledTestService) ListPlansByAccount(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error) {
	return s.planRepo.ListByAccountID(ctx, accountID)
}

// UpdatePlan applies keepalive settings and resumes enabled plans immediately.
func (s *ScheduledTestService) UpdatePlan(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error) {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	if err := validateKeepalivePlan(plan); err != nil {
		return nil, err
	}
	setKeepaliveNextRun(plan)
	return s.planRepo.Update(ctx, plan)
}

func setKeepaliveNextRun(plan *ScheduledTestPlan) {
	plan.NextRunAt = nil
	if plan.Enabled {
		now := time.Now()
		plan.NextRunAt = &now
	}
}

func validateKeepalivePlan(plan *ScheduledTestPlan) error {
	if plan.AccountID <= 0 {
		return fmt.Errorf("account_id must be positive")
	}
	if isKeepaliveMediaModel(plan.ModelID) {
		return fmt.Errorf("keepalive requires a text model")
	}
	for _, seconds := range []int{plan.ProbeIntervalSeconds, plan.KeepaliveIntervalSeconds, plan.KeepaliveMaxIntervalSeconds} {
		if seconds < 1 || seconds > maxKeepaliveIntervalSeconds {
			return fmt.Errorf("keepalive intervals must be between 1 and %d seconds", maxKeepaliveIntervalSeconds)
		}
	}
	if plan.KeepaliveMaxIntervalSeconds < plan.KeepaliveIntervalSeconds {
		return fmt.Errorf("maximum keepalive interval must not be shorter than minimum interval")
	}
	if plan.MaxResults < 1 || plan.MaxResults > 1000 {
		return fmt.Errorf("max_results must be between 1 and 1000")
	}
	return nil
}

// DeletePlan removes a plan and its results (via CASCADE).
func (s *ScheduledTestService) DeletePlan(ctx context.Context, id int64) error {
	s.planMu.Lock()
	defer s.planMu.Unlock()
	return s.planRepo.Delete(ctx, id)
}

// ListResults returns the most recent results for a plan.
func (s *ScheduledTestService) ListResults(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.resultRepo.ListByPlanID(ctx, planID, limit)
}

// SaveResult inserts a result and prunes old entries beyond maxResults.
func (s *ScheduledTestService) SaveResult(ctx context.Context, planID int64, maxResults int, result *ScheduledTestResult) error {
	result.PlanID = planID
	if _, err := s.resultRepo.Create(ctx, result); err != nil {
		return err
	}
	return s.resultRepo.PruneOldResults(ctx, planID, maxResults)
}
