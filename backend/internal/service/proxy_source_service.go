package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ProxySourceService struct {
	db                 *sql.DB
	proxyProber        ProxyExitInfoProber
	proxyLatencyCache  ProxyLatencyCache
	proxyProbeResolver ProxyProbeURLResolver
	proxySourceSchedMu sync.Mutex
	proxySourceCancel  context.CancelFunc
	proxySourceDone    chan struct{}
	proxyQualityMu     sync.Mutex
	proxyQualityCancel context.CancelFunc
	proxyQualityJobs   chan userProxyQualityJob
	proxyQualityDone   chan struct{}
	proxyQualityRunner userProxyQualityRunner
}

func NewProxySourceService(db *sql.DB) *ProxySourceService {
	return &ProxySourceService{db: db, proxyProbeResolver: DefaultProxyProbeRuntimeResolver()}
}

var ErrProxyResourceNotFound = infraerrors.NotFound("USER_RESOURCE_NOT_FOUND", "resource not found")

var resourceAuthHeaderPattern = regexp.MustCompile(`(?i)\b((?:authorization|proxy[-_]?authorization)\s*[:=]\s*)(?:bearer|basic)\s+[^\s,&}]+`)

var proxyImportURLPattern = regexp.MustCompile(`(?i)\b(?:https?|socks(?:5h?)?|vmess|vless|trojan|ss|hysteria2?|hy2|tuic|anytls|naive(?:\+https|\+quic)?|wireguard|wg)://[^\s,;]+`)

var proxyImportCredentialPattern = regexp.MustCompile(`(?i)\b[^\s/@:]+:[^\s/@]+@(?:\[[0-9a-f:]+\]|[a-z0-9.-]+)(?::\d{1,5})?`)

var proxyImportEndpointPattern = regexp.MustCompile(`(?i)\b(?:(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}|(?:\d{1,3}\.){3}\d{1,3})(?::\d{1,5})?\b`)

var proxyImportOpaqueSecretPattern = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)

const userResourceBatchMaxItems = 1000

const userResourceMaxProxies = 1000

const userResourceMaxProxySources = 100

type userResourceDBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func userResourceOwner(ownerID int64) *int64 {
	owner := ownerID
	return &owner
}

func userResourceOwnerValue(ownerID *int64) any {
	if ownerID == nil {
		return nil
	}
	return *ownerID
}

func userResourceOwnerMatches(value any, ownerID *int64) bool {
	if ownerID == nil {
		return value == nil
	}
	return urToInt64(value) == *ownerID
}

func (s *ProxySourceService) SetProxyObservabilityServices(
	proxyProber ProxyExitInfoProber,
	proxyLatencyCache ProxyLatencyCache,
) {
	if s == nil {
		return
	}
	s.proxyProber = proxyProber
	s.proxyLatencyCache = proxyLatencyCache
}

func (s *ProxySourceService) SetProxyProbeResolver(resolver ProxyProbeURLResolver) {
	if s == nil {
		return
	}
	s.proxyProbeResolver = resolver
}

type ProxySourceListOptions struct {
	Page      int
	PageSize  int
	Search    string
	Status    string
	Platform  string
	Type      string
	Protocol  string
	GroupID   int64
	UserID    int64
	APIKeyID  int64
	AccountID int64
	SourceID  int64
	StartDate string
	EndDate   string
	Timezone  string
	SortBy    string
	SortOrder string
	OwnedOnly bool
}

type ProxySourcePage struct {
	Items    []map[string]any `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
	Pages    int              `json:"pages"`
}

type ProxyImportResult struct {
	Created []map[string]any `json:"created"`
	Updated []map[string]any `json:"updated,omitempty"`
	Errors  []string         `json:"errors"`
}

type ProxySourceSyncResult struct {
	SourceID      int64            `json:"source_id"`
	Status        string           `json:"status"`
	ImportedCount int              `json:"imported_count"`
	CreatedCount  int              `json:"created_count"`
	UpdatedCount  int              `json:"updated_count"`
	Errors        []string         `json:"errors,omitempty"`
	Created       []map[string]any `json:"created,omitempty"`
	Updated       []map[string]any `json:"updated,omitempty"`
}

type ProxySourceSyncAllItem struct {
	SourceID      int64  `json:"source_id"`
	Name          string `json:"name"`
	Status        string `json:"status"`
	ImportedCount int    `json:"imported_count"`
	CreatedCount  int    `json:"created_count"`
	UpdatedCount  int    `json:"updated_count"`
	Error         string `json:"error,omitempty"`
}

type ProxySourceSyncAllResult struct {
	Total         int                      `json:"total"`
	SuccessCount  int                      `json:"success_count"`
	PartialCount  int                      `json:"partial_count"`
	FailedCount   int                      `json:"failed_count"`
	SkippedCount  int                      `json:"skipped_count"`
	DeferredCount int                      `json:"deferred_count"`
	CreatedCount  int                      `json:"created_count"`
	UpdatedCount  int                      `json:"updated_count"`
	Items         []ProxySourceSyncAllItem `json:"items"`
}

type columnSpec struct {
	Kind   string
	Create bool
	Update bool
}

const colString = "string"

const colInt = "int"

const colInt64 = "int64"

const colFloat = "float"

const colBool = "bool"

const colTime = "time"

const colJSON = "json"

var proxyWritableColumns = map[string]columnSpec{
	"name":             {Kind: colString, Create: true, Update: true},
	"is_public":        {Kind: colBool, Create: true, Update: true},
	"kind":             {Kind: colString, Create: true, Update: true},
	"protocol":         {Kind: colString, Create: true, Update: true},
	"host":             {Kind: colString, Create: true, Update: true},
	"port":             {Kind: colInt, Create: true, Update: true},
	"username":         {Kind: colString, Create: true, Update: true},
	"password":         {Kind: colString, Create: true, Update: true},
	"status":           {Kind: colString, Create: true, Update: true},
	"expires_at":       {Kind: colTime, Create: true, Update: true},
	"fallback_mode":    {Kind: colString, Create: true, Update: true},
	"backup_proxy_id":  {Kind: colInt64, Create: true, Update: true},
	"expiry_warn_days": {Kind: colInt, Create: true, Update: true},
	"extra":            {Kind: colJSON, Create: true, Update: true},
}

func (s *ProxySourceService) ensureDB() error {
	if s == nil || s.db == nil {
		return infraerrors.ServiceUnavailable("USER_RESOURCE_DB_UNAVAILABLE", "database is not available")
	}
	return nil
}

func (s *ProxySourceService) ensureResourceCapacityForOwner(ctx context.Context, table string, ownerID *int64, limit int) error {
	// System resources remain governed by administrator access rather than the
	// per-user resource quotas used by /my endpoints.
	if ownerID == nil {
		return nil
	}
	if limit <= 0 {
		return infraerrors.New(http.StatusTooManyRequests, "USER_RESOURCE_LIMIT_REACHED", "resource limit reached")
	}
	switch table {
	case "groups", "accounts", "proxies", "proxy_sources":
	default:
		return fmt.Errorf("unsupported owned resource table %q", table)
	}
	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL", table)
	if err := s.db.QueryRowContext(ctx, query, userResourceOwnerValue(ownerID)).Scan(&count); err != nil {
		return err
	}
	if count >= limit {
		return infraerrors.Newf(http.StatusTooManyRequests, "USER_RESOURCE_LIMIT_REACHED", "%s limit of %d reached", table, limit)
	}
	return nil
}

func normalizeResourcePage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 1000 {
		pageSize = 1000
	}
	return page, pageSize
}

func paged(items []map[string]any, total int64, page, pageSize int) *ProxySourcePage {
	pages := int(math.Ceil(float64(total) / float64(pageSize)))
	if pages < 1 {
		pages = 1
	}
	return &ProxySourcePage{Items: items, Total: total, Page: page, PageSize: pageSize, Pages: pages}
}

func nextArg(args *[]any, v any) string {
	*args = append(*args, v)
	return fmt.Sprintf("$%d", len(*args))
}

func (s *ProxySourceService) getProxyForExactOwner(ctx context.Context, ownerID *int64, proxyID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
  p.id, p.owner_user_id, p.is_public, p.kind, p.name, p.protocol, p.host, p.port,
  p.username, p.password, (COALESCE(p.username, '') <> '' OR COALESCE(p.password, '') <> '') AS has_auth,
  p.status, p.expires_at, p.fallback_mode, p.backup_proxy_id, p.expiry_warn_days,
  COALESCE(p.extra, '{}'::jsonb)::text AS extra, p.created_at, p.updated_at,
  (SELECT COUNT(*) FROM accounts a WHERE a.proxy_id = p.id AND a.deleted_at IS NULL)::bigint AS account_count
FROM proxies p
WHERE p.id = $1
  AND p.owner_user_id IS NOT DISTINCT FROM $2
  AND p.deleted_at IS NULL
LIMIT 1`, proxyID, userResourceOwnerValue(ownerID))
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrProxyResourceNotFound
	}
	s.attachProxyObservability(ctx, items)
	return items[0], nil
}

func (s *ProxySourceService) createProxyForOwner(ctx context.Context, ownerID *int64, payload map[string]any, preserveSourceMetadata bool) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	payload = clonePayload(payload)
	if !preserveSourceMetadata {
		stripProxySourceMetadata(payload)
	}
	defaultPayload(payload, map[string]any{
		"kind":             "standard",
		"is_public":        false,
		"status":           StatusActive,
		"fallback_mode":    FallbackModeNone,
		"expiry_warn_days": 7,
		"extra":            map[string]any{},
	})
	if err := s.normalizeAndValidateProxyPayloadForOwner(ctx, ownerID, 0, nil, payload); err != nil {
		return nil, err
	}
	if err := s.ensureResourceCapacityForOwner(ctx, "proxies", ownerID, userResourceMaxProxies); err != nil {
		return nil, err
	}
	id, err := s.insertForOwnerWith(ctx, s.db, "proxies", ownerID, proxyWritableColumns, payload, []string{"name", "protocol", "host", "port"})
	if err != nil {
		return nil, err
	}
	return s.getProxyForExactOwner(ctx, ownerID, id)
}

func (s *ProxySourceService) attachProxyObservability(ctx context.Context, items []map[string]any) {
	if s == nil || s.proxyLatencyCache == nil || len(items) == 0 {
		return
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		if id := urToInt64(item["id"]); id > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	observations, err := s.proxyLatencyCache.GetProxyLatencies(ctx, ids)
	if err != nil {
		return
	}
	for _, item := range items {
		info := observations[urToInt64(item["id"])]
		if info == nil {
			continue
		}
		if info.Success {
			item["latency_status"] = "success"
			item["latency_ms"] = info.LatencyMs
		} else {
			item["latency_status"] = "failed"
		}
		item["latency_message"] = info.Message
		item["ip_address"] = info.IPAddress
		item["country"] = info.Country
		item["country_code"] = info.CountryCode
		item["region"] = info.Region
		item["city"] = info.City
		if hasCurrentProxyQuality(info) {
			item["quality_status"] = info.QualityStatus
			item["quality_score"] = info.QualityScore
			item["quality_grade"] = info.QualityGrade
			item["quality_summary"] = info.QualitySummary
			item["quality_checked"] = info.QualityCheckedAt
		}
	}
}

func (s *ProxySourceService) saveProxyObservation(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) {
	if s == nil || s.proxyLatencyCache == nil || info == nil || proxyID <= 0 {
		return
	}
	merged := *info
	if observations, err := s.proxyLatencyCache.GetProxyLatencies(ctx, []int64{proxyID}); err == nil {
		if existing := observations[proxyID]; existing != nil &&
			merged.QualityCheckedAt == nil &&
			merged.QualityScore == nil &&
			merged.QualityGrade == "" &&
			merged.QualityStatus == "" &&
			merged.QualitySummary == "" &&
			merged.QualityCFRay == "" {
			merged.QualityStatus = existing.QualityStatus
			merged.QualityScore = existing.QualityScore
			merged.QualityGrade = existing.QualityGrade
			merged.QualitySummary = existing.QualitySummary
			merged.QualityCheckedAt = existing.QualityCheckedAt
			merged.QualityCFRay = existing.QualityCFRay
			merged.QualityEngine = existing.QualityEngine
		}
	}
	_ = s.proxyLatencyCache.SetProxyLatency(ctx, proxyID, &merged)
}

func (s *ProxySourceService) ImportSystemProxyNodes(ctx context.Context, namePrefix, raw string, isPublic bool) (*ProxyImportResult, error) {
	result, err := s.importProxyNodesForOwner(ctx, nil, namePrefix, raw, isPublic)
	if err == nil {
		s.enqueueSystemProxyQualityChecks(proxyIDsFromResourceItems(result.Created))
	}
	return result, err
}

func (s *ProxySourceService) importProxyNodesForOwner(ctx context.Context, ownerID *int64, namePrefix, raw string, isPublic bool) (*ProxyImportResult, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	nodes := parseProxyNodeLines(raw)
	if len(nodes) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "proxy imports cannot exceed 1000 nodes")
	}
	result := &ProxyImportResult{Created: []map[string]any{}, Errors: []string{}}
	for i, node := range nodes {
		if node.Err != "" {
			result.Errors = append(result.Errors, formatProxyImportError(i, node.Err))
			continue
		}
		name := node.Name
		if name == "" {
			name = fmt.Sprintf("%s-%d", strings.TrimSpace(namePrefix), i+1)
		}
		if name == "-" || name == "" {
			name = fmt.Sprintf("node-%d", i+1)
		}
		payload := map[string]any{
			"name":      name,
			"is_public": isPublic,
			"kind":      node.Kind,
			"protocol":  node.Protocol,
			"host":      node.Host,
			"port":      node.Port,
			"username":  node.Username,
			"password":  node.Password,
			"extra":     map[string]any{"raw": node.Raw, "network": node.Network},
		}
		created, err := s.createProxyForOwner(ctx, ownerID, payload, false)
		if err != nil {
			result.Errors = append(result.Errors, formatProxyImportError(i, err.Error()))
			continue
		}
		result.Created = append(result.Created, created)
	}
	return result, nil
}

