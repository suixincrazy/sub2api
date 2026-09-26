package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type keepaliveHandlerPlanRepo struct {
	service.ScheduledTestPlanRepository
	saved *service.ScheduledTestPlan
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
