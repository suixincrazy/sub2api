package service

import (
	"context"
	"time"
)

type ProxyLatencyInfo struct {
	Success          bool      `json:"success"`
	LatencyMs        *int64    `json:"latency_ms,omitempty"`
	Message          string    `json:"message,omitempty"`
	IPAddress        string    `json:"ip_address,omitempty"`
	Country          string    `json:"country,omitempty"`
	CountryCode      string    `json:"country_code,omitempty"`
	Region           string    `json:"region,omitempty"`
	City             string    `json:"city,omitempty"`
	QualityStatus    string    `json:"quality_status,omitempty"`
	QualityScore     *int      `json:"quality_score,omitempty"`
	QualityGrade     string    `json:"quality_grade,omitempty"`
	QualitySummary   string    `json:"quality_summary,omitempty"`
	QualityCheckedAt *int64    `json:"quality_checked_at,omitempty"`
	QualityCFRay     string    `json:"quality_cf_ray,omitempty"`
	QualityEngine    string    `json:"quality_engine,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// proxyQualityEngineVersion distinguishes current quality results from cache
// entries written before the runtime-backed probe was introduced.
const proxyQualityEngineVersion = "runtime-v2"

func hasCurrentProxyQuality(info *ProxyLatencyInfo) bool {
	return info != nil && info.QualityCheckedAt != nil && info.QualityEngine == proxyQualityEngineVersion
}

type ProxyLatencyCache interface {
	GetProxyLatencies(ctx context.Context, proxyIDs []int64) (map[int64]*ProxyLatencyInfo, error)
	SetProxyLatency(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) error
}