func (s *ProxySourceService) qualityCheckProxyForOwner(ctx context.Context, ownerID *int64, proxyID int64) (*ProxyQualityCheckResult, error) {
	proxyItem, err := s.getSelectableProxyRawForOwner(ctx, ownerID, proxyID)
	if err != nil {
		return nil, err
	}
	result := &ProxyQualityCheckResult{
		ProxyID:   proxyID,
		Score:     100,
		Grade:     "A",
		CheckedAt: time.Now().Unix(),
		Items:     make([]ProxyQualityCheckItem, 0, len(proxyQualityTargets)+1),
	}
	if !userResourceOwnerMatches(proxyItem["owner_user_id"], ownerID) {
		defer func() {
			result.ExitIP = ""
			for index := range result.Items {
				if result.Items[index].Target == "base_connectivity" {
					result.Items[index].Message = ""
				}
			}
		}()
	}
	var exitInfo *ProxyExitInfo
	defer func() {
		s.saveUserProxyQualitySnapshot(ctx, proxyID, result, exitInfo)
	}()
	proxyURL, cleanup, resolveErr := resolveProxyProbeURL(ctx, s.proxyProbeResolver, proxyFromResourceMap(proxyItem))
	if resolveErr != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "base_connectivity",
			Status:  "fail",
			Message: logredact.RedactText(resolveErr.Error()),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		return result, nil
	}
	defer cleanup()

	var base ProxyQualityCheckItem
	if s.proxyProber != nil {
		probedExitInfo, latency, probeErr := s.proxyProber.ProbeProxy(ctx, proxyURL)
		if probeErr != nil {
			base = ProxyQualityCheckItem{
				Target:  "base_connectivity",
				Status:  "fail",
				Message: logredact.RedactText(probeErr.Error()),
			}
		} else {
			exitInfo = probedExitInfo
			base = ProxyQualityCheckItem{
				Target:    "base_connectivity",
				Status:    "pass",
				LatencyMs: latency,
				Message:   "proxy endpoint reachable",
			}
			result.BaseLatencyMs = latency
			if exitInfo != nil {
				result.ExitIP = exitInfo.IP
				result.Country = exitInfo.Country
				result.CountryCode = exitInfo.CountryCode
			}
		}
	} else {
		base = probeProxyEndpoint(ctx, proxyURL)
	}
	result.Items = append(result.Items, base)
	if base.Status == "pass" {
		result.PassedCount++
		if result.BaseLatencyMs == 0 {
			result.BaseLatencyMs = base.LatencyMs
		}
	} else {
		result.FailedCount++
		finalizeProxyQualityResult(result)
		return result, nil
	}

	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:              proxyURL,
		Timeout:               proxyQualityRequestTimeout,
		ResponseHeaderTimeout: proxyQualityResponseHeaderTimeout,
	})
	if err != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "http_client",
			Status:  "fail",
			Message: "failed to create proxy HTTP client",
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		return result, nil
	}
	for _, target := range proxyQualityTargets {
		item := runProxyQualityTarget(ctx, client, target)
		result.Items = append(result.Items, item)
		switch item.Status {
		case "pass":
			result.PassedCount++
		case "warn":
			result.WarnCount++
		case "challenge":
			result.ChallengeCount++
		default:
			result.FailedCount++
		}
	}
	finalizeProxyQualityResult(result)
	return result, nil
}

func (s *ProxySourceService) saveUserProxyQualitySnapshot(
	ctx context.Context,
	proxyID int64,
	result *ProxyQualityCheckResult,
	exitInfo *ProxyExitInfo,
) {
	if result == nil {
		return
	}
	score := result.Score
	checkedAt := result.CheckedAt
	info := &ProxyLatencyInfo{
		Success:          proxyQualityBaseConnectivityPass(result),
		Message:          result.Summary,
		QualityStatus:    proxyQualityOverallStatus(result),
		QualityScore:     &score,
		QualityGrade:     result.Grade,
		QualitySummary:   result.Summary,
		QualityCheckedAt: &checkedAt,
		QualityCFRay:     proxyQualityFirstCFRay(result),
		QualityEngine:    proxyQualityEngineVersion,
		UpdatedAt:        time.Now(),
	}
	if result.BaseLatencyMs > 0 {
		latency := result.BaseLatencyMs
		info.LatencyMs = &latency
	}
	if exitInfo != nil {
		info.IPAddress = exitInfo.IP
		info.Country = exitInfo.Country
		info.CountryCode = exitInfo.CountryCode
		info.Region = exitInfo.Region
		info.City = exitInfo.City
	}
	s.saveProxyObservation(ctx, proxyID, info)
}

const proxySourceSelectColumns = `id, owner_user_id, name, subscription_url, is_public, refresh_interval_minutes,
       last_synced_at, last_sync_status, last_sync_error, last_imported_count,
       sync_enabled, sub_traffic_used, sub_traffic_total, sub_expires_at, sub_info_updated_at,
       CASE
         WHEN NOT sync_enabled THEN NULL::timestamptz
         WHEN last_synced_at IS NULL THEN NOW()
         ELSE last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute')
       END AS next_sync_at,
       (SELECT COUNT(*) FROM proxies pn
         WHERE pn.owner_user_id IS NOT DISTINCT FROM proxy_sources.owner_user_id
           AND pn.deleted_at IS NULL
           AND pn.extra->>'source_id' = proxy_sources.id::text)::bigint AS node_count,
       (SELECT COUNT(*) FROM proxies pn
         WHERE pn.owner_user_id IS NOT DISTINCT FROM proxy_sources.owner_user_id
           AND pn.deleted_at IS NULL
           AND pn.status = 'active'
           AND pn.extra->>'source_id' = proxy_sources.id::text)::bigint AS active_node_count,
       created_at, updated_at`

func (s *ProxySourceService) ListSystemProxySources(ctx context.Context, opts ProxySourceListOptions) (*ProxySourcePage, error) {
	return s.listProxySourcesForOwner(ctx, nil, opts)
}

func (s *ProxySourceService) listProxySourcesForOwner(ctx context.Context, ownerID *int64, opts ProxySourceListOptions) (*ProxySourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{userResourceOwnerValue(ownerID)}
	where := []string{"owner_user_id IS NOT DISTINCT FROM $1", "deleted_at IS NULL"}
	if opts.Status != "" {
		where = append(where, "last_sync_status = "+nextArg(&args, opts.Status))
	}
	if opts.Search != "" {
		p := "%" + opts.Search + "%"
		where = append(where, "(name ILIKE "+nextArg(&args, p)+" OR subscription_url ILIKE "+nextArg(&args, p)+")")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxy_sources WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT `+proxySourceSelectColumns+`
FROM proxy_sources
WHERE `+whereSQL+`
ORDER BY updated_at DESC, id DESC
LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	return paged(items, total, page, pageSize), nil
}

func (s *ProxySourceService) CreateSystemProxySource(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return s.createProxySourceForOwner(ctx, nil, payload)
}

func (s *ProxySourceService) createProxySourceForOwner(ctx context.Context, ownerID *int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(urAsString(payload["name"]))
	subscriptionURL := strings.TrimSpace(urAsString(payload["subscription_url"]))
	if name == "" || subscriptionURL == "" {
		return nil, infraerrors.BadRequest("PROXY_SOURCE_REQUIRED", "name and subscription_url are required")
	}
	if err := validateExternalHTTPURL(ctx, subscriptionURL); err != nil {
		return nil, err
	}
	if err := s.ensureResourceCapacityForOwner(ctx, "proxy_sources", ownerID, userResourceMaxProxySources); err != nil {
		return nil, err
	}
	interval := toInt(payload["refresh_interval_minutes"])
	if interval <= 0 {
		interval = 1440
	}
	if interval < 5 || interval > 7*24*60 {
		return nil, infraerrors.BadRequest("PROXY_SOURCE_INTERVAL_INVALID", "refresh_interval_minutes must be between 5 and 10080")
	}
	isPublic := false
	if rawVisibility, exists := payload["is_public"]; exists {
		var err error
		isPublic, err = strictBoolValue(rawVisibility)
		if err != nil {
			return nil, invalidUserResourceField("is_public", err.Error())
		}
	}
	syncEnabled := true
	if rawSyncEnabled, exists := payload["sync_enabled"]; exists {
		var err error
		syncEnabled, err = strictBoolValue(rawSyncEnabled)
		if err != nil {
			return nil, invalidUserResourceField("sync_enabled", err.Error())
		}
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `
INSERT INTO proxy_sources (owner_user_id, name, subscription_url, is_public, refresh_interval_minutes, sync_enabled, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
RETURNING id`, userResourceOwnerValue(ownerID), name, subscriptionURL, isPublic, interval, syncEnabled).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.getProxySourceForOwner(ctx, ownerID, id)
}

func (s *ProxySourceService) getProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+proxySourceSelectColumns+`
FROM proxy_sources
WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
LIMIT 1`, sourceID, userResourceOwnerValue(ownerID))
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrProxyResourceNotFound
	}
	return items[0], nil
}

func (s *ProxySourceService) UpdateSystemProxySource(ctx context.Context, sourceID int64, payload map[string]any) (map[string]any, error) {
	return s.updateProxySourceForOwner(ctx, nil, sourceID, payload)
}

func (s *ProxySourceService) updateProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	assignments := []string{}
	args := []any{}
	if _, ok := payload["name"]; ok {
		name := strings.TrimSpace(urAsString(payload["name"]))
		if name == "" {
			return nil, infraerrors.BadRequest("PROXY_SOURCE_NAME_REQUIRED", "name is required")
		}
		args = append(args, name)
		assignments = append(assignments, fmt.Sprintf("name = $%d", len(args)))
	}
	if _, ok := payload["subscription_url"]; ok {
		subscriptionURL := strings.TrimSpace(urAsString(payload["subscription_url"]))
		if subscriptionURL == "" {
			return nil, infraerrors.BadRequest("PROXY_SOURCE_URL_REQUIRED", "subscription_url is required")
		}
		if err := validateExternalHTTPURL(ctx, subscriptionURL); err != nil {
			return nil, err
		}
		args = append(args, subscriptionURL)
		assignments = append(assignments, fmt.Sprintf("subscription_url = $%d", len(args)))
	}
	if _, ok := payload["refresh_interval_minutes"]; ok {
		interval := toInt(payload["refresh_interval_minutes"])
		if interval < 5 || interval > 7*24*60 {
			return nil, infraerrors.BadRequest("PROXY_SOURCE_INTERVAL_INVALID", "refresh_interval_minutes must be between 5 and 10080")
		}
		args = append(args, interval)
		assignments = append(assignments, fmt.Sprintf("refresh_interval_minutes = $%d", len(args)))
	}
	if _, ok := payload["sync_enabled"]; ok {
		syncEnabled, err := strictBoolValue(payload["sync_enabled"])
		if err != nil {
			return nil, invalidUserResourceField("sync_enabled", err.Error())
		}
		args = append(args, syncEnabled)
		assignments = append(assignments, fmt.Sprintf("sync_enabled = $%d", len(args)))
	}
	visibilityChanged := false
	if _, ok := payload["is_public"]; ok {
		isPublic, err := strictBoolValue(payload["is_public"])
		if err != nil {
			return nil, invalidUserResourceField("is_public", err.Error())
		}
		args = append(args, isPublic)
		assignments = append(assignments, fmt.Sprintf("is_public = $%d", len(args)))
		visibilityChanged = true
	}
	if len(assignments) == 0 {
		return s.getProxySourceForOwner(ctx, ownerID, sourceID)
	}
	assignments = append(assignments, "updated_at = NOW()")
	args = append(args, sourceID, userResourceOwnerValue(ownerID))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, fmt.Sprintf(`
UPDATE proxy_sources SET %s
WHERE id = $%d AND owner_user_id IS NOT DISTINCT FROM $%d AND deleted_at IS NULL`, strings.Join(assignments, ", "), len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrProxyResourceNotFound
	}
	if visibilityChanged {
		isPublic := toBool(payload["is_public"])
		if _, err := tx.ExecContext(ctx, `
UPDATE proxies
SET is_public = $1, updated_at = NOW()
WHERE owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL AND extra->>'source_id' = $3`,
			isPublic, userResourceOwnerValue(ownerID), strconv.FormatInt(sourceID, 10)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.getProxySourceForOwner(ctx, ownerID, sourceID)
}

func (s *ProxySourceService) DeleteSystemProxySource(ctx context.Context, sourceID int64) error {
	return s.deleteProxySourceForOwner(ctx, nil, sourceID)
}

func (s *ProxySourceService) deleteProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
UPDATE proxy_sources SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL`, sourceID, userResourceOwnerValue(ownerID))
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrProxyResourceNotFound
	}
	staleIDs, accountIDs, err := s.disableMissingProxySourceNodesWith(ctx, tx, ownerID, sourceID, nil)
	if err != nil {
		return err
	}
	if err := enqueueProxyDependentChangesWith(ctx, tx, accountIDs); err != nil {
		return err
	}
	for _, proxyID := range staleIDs {
		if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
			return fmt.Errorf("stop proxy source runtime before delete: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, proxyID := range staleIDs {
		if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
			slog.Error("proxy source runtime cleanup failed", "owner_user_id", userResourceOwnerValue(ownerID), "source_id", sourceID, "proxy_id", proxyID, "error", err)
		}
	}
	return nil
}

func (s *ProxySourceService) SyncProxySource(ctx context.Context, ownerID, sourceID int64) (*ProxySourceSyncResult, error) {
	return s.syncProxySourceForOwner(ctx, userResourceOwner(ownerID), sourceID)
}

func (s *ProxySourceService) SyncSystemProxySource(ctx context.Context, sourceID int64) (*ProxySourceSyncResult, error) {
	return s.syncProxySourceForOwner(ctx, nil, sourceID)
}

func (s *ProxySourceService) syncProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) (*ProxySourceSyncResult, error) {
	source, err := s.getProxySourceForOwner(ctx, ownerID, sourceID)
	if err != nil {
		return nil, err
	}
	subscriptionURL := urAsString(source["subscription_url"])
	content, userInfoHeader, err := fetchProxySubscription(ctx, subscriptionURL)
	if err != nil {
		if statusErr := s.recordProxySourceSyncError(ctx, ownerID, sourceID, err); statusErr != nil {
			return nil, errors.Join(err, fmt.Errorf("record proxy source sync failure: %w", statusErr))
		}
		return nil, err
	}
	// Informational only: a failure here must not abort a successful import.
	if infoErr := s.recordProxySourceSubscriptionInfo(ctx, ownerID, sourceID, parseProxySubscriptionUserInfo(userInfoHeader)); infoErr != nil {
		slog.Warn("record proxy source subscription info failed", "owner_user_id", userResourceOwnerValue(ownerID), "source_id", sourceID, "error", infoErr)
	}
	imported, err := s.syncProxySourceNodes(ctx, ownerID, sourceID, urAsString(source["name"]), content, toBool(source["is_public"]))
	if err != nil {
		if statusErr := s.recordProxySourceSyncError(ctx, ownerID, sourceID, err); statusErr != nil {
			return nil, errors.Join(err, fmt.Errorf("record proxy source sync failure: %w", statusErr))
		}
		return nil, err
	}
	status, importedCount, _ := proxySourceSyncSummary(imported)
	qualityProxyIDs := proxyIDsForProxySourceQualityChecks(imported)
	if ownerID == nil {
		s.enqueueSystemProxyQualityChecks(qualityProxyIDs)
	} else {
		s.enqueueImportedProxyQualityChecks(*ownerID, qualityProxyIDs)
	}
	return &ProxySourceSyncResult{
		SourceID:      sourceID,
		Status:        status,
		ImportedCount: importedCount,
		CreatedCount:  len(imported.Created),
		UpdatedCount:  len(imported.Updated),
		Errors:        imported.Errors,
		Created:       imported.Created,
		Updated:       imported.Updated,
	}, nil
}

const proxySourceSyncStatusSkipped = "skipped"

const proxySourceSyncStatusDeferred = "deferred"

const proxySourceSyncAllBudget = 100 * time.Second

const proxySourceSyncAllErrorLimit = 1000

func (s *ProxySourceService) SyncAllSystemProxySources(ctx context.Context) (*ProxySourceSyncAllResult, error) {
	return s.syncAllProxySourcesForOwner(ctx, nil)
}

func (s *ProxySourceService) syncAllProxySourcesForOwner(ctx context.Context, ownerID *int64) (*ProxySourceSyncAllResult, error) {
	sources, err := s.listProxySourceSyncTargets(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	result := &ProxySourceSyncAllResult{Total: len(sources), Items: make([]ProxySourceSyncAllItem, 0, len(sources))}
	deadline := time.Now().Add(proxySourceSyncAllBudget)
	for _, source := range sources {
		item := ProxySourceSyncAllItem{SourceID: source.id, Name: source.name}
		if !source.syncEnabled {
			// A paused source is paused deliberately: syncing it here would revive
			// nodes the owner froze, and a source they already know is broken would
			// add a failure line to every single run.
			item.Status = proxySourceSyncStatusSkipped
			result.SkippedCount++
		} else if time.Now().After(deadline) {
			item.Status = proxySourceSyncStatusDeferred
			result.DeferredCount++
		} else if syncResult, syncErr := s.syncProxySourceForOwner(ctx, ownerID, source.id); syncErr != nil {
			item.Status = "error"
			item.Error = safeSyncError(syncErr)
			result.FailedCount++
		} else {
			item.Status = syncResult.Status
			item.ImportedCount = syncResult.ImportedCount
			item.CreatedCount = syncResult.CreatedCount
			item.UpdatedCount = syncResult.UpdatedCount
			item.Error = truncateUTF8(strings.Join(syncResult.Errors, "\n"), proxySourceSyncAllErrorLimit)
			result.CreatedCount += syncResult.CreatedCount
			result.UpdatedCount += syncResult.UpdatedCount
			switch syncResult.Status {
			case "success":
				result.SuccessCount++
			case "partial":
				result.PartialCount++
			default:
				result.FailedCount++
			}
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

type proxySourceSyncTarget struct {
	id          int64
	name        string
	syncEnabled bool
}

func (s *ProxySourceService) listProxySourceSyncTargets(ctx context.Context, ownerID *int64) ([]proxySourceSyncTarget, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, sync_enabled FROM proxy_sources
WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL
ORDER BY id ASC`, userResourceOwnerValue(ownerID))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	targets := make([]proxySourceSyncTarget, 0)
	for rows.Next() {
		var target proxySourceSyncTarget
		if err := rows.Scan(&target.id, &target.name, &target.syncEnabled); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
}

