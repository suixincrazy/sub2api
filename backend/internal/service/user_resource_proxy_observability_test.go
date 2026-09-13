package service

import (
	"context"
	"testing"
	"time"
)

type userResourceProxyLatencyCacheStub struct {
	items map[int64]*ProxyLatencyInfo
}

func (s *userResourceProxyLatencyCacheStub) GetProxyLatencies(_ context.Context, ids []int64) (map[int64]*ProxyLatencyInfo, error) {
	result := make(map[int64]*ProxyLatencyInfo, len(ids))
	for _, id := range ids {
		if item := s.items[id]; item != nil {
			copy := *item
			result[id] = &copy
		}
	}
	return result, nil
}

func (s *userResourceProxyLatencyCacheStub) SetProxyLatency(_ context.Context, id int64, info *ProxyLatencyInfo) error {
	copy := *info
	s.items[id] = &copy
	return nil
}

func TestUserResourceProxyObservabilityAttachesSafeDisplayFields(t *testing.T) {
	latency := int64(87)
	score := 94
	checkedAt := int64(1234)
	cache := &userResourceProxyLatencyCacheStub{items: map[int64]*ProxyLatencyInfo{
		11: {
			Success:          true,
			LatencyMs:        &latency,
			Country:          "United States",
			CountryCode:      "US",
			City:             "Los Angeles",
			QualityStatus:    "healthy",
			QualityScore:     &score,
			QualityGrade:     "A",
			QualitySummary:   "all checks passed",
			QualityCheckedAt: &checkedAt,
			QualityEngine:    proxyQualityEngineVersion,
		},
	}}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetProxyObservabilityServices(nil, cache)
	items := []map[string]any{{"id": int64(11), "name": "proxy"}}

	svc.attachProxyObservability(context.Background(), items)

	item := items[0]
	if item["latency_status"] != "success" || item["country_code"] != "US" || item["city"] != "Los Angeles" {
		t.Fatalf("proxy observation was not attached: %#v", item)
	}
	qualityChecked, ok := item["quality_checked"].(*int64)
	if item["quality_grade"] != "A" || !ok || qualityChecked == nil || *qualityChecked != checkedAt {
		t.Fatalf("proxy quality summary was not attached: %#v", item)
	}
	for _, sensitive := range []string{"username", "password", "extra"} {
		if _, ok := item[sensitive]; ok {
			t.Fatalf("observability enrichment introduced sensitive field %q: %#v", sensitive, item)
		}
	}
}

func TestUserResourceProxyObservationPreservesQualityOnConnectivityRefresh(t *testing.T) {
	score := 88
	checkedAt := int64(5678)
	cache := &userResourceProxyLatencyCacheStub{items: map[int64]*ProxyLatencyInfo{
		12: {
			QualityStatus:    "warn",
			QualityScore:     &score,
			QualityGrade:     "B",
			QualitySummary:   "one warning",
			QualityCheckedAt: &checkedAt,
		},
	}}
	svc := NewUserResourceService(nil, nil, nil, nil)
	svc.SetProxyObservabilityServices(nil, cache)
	latency := int64(42)

	svc.saveProxyObservation(context.Background(), 12, &ProxyLatencyInfo{
		Success:   true,
		LatencyMs: &latency,
		UpdatedAt: time.Now(),
	})

	saved := cache.items[12]
	if saved == nil || saved.QualityStatus != "warn" || saved.QualityGrade != "B" {
		t.Fatalf("connectivity refresh discarded quality data: %#v", saved)
	}
}
