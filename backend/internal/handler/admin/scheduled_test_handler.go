package admin

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// ScheduledTestHandler handles admin scheduled-test-plan management.
type ScheduledTestHandler struct {
	scheduledTestSvc *service.ScheduledTestService
}

// NewScheduledTestHandler creates a new ScheduledTestHandler.
func NewScheduledTestHandler(scheduledTestSvc *service.ScheduledTestService) *ScheduledTestHandler {
	return &ScheduledTestHandler{scheduledTestSvc: scheduledTestSvc}
}

type createScheduledTestPlanRequest struct {
	AccountID                   int64  `json:"account_id" binding:"required"`
	ModelID                     string `json:"model_id"`
	CronExpression              string `json:"cron_expression"`
	Enabled                     *bool  `json:"enabled"`
	MaxResults                  int    `json:"max_results" binding:"omitempty,min=1,max=1000"`
	AutoRecover                 *bool  `json:"auto_recover"`
	ProbeIntervalSeconds        *int   `json:"probe_interval_seconds" binding:"omitempty,min=1,max=86400"`
	KeepaliveIntervalSeconds    *int   `json:"keepalive_interval_seconds" binding:"omitempty,min=1,max=86400"`
	KeepaliveMaxIntervalSeconds *int   `json:"keepalive_max_interval_seconds" binding:"omitempty,min=1,max=86400"`
}

type updateScheduledTestPlanRequest struct {
	ExpectedUpdatedAt           *time.Time `json:"expected_updated_at"`
	ModelID                     string     `json:"model_id"`
	CronExpression              string     `json:"cron_expression"`
	Enabled                     *bool      `json:"enabled"`
	MaxResults                  int        `json:"max_results" binding:"omitempty,min=1,max=1000"`
	AutoRecover                 *bool      `json:"auto_recover"`
	ProbeIntervalSeconds        *int       `json:"probe_interval_seconds" binding:"omitempty,min=1,max=86400"`
	KeepaliveIntervalSeconds    *int       `json:"keepalive_interval_seconds" binding:"omitempty,min=1,max=86400"`
	KeepaliveMaxIntervalSeconds *int       `json:"keepalive_max_interval_seconds" binding:"omitempty,min=1,max=86400"`
}

// ListByAccount GET /admin/accounts/:id/scheduled-test-plans
func (h *ScheduledTestHandler) ListByAccount(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid account id")
		return
	}

	plans, err := h.scheduledTestSvc.ListPlansByAccount(c.Request.Context(), accountID)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, plans)
}

// Create POST /admin/scheduled-test-plans
func (h *ScheduledTestHandler) Create(c *gin.Context) {
	var req createScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	plan := &service.ScheduledTestPlan{
		AccountID:      req.AccountID,
		ModelID:        req.ModelID,
		CronExpression: req.CronExpression,
		Enabled:        true,
		MaxResults:     req.MaxResults,
	}
	if req.Enabled != nil {
		plan.Enabled = *req.Enabled
	}
	if req.AutoRecover != nil {
		plan.AutoRecover = *req.AutoRecover
	}
	if req.ProbeIntervalSeconds != nil {
		plan.ProbeIntervalSeconds = *req.ProbeIntervalSeconds
	}
	if req.KeepaliveIntervalSeconds != nil {
		plan.KeepaliveIntervalSeconds = *req.KeepaliveIntervalSeconds
	}
	if req.KeepaliveMaxIntervalSeconds != nil {
		plan.KeepaliveMaxIntervalSeconds = *req.KeepaliveMaxIntervalSeconds
	}

	created, err := h.scheduledTestSvc.CreatePlan(c.Request.Context(), plan)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, created)
}

// Update PUT /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Update(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	existing, err := h.scheduledTestSvc.GetPlan(c.Request.Context(), planID)
	if err != nil {
		response.NotFound(c, "plan not found")
		return
	}

	var req updateScheduledTestPlanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	// Use the revision the editor observed, not just the freshly loaded row.
	// Old clients cannot re-arm recovery from a cached form without a revision.
	stale := req.ExpectedUpdatedAt != nil && !req.ExpectedUpdatedAt.Equal(existing.UpdatedAt)
	unguardedOptIn := req.AutoRecover != nil && *req.AutoRecover && !existing.AutoRecover && req.ExpectedUpdatedAt == nil
	if stale || unguardedOptIn {
		response.Error(c, http.StatusConflict, "Keepalive plan changed; refresh and retry")
		return
	}

	if req.ModelID != "" {
		existing.ModelID = req.ModelID
	}
	if req.CronExpression != "" {
		existing.CronExpression = req.CronExpression
	}
	if req.Enabled != nil {
		existing.Enabled = *req.Enabled
	}
	if req.MaxResults > 0 {
		existing.MaxResults = req.MaxResults
	}
	if req.AutoRecover != nil {
		existing.AutoRecover = *req.AutoRecover
	}
	if req.ProbeIntervalSeconds != nil {
		existing.ProbeIntervalSeconds = *req.ProbeIntervalSeconds
	}
	if req.KeepaliveIntervalSeconds != nil {
		existing.KeepaliveIntervalSeconds = *req.KeepaliveIntervalSeconds
	}
	if req.KeepaliveMaxIntervalSeconds != nil {
		existing.KeepaliveMaxIntervalSeconds = *req.KeepaliveMaxIntervalSeconds
	}

	updated, err := h.scheduledTestSvc.UpdatePlan(c.Request.Context(), existing)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			response.Error(c, http.StatusConflict, "Keepalive plan changed; refresh and retry")
			return
		}
		response.BadRequest(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, updated)
}

// Delete DELETE /admin/scheduled-test-plans/:id
func (h *ScheduledTestHandler) Delete(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	if err := h.scheduledTestSvc.DeletePlan(c.Request.Context(), planID); err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

// ListResults GET /admin/scheduled-test-plans/:id/results
func (h *ScheduledTestHandler) ListResults(c *gin.Context) {
	planID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "invalid plan id")
		return
	}

	limit := 50
	if l, err := strconv.Atoi(c.Query("limit")); err == nil && l > 0 {
		limit = l
	}

	results, err := h.scheduledTestSvc.ListResults(c.Request.Context(), planID, limit)
	if err != nil {
		response.InternalError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, results)
}
