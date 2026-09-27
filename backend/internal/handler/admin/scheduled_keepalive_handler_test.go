package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type keepaliveHandlerPlanRepo struct {
	service.ScheduledTestPlanRepository
	saved     *service.ScheduledTestPlan
	current   *service.ScheduledTestPlan
	updateErr error
}

func (r *keepaliveHandlerPlanRepo) GetByID(_ context.Context, _ int64) (*service.ScheduledTestPlan, error) {
	copy := *r.current
	return &copy, nil
}

func (r *keepaliveHandlerPlanRepo) Update(_ context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	if r.updateErr != nil {
		return nil, r.updateErr
	}
	r.saved = plan
	return plan, nil
}

func (r *keepaliveHandlerPlanRepo) Create(_ context.Context, plan *service.ScheduledTestPlan) (*service.ScheduledTestPlan, error) {
	r.saved = plan
	plan.ID = 1
	return plan, nil
}

func TestScheduledKeepaliveCreateAcceptsIntervalsWithoutCron(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"account_id":8,"model_id":"gpt-6-astra","probe_interval_seconds":2,"keepalive_interval_seconds":60,"keepalive_max_interval_seconds":90}`, http.StatusOK},
		{`{"account_id":8,"probe_interval_seconds":0}`, http.StatusBadRequest},
		{`{"account_id":8,"probe_interval_seconds":-2}`, http.StatusBadRequest},
		{`{"account_id":8,"keepalive_interval_seconds":100,"keepalive_max_interval_seconds":50}`, http.StatusBadRequest},
		{`{"account_id":8,"probe_interval_seconds":86401}`, http.StatusBadRequest},
	} {
		repo := &keepaliveHandlerPlanRepo{}
		handler := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.Create(c)
		require.Equal(t, tc.status, w.Code, w.Body.String())
		if tc.status == http.StatusOK {
			var plan service.ScheduledTestPlan
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &plan))
			require.Equal(t, 2, plan.ProbeIntervalSeconds)
			require.Equal(t, 60, plan.KeepaliveIntervalSeconds)
			require.Equal(t, 90, plan.KeepaliveMaxIntervalSeconds)
			require.NotNil(t, plan.NextRunAt)
			require.Empty(t, plan.CronExpression)
		} else {
			require.Nil(t, repo.saved)
		}
	}
}

func TestScheduledKeepaliveUpdateRejectsStaleRecoveryOptIn(t *testing.T) {
	now := time.Date(2026, 9, 27, 1, 0, 0, 123456000, time.UTC)
	for _, tc := range []struct {
		name      string
		body      string
		status    int
		updateErr error
	}{
		{name: "stale_edit_form", body: `{"auto_recover":true,"probe_interval_seconds":3,"expected_updated_at":"2026-09-27T00:00:00.123456Z"}`, status: http.StatusConflict},
		{name: "legacy_stale_form", body: `{"auto_recover":true,"probe_interval_seconds":3}`, status: http.StatusConflict},
		{name: "explicit_current_opt_in", body: `{"auto_recover":true,"expected_updated_at":"2026-09-27T01:00:00.123456Z"}`, status: http.StatusOK},
		{name: "interval_edit_without_opt_in", body: `{"probe_interval_seconds":3}`, status: http.StatusOK},
		{name: "pause_after_handler_read", body: `{"auto_recover":true,"expected_updated_at":"2026-09-27T01:00:00.123456Z"}`, status: http.StatusConflict, updateErr: sql.ErrNoRows},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &keepaliveHandlerPlanRepo{current: &service.ScheduledTestPlan{
				ID: 1, AccountID: 8, Enabled: true, AutoRecover: false, UpdatedAt: now,
				MaxResults: 100, ProbeIntervalSeconds: 2, KeepaliveIntervalSeconds: 60, KeepaliveMaxIntervalSeconds: 90,
			}, updateErr: tc.updateErr}
			handler := NewScheduledTestHandler(service.NewScheduledTestService(repo, nil))
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Params = gin.Params{{Key: "id", Value: "1"}}
			c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Update(c)
			require.Equal(t, tc.status, w.Code, w.Body.String())
			if tc.status == http.StatusConflict {
				require.Nil(t, repo.saved)
			} else {
				require.NotNil(t, repo.saved)
				require.Equal(t, tc.name == "explicit_current_opt_in", repo.saved.AutoRecover)
			}
		})
	}
}