func (s *ProxySourceService) syncProxySourceNodes(ctx context.Context, ownerID *int64, sourceID int64, sourceName, raw string, isPublic bool) (*ProxyImportResult, error) {
	nodes := parseProxyNodeLines(raw)
	if len(nodes) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "proxy imports cannot exceed 1000 nodes")
	}
	result := &ProxyImportResult{Created: []map[string]any{}, Updated: []map[string]any{}, Errors: []string{}}
	existingByKey, err := proxySourceNodesByKey(ctx, s.db, ownerID, sourceID)
	if err != nil {
		return nil, err
	}
	type preparedNode struct {
		key     string
		payload map[string]any
	}
	prepared := make([]preparedNode, 0, len(nodes))
	seenKeys := make([]string, 0, len(nodes))
	keyOccurrences := make(map[string]int, len(nodes))
	for index, node := range nodes {
		if node.Err != "" {
			result.Errors = append(result.Errors, formatProxyImportError(index, node.Err))
			continue
		}
		baseKey := proxySourceNodeBaseKey(node)
		keyOccurrences[baseKey]++
		nodeKey := proxySourceNodeKey(baseKey, keyOccurrences[baseKey])
		seenKeys = append(seenKeys, nodeKey)
		name := proxySourceNodeName(sourceID, sourceName, node.Name, index+1)
		payload := map[string]any{
			"name":      name,
			"is_public": isPublic,
			"kind":      node.Kind,
			"protocol":  node.Protocol,
			"host":      node.Host,
			"port":      node.Port,
			"username":  node.Username,
			"password":  node.Password,
			"status":    StatusActive,
			"extra": map[string]any{
				"raw": node.Raw, "network": node.Network,
				"source_id": sourceID, "source_node_key": nodeKey,
			},
		}
		existing := existingByKey[nodeKey]
		proxyID := urToInt64(existing["id"])
		if existing == nil {
			defaultPayload(payload, map[string]any{
				"fallback_mode":    FallbackModeNone,
				"expiry_warn_days": 7,
			})
		}
		if err := s.normalizeAndValidateProxyPayloadForOwner(ctx, ownerID, proxyID, existing, payload); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("entry %d: validation failed", index+1))
			continue
		}
		prepared = append(prepared, preparedNode{key: nodeKey, payload: payload})
	}
	// A non-empty response where every entry failed to parse is not authoritative;
	// preserve the last known-good nodes so a format regression cannot drop a pool.
	allEntriesFailed := len(nodes) > 0 && len(seenKeys) == 0 && len(result.Errors) > 0
	authoritative := !allEntriesFailed
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var lockedSourceID int64
	if err := tx.QueryRowContext(ctx, `
SELECT id FROM proxy_sources
WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
FOR UPDATE`, sourceID, userResourceOwnerValue(ownerID)).Scan(&lockedSourceID); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrProxyResourceNotFound
		}
		return nil, err
	}
	currentByKey, err := proxySourceNodesByKey(ctx, tx, ownerID, sourceID)
	if err != nil {
		return nil, err
	}
	newCount := 0
	for _, candidate := range prepared {
		if currentByKey[candidate.key] == nil {
			newCount++
		}
	}
	if newCount > 0 && ownerID != nil {
		var ownedCount int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxies WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL", userResourceOwnerValue(ownerID)).Scan(&ownedCount); err != nil {
			return nil, err
		}
		if ownedCount+newCount > userResourceMaxProxies {
			return nil, infraerrors.Newf(http.StatusTooManyRequests, "USER_RESOURCE_LIMIT_REACHED", "proxies limit of %d reached", userResourceMaxProxies)
		}
	}
	updatedRuntimeIDs := make([]int64, 0, len(prepared))
	for _, candidate := range prepared {
		payload := clonePayload(candidate.payload)
		if current := currentByKey[candidate.key]; current != nil {
			proxyID := urToInt64(current["id"])
			if err := s.updateForOwnerWith(ctx, tx, "proxies", ownerID, proxyID, proxyWritableColumns, payload); err != nil {
				return nil, err
			}
			if ProxyConnectionKey(proxyFromResourceMap(current)) != ProxyConnectionKey(proxyFromResourceMap(mergeResourceState(current, payload, proxyWritableColumns))) {
				updatedRuntimeIDs = append(updatedRuntimeIDs, proxyID)
			}
			result.Updated = append(result.Updated, proxySourceMutationItem(proxyID, ownerID, payload))
			continue
		}
		defaultPayload(payload, map[string]any{
			"fallback_mode":    FallbackModeNone,
			"expiry_warn_days": 7,
			"extra":            map[string]any{},
		})
		proxyID, err := s.insertForOwnerWith(ctx, tx, "proxies", ownerID, proxyWritableColumns, payload, []string{"name", "protocol", "host", "port"})
		if err != nil {
			return nil, err
		}
		result.Created = append(result.Created, proxySourceMutationItem(proxyID, ownerID, payload))
	}
	staleIDs := []int64{}
	accountIDs := []int64{}
	if authoritative {
		staleIDs, accountIDs, err = s.disableMissingProxySourceNodesWith(ctx, tx, ownerID, sourceID, seenKeys)
		if err != nil {
			return nil, err
		}
	}
	updatedAccountIDs, err := clearProxyDependentProbeWith(ctx, tx, updatedRuntimeIDs)
	if err != nil {
		return nil, err
	}
	accountIDs = append(accountIDs, updatedAccountIDs...)
	if err := enqueueProxyDependentChangesWith(ctx, tx, accountIDs); err != nil {
		return nil, err
	}
	status, importedCount, errorText := proxySourceSyncSummary(result)
	res, err := tx.ExecContext(ctx, `
UPDATE proxy_sources
SET last_synced_at = NOW(), last_sync_status = $1, last_sync_error = $2,
    last_imported_count = $3, updated_at = NOW()
WHERE id = $4 AND owner_user_id IS NOT DISTINCT FROM $5 AND deleted_at IS NULL`, status, errorText, importedCount, sourceID, userResourceOwnerValue(ownerID))
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrProxyResourceNotFound
	}
	runtimeIDs := uniquePositiveInt64s(append(updatedRuntimeIDs, staleIDs...))
	for _, proxyID := range runtimeIDs {
		if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
			return nil, fmt.Errorf("stop proxy source runtime before sync: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, proxyID := range runtimeIDs {
		if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
			slog.Error("proxy source runtime cleanup failed after sync", "owner_user_id", userResourceOwnerValue(ownerID), "source_id", sourceID, "proxy_id", proxyID, "error", err)
		}
	}
	return result, nil
}

func proxySourceNodesByKey(ctx context.Context, db userResourceDBTX, ownerID *int64, sourceID int64) (map[string]map[string]any, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, owner_user_id, is_public, kind, name, protocol, host, port,
       username, password, status, expires_at, fallback_mode, backup_proxy_id,
       expiry_warn_days, COALESCE(extra, '{}'::jsonb)::text AS extra
FROM proxies
WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL AND extra->>'source_id' = $2`, userResourceOwnerValue(ownerID), strconv.FormatInt(sourceID, 10))
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	result := make(map[string]map[string]any, len(items))
	for _, item := range items {
		extra, _ := item["extra"].(map[string]any)
		if key := urAsString(extra["source_node_key"]); key != "" {
			result[key] = item
		}
	}
	return result, nil
}

func proxySourceMutationItem(proxyID int64, ownerID *int64, payload map[string]any) map[string]any {
	item := clonePayload(payload)
	item["id"] = proxyID
	item["owner_user_id"] = userResourceOwnerValue(ownerID)
	item["has_auth"] = urAsString(payload["username"]) != "" || urAsString(payload["password"]) != ""
	return item
}

func proxySourceSyncSummary(result *ProxyImportResult) (string, int, string) {
	if result == nil {
		return "error", 0, "proxy source sync returned no result"
	}
	result.Errors = redactProxyImportErrors(result.Errors)
	status := "success"
	if len(result.Errors) > 0 {
		status = "partial"
	}
	importedCount := len(result.Created) + len(result.Updated)
	if importedCount == 0 && len(result.Errors) > 0 {
		status = "error"
	}
	errorText := strings.Join(result.Errors, "\n")
	if len(errorText) > 64*1024 {
		errorText = errorText[:64*1024]
	}
	return status, importedCount, errorText
}

func (s *ProxySourceService) recordProxySourceSyncError(ctx context.Context, ownerID *int64, sourceID int64, syncErr error) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_synced_at = NOW(), last_sync_status = 'error', last_sync_error = $1, updated_at = NOW()
WHERE id = $2 AND owner_user_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL`, safeSyncError(syncErr), sourceID, userResourceOwnerValue(ownerID))
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrProxyResourceNotFound
	}
	return nil
}

func (s *ProxySourceService) disableMissingProxySourceNodesWith(ctx context.Context, db userResourceDBTX, ownerID *int64, sourceID int64, activeKeys []string) ([]int64, []int64, error) {
	rows, err := db.QueryContext(ctx, `
UPDATE proxies
SET status = 'disabled', updated_at = NOW()
WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL AND status <> 'disabled'
  AND extra->>'source_id' = $2
  AND NOT (extra->>'source_node_key' = ANY($3))
RETURNING id`, userResourceOwnerValue(ownerID), strconv.FormatInt(sourceID, 10), pq.Array(append([]string{}, activeKeys...)))
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]int64, 0)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	accountIDs, err := clearProxyDependentProbeWith(ctx, db, ids)
	if err != nil {
		return nil, nil, err
	}
	return ids, accountIDs, nil
}

