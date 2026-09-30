package handler

import (
	"context"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// Keep reservations on the active turn; the connection context outlives billing.
type openAIWSTurnReservation struct {
	mu          sync.Mutex
	reservation *service.InflightReservation
	done        func()
}

func (r *openAIWSTurnReservation) Begin(ctx context.Context, billing *service.BillingCacheService, estimator inflightReservationEstimator, apiKey *service.APIKey, subscription *service.UserSubscription, req service.InflightEstimateRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked()
	turnCtx, done, err := reserveInflightBalanceCtx(ctx, billing, estimator, apiKey, subscription, req)
	if err != nil {
		return err
	}
	r.reservation = service.InflightReservationFromContext(turnCtx)
	r.done = done
	return nil
}

func (r *openAIWSTurnReservation) Context(parent context.Context) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	return service.WithInflightReservation(parent, r.reservation)
}

func (r *openAIWSTurnReservation) End() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.endLocked()
}

func (r *openAIWSTurnReservation) endLocked() {
	if r.done != nil {
		r.done()
	}
	r.done = nil
	r.reservation = nil
}
