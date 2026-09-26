package service

import (
	"context"
	"time"
)

// ScheduledTestPlan represents a persistent account keepalive plan.
type ScheduledTestPlan struct {
	ID                          int64      `json:"id"`
	AccountID                   int64      `json:"account_id"`
	ModelID                     string     `json:"model_id"`
	CronExpression              string     `json:"cron_expression"`
	Enabled                     bool       `json:"enabled"`
	MaxResults                  int        `json:"max_results"`
	AutoRecover                 bool       `json:"auto_recover"`
	ProbeIntervalSeconds        int        `json:"probe_interval_seconds"`
	KeepaliveIntervalSeconds    int        `json:"keepalive_interval_seconds"`
	KeepaliveMaxIntervalSeconds int        `json:"keepalive_max_interval_seconds"`
	LastStatus                  string     `json:"last_status"`
	ConsecutiveFailures         int        `json:"consecutive_failures"`
	LastRunAt                   *time.Time `json:"last_run_at"`
	NextRunAt                   *time.Time `json:"next_run_at"`
	CreatedAt                   time.Time  `json:"created_at"`
	UpdatedAt                   time.Time  `json:"updated_at"`
}

// ScheduledTestResult represents a single test execution result.
type ScheduledTestResult struct {
	ID           int64         `json:"id"`
	PlanID       int64         `json:"plan_id"`
	Status       string        `json:"status"`
	ResponseText string        `json:"response_text"`
	ErrorMessage string        `json:"error_message"`
	LatencyMs    int64         `json:"latency_ms"`
	StartedAt    time.Time     `json:"started_at"`
	FinishedAt   time.Time     `json:"finished_at"`
	CreatedAt    time.Time     `json:"created_at"`
	HTTPStatus   int           `json:"-"`
	RetryAfter   time.Duration `json:"-"`
}

// ScheduledTestPlanRepository defines the data access interface for test plans.
type ScheduledTestPlanRepository interface {
	Create(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	GetByID(ctx context.Context, id int64) (*ScheduledTestPlan, error)
	ListByAccountID(ctx context.Context, accountID int64) ([]*ScheduledTestPlan, error)
	ListEnabled(ctx context.Context) ([]*ScheduledTestPlan, error)
	Update(ctx context.Context, plan *ScheduledTestPlan) (*ScheduledTestPlan, error)
	Delete(ctx context.Context, id int64) error
	UpdateAfterRun(ctx context.Context, plan *ScheduledTestPlan, lastRunAt time.Time, nextRunAt time.Time, status string, failures int) (bool, error)
}

// ScheduledTestResultRepository defines the data access interface for test results.
type ScheduledTestResultRepository interface {
	Create(ctx context.Context, result *ScheduledTestResult) (*ScheduledTestResult, error)
	ListByPlanID(ctx context.Context, planID int64, limit int) ([]*ScheduledTestResult, error)
	PruneOldResults(ctx context.Context, planID int64, keepCount int) error
}