func clearProxyDependentProbeWith(ctx context.Context, db userResourceDBTX, proxyIDs []int64) ([]int64, error) {
	proxyIDs = uniquePositiveInt64s(proxyIDs)
	if len(proxyIDs) == 0 {
		return nil, nil
	}
	accountRows, err := db.QueryContext(ctx, `
UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb) - 'upstream_billing_probe', updated_at = NOW()
WHERE deleted_at IS NULL
  AND (
    proxy_id = ANY($1)
    OR proxy_id IN (
      SELECT id FROM proxies WHERE backup_proxy_id = ANY($1) AND deleted_at IS NULL
    )
  )
	RETURNING id`, pq.Array(proxyIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = accountRows.Close() }()
	accountIDs := make([]int64, 0)
	for accountRows.Next() {
		var id int64
		if err := accountRows.Scan(&id); err != nil {
			return nil, err
		}
		accountIDs = append(accountIDs, id)
	}
	return accountIDs, accountRows.Err()
}

func enqueueProxyDependentChangesWith(ctx context.Context, db userResourceDBTX, accountIDs []int64) error {
	accountIDs = uniquePositiveInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"account_ids": accountIDs})
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `
INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, created_at)
VALUES ($1, NULL, NULL, $2::jsonb, NOW())`, SchedulerOutboxEventAccountBulkChanged, payload)
	return err
}

func (s *ProxySourceService) validateProxySelectableForOwner(ctx context.Context, ownerID *int64, proxyID int64) error {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM proxies
  WHERE id = $1 AND deleted_at IS NULL
    AND status = 'active' AND (expires_at IS NULL OR expires_at > NOW())
    AND (
      ($2::bigint IS NULL AND owner_user_id IS NULL)
      OR ($2::bigint IS NOT NULL AND (owner_user_id = $2 OR is_public = true))
    )
)`, proxyID, userResourceOwnerValue(ownerID)).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return infraerrors.Forbidden("PROXY_NOT_SELECTABLE", "proxy is not selectable")
	}
	return nil
}

func (s *ProxySourceService) insertForOwnerWith(ctx context.Context, db userResourceDBTX, table string, ownerID *int64, specs map[string]columnSpec, payload map[string]any, required []string) (int64, error) {
	for _, key := range required {
		if _, ok := payload[key]; !ok || isBlank(payload[key]) {
			return 0, infraerrors.BadRequest("RESOURCE_REQUIRED_FIELD", key+" is required")
		}
	}
	cols := []string{"owner_user_id", "created_at", "updated_at"}
	args := []any{userResourceOwnerValue(ownerID), time.Now(), time.Now()}
	placeholders := []string{"$1", "$2", "$3"}
	keys := urSortedKeys(payload)
	for _, key := range keys {
		spec, ok := specs[key]
		if !ok || !spec.Create {
			continue
		}
		v, err := coerceColumnValue(spec.Kind, payload[key])
		if err != nil {
			return 0, err
		}
		cols = append(cols, key)
		args = append(args, v)
		placeholders = append(placeholders, placeholderFor(spec.Kind, len(args)))
	}
	var id int64
	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s) RETURNING id", table, strings.Join(cols, ", "), strings.Join(placeholders, ", "))
	if err := db.QueryRowContext(ctx, query, args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (s *ProxySourceService) updateForOwnerWith(ctx context.Context, db userResourceDBTX, table string, ownerID *int64, id int64, specs map[string]columnSpec, payload map[string]any) error {
	assignments := []string{}
	args := []any{}
	for _, key := range urSortedKeys(payload) {
		spec, ok := specs[key]
		if !ok || !spec.Update {
			continue
		}
		v, err := coerceColumnValue(spec.Kind, payload[key])
		if err != nil {
			return err
		}
		args = append(args, v)
		assignments = append(assignments, fmt.Sprintf("%s = %s", key, placeholderFor(spec.Kind, len(args))))
	}
	if len(assignments) == 0 {
		return nil
	}
	assignments = append(assignments, "updated_at = NOW()")
	args = append(args, id, userResourceOwnerValue(ownerID))
	query := fmt.Sprintf("UPDATE %s SET %s WHERE id = $%d AND owner_user_id IS NOT DISTINCT FROM $%d AND deleted_at IS NULL", table, strings.Join(assignments, ", "), len(args)-1, len(args))
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return translateUserResourceProxyConstraintError(err)
	}
	if affected(res) == 0 {
		return ErrProxyResourceNotFound
	}
	return nil
}

// translateUserResourceProxyConstraintError keeps the database trigger that
// protects in-use public proxies authoritative while exposing a stable API
// error to callers.
func translateUserResourceProxyConstraintError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) && pgErr != nil && pgErr.Code == "23514" &&
		strings.Contains(strings.ToLower(pgErr.Message), "public proxy is still used by another user resource") {
		return infraerrors.Conflict("PROXY_PUBLIC_IN_USE", "public proxy is currently in use")
	}
	return err
}

func placeholderFor(kind string, idx int) string {
	if kind == colJSON {
		return fmt.Sprintf("$%d::jsonb", idx)
	}
	return fmt.Sprintf("$%d", idx)
}

func coerceColumnValue(kind string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch kind {
	case colString:
		v, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		return strings.TrimSpace(v), nil
	case colInt:
		v, err := strictInt64Value(value)
		if err != nil {
			return nil, err
		}
		if int64(int(v)) != v {
			return nil, fmt.Errorf("is out of range")
		}
		return int(v), nil
	case colInt64:
		if isBlank(value) {
			return nil, nil
		}
		v, err := strictInt64Value(value)
		if err != nil {
			return nil, err
		}
		return v, nil
	case colFloat:
		if isBlank(value) {
			return nil, nil
		}
		return strictFloatValue(value)
	case colBool:
		return strictBoolValue(value)
	case colTime:
		return coerceTime(value)
	case colJSON:
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	default:
		return value, nil
	}
}

func coerceTime(value any) (*time.Time, error) {
	if value == nil || isBlank(value) {
		return nil, nil
	}
	if t, ok := value.(time.Time); ok {
		return &t, nil
	}
	s := urAsString(value)
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}
	return nil, infraerrors.BadRequest("INVALID_TIME", "invalid time value")
}

func scanRowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := make(map[string]any, len(cols))
		for i, col := range cols {
			item[col] = normalizeScannedValue(values[i])
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func normalizeScannedValue(v any) any {
	switch t := v.(type) {
	case nil:
		return nil
	case []byte:
		return decodeStringValue(string(t))
	case string:
		return decodeStringValue(t)
	default:
		return t
	}
}

func decodeStringValue(s string) any {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return decoded
		}
	}
	return s
}

func clonePayload(payload map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range payload {
		out[k] = v
	}
	return out
}

func defaultPayload(payload map[string]any, defaults map[string]any) {
	for k, v := range defaults {
		if _, ok := payload[k]; !ok {
			payload[k] = v
		}
	}
}

func urSortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func isBlank(v any) bool {
	if v == nil {
		return true
	}
	if s, ok := v.(string); ok {
		return strings.TrimSpace(s) == ""
	}
	return false
}

func urAsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(t)
	case fmt.Stringer:
		return t.String()
	case float64:
		if math.Trunc(t) == t {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		return strconv.FormatBool(t)
	default:
		return fmt.Sprintf("%v", t)
	}
}

func toInt(v any) int {
	return int(urToInt64(v))
}

func urToInt64(v any) int64 {
	switch t := v.(type) {
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case float64:
		return int64(t)
	case float32:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return i
	default:
		return 0
	}
}

func toBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(t))
		return b
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}

func uniquePositiveInt64s(ids []int64) []int64 {
	seen := map[int64]struct{}{}
	out := []int64{}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func affected(res sql.Result) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func redactProxyImportErrorText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "proxy import failed"
	}
	value = resourceAuthHeaderPattern.ReplaceAllString(value, `$1***`)
	value = logredact.RedactText(value,
		"api_key", "apikey", "authorization", "host", "key", "node", "password",
		"proxy_authorization", "proxy-authorization", "raw", "secret", "subscription_url",
		"token", "uri", "url", "username",
	)
	value = proxyImportURLPattern.ReplaceAllString(value, "<proxy-uri-redacted>")
	value = proxyImportCredentialPattern.ReplaceAllString(value, "<proxy-credentials-redacted>")
	value = proxyImportEndpointPattern.ReplaceAllString(value, "<proxy-endpoint-redacted>")
	value = proxyImportOpaqueSecretPattern.ReplaceAllString(value, "***")
	value = strings.TrimSpace(value)
	if value == "" || value == "***" {
		return "proxy import failed"
	}
	const maxProxyImportErrorRunes = 512
	runes := []rune(value)
	if len(runes) > maxProxyImportErrorRunes {
		value = string(runes[:maxProxyImportErrorRunes])
	}
	return value
}

func formatProxyImportError(index int, detail string) string {
	return fmt.Sprintf("entry %d: %s", index+1, redactProxyImportErrorText(detail))
}

func redactProxyImportErrors(items []string) []string {
	if len(items) == 0 {
		return items
	}
	redacted := make([]string, len(items))
	for index, item := range items {
		redacted[index] = redactProxyImportErrorText(item)
	}
	return redacted
}

func RedactProxyImportResultForUserResponse(result *ProxyImportResult) {
	if result == nil {
		return
	}
	for _, item := range result.Created {
		RedactProxyForUserResponse(item)
	}
	for _, item := range result.Updated {
		RedactProxyForUserResponse(item)
	}
	result.Errors = redactProxyImportErrors(result.Errors)
}

func RedactProxySourceSyncResultForUserResponse(result *ProxySourceSyncResult) {
	if result == nil {
		return
	}
	for _, item := range result.Created {
		RedactProxyForUserResponse(item)
	}
	for _, item := range result.Updated {
		RedactProxyForUserResponse(item)
	}
	result.Errors = redactProxyImportErrors(result.Errors)
}

func RedactProxyForUserResponse(item map[string]any) {
	if item == nil {
		return
	}
	// redactPublicProxy already reduced foreign public proxies to an
	// allowlisted metadata shape. Keep that contract stable instead of adding
	// blank credential fields back during handler-level redaction.
	if hidden, ok := item["details_hidden"].(bool); ok && hidden {
		return
	}
	item["username"] = ""
	item["password"] = ""
	item["extra"] = redactProxyExtra(item["extra"])
}

func redactProxyExtra(v any) any {
	out, redacted := redactProxyExtraValue(v)
	if redacted {
		if object, ok := out.(map[string]any); ok {
			object["redacted"] = true
		}
	}
	return out
}

func redactProxyExtraValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		redacted := false
		for key, item := range typed {
			if isSensitiveProxyExtraKey(key) {
				out[key] = ""
				redacted = true
				continue
			}
			clean, nestedRedacted := redactProxyExtraValue(item)
			out[key] = clean
			redacted = redacted || nestedRedacted
		}
		return out, redacted
	case []any:
		out := make([]any, len(typed))
		redacted := false
		for index, item := range typed {
			clean, nestedRedacted := redactProxyExtraValue(item)
			out[index] = clean
			redacted = redacted || nestedRedacted
		}
		return out, redacted
	case string:
		lower := strings.ToLower(strings.TrimSpace(typed))
		for _, prefix := range []string{
			"vmess://", "vless://", "trojan://", "ss://",
			"hysteria://", "hysteria2://", "hy2://", "tuic://",
			"anytls://", "naive://", "wireguard://",
		} {
			if strings.HasPrefix(lower, prefix) {
				return "", true
			}
		}
		for _, prefix := range []string{"socks://", "socks5://", "http://", "https://"} {
			if strings.HasPrefix(lower, prefix) && strings.Contains(lower, "@") {
				return "", true
			}
		}
		return typed, false
	default:
		return value, false
	}
}

func isSensitiveProxyExtraKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "raw", "uri", "node", "node_uri", "share_link", "outbound", "xray_outbound",
		"password", "pass", "passwd", "username", "user", "authorization", "proxy_authorization",
		"private_key", "server_key", "public_key", "short_id", "uuid", "token", "secret",
		"credential", "credentials", "auth":
		return true
	default:
		return false
	}
}

func mapSliceFromAny(v any) []map[string]any {
	switch items := v.(type) {
	case []map[string]any:
		return items
	case []any:
		out := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func (s *ProxySourceService) getSelectableProxyRawForOwner(ctx context.Context, ownerID *int64, proxyID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT
  p.id, p.owner_user_id, p.is_public, p.kind, p.name, p.protocol, p.host, p.port,
  p.username, p.password, (COALESCE(p.username, '') <> '' OR COALESCE(p.password, '') <> '') AS has_auth,
  p.status, p.expires_at, p.fallback_mode, p.backup_proxy_id, p.expiry_warn_days,
  COALESCE(p.extra, '{}'::jsonb)::text AS extra, p.created_at, p.updated_at
FROM proxies p
WHERE p.id = $1
  AND p.deleted_at IS NULL
  AND (
    ($2::bigint IS NULL AND p.owner_user_id IS NULL)
    OR ($2::bigint IS NOT NULL AND (p.owner_user_id = $2 OR p.is_public = true))
  )
LIMIT 1`, proxyID, userResourceOwnerValue(ownerID))
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrProxyResourceNotFound
	}
	return items[0], nil
}

func probeProxyEndpoint(ctx context.Context, proxyURL string) ProxyQualityCheckItem {
	item := ProxyQualityCheckItem{Target: "base_connectivity"}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" || portFromURL(parsed) <= 0 {
		item.Status = "fail"
		item.Message = "proxy endpoint is invalid or unavailable"
		return item
	}
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(parsed.Hostname(), strconv.Itoa(portFromURL(parsed))))
	item.LatencyMs = time.Since(start).Milliseconds()
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		item.Status = "fail"
		item.Message = "proxy endpoint is not reachable"
		return item
	}
	item.Status = "pass"
	item.Message = "proxy endpoint reachable"
	return item
}

const proxySubscriptionMaxBytes int64 = 2 * 1024 * 1024

