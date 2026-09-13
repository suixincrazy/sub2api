package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestImportedProxyQualityChecksUseBoundedWorkers(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	started := make(chan int64, 6)
	completed := make(chan int64, 6)
	release := make(chan struct{})
	var active atomic.Int32
	var maximum atomic.Int32
	svc.proxyQualityRunner = func(ctx context.Context, _ *int64, proxyID int64) error {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		started <- proxyID
		select {
		case <-release:
		case <-ctx.Done():
		}
		active.Add(-1)
		completed <- proxyID
		return nil
	}
	svc.StartProxyQualityWorkers()
	defer func() { _ = svc.Close() }()

	svc.enqueueImportedProxyQualityChecks(9, []int64{1, 2, 3, 4, 5, 6})
	for range userProxyQualityWorkerCount {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("quality worker did not start")
		}
	}
	select {
	case proxyID := <-started:
		t.Fatalf("quality concurrency exceeded %d workers; proxy %d started early", userProxyQualityWorkerCount, proxyID)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 6 {
		select {
		case <-completed:
		case <-time.After(2 * time.Second):
			t.Fatal("queued quality check did not complete")
		}
	}
	if maximum.Load() != userProxyQualityWorkerCount {
		t.Fatalf("maximum concurrency = %d, want %d", maximum.Load(), userProxyQualityWorkerCount)
	}
}

func TestImportedProxyQualityChecksUseUserOwner(t *testing.T) {
	type qualityCall struct {
		ownerID *int64
		proxyID int64
	}

	svc := NewUserResourceService(nil, nil, nil, nil)
	calls := make(chan qualityCall, 1)
	svc.proxyQualityRunner = func(_ context.Context, ownerID *int64, proxyID int64) error {
		calls <- qualityCall{ownerID: cloneProxyQualityOwnerID(ownerID), proxyID: proxyID}
		return nil
	}
	svc.StartProxyQualityWorkers()
	defer func() { _ = svc.Close() }()

	svc.enqueueImportedProxyQualityChecks(9, []int64{41})
	select {
	case call := <-calls:
		if call.ownerID == nil || *call.ownerID != 9 {
			t.Fatalf("quality owner = %v, want 9", proxyQualityOwnerLogValue(call.ownerID))
		}
		if call.proxyID != 41 {
			t.Fatalf("quality proxy = %d, want 41", call.proxyID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("user-owned quality check did not reach worker pool")
	}
}

func TestSystemProxyQualityChecksUseNilOwnerInWorkerPool(t *testing.T) {
	type qualityCall struct {
		ownerID *int64
		proxyID int64
	}

	svc := NewUserResourceService(nil, nil, nil, nil)
	calls := make(chan qualityCall, 1)
	svc.proxyQualityRunner = func(_ context.Context, ownerID *int64, proxyID int64) error {
		calls <- qualityCall{ownerID: cloneProxyQualityOwnerID(ownerID), proxyID: proxyID}
		return nil
	}
	svc.StartProxyQualityWorkers()
	defer func() { _ = svc.Close() }()

	svc.enqueueSystemProxyQualityChecks([]int64{51})
	select {
	case call := <-calls:
		if call.ownerID != nil {
			t.Fatalf("system quality owner = %d, want nil", *call.ownerID)
		}
		if call.proxyID != 51 {
			t.Fatalf("quality proxy = %d, want 51", call.proxyID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("system quality check did not reach worker pool")
	}
}

func TestImportedProxyQualityChecksAreIgnoredUntilWorkersStart(t *testing.T) {
	svc := NewUserResourceService(nil, nil, nil, nil)
	called := atomic.Bool{}
	svc.proxyQualityRunner = func(context.Context, *int64, int64) error {
		called.Store(true)
		return nil
	}
	svc.enqueueImportedProxyQualityChecks(9, []int64{1})
	time.Sleep(20 * time.Millisecond)
	if called.Load() {
		t.Fatal("quality check ran without a controlled worker pool")
	}
}
