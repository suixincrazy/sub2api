package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

const (
	userProxyQualityWorkerCount = 3
	userProxyQualityQueueSize   = 2048
	userProxyQualityTimeout     = 2 * time.Minute
)

type userProxyQualityJob struct {
	ownerID *int64
	proxyID int64
}

type userProxyQualityRunner func(context.Context, *int64, int64) error

func (s *UserResourceService) StartProxyQualityWorkers() {
	if s == nil {
		return
	}
	s.proxyQualityMu.Lock()
	if s.proxyQualityCancel != nil {
		s.proxyQualityMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	jobs := make(chan userProxyQualityJob, userProxyQualityQueueSize)
	done := make(chan struct{})
	runner := s.proxyQualityRunner
	if runner == nil {
		runner = func(ctx context.Context, ownerID *int64, proxyID int64) error {
			_, err := s.qualityCheckProxyForOwner(ctx, ownerID, proxyID)
			return err
		}
	}
	s.proxyQualityCancel = cancel
	s.proxyQualityJobs = jobs
	s.proxyQualityDone = done
	s.proxyQualityMu.Unlock()

	go func() {
		var workers sync.WaitGroup
		workers.Add(userProxyQualityWorkerCount)
		for range userProxyQualityWorkerCount {
			go func() {
				defer workers.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case job := <-jobs:
						jobCtx, jobCancel := context.WithTimeout(ctx, userProxyQualityTimeout)
						err := runner(jobCtx, job.ownerID, job.proxyID)
						jobCancel()
						if err != nil && ctx.Err() == nil {
							logger.LegacyPrintf(
								"service.user_resources",
								"automatic proxy quality check failed: owner_id=%v proxy_id=%d err=%s",
								proxyQualityOwnerLogValue(job.ownerID),
								job.proxyID,
								logredact.RedactText(err.Error()),
							)
						}
					}
				}
			}()
		}
		workers.Wait()
		close(done)
	}()
}

func (s *UserResourceService) stopProxyQualityWorkers() {
	if s == nil {
		return
	}
	s.proxyQualityMu.Lock()
	cancel := s.proxyQualityCancel
	done := s.proxyQualityDone
	s.proxyQualityCancel = nil
	s.proxyQualityJobs = nil
	s.proxyQualityDone = nil
	s.proxyQualityMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.LegacyPrintf("service.user_resources", "proxy quality workers shutdown timed out")
		}
	}
}

func (s *UserResourceService) enqueueImportedProxyQualityChecks(ownerID int64, proxyIDs []int64) {
	if ownerID <= 0 {
		return
	}
	ownerCopy := ownerID
	s.enqueueProxyQualityChecks(&ownerCopy, proxyIDs)
}

func (s *UserResourceService) enqueueSystemProxyQualityChecks(proxyIDs []int64) {
	s.enqueueProxyQualityChecks(nil, proxyIDs)
}

func (s *UserResourceService) enqueueProxyQualityChecks(ownerID *int64, proxyIDs []int64) {
	proxyIDs = uniquePositiveInt64s(proxyIDs)
	if s == nil || len(proxyIDs) == 0 || (ownerID != nil && *ownerID <= 0) {
		return
	}
	ownerID = cloneProxyQualityOwnerID(ownerID)
	s.proxyQualityMu.Lock()
	jobs := s.proxyQualityJobs
	done := s.proxyQualityDone
	s.proxyQualityMu.Unlock()
	if jobs == nil || done == nil {
		return
	}

	go func(ids []int64) {
		for _, proxyID := range ids {
			select {
			case jobs <- userProxyQualityJob{ownerID: ownerID, proxyID: proxyID}:
			case <-done:
				return
			}
		}
	}(append([]int64(nil), proxyIDs...))
}

func cloneProxyQualityOwnerID(ownerID *int64) *int64 {
	if ownerID == nil {
		return nil
	}
	ownerCopy := *ownerID
	return &ownerCopy
}

func proxyQualityOwnerLogValue(ownerID *int64) any {
	if ownerID == nil {
		return "system"
	}
	return *ownerID
}

func proxyIDsFromResourceItems(items []map[string]any) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if id := urToInt64(item["id"]); id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func proxyIDsForProxySourceQualityChecks(result *ProxyImportResult) []int64 {
	if result == nil {
		return nil
	}
	ids := append(proxyIDsFromResourceItems(result.Created), proxyIDsFromResourceItems(result.Updated)...)
	return uniquePositiveInt64s(ids)
}