func validateExternalHTTPURL(ctx context.Context, raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Hostname() == "" {
		return infraerrors.BadRequest("PROXY_SOURCE_URL_INVALID", "subscription_url is invalid")
	}
	if u.User != nil {
		return infraerrors.BadRequest("PROXY_SOURCE_URL_INVALID", "subscription_url must not contain credentials")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return infraerrors.BadRequest("PROXY_SOURCE_URL_SCHEME", "subscription_url must use http or https")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || host == "metadata" || host == "metadata.google.internal" {
		return infraerrors.Forbidden("PROXY_SOURCE_URL_FORBIDDEN", "subscription_url host is not allowed")
	}
	_, err = resolveExternalHostIPs(ctx, host)
	return err
}

func resolveExternalHostIPs(ctx context.Context, host string) ([]net.IP, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || host == "localhost" || host == "metadata" || host == "metadata.google.internal" {
		return nil, infraerrors.Forbidden("PROXY_SOURCE_URL_FORBIDDEN", "subscription_url host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedOutboundIP(ip) {
			return nil, infraerrors.Forbidden("PROXY_SOURCE_URL_FORBIDDEN", "subscription_url host is not allowed")
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, infraerrors.BadRequest("PROXY_SOURCE_URL_RESOLVE_FAILED", "subscription_url host cannot be resolved")
	}
	if len(addrs) == 0 {
		return nil, infraerrors.BadRequest("PROXY_SOURCE_URL_RESOLVE_FAILED", "subscription_url host cannot be resolved")
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if isBlockedOutboundIP(addr.IP) {
			return nil, infraerrors.Forbidden("PROXY_SOURCE_URL_FORBIDDEN", "subscription_url host is not allowed")
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

func isBlockedOutboundIP(ip net.IP) bool {
	return ip == nil || isPrivateIP(ip) || ip.IsMulticast()
}

// fetchProxySubscription returns the subscription body plus the raw
// `subscription-userinfo` response header, which most airport panels use to
// report the plan's traffic quota and expiry date.
func fetchProxySubscription(ctx context.Context, subscriptionURL string) (string, string, error) {
	if err := validateExternalHTTPURL(ctx, subscriptionURL); err != nil {
		return "", "", err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return "", "", errors.New("default HTTP transport is unavailable")
	}
	transport := defaultTransport.Clone()
	defer transport.CloseIdleConnections()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, infraerrors.BadRequest("PROXY_SOURCE_ADDRESS_INVALID", "subscription target address is invalid")
		}
		ips, err := resolveExternalHostIPs(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		if lastErr != nil {
			return nil, infraerrors.BadRequest("PROXY_SOURCE_CONNECT_FAILED", "subscription host is not reachable")
		}
		return nil, infraerrors.BadRequest("PROXY_SOURCE_CONNECT_FAILED", "subscription host is not reachable")
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return infraerrors.BadRequest("PROXY_SOURCE_REDIRECT_LIMIT", "too many redirects")
			}
			return validateExternalHTTPURL(req.Context(), req.URL.String())
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, subscriptionURL, nil)
	if err != nil {
		return "", "", infraerrors.BadRequest("PROXY_SOURCE_URL_INVALID", "subscription_url is invalid")
	}
	req.Header.Set("Accept", "text/plain, application/octet-stream, application/yaml, application/json;q=0.5, */*;q=0.1")
	req.Header.Set("User-Agent", "sub2api-user-proxy-source/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", infraerrors.BadRequest("PROXY_SOURCE_FETCH_FAILED", "subscription fetch failed")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", "", infraerrors.BadRequest("PROXY_SOURCE_FETCH_STATUS", fmt.Sprintf("subscription upstream returned HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, proxySubscriptionMaxBytes+1))
	if err != nil {
		return "", "", infraerrors.BadRequest("PROXY_SOURCE_READ_FAILED", "subscription read failed")
	}
	if int64(len(body)) > proxySubscriptionMaxBytes {
		return "", "", infraerrors.BadRequest("PROXY_SOURCE_TOO_LARGE", "subscription response is too large")
	}
	userInfo := resp.Header.Get("Subscription-Userinfo")
	if len(userInfo) > 512 {
		userInfo = userInfo[:512]
	}
	return string(body), userInfo, nil
}

type proxySubscriptionUserInfo struct {
	TrafficUsed  int64
	TrafficTotal int64
	ExpiresAt    *time.Time
	HasUsed      bool
	HasTotal     bool
	HasExpiry    bool
}

func (info proxySubscriptionUserInfo) hasAny() bool {
	return info.HasUsed || info.HasTotal || info.HasExpiry
}

// parseProxySubscriptionUserInfo reads the widely used
// `upload=..; download=..; total=..; expire=..` form. Unknown keys, malformed
// numbers and negative values are ignored rather than failing the sync: the
// snapshot is informational and must never block a node import.
func parseProxySubscriptionUserInfo(header string) proxySubscriptionUserInfo {
	info := proxySubscriptionUserInfo{}
	if strings.TrimSpace(header) == "" {
		return info
	}
	var upload, download int64
	sawUpload, sawDownload := false, false
	for _, part := range strings.Split(header, ";") {
		rawKey, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(rawKey))
		number, ok := parseSubscriptionUserInfoNumber(strings.TrimSpace(rawValue))
		if !ok {
			continue
		}
		switch key {
		case "upload":
			upload, sawUpload = number, true
		case "download":
			download, sawDownload = number, true
		case "total":
			info.TrafficTotal, info.HasTotal = number, true
		case "expire":
			// `expire=0` is how panels report "never expires"; record that as a
			// known-empty expiry instead of silently keeping a stale date.
			if number > 0 {
				expires := time.Unix(number, 0).UTC()
				info.ExpiresAt = &expires
			}
			info.HasExpiry = true
		}
	}
	if sawUpload || sawDownload {
		info.TrafficUsed = upload + download
		info.HasUsed = true
	}
	return info
}

func parseSubscriptionUserInfoNumber(value string) (int64, bool) {
	if value == "" {
		return 0, false
	}
	if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
		if parsed < 0 {
			return 0, false
		}
		return parsed, true
	}
	// Some panels report decimals (e.g. `total=1073741824.0`).
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 || parsed > float64(math.MaxInt64/2) {
		return 0, false
	}
	return int64(parsed), true
}

// recordProxySourceSubscriptionInfo stores the airport quota/expiry snapshot in
// its own statement, outside the node-import transaction: the numbers stay valid
// even when parsing the node list fails.
func (s *ProxySourceService) recordProxySourceSubscriptionInfo(ctx context.Context, ownerID *int64, sourceID int64, info proxySubscriptionUserInfo) error {
	if !info.hasAny() {
		return nil
	}
	if err := s.ensureDB(); err != nil {
		return err
	}
	assignments := []string{}
	args := []any{}
	if info.HasUsed {
		args = append(args, info.TrafficUsed)
		assignments = append(assignments, fmt.Sprintf("sub_traffic_used = $%d", len(args)))
	}
	if info.HasTotal {
		args = append(args, info.TrafficTotal)
		assignments = append(assignments, fmt.Sprintf("sub_traffic_total = $%d", len(args)))
	}
	if info.HasExpiry {
		args = append(args, info.ExpiresAt)
		assignments = append(assignments, fmt.Sprintf("sub_expires_at = $%d", len(args)))
	}
	assignments = append(assignments, "sub_info_updated_at = NOW()", "updated_at = NOW()")
	args = append(args, sourceID, userResourceOwnerValue(ownerID))
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE proxy_sources SET %s
WHERE id = $%d AND owner_user_id IS NOT DISTINCT FROM $%d AND deleted_at IS NULL`,
		strings.Join(assignments, ", "), len(args)-1, len(args)), args...)
	return err
}

func safeSyncError(err error) string {
	if err == nil {
		return ""
	}
	msg := redactProxyImportErrorText(err.Error())
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	return msg
}

func proxyFromResourceMap(item map[string]any) *Proxy {
	extra, _ := item["extra"].(map[string]any)
	var ownerUserID *int64
	if id := urToInt64(item["owner_user_id"]); id > 0 {
		ownerUserID = &id
	}
	return &Proxy{
		ID:          urToInt64(item["id"]),
		Name:        urAsString(item["name"]),
		OwnerUserID: ownerUserID,
		IsPublic:    toBool(item["is_public"]),
		Kind:        urAsString(item["kind"]),
		Protocol:    urAsString(item["protocol"]),
		Host:        urAsString(item["host"]),
		Port:        toInt(item["port"]),
		Username:    urAsString(item["username"]),
		Password:    urAsString(item["password"]),
		Extra:       extra,
	}
}

type parsedProxyNode struct {
	Name     string
	Kind     string
	Protocol string
	Host     string
	Port     int
	Username string
	Password string
	Network  string
	Raw      string
	Err      string
}

func stripProxySourceMetadata(payload map[string]any) {
	extra, ok := payload["extra"].(map[string]any)
	if !ok {
		return
	}
	delete(extra, "source_id")
	delete(extra, "source_node_key")
}

func proxySourceNodeBaseKey(node parsedProxyNode) string {
	if name := strings.ToLower(strings.TrimSpace(node.Name)); name != "" {
		return "name:" + name
	}
	return strings.Join([]string{
		"endpoint",
		strings.ToLower(strings.TrimSpace(node.Kind)),
		strings.ToLower(strings.TrimSpace(node.Protocol)),
		strings.ToLower(strings.TrimSuffix(strings.TrimSpace(node.Host), ".")),
		strconv.Itoa(node.Port),
		strings.ToLower(strings.TrimSpace(node.Network)),
	}, ":")
}

func proxySourceNodeKey(base string, occurrence int) string {
	if occurrence < 1 {
		occurrence = 1
	}
	sum := sha256.Sum256([]byte(base + "\x00" + strconv.Itoa(occurrence)))
	return hex.EncodeToString(sum[:])
}

func proxySourceNodeName(sourceID int64, sourceName, nodeName string, index int) string {
	sourceName = strings.TrimSpace(sourceName)
	nodeName = strings.TrimSpace(nodeName)
	if sourceName == "" {
		sourceName = "Subscription"
	}
	if nodeName == "" {
		nodeName = "Node"
	}
	suffix := fmt.Sprintf(" [%d:%d]", sourceID, index)
	prefix := sourceName + " / " + nodeName
	maxPrefixRunes := 100 - len([]rune(suffix))
	if maxPrefixRunes < 1 {
		maxPrefixRunes = 1
	}
	prefixRunes := []rune(prefix)
	if len(prefixRunes) > maxPrefixRunes {
		prefix = string(prefixRunes[:maxPrefixRunes])
	}
	return prefix + suffix
}

func parseProxyNodeLines(raw string) []parsedProxyNode {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if decoded, ok := decodeProxySubscriptionPayload(raw); ok {
		raw = decoded
	}
	if nodes, ok := parseSingBoxProxyNodes(raw); ok {
		return nodes
	}
	if nodes, ok := parseClashProxyNodes(raw); ok {
		return nodes
	}
	lines := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' })
	out := make([]parsedProxyNode, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, parseProxyNode(line))
	}
	return out
}

func parseSingBoxProxyNodes(raw string) ([]parsedProxyNode, bool) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}

	var document any
	if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
		return nil, false
	}

	items := singBoxOutboundItems(document)
	if len(items) == 0 {
		return nil, false
	}
	out := make([]parsedProxyNode, 0, len(items))
	for _, outbound := range items {
		protocol := strings.ToLower(strings.TrimSpace(urAsString(outbound["type"])))
		switch protocol {
		case "direct", "block", "dns", "selector", "urltest":
			continue
		}
		native, err := normalizeNativeSingBoxNode(outbound)
		if err != nil {
			out = append(out, parsedProxyNode{Err: err.Error()})
			continue
		}
		node := parseSingBoxProxyNode(native)
		if node.Kind == "xray" && node.Err == "" {
			encoded, _ := json.Marshal(native)
			node.Raw = string(encoded)
		}
		out = append(out, node)
	}
	if len(out) == 0 {
		return []parsedProxyNode{{Err: "sing-box config contains no supported proxy outbounds"}}, true
	}
	return out, true
}

func singBoxOutboundItems(document any) []map[string]any {
	if node, ok := document.(map[string]any); ok && node["type"] != nil {
		return []map[string]any{node}
	}
	toMaps := func(values []any) []map[string]any {
		out := make([]map[string]any, 0, len(values))
		for _, value := range values {
			if item, ok := value.(map[string]any); ok {
				out = append(out, item)
			}
		}
		return out
	}

	switch value := document.(type) {
	case []any:
		return toMaps(value)
	case map[string]any:
		combined := make([]map[string]any, 0)
		for _, key := range []string{"outbounds", "endpoints"} {
			if items, ok := value[key].([]any); ok && len(items) > 0 {
				combined = append(combined, toMaps(items)...)
			}
		}
		if len(combined) > 0 {
			return combined
		}
		if strings.TrimSpace(urAsString(value["type"])) != "" {
			return []map[string]any{value}
		}
	}
	return nil
}

func parseSingBoxProxyNode(outbound map[string]any) parsedProxyNode {
	if node := parseSingBoxExtendedProxyNode(outbound); node.Kind != "" || node.Err != "" {
		return node
	}
	clash := map[string]any{
		"name":        urAsString(outbound["tag"]),
		"type":        urAsString(outbound["type"]),
		"server":      urAsString(outbound["server"]),
		"port":        toInt(outbound["server_port"]),
		"username":    urAsString(outbound["username"]),
		"password":    urAsString(outbound["password"]),
		"uuid":        urAsString(outbound["uuid"]),
		"cipher":      urAsString(outbound["method"]),
		"plugin":      urAsString(outbound["plugin"]),
		"plugin-opts": outbound["plugin_opts"],
		"alterId":     toInt(outbound["alter_id"]),
		"flow":        urAsString(outbound["flow"]),
	}
	protocol := strings.ToLower(strings.TrimSpace(urAsString(clash["type"])))
	switch protocol {
	case "socks":
		clash["type"] = "socks5"
	case "shadowsocks":
		clash["type"] = "ss"
	}

	if transport, ok := outbound["transport"].(map[string]any); ok {
		clash["network"] = urAsString(transport["type"])
		clash["path"] = urAsString(transport["path"])
		clash["ws-path"] = urAsString(transport["path"])
		clash["service-name"] = urAsString(transport["service_name"])
		clash["grpc-opts"] = map[string]any{"grpc-service-name": urAsString(transport["service_name"])}
		if headers, ok := transport["headers"].(map[string]any); ok {
			clash["host"] = urAsString(headers["Host"])
			if clash["host"] == "" {
				clash["host"] = urAsString(headers["host"])
			}
		}
	}
	if tls, ok := outbound["tls"].(map[string]any); ok && toBool(tls["enabled"]) {
		clash["tls"] = true
		clash["servername"] = urAsString(tls["server_name"])
		clash["sni"] = urAsString(tls["server_name"])
		clash["skip-cert-verify"] = toBool(tls["insecure"])
		if reality, ok := tls["reality"].(map[string]any); ok && toBool(reality["enabled"]) {
			clash["security"] = "reality"
			clash["reality-opts"] = map[string]any{
				"public-key": urAsString(reality["public_key"]),
				"short-id":   urAsString(reality["short_id"]),
			}
		}
		if protocol == "http" {
			clash["type"] = "https"
		}
	}
	return parseClashProxyNode(clash)
}

// parseSingBoxExtendedProxyNode keeps the native sing-box fields for protocols
// that do not have a lossless Clash representation. The resulting canonical URI
// is stored in the proxy record and rebuilt by the controlled runtime later.
func parseSingBoxExtendedProxyNode(outbound map[string]any) parsedProxyNode {
	protocol := canonicalSingBoxProtocol(urAsString(outbound["type"]))
	if protocol != "hysteria" && protocol != "hysteria2" && protocol != "tuic" && protocol != "anytls" && protocol != "naive" && protocol != "wireguard" {
		return parsedProxyNode{}
	}
	name := urAsString(outbound["tag"])
	server := urAsString(outbound["server"])
	port := toInt(outbound["server_port"])
	if port <= 0 && (protocol == "hysteria" || protocol == "hysteria2") {
		port = firstPortFromAny(outbound["server_ports"])
	}
	if protocol == "wireguard" {
		peers, _ := outbound["peers"].([]any)
		if len(peers) == 0 {
			return parsedProxyNode{Name: name, Err: "wireguard node has no peer"}
		}
		peer, _ := peers[0].(map[string]any)
		server = urAsString(peer["address"])
		port = toInt(peer["port"])
		if server == "" || port <= 0 {
			return parsedProxyNode{Name: name, Err: "wireguard node missing peer endpoint"}
		}
	}
	if server == "" || port <= 0 {
		return parsedProxyNode{Name: name, Err: "sing-box node missing server or port"}
	}

	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(server, strconv.Itoa(port))}
	q := url.Values{}
	tls, _ := outbound["tls"].(map[string]any)
	addCanonicalQuery(q, "sni", urAsString(tls["server_name"]))
	if toBool(tls["insecure"]) {
		q.Set("insecure", "1")
	}
	if alpn := stringSliceFromAny(tls["alpn"]); len(alpn) > 0 {
		q.Set("alpn", strings.Join(alpn, ","))
	}
	if utls, ok := tls["utls"].(map[string]any); ok && toBool(utls["enabled"]) {
		addCanonicalQuery(q, "fp", urAsString(utls["fingerprint"]))
	}
	switch protocol {
	case "hysteria":
		u.User = url.User(urAsString(outbound["auth_str"]))
		addCanonicalQuery(q, "upmbps", urAsString(outbound["up_mbps"]))
		addCanonicalQuery(q, "downmbps", urAsString(outbound["down_mbps"]))
		addCanonicalQuery(q, "obfs", urAsString(outbound["obfs"]))
		addCanonicalQuery(q, "recv_window_conn", urAsString(outbound["recv_window_conn"]))
		addCanonicalQuery(q, "recv_window", urAsString(outbound["recv_window"]))
		addSingBoxPortHoppingQuery(q, outbound)
	case "hysteria2":
		u.User = url.User(urAsString(outbound["password"]))
		if obfs, ok := outbound["obfs"].(map[string]any); ok {
			addCanonicalQuery(q, "obfs", urAsString(obfs["type"]))
			addCanonicalQuery(q, "obfs-password", urAsString(obfs["password"]))
		}
		addCanonicalQuery(q, "upmbps", urAsString(outbound["up_mbps"]))
		addCanonicalQuery(q, "downmbps", urAsString(outbound["down_mbps"]))
		addSingBoxPortHoppingQuery(q, outbound)
	case "tuic":
		u.User = url.UserPassword(urAsString(outbound["uuid"]), urAsString(outbound["password"]))
		addCanonicalQuery(q, "congestion_control", urAsString(outbound["congestion_control"]))
		addCanonicalQuery(q, "udp_relay_mode", urAsString(outbound["udp_relay_mode"]))
	case "anytls":
		u.User = url.User(urAsString(outbound["password"]))
	case "naive":
		username := urAsString(outbound["username"])
		password := urAsString(outbound["password"])
		if username != "" {
			u.User = url.UserPassword(username, password)
		} else {
			u.User = url.User(password)
		}
		if toBool(outbound["quic"]) {
			u.Scheme = "naive+quic"
		} else {
			u.Scheme = "naive+https"
		}
	case "wireguard":
		u.User = url.User(urAsString(outbound["private_key"]))
		addCanonicalQuery(q, "publickey", urAsString(firstMapFromAny(outbound["peers"], "public_key")))
		addCanonicalQuery(q, "presharedkey", urAsString(firstMapFromAny(outbound["peers"], "pre_shared_key")))
		addCanonicalQuery(q, "reserved", strings.Join(stringSliceFromAny(firstMapFromAny(outbound["peers"], "reserved")), ","))
		addCanonicalQuery(q, "address", strings.Join(stringSliceFromAny(outbound["address"]), ","))
		addCanonicalQuery(q, "allowedips", strings.Join(stringSliceFromAny(firstMapFromAny(outbound["peers"], "allowed_ips")), ","))
		addCanonicalQuery(q, "mtu", urAsString(outbound["mtu"]))
	}
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return parsedProxyNode{Name: name, Kind: "xray", Protocol: protocol, Host: server, Port: port, Network: "", Raw: u.String()}
}

func addSingBoxPortHoppingQuery(q url.Values, outbound map[string]any) {
	addCanonicalQuery(q, "mport", strings.Join(stringSliceFromAny(outbound["server_ports"]), ","))
	addCanonicalQuery(q, "hop_interval", urAsString(outbound["hop_interval"]))
}

func addCanonicalQuery(q url.Values, key, value string) {
	if strings.TrimSpace(value) != "" {
		q.Set(key, value)
	}
}

func firstMapFromAny(value any, key string) any {
	if values, ok := value.([]any); ok && len(values) > 0 {
		if item, ok := values[0].(map[string]any); ok {
			return item[key]
		}
	}
	if values, ok := value.([]map[string]any); ok && len(values) > 0 {
		return values[0][key]
	}
	return nil
}

func stringSliceFromAny(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, item := range values {
			if value := strings.TrimSpace(urAsString(item)); value != "" {
				out = append(out, value)
			}
		}
		return out
	case string:
		return splitCSV(values)
	default:
		return nil
	}
}

func firstPortFromAny(value any) int {
	values := stringSliceFromAny(value)
	if len(values) == 0 {
		return 0
	}
	first := strings.TrimSpace(values[0])
	if index := strings.IndexAny(first, ":-"); index >= 0 {
		first = first[:index]
	}
	port, _ := strconv.Atoi(first)
	return port
}

func decodeProxySubscriptionPayload(raw string) (string, bool) {
	decoded, ok := decodeShareBase64(raw)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(decoded)
	if strings.Contains(trimmed, "://") || strings.Contains(strings.ToLower(trimmed), "proxies:") {
		return trimmed, true
	}
	// A sing-box subscription can be a JSON document containing only
	// outbounds/endpoints, so URI/YAML string heuristics are insufficient.
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var document any
		if err := json.Unmarshal([]byte(trimmed), &document); err == nil {
			if len(singBoxOutboundItems(document)) > 0 {
				return trimmed, true
			}
		}
	}
	return "", false
}

type clashProxyDocument struct {
	Proxies []map[string]any `yaml:"proxies"`
}

func parseClashProxyNodes(raw string) ([]parsedProxyNode, bool) {
	if !strings.Contains(raw, "proxies:") && !strings.Contains(raw, `"proxies"`) {
		return nil, false
	}
	var doc clashProxyDocument
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil || len(doc.Proxies) == 0 {
		return nil, false
	}
	out := make([]parsedProxyNode, 0, len(doc.Proxies))
	for _, proxy := range doc.Proxies {
		out = append(out, parseClashProxyNode(proxy))
	}
	return out, true
}

func parseClashProxyNode(proxy map[string]any) parsedProxyNode {
	name := clashString(proxy, "name")
	protocol := strings.ToLower(clashString(proxy, "type"))
	host := clashString(proxy, "server", "address", "host")
	port := toInt(proxy["port"])
	if protocol == "" {
		return parsedProxyNode{Name: name, Err: "unsupported clash proxy type"}
	}
	switch protocol {
	case "socks":
		protocol = "socks5h"
	case "shadowsocks":
		protocol = "ss"
	}
	if port <= 0 && (protocol == "hysteria" || protocol == "hysteria2" || protocol == "hy2") {
		port = firstPortFromAny(proxy["ports"])
	}
	if host == "" || port <= 0 {
		return parsedProxyNode{Name: name, Err: "missing host or port"}
	}

	switch protocol {
	case "http", "https", "socks5", "socks5h":
		if protocol == "http" && toBool(proxy["tls"]) {
			protocol = "https"
		}
		username := clashString(proxy, "username", "user")
		password := clashString(proxy, "password", "pass")
		raw := buildClashStandardURI(protocol, host, port, username, password, name)
		return parsedProxyNode{Name: name, Kind: "standard", Protocol: protocol, Host: host, Port: port, Username: username, Password: password, Raw: raw}
	case "ss":
		cipher := clashString(proxy, "cipher", "method")
		password := clashString(proxy, "password")
		if cipher == "" || password == "" {
			return parsedProxyNode{Name: name, Err: "shadowsocks node missing method or password"}
		}
		raw := buildClashShadowsocksURI(proxy, cipher, password, host, port, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "ss", Host: host, Port: port, Username: cipher, Password: password, Raw: raw}
	case "vmess":
		uuid := clashString(proxy, "uuid", "id")
		if uuid == "" {
			return parsedProxyNode{Name: name, Err: "vmess node missing uuid"}
		}
		raw := buildClashVMessURI(proxy, host, port, uuid, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "vmess", Host: host, Port: port, Username: uuid, Network: clashString(proxy, "network"), Raw: raw}
	case "vless":
		uuid := clashString(proxy, "uuid", "id")
		if uuid == "" {
			return parsedProxyNode{Name: name, Err: "vless node missing uuid"}
		}
		raw := buildClashVLESSURI(proxy, host, port, uuid, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "vless", Host: host, Port: port, Username: uuid, Network: clashString(proxy, "network"), Raw: raw}
	case "trojan":
		password := clashString(proxy, "password")
		if password == "" {
			return parsedProxyNode{Name: name, Err: "trojan node missing password"}
		}
		raw := buildClashTrojanURI(proxy, host, port, password, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "trojan", Host: host, Port: port, Password: password, Network: clashString(proxy, "network"), Raw: raw}
	case "hysteria":
		auth := clashString(proxy, "auth-str", "auth_str", "auth", "password")
		upMbps := parseProxyInt(clashString(proxy, "up", "up-mbps", "up_mbps"))
		downMbps := parseProxyInt(clashString(proxy, "down", "down-mbps", "down_mbps"))
		if auth == "" || upMbps <= 0 || downMbps <= 0 {
			return parsedProxyNode{Name: name, Err: "hysteria node missing authentication or bandwidth"}
		}
		proxy = cloneProxyMap(proxy)
		proxy["up"] = strconv.Itoa(upMbps)
		proxy["down"] = strconv.Itoa(downMbps)
		raw := buildClashHysteriaURI(proxy, host, port, auth, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "hysteria", Host: host, Port: port, Password: auth, Raw: raw}
	case "hysteria2", "hy2":
		password := clashString(proxy, "password", "auth")
		if password == "" {
			return parsedProxyNode{Name: name, Err: "hysteria2 node missing password"}
		}
		raw := buildClashHysteria2URI(proxy, host, port, password, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "hysteria2", Host: host, Port: port, Password: password, Raw: raw}
	case "tuic":
		uuid := clashString(proxy, "uuid", "id")
		password := clashString(proxy, "password")
		if uuid == "" || password == "" {
			return parsedProxyNode{Name: name, Err: "tuic node missing uuid or password"}
		}
		raw := buildClashTUICURI(proxy, host, port, uuid, password, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "tuic", Host: host, Port: port, Password: password, Raw: raw}
	case "anytls":
		password := clashString(proxy, "password")
		if password == "" {
			return parsedProxyNode{Name: name, Err: "anytls node missing password"}
		}
		raw := buildClashAnyTLSURI(proxy, host, port, password, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "anytls", Host: host, Port: port, Password: password, Raw: raw}
	case "wireguard":
		privateKey := clashString(proxy, "private-key", "private_key")
		publicKey := clashString(proxy, "public-key", "public_key")
		address := clashString(proxy, "ip", "address")
		if privateKey == "" || publicKey == "" || address == "" {
			return parsedProxyNode{Name: name, Err: "wireguard node missing keys or interface address"}
		}
		raw := buildClashWireGuardURI(proxy, host, port, privateKey, publicKey, address, name)
		return parsedProxyNode{Name: name, Kind: "xray", Protocol: "wireguard", Host: host, Port: port, Password: privateKey, Raw: raw}
	default:
		return parsedProxyNode{Name: name, Err: "unsupported clash proxy type"}
	}
}

func buildClashHysteriaURI(proxy map[string]any, host string, port int, auth, name string) string {
	u := &url.URL{Scheme: "hysteria", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(auth)}
	q := url.Values{}
	addClashQuery(q, "sni", clashString(proxy, "sni", "servername"))
	addClashQuery(q, "upmbps", clashString(proxy, "up", "up-mbps", "up_mbps"))
	addClashQuery(q, "downmbps", clashString(proxy, "down", "down-mbps", "down_mbps"))
	addClashQuery(q, "obfs", clashString(proxy, "obfs"))
	addClashQuery(q, "recv_window_conn", clashString(proxy, "recv-window-conn", "recv_window_conn"))
	addClashQuery(q, "recv_window", clashString(proxy, "recv-window", "recv_window"))
	addClashQuery(q, "alpn", strings.Join(stringSliceFromAny(proxy["alpn"]), ","))
	addClashQuery(q, "mport", strings.Join(stringSliceFromAny(proxy["ports"]), ","))
	addClashQuery(q, "hop_interval", clashString(proxy, "hop-interval", "hop_interval"))
	if toBool(proxy["skip-cert-verify"]) || toBool(proxy["insecure"]) {
		q.Set("insecure", "1")
	}
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashHysteria2URI(proxy map[string]any, host string, port int, password, name string) string {
	u := &url.URL{Scheme: "hy2", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(password)}
	q := url.Values{}
	addClashQuery(q, "sni", clashString(proxy, "sni", "servername"))
	if toBool(proxy["skip-cert-verify"]) || toBool(proxy["insecure"]) {
		q.Set("insecure", "1")
	}
	addClashQuery(q, "obfs", clashString(proxy, "obfs"))
	addClashQuery(q, "obfs-password", clashString(proxy, "obfs-password", "obfs_password"))
	addClashQuery(q, "upmbps", clashString(proxy, "up", "up-mbps", "up_mbps"))
	addClashQuery(q, "downmbps", clashString(proxy, "down", "down-mbps", "down_mbps"))
	addClashQuery(q, "alpn", strings.Join(stringSliceFromAny(proxy["alpn"]), ","))
	addClashQuery(q, "mport", strings.Join(stringSliceFromAny(proxy["ports"]), ","))
	addClashQuery(q, "hop_interval", clashString(proxy, "hop-interval", "hop_interval"))
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashTUICURI(proxy map[string]any, host string, port int, uuid, password, name string) string {
	u := &url.URL{Scheme: "tuic", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.UserPassword(uuid, password)}
	q := url.Values{}
	addClashQuery(q, "sni", clashString(proxy, "sni", "servername"))
	addClashQuery(q, "congestion_control", clashString(proxy, "congestion-controller", "congestion_control"))
	addClashQuery(q, "udp_relay_mode", clashString(proxy, "udp-relay-mode", "udp_relay_mode"))
	if toBool(proxy["skip-cert-verify"]) {
		q.Set("insecure", "1")
	}
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashAnyTLSURI(proxy map[string]any, host string, port int, password, name string) string {
	u := &url.URL{Scheme: "anytls", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(password)}
	q := url.Values{}
	addClashQuery(q, "sni", clashString(proxy, "sni", "servername"))
	if toBool(proxy["skip-cert-verify"]) {
		q.Set("insecure", "1")
	}
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashWireGuardURI(proxy map[string]any, host string, port int, privateKey, publicKey, address, name string) string {
	u := &url.URL{Scheme: "wireguard", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(privateKey)}
	if ipv6 := clashString(proxy, "ipv6"); ipv6 != "" {
		address += "," + ipv6
	}
	q := url.Values{"publickey": []string{publicKey}, "address": []string{address}}
	addClashQuery(q, "presharedkey", clashString(proxy, "pre-shared-key", "preshared-key", "presharedkey"))
	addClashQuery(q, "reserved", strings.Join(stringSliceFromAny(proxy["reserved"]), ","))
	addClashQuery(q, "mtu", clashString(proxy, "mtu"))
	addClashQuery(q, "allowedips", strings.Join(stringSliceFromAny(proxy["allowed-ips"]), ","))
	u.RawQuery = q.Encode()
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func clashString(proxy map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := proxy[key]; ok {
			if s := strings.TrimSpace(urAsString(value)); s != "" {
				return s
			}
		}
	}
	return ""
}

func cloneProxyMap(proxy map[string]any) map[string]any {
	clone := make(map[string]any, len(proxy))
	for key, value := range proxy {
		clone[key] = value
	}
	return clone
}

func clashNestedString(proxy map[string]any, mapKey string, keys ...string) string {
	nested, ok := proxy[mapKey].(map[string]any)
	if !ok {
		return ""
	}
	return clashString(nested, keys...)
}

func buildClashStandardURI(protocol, host string, port int, username, password, name string) string {
	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if username != "" || password != "" {
		u.User = url.UserPassword(username, password)
	}
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashShadowsocksURI(proxy map[string]any, cipher, password, host string, port int, name string) string {
	u := &url.URL{Scheme: "ss", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.UserPassword(cipher, password)}
	if plugin := clashString(proxy, "plugin"); plugin != "" {
		if options := clashPluginOptions(proxy["plugin-opts"]); options != "" {
			plugin += ";" + options
		}
		u.RawQuery = url.Values{"plugin": []string{plugin}}.Encode()
	}
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func clashPluginOptions(raw any) string {
	if value, ok := raw.(string); ok {
		return strings.TrimSpace(value)
	}
	options, ok := raw.(map[string]any)
	if !ok || len(options) == 0 {
		return ""
	}
	keys := make([]string, 0, len(options))
	for key := range options {
		if strings.TrimSpace(key) != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(urAsString(options[key]))
		if value == "" {
			continue
		}
		parts = append(parts, key+"="+value)
	}
	return strings.Join(parts, ";")
}

func buildClashVMessURI(proxy map[string]any, host string, port int, uuid, name string) string {
	network, path, transportHost, serviceName, _, _ := clashTransportOptions(proxy)
	if network == "grpc" && serviceName != "" {
		path = serviceName
	}
	node := map[string]any{
		"v":             "2",
		"ps":            name,
		"add":           host,
		"port":          strconv.Itoa(port),
		"id":            uuid,
		"aid":           toInt(proxy["alterId"]),
		"scy":           clashString(proxy, "cipher"),
		"net":           network,
		"type":          clashString(proxy, "header-type", "headerType"),
		"host":          transportHost,
		"path":          path,
		"tls":           clashClashTLS(proxy),
		"sni":           clashString(proxy, "servername", "sni"),
		"fp":            clashString(proxy, "client-fingerprint", "fingerprint"),
		"alpn":          strings.Join(stringSliceFromAny(proxy["alpn"]), ","),
		"allowInsecure": toBool(proxy["skip-cert-verify"]),
	}
	raw, _ := json.Marshal(node)
	return "vmess://" + base64.RawStdEncoding.EncodeToString(raw)
}

func buildClashVLESSURI(proxy map[string]any, host string, port int, uuid, name string) string {
	network, path, transportHost, serviceName, mode, extra := clashTransportOptions(proxy)
	q := url.Values{}
	addClashQuery(q, "type", network)
	addClashQuery(q, "security", clashClashSecurity(proxy))
	addClashQuery(q, "sni", clashString(proxy, "servername", "sni"))
	addClashQuery(q, "fp", clashString(proxy, "client-fingerprint", "fingerprint"))
	addClashQuery(q, "alpn", strings.Join(stringSliceFromAny(proxy["alpn"]), ","))
	if toBool(proxy["skip-cert-verify"]) {
		q.Set("allowInsecure", "true")
	}
	addClashQuery(q, "flow", clashString(proxy, "flow"))
	addClashQuery(q, "path", path)
	addClashQuery(q, "host", transportHost)
	addClashQuery(q, "serviceName", serviceName)
	addClashQuery(q, "mode", mode)
	addClashQuery(q, "extra", extra)
	addClashQuery(q, "pbk", clashNestedString(proxy, "reality-opts", "public-key", "publicKey", "pbk"))
	addClashQuery(q, "sid", clashNestedString(proxy, "reality-opts", "short-id", "shortId", "sid"))
	u := &url.URL{Scheme: "vless", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(uuid), RawQuery: q.Encode()}
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func buildClashTrojanURI(proxy map[string]any, host string, port int, password, name string) string {
	network, path, transportHost, serviceName, mode, extra := clashTransportOptions(proxy)
	q := url.Values{}
	addClashQuery(q, "type", network)
	addClashQuery(q, "security", clashClashSecurity(proxy))
	addClashQuery(q, "sni", clashString(proxy, "servername", "sni"))
	addClashQuery(q, "fp", clashString(proxy, "client-fingerprint", "fingerprint"))
	addClashQuery(q, "alpn", strings.Join(stringSliceFromAny(proxy["alpn"]), ","))
	if toBool(proxy["skip-cert-verify"]) {
		q.Set("allowInsecure", "true")
	}
	addClashQuery(q, "flow", clashString(proxy, "flow"))
	addClashQuery(q, "path", path)
	addClashQuery(q, "host", transportHost)
	addClashQuery(q, "serviceName", serviceName)
	addClashQuery(q, "mode", mode)
	addClashQuery(q, "extra", extra)
	u := &url.URL{Scheme: "trojan", Host: net.JoinHostPort(host, strconv.Itoa(port)), User: url.User(password), RawQuery: q.Encode()}
	if name != "" {
		u.Fragment = name
	}
	return u.String()
}

func clashTransportOptions(proxy map[string]any) (network, path, host, serviceName, mode, extra string) {
	network = strings.ToLower(clashString(proxy, "network"))
	path = clashString(proxy, "ws-path", "path")
	host = clashString(proxy, "host")
	serviceName = clashNestedString(proxy, "grpc-opts", "grpc-service-name", "serviceName", "service-name")

	var opts map[string]any
	switch network {
	case "ws", "websocket":
		opts = clashNestedMap(proxy, "ws-opts")
	case "httpupgrade", "http-upgrade":
		opts = clashNestedMap(proxy, "http-upgrade-opts", "httpupgrade-opts")
	case "xhttp", "splithttp":
		opts = clashNestedMap(proxy, "xhttp-opts", "splithttp-opts")
		mode = clashString(opts, "mode")
		extra = clashJSONValue(opts["extra"])
	}
	if opts != nil {
		if nestedPath := clashString(opts, "path"); nestedPath != "" {
			path = nestedPath
		}
		if nestedHost := clashString(opts, "host"); nestedHost != "" {
			host = nestedHost
		}
		if headerHost := clashNestedString(opts, "headers", "Host", "host"); headerHost != "" {
			host = headerHost
		}
	}
	return network, path, host, serviceName, mode, extra
}

func clashNestedMap(proxy map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if nested, ok := proxy[key].(map[string]any); ok {
			return nested
		}
	}
	return nil
}

func clashJSONValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func clashClashSecurity(proxy map[string]any) string {
	if security := clashString(proxy, "security"); security != "" {
		return security
	}
	if reality := proxy["reality-opts"]; reality != nil {
		return "reality"
	}
	if toBool(proxy["tls"]) {
		return "tls"
	}
	return ""
}

func clashClashTLS(proxy map[string]any) string {
	if toBool(proxy["tls"]) {
		return "tls"
	}
	return ""
}

func addClashQuery(q url.Values, key, value string) {
	if strings.TrimSpace(value) != "" {
		q.Set(key, value)
	}
}

func parseProxyNode(line string) parsedProxyNode {
	lower := strings.ToLower(strings.TrimSpace(line))
	if strings.HasPrefix(lower, "vmess://") {
		return parseVMessShareNode(line)
	}
	if strings.HasPrefix(lower, "ss://") {
		return parseShadowsocksNode(line)
	}
	u, err := url.Parse(line)
	if err != nil || u.Scheme == "" {
		return parsedProxyNode{Raw: line, Err: "unsupported node"}
	}
	node := parsedProxyNode{Raw: line, Kind: "xray", Protocol: strings.ToLower(u.Scheme), Host: u.Hostname(), Network: u.Query().Get("type")}
	if name, err := url.QueryUnescape(strings.TrimPrefix(u.Fragment, "#")); err == nil {
		node.Name = name
	}
	if port, _ := strconv.Atoi(u.Port()); port > 0 {
		node.Port = port
	}
	if u.User != nil {
		node.Username = u.User.Username()
		node.Password, _ = u.User.Password()
	}
	if node.Protocol == "http" || node.Protocol == "https" || node.Protocol == "socks" || node.Protocol == "socks5" || node.Protocol == "socks5h" {
		node.Kind = "standard"
	}
	if canonicalSingBoxProtocol(node.Protocol) != node.Protocol {
		node.Protocol = canonicalSingBoxProtocol(node.Protocol)
	}
	if node.Host == "" || node.Port <= 0 {
		return parsedProxyNode{Raw: line, Err: "missing host or port"}
	}
	if node.Protocol == "socks" || node.Protocol == "socks5h" {
		node.Protocol = "socks5h"
	}
	if node.Kind == "xray" {
		switch node.Protocol {
		case "vmess", "vless", "trojan", "ss", "hysteria", "hysteria2", "tuic", "anytls", "naive", "wireguard":
		default:
			return parsedProxyNode{Raw: line, Err: "unsupported node protocol"}
		}
	}
	return node
}

func parseVMessShareNode(line string) parsedProxyNode {
	payload := strings.TrimSpace(line)
	payload = payload[len("vmess://"):]
	if index := strings.IndexAny(payload, "?#"); index >= 0 {
		payload = payload[:index]
	}
	decoded, ok := decodeShareBase64(payload)
	if !ok {
		return parsedProxyNode{Raw: line, Err: "invalid vmess payload"}
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(decoded), &data); err != nil {
		return parsedProxyNode{Raw: line, Err: "invalid vmess payload"}
	}
	host := strings.TrimSpace(urAsString(data["add"]))
	port := toInt(data["port"])
	uuid := strings.TrimSpace(urAsString(data["id"]))
	if host == "" || port <= 0 || port > 65535 || uuid == "" {
		return parsedProxyNode{Raw: line, Err: "vmess node missing address, port or id"}
	}
	return parsedProxyNode{
		Raw:      line,
		Name:     strings.TrimSpace(urAsString(data["ps"])),
		Kind:     "xray",
		Protocol: "vmess",
		Host:     host,
		Port:     port,
		Username: uuid,
		Network:  strings.TrimSpace(urAsString(data["net"])),
	}
}

func parseShadowsocksNode(line string) parsedProxyNode {
	method, password, host, port, err := parseShadowsocksShare(line)
	if err != nil {
		return parsedProxyNode{Raw: line, Err: "invalid shadowsocks payload"}
	}
	name := ""
	if parsed, parseErr := url.Parse(line); parseErr == nil {
		if decoded, decodeErr := url.QueryUnescape(strings.TrimPrefix(parsed.Fragment, "#")); decodeErr == nil {
			name = decoded
		}
	}
	return parsedProxyNode{
		Raw:      line,
		Name:     name,
		Kind:     "xray",
		Protocol: "ss",
		Host:     host,
		Port:     port,
		Username: method,
		Password: password,
	}
}

func invalidUserResourceField(field, reason string) error {
	return infraerrors.BadRequest("USER_RESOURCE_INVALID", fmt.Sprintf("%s %s", field, reason))
}

func mergeResourceState(existing, payload map[string]any, specs map[string]columnSpec) map[string]any {
	state := map[string]any{}
	for key := range specs {
		if value, ok := existing[key]; ok {
			state[key] = value
		}
		if value, ok := payload[key]; ok {
			state[key] = value
		}
	}
	return state
}

func validateResourcePayloadTypes(payload map[string]any, specs map[string]columnSpec) error {
	for key, value := range payload {
		spec, ok := specs[key]
		if !ok || value == nil {
			continue
		}
		if _, err := coerceColumnValue(spec.Kind, value); err != nil {
			return invalidUserResourceField(key, err.Error())
		}
	}
	return nil
}

func validateAllowedValue(field, value string, allowed ...string) error {
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return invalidUserResourceField(field, "has an unsupported value")
}

func (s *ProxySourceService) normalizeAndValidateProxyPayloadForOwner(ctx context.Context, ownerID *int64, proxyID int64, existing, payload map[string]any) error {
	if err := validateResourcePayloadTypes(payload, proxyWritableColumns); err != nil {
		return err
	}
	state := mergeResourceState(existing, payload, proxyWritableColumns)
	name := strings.TrimSpace(urAsString(state["name"]))
	if name == "" {
		return invalidUserResourceField("name", "is required")
	}
	payload["name"] = name
	kind := strings.ToLower(strings.TrimSpace(urAsString(state["kind"])))
	if err := validateAllowedValue("kind", kind, "standard", "xray"); err != nil {
		return err
	}
	payload["kind"] = kind
	protocol := strings.ToLower(strings.TrimSpace(urAsString(state["protocol"])))
	protocol = canonicalStandardProxyProtocol(protocol)
	if kind == "standard" {
		if err := validateAllowedValue("protocol", protocol, "http", "https", "socks5", "socks5h"); err != nil {
			return err
		}
	} else if err := validateAllowedValue("protocol", protocol,
		"http", "https", "socks5", "socks5h", "vmess", "vless", "trojan", "ss", "shadowsocks",
		"hysteria", "hysteria2", "tuic", "anytls", "naive", "wireguard"); err != nil {
		return err
	}
	payload["protocol"] = protocol
	host := strings.TrimSpace(urAsString(state["host"]))
	if host == "" {
		return invalidUserResourceField("host", "is required")
	}
	port, err := strictInt64Value(state["port"])
	if err != nil || port < 1 || port > 65535 {
		return invalidUserResourceField("port", "must be an integer between 1 and 65535")
	}
	status := strings.ToLower(strings.TrimSpace(urAsString(state["status"])))
	if err := validateAllowedValue("status", status, StatusActive, StatusDisabled, StatusError); err != nil {
		return err
	}
	payload["status"] = status
	mode := strings.ToLower(strings.TrimSpace(urAsString(state["fallback_mode"])))
	if err := validateAllowedValue("fallback_mode", mode, FallbackModeNone, FallbackModeProxy, FallbackModeDirect); err != nil {
		return err
	}
	payload["fallback_mode"] = mode
	warnDays, err := strictInt64Value(state["expiry_warn_days"])
	if err != nil || warnDays < 0 {
		return invalidUserResourceField("expiry_warn_days", "must be an integer >= 0")
	}
	backupID := urToInt64(state["backup_proxy_id"])
	if mode == FallbackModeProxy && backupID <= 0 {
		return invalidUserResourceField("backup_proxy_id", "is required when fallback_mode=proxy")
	}
	if backupID == proxyID && backupID > 0 {
		return invalidUserResourceField("backup_proxy_id", "cannot reference the same proxy")
	}
	if backupID > 0 {
		if err := s.validateProxySelectableForOwner(ctx, ownerID, backupID); err != nil {
			return err
		}
	} else if _, ok := payload["backup_proxy_id"]; ok {
		payload["backup_proxy_id"] = nil
	}
	if kind == "xray" {
		extra, ok := state["extra"].(map[string]any)
		if !ok {
			return invalidUserResourceField("extra", "must be an object for xray proxies")
		}
		if _, exists := extra["outbound"]; exists {
			return invalidUserResourceField("extra.outbound", "is not accepted for user-owned proxies")
		}
		if _, exists := extra["xray_outbound"]; exists {
			return invalidUserResourceField("extra.xray_outbound", "is not accepted for user-owned proxies")
		}
		if _, exists := extra["sing_box_outbound"]; exists {
			return invalidUserResourceField("extra.sing_box_outbound", "is not accepted for user-owned proxies")
		}
		if _, exists := extra["sing_box_endpoint"]; exists {
			return invalidUserResourceField("extra.sing_box_endpoint", "is not accepted for user-owned proxies")
		}
		candidate := &Proxy{Kind: kind, Protocol: protocol, Host: host, Port: int(port), Username: urAsString(state["username"]), Password: urAsString(state["password"]), Extra: extra}
		if requiresSingBoxRuntime(candidate) {
			spec, err := buildSingBoxRuntimeSpec(xrayRawNode(candidate), candidate)
			if err != nil {
				return invalidUserResourceField("extra", "does not contain a valid sing-box node")
			}
			if err := validateUserSingBoxSpecHosts(ctx, spec); err != nil {
				return invalidUserResourceField("extra", "sing-box node must resolve to a public endpoint")
			}
			return nil
		}
		outbound, err := buildXrayOutbound(xrayRawNode(candidate), candidate)
		if err != nil {
			return invalidUserResourceField("extra", "does not contain a valid xray node")
		}
		if err := validateUserXrayOutboundHosts(ctx, outbound); err != nil {
			return invalidUserResourceField("extra", "xray node must resolve to a public endpoint")
		}
	} else {
		if _, err := resolveExternalHostIPs(ctx, host); err != nil {
			return invalidUserResourceField("host", "must resolve to a public endpoint")
		}
	}
	return nil
}

func validateUserXrayOutboundHosts(ctx context.Context, outbound map[string]any) error {
	return pinUserOwnedXrayOutbound(ctx, outbound)
}

func strictFloatValue(value any) (float64, error) {
	var out float64
	switch typed := value.(type) {
	case float64:
		out = typed
	case float32:
		out = float64(typed)
	case int:
		out = float64(typed)
	case int8:
		out = float64(typed)
	case int16:
		out = float64(typed)
	case int32:
		out = float64(typed)
	case int64:
		out = float64(typed)
	case uint:
		out = float64(typed)
	case uint8:
		out = float64(typed)
	case uint16:
		out = float64(typed)
	case uint32:
		out = float64(typed)
	case uint64:
		out = float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, err
		}
		out = parsed
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		if err != nil {
			return 0, err
		}
		out = parsed
	default:
		return 0, fmt.Errorf("must be a number")
	}
	if math.IsNaN(out) || math.IsInf(out, 0) {
		return 0, fmt.Errorf("must be finite")
	}
	return out, nil
}

func strictInt64Value(value any) (int64, error) {
	switch typed := value.(type) {
	case int:
		return int64(typed), nil
	case int8:
		return int64(typed), nil
	case int16:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case int64:
		return typed, nil
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, fmt.Errorf("is out of range")
		}
		return int64(typed), nil
	case uint8:
		return int64(typed), nil
	case uint16:
		return int64(typed), nil
	case uint32:
		return int64(typed), nil
	case uint64:
		if typed > math.MaxInt64 {
			return 0, fmt.Errorf("is out of range")
		}
		return int64(typed), nil
	case float32:
		return strictInt64Value(float64(typed))
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < math.MinInt64 || typed > math.MaxInt64 {
			return 0, fmt.Errorf("must be an integer")
		}
		return int64(typed), nil
	case json.Number:
		return typed.Int64()
	case string:
		return strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

func strictBoolValue(value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case string:
		return strconv.ParseBool(strings.TrimSpace(typed))
	default:
		return false, fmt.Errorf("must be a boolean")
	}
}

const userProxySourceSchedulerBatchSize = 20

const userProxySourceSyncTimeout = 3 * time.Minute

type dueUserProxySource struct {
	ID      int64
	OwnerID sql.NullInt64
}

func (s *ProxySourceService) StartProxySourceScheduler(interval time.Duration) {
	if s == nil || s.db == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}
	s.proxySourceSchedMu.Lock()
	if s.proxySourceCancel != nil {
		s.proxySourceSchedMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.proxySourceCancel = cancel
	s.proxySourceDone = done
	s.proxySourceSchedMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.runDueProxySourceSyncs(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runDueProxySourceSyncs(ctx)
			}
		}
	}()
}

func (s *ProxySourceService) Close() error {
	if s == nil {
		return nil
	}
	s.proxySourceSchedMu.Lock()
	cancel := s.proxySourceCancel
	done := s.proxySourceDone
	s.proxySourceCancel = nil
	s.proxySourceDone = nil
	s.proxySourceSchedMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.LegacyPrintf("service.user_resources", "proxy source scheduler shutdown timed out")
		}
	}
	s.stopProxyQualityWorkers()
	return nil
}

func (s *ProxySourceService) runDueProxySourceSyncs(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_user_id
FROM proxy_sources
WHERE deleted_at IS NULL
  AND sync_enabled
  AND (last_synced_at IS NULL OR last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute') <= NOW())
  AND (last_sync_status <> 'syncing' OR updated_at < NOW() - INTERVAL '10 minutes')
ORDER BY COALESCE(last_synced_at, created_at) ASC
LIMIT $1`, userProxySourceSchedulerBatchSize)
	if err != nil {
		logger.LegacyPrintf("service.user_resources", "list due proxy sources failed: %v", err)
		return
	}
	sources := make([]dueUserProxySource, 0, userProxySourceSchedulerBatchSize)
	for rows.Next() {
		var source dueUserProxySource
		if err := rows.Scan(&source.ID, &source.OwnerID); err != nil {
			_ = rows.Close()
			logger.LegacyPrintf("service.user_resources", "scan due proxy source failed: %v", err)
			return
		}
		sources = append(sources, source)
	}
	if err := rows.Close(); err != nil {
		logger.LegacyPrintf("service.user_resources", "close due proxy source rows failed: %v", err)
	}
	if err := rows.Err(); err != nil {
		logger.LegacyPrintf("service.user_resources", "iterate due proxy sources failed: %v", err)
		return
	}

	for _, source := range sources {
		if ctx.Err() != nil {
			return
		}
		claimed, err := s.claimDueProxySource(ctx, source)
		if err != nil {
			logger.LegacyPrintf("service.user_resources", "claim proxy source id=%d failed: %v", source.ID, err)
			continue
		}
		if !claimed {
			continue
		}
		syncCtx, cancel := context.WithTimeout(ctx, userProxySourceSyncTimeout)
		syncErr := s.syncDueProxySource(syncCtx, source)
		cancel()
		if syncErr != nil {
			s.markScheduledProxySourceFailure(source, syncErr)
		}
	}
}

func (s *ProxySourceService) syncDueProxySource(ctx context.Context, source dueUserProxySource) error {
	return syncDueProxySourceWith(
		ctx,
		source,
		func(ctx context.Context, sourceID int64) error {
			_, err := s.SyncSystemProxySource(ctx, sourceID)
			return err
		},
		func(ctx context.Context, ownerID, sourceID int64) error {
			_, err := s.SyncProxySource(ctx, ownerID, sourceID)
			return err
		},
	)
}

func syncDueProxySourceWith(
	ctx context.Context,
	source dueUserProxySource,
	syncSystem func(context.Context, int64) error,
	syncUser func(context.Context, int64, int64) error,
) error {
	if source.OwnerID.Valid {
		return syncUser(ctx, source.OwnerID.Int64, source.ID)
	}
	return syncSystem(ctx, source.ID)
}

func dueProxySourceOwnerValue(ownerID sql.NullInt64) any {
	if !ownerID.Valid {
		return nil
	}
	return ownerID.Int64
}

func (s *ProxySourceService) claimDueProxySource(ctx context.Context, source dueUserProxySource) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_sync_status = 'syncing', last_sync_error = NULL, updated_at = NOW()
WHERE id = $1 AND owner_user_id IS NOT DISTINCT FROM $2 AND deleted_at IS NULL
  AND sync_enabled
  AND (last_synced_at IS NULL OR last_synced_at + (refresh_interval_minutes * INTERVAL '1 minute') <= NOW())
  AND (last_sync_status <> 'syncing' OR updated_at < NOW() - INTERVAL '10 minutes')`, source.ID, dueProxySourceOwnerValue(source.OwnerID))
	if err != nil {
		return false, err
	}
	return affected(result) == 1, nil
}

func (s *ProxySourceService) markScheduledProxySourceFailure(source dueUserProxySource, syncErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_synced_at = NOW(), last_sync_status = 'error', last_sync_error = $1, last_imported_count = 0, updated_at = NOW()
WHERE id = $2 AND owner_user_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL`, safeSyncError(syncErr), source.ID, dueProxySourceOwnerValue(source.OwnerID))
	if err != nil {
		logger.LegacyPrintf("service.user_resources", "mark proxy source id=%d failure failed: %v", source.ID, err)
	}
}

const userProxyQualityWorkerCount = 3

const userProxyQualityQueueSize = 2048

const userProxyQualityTimeout = 2 * time.Minute

type userProxyQualityJob struct {
	ownerID *int64
	proxyID int64
}

type userProxyQualityRunner func(context.Context, *int64, int64) error

func (s *ProxySourceService) StartProxyQualityWorkers() {
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

func (s *ProxySourceService) stopProxyQualityWorkers() {
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

func (s *ProxySourceService) enqueueImportedProxyQualityChecks(ownerID int64, proxyIDs []int64) {
	if ownerID <= 0 {
		return
	}
	ownerCopy := ownerID
	s.enqueueProxyQualityChecks(&ownerCopy, proxyIDs)
}

func (s *ProxySourceService) enqueueSystemProxyQualityChecks(proxyIDs []int64) {
	s.enqueueProxyQualityChecks(nil, proxyIDs)
}

func (s *ProxySourceService) enqueueProxyQualityChecks(ownerID *int64, proxyIDs []int64) {
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
