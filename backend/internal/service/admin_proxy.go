package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/util/httputil"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
)

// Proxy management implementations
func (s *adminServiceImpl) ListProxies(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]Proxy, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	proxies, result, err := s.proxyRepo.ListWithFilters(ctx, params, protocol, status, search)
	if err != nil {
		return nil, 0, err
	}
	return proxies, result.Total, nil
}

func (s *adminServiceImpl) ListProxiesWithAccountCount(ctx context.Context, page, pageSize int, protocol, status, search string, sortBy, sortOrder string) ([]ProxyWithAccountCount, int64, error) {
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	proxies, result, err := s.proxyRepo.ListWithFiltersAndAccountCount(ctx, params, protocol, status, search)
	if err != nil {
		return nil, 0, err
	}
	s.attachProxyLatency(ctx, proxies)
	return proxies, result.Total, nil
}

type proxyOwnerScopeRepository interface {
	ListWithAccountCountAndOwnerScope(ctx context.Context, params pagination.PaginationParams, protocol, status, search, ownerScope string, sourceID int64) ([]ProxyWithAccountCount, *pagination.PaginationResult, error)
}

type proxyUserOwnedAccountCounter interface {
	CountUserOwnedAccountsByProxyID(ctx context.Context, proxyID int64) (int64, error)
}

func (s *adminServiceImpl) ListProxiesWithAccountCountByOwnerScope(ctx context.Context, page, pageSize int, protocol, status, search, ownerScope string, sourceID int64, sortBy, sortOrder string) ([]ProxyWithAccountCount, int64, error) {
	repo, ok := s.proxyRepo.(proxyOwnerScopeRepository)
	if !ok {
		return nil, 0, infraerrors.ServiceUnavailable("RESOURCE_OWNER_FILTER_UNAVAILABLE", "resource owner filter is not available")
	}
	params := pagination.PaginationParams{Page: page, PageSize: pageSize, SortBy: sortBy, SortOrder: sortOrder}
	proxies, result, err := repo.ListWithAccountCountAndOwnerScope(ctx, params, protocol, status, search, ownerScope, sourceID)
	if err != nil {
		return nil, 0, err
	}
	s.attachProxyLatency(ctx, proxies)
	return proxies, result.Total, nil
}

func (s *adminServiceImpl) GetAllProxies(ctx context.Context) ([]Proxy, error) {
	return s.proxyRepo.ListActive(ctx)
}

func (s *adminServiceImpl) GetAllProxiesWithAccountCount(ctx context.Context) ([]ProxyWithAccountCount, error) {
	proxies, err := s.proxyRepo.ListActiveWithAccountCount(ctx)
	if err != nil {
		return nil, err
	}
	s.attachProxyLatency(ctx, proxies)
	return proxies, nil
}

func (s *adminServiceImpl) GetProxy(ctx context.Context, id int64) (*Proxy, error) {
	return s.proxyRepo.GetByID(ctx, id)
}

func (s *adminServiceImpl) GetProxiesByIDs(ctx context.Context, ids []int64) ([]Proxy, error) {
	return s.proxyRepo.ListByIDs(ctx, ids)
}

func (s *adminServiceImpl) CreateProxy(ctx context.Context, input *CreateProxyInput) (*Proxy, error) {
	if !isJSONTimeInRange(input.ExpiresAt) {
		return nil, infraerrors.BadRequest("PROXY_EXPIRY_INVALID", "proxy expiry year must be between 0 and 9999")
	}
	kind := normalizeAdminProxyKind(input.Kind)
	protocol := strings.ToLower(strings.TrimSpace(input.Protocol))
	if err := validateAdminProxyMode(kind, protocol, input.Extra); err != nil {
		return nil, err
	}
	// 规范化 fallback_mode
	mode := input.FallbackMode
	if mode == "" {
		mode = FallbackModeNone
	}
	// 校验：mode=proxy 必须有 backup
	if mode == FallbackModeProxy && input.BackupProxyID == nil {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_REQUIRED", "backup proxy required when fallback_mode=proxy")
	}
	if input.ExpiryWarnDays < 0 {
		return nil, infraerrors.BadRequest("PROXY_WARN_DAYS_INVALID", "expiry_warn_days must be >= 0")
	}

	proxy := &Proxy{
		Name:           input.Name,
		IsPublic:       input.IsPublic,
		Kind:           kind,
		Protocol:       protocol,
		Host:           input.Host,
		Port:           input.Port,
		Username:       input.Username,
		Password:       input.Password,
		Status:         StatusActive,
		ExpiresAt:      input.ExpiresAt,
		FallbackMode:   mode,
		BackupProxyID:  input.BackupProxyID,
		ExpiryWarnDays: input.ExpiryWarnDays,
		Extra:          input.Extra,
	}
	if err := s.validateProxyFallbackOwner(ctx, proxy, input.BackupProxyID); err != nil {
		return nil, err
	}
	if err := s.proxyRepo.Create(ctx, proxy); err != nil {
		return nil, err
	}
	// Probe latency asynchronously so creation isn't blocked by network timeout.
	go s.probeProxyLatency(context.Background(), proxy)
	return proxy, nil
}

func (s *adminServiceImpl) UpdateProxy(ctx context.Context, id int64, input *UpdateProxyInput) (*Proxy, error) {
	if !isJSONTimeInRange(input.ExpiresAt) {
		return nil, infraerrors.BadRequest("PROXY_EXPIRY_INVALID", "proxy expiry year must be between 0 and 9999")
	}
	// 校验：backup_proxy_id 不能是自身
	if input.BackupProxyID != nil && *input.BackupProxyID == id {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_SELF", "backup proxy cannot be itself")
	}
	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	kind := proxy.Kind
	if strings.TrimSpace(input.Kind) != "" {
		kind = normalizeAdminProxyKind(input.Kind)
	}
	protocol := proxy.Protocol
	if strings.TrimSpace(input.Protocol) != "" {
		protocol = strings.ToLower(strings.TrimSpace(input.Protocol))
	}
	extra := proxy.Extra
	if input.Extra != nil {
		extra = input.Extra
	}
	// 官方 v0.2.4 起支持代理「部分更新」（只改名字／状态／到期时间等）。
	// Xray 独有的模式校验只在本次请求真的带了 kind／protocol／extra 时才重跑，
	// 否则库里 kind／protocol 为空的历史代理会让任何部分更新都被拒掉。
	if strings.TrimSpace(input.Kind) != "" || strings.TrimSpace(input.Protocol) != "" || input.Extra != nil {
		if err := validateAdminProxyMode(kind, protocol, extra); err != nil {
			return nil, err
		}
	}
	if input.IsPublic != nil && *input.IsPublic && proxy.OwnerUserID != nil {
		return nil, infraerrors.BadRequest("PROXY_PUBLIC_OWNER_INVALID", "only system proxies can be public")
	}
	if input.IsPublic != nil && proxy.OwnerUserID == nil && proxy.IsPublic && !*input.IsPublic {
		counter, ok := s.proxyRepo.(proxyUserOwnedAccountCounter)
		if !ok {
			return nil, infraerrors.ServiceUnavailable("PROXY_PUBLIC_USAGE_CHECK_UNAVAILABLE", "cannot verify public proxy usage")
		}
		count, err := counter.CountUserOwnedAccountsByProxyID(ctx, id)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, infraerrors.Conflict("PROXY_PUBLIC_IN_USE", "public proxy is still used by user-owned accounts")
		}
	}
	if err := s.validateProxyFallbackOwner(ctx, proxy, input.BackupProxyID); err != nil {
		return nil, err
	}

	// Merge only supplied fields, then validate the resulting fallback configuration.
	mode := proxy.FallbackMode
	if input.FallbackMode != "" {
		mode = input.FallbackMode
	}
	backupID := proxy.BackupProxyID
	if input.BackupProxyID != nil || input.ClearBackupID {
		backupID = input.BackupProxyID
	}
	if mode == FallbackModeProxy && backupID == nil {
		return nil, infraerrors.BadRequest("PROXY_BACKUP_REQUIRED", "backup proxy required when fallback_mode=proxy")
	}
	if input.ExpiryWarnDays != nil && *input.ExpiryWarnDays < 0 {
		return nil, infraerrors.BadRequest("PROXY_WARN_DAYS_INVALID", "expiry_warn_days must be >= 0")
	}

	if input.Name != "" {
		proxy.Name = input.Name
	}
	if input.IsPublic != nil {
		proxy.IsPublic = *input.IsPublic
	}
	proxy.Kind = kind
	proxy.Protocol = protocol
	proxy.Extra = extra
	if input.Host != "" {
		proxy.Host = input.Host
	}
	if input.Port != 0 {
		proxy.Port = input.Port
	}
	if input.Username != "" {
		proxy.Username = input.Username
	}
	if input.Password != "" {
		proxy.Password = input.Password
	}
	if input.Status != "" {
		proxy.Status = input.Status
	}
	if input.ExpiresAt != nil || input.ClearExpiresAt {
		proxy.ExpiresAt = input.ExpiresAt
	}
	proxy.FallbackMode = mode
	proxy.BackupProxyID = backupID
	if input.ExpiryWarnDays != nil {
		proxy.ExpiryWarnDays = *input.ExpiryWarnDays
	}

	if err := stopProxyRuntimesWithRetry(id); err != nil {
		return nil, fmt.Errorf("stop previous proxy runtime: %w", err)
	}
	if err := s.proxyRepo.Update(ctx, proxy); err != nil {
		return nil, err
	}
	return proxy, nil
}

func normalizeAdminProxyKind(kind string) string {
	normalized := strings.ToLower(strings.TrimSpace(kind))
	if normalized == "" {
		return "standard"
	}
	return normalized
}

func validateAdminProxyMode(kind, protocol string, extra map[string]any) error {
	standardProtocols := map[string]struct{}{"http": {}, "https": {}, "socks5": {}, "socks5h": {}}
	xrayProtocols := map[string]struct{}{
		"vmess": {}, "vless": {}, "trojan": {}, "ss": {},
		"hysteria": {}, "hysteria2": {}, "tuic": {}, "anytls": {}, "naive": {}, "wireguard": {},
	}

	switch kind {
	case "standard":
		if _, ok := standardProtocols[protocol]; !ok {
			return infraerrors.BadRequest("PROXY_PROTOCOL_INVALID", "standard proxy protocol must be http, https, socks5, or socks5h")
		}
	case "xray":
		if _, ok := xrayProtocols[protocol]; !ok {
			return infraerrors.BadRequest("PROXY_PROTOCOL_INVALID", "xray proxy protocol is not supported")
		}
		raw, _ := extra["raw"].(string)
		if strings.TrimSpace(raw) == "" {
			return infraerrors.BadRequest("PROXY_XRAY_RAW_REQUIRED", "xray proxy requires extra.raw node URI")
		}
		candidate := &Proxy{Kind: kind, Protocol: protocol, Extra: extra}
		if requiresSingBoxRuntime(candidate) {
			if _, err := buildSingBoxRuntimeSpec(raw, candidate); err != nil {
				return infraerrors.BadRequest("PROXY_XRAY_RAW_INVALID", "xray proxy contains an invalid sing-box node URI")
			}
		} else if _, err := buildXrayOutbound(raw, candidate); err != nil {
			return infraerrors.BadRequest("PROXY_XRAY_RAW_INVALID", "xray proxy contains an invalid node URI")
		}
	default:
		return infraerrors.BadRequest("PROXY_KIND_INVALID", "proxy kind must be standard or xray")
	}
	return nil
}

func (s *adminServiceImpl) validateProxyFallbackOwner(ctx context.Context, current *Proxy, backupID *int64) error {
	if current == nil || backupID == nil {
		return nil
	}
	if current.ID > 0 && *backupID == current.ID {
		return infraerrors.BadRequest("PROXY_BACKUP_SELF", "backup proxy cannot be itself")
	}
	backup, err := s.proxyRepo.GetByID(ctx, *backupID)
	if err != nil {
		return err
	}
	if current.OwnerUserID == nil {
		if backup.OwnerUserID != nil {
			return infraerrors.BadRequest("PROXY_BACKUP_OWNER_INVALID", "system proxies cannot use user-owned backup proxies")
		}
		return nil
	}
	if backup.OwnerUserID != nil && *backup.OwnerUserID == *current.OwnerUserID {
		return nil
	}
	if backup.OwnerUserID == nil && backup.IsPublic {
		return nil
	}
	return infraerrors.BadRequest("PROXY_BACKUP_OWNER_INVALID", "user proxies can only use owned or public system backup proxies")
}

func (s *adminServiceImpl) DeleteProxy(ctx context.Context, id int64) error {
	count, err := s.proxyRepo.CountAccountsByProxyID(ctx, id)
	if err != nil {
		return err
	}
	if count > 0 {
		return ErrProxyInUse
	}
	fallbackCount, err := s.proxyRepo.CountFallbackReferencesByProxyID(ctx, id)
	if err != nil {
		return err
	}
	if fallbackCount > 0 {
		return ErrProxyInUse
	}
	if err := stopProxyRuntimesWithRetry(id); err != nil {
		return err
	}
	return s.proxyRepo.Delete(ctx, id)
}

func (s *adminServiceImpl) BatchDeleteProxies(ctx context.Context, ids []int64) (*ProxyBatchDeleteResult, error) {
	result := &ProxyBatchDeleteResult{}
	if len(ids) == 0 {
		return result, nil
	}

	for _, id := range ids {
		count, err := s.proxyRepo.CountAccountsByProxyID(ctx, id)
		if err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: err.Error(),
			})
			continue
		}
		if count > 0 {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: ErrProxyInUse.Error(),
			})
			continue
		}
		fallbackCount, err := s.proxyRepo.CountFallbackReferencesByProxyID(ctx, id)
		if err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{ID: id, Reason: err.Error()})
			continue
		}
		if fallbackCount > 0 {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{ID: id, Reason: ErrProxyInUse.Error()})
			continue
		}
		if err := stopProxyRuntimesWithRetry(id); err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: err.Error(),
			})
			continue
		}
		if err := s.proxyRepo.Delete(ctx, id); err != nil {
			result.Skipped = append(result.Skipped, ProxyBatchDeleteSkipped{
				ID:     id,
				Reason: err.Error(),
			})
			continue
		}
		result.DeletedIDs = append(result.DeletedIDs, id)
	}

	return result, nil
}

func (s *adminServiceImpl) GetProxyAccounts(ctx context.Context, proxyID int64) ([]ProxyAccountSummary, error) {
	return s.proxyRepo.ListAccountSummariesByProxyID(ctx, proxyID)
}

func (s *adminServiceImpl) CheckProxyExists(ctx context.Context, host string, port int, username, password string) (bool, error) {
	return s.proxyRepo.ExistsByHostPortAuth(ctx, host, port, username, password)
}

func (s *adminServiceImpl) TestProxy(ctx context.Context, id int64) (*ProxyTestResult, error) {
	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	proxyURL, cleanup, resolveErr := resolveProxyProbeURL(ctx, s.proxyProbeResolver, proxy)
	if resolveErr != nil {
		message := logredact.RedactText(resolveErr.Error())
		s.saveProxyLatency(ctx, id, &ProxyLatencyInfo{
			Success:   false,
			Message:   message,
			UpdatedAt: time.Now(),
		})
		return &ProxyTestResult{Success: false, Message: message}, nil
	}
	defer cleanup()
	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxyURL)
	if err != nil {
		s.saveProxyLatency(ctx, id, &ProxyLatencyInfo{
			Success:   false,
			Message:   err.Error(),
			UpdatedAt: time.Now(),
		})
		return &ProxyTestResult{
			Success: false,
			Message: err.Error(),
		}, nil
	}

	latency := latencyMs
	s.saveProxyLatency(ctx, id, &ProxyLatencyInfo{
		Success:     true,
		LatencyMs:   &latency,
		Message:     "Proxy is accessible",
		IPAddress:   exitInfo.IP,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
		Region:      exitInfo.Region,
		City:        exitInfo.City,
		UpdatedAt:   time.Now(),
	})
	return &ProxyTestResult{
		Success:     true,
		Message:     "Proxy is accessible",
		LatencyMs:   latencyMs,
		IPAddress:   exitInfo.IP,
		City:        exitInfo.City,
		Region:      exitInfo.Region,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
	}, nil
}

func (s *adminServiceImpl) CheckProxyQuality(ctx context.Context, id int64) (*ProxyQualityCheckResult, error) {
	proxy, err := s.proxyRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	result := &ProxyQualityCheckResult{
		ProxyID:   id,
		Score:     100,
		Grade:     "A",
		CheckedAt: time.Now().Unix(),
		Items:     make([]ProxyQualityCheckItem, 0, len(proxyQualityTargets)+1),
	}

	if s.proxyProber == nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "base_connectivity",
			Status:  "fail",
			Message: "代理探测服务未配置",
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, nil)
		return result, nil
	}
	proxyURL, cleanup, resolveErr := resolveProxyProbeURL(ctx, s.proxyProbeResolver, proxy)
	if resolveErr != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "base_connectivity",
			Status:  "fail",
			Message: logredact.RedactText(resolveErr.Error()),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, nil)
		return result, nil
	}
	defer cleanup()

	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxyURL)
	if err != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:    "base_connectivity",
			Status:    "fail",
			LatencyMs: latencyMs,
			Message:   err.Error(),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, nil)
		return result, nil
	}

	result.ExitIP = exitInfo.IP
	result.Country = exitInfo.Country
	result.CountryCode = exitInfo.CountryCode
	result.BaseLatencyMs = latencyMs
	result.Items = append(result.Items, ProxyQualityCheckItem{
		Target:    "base_connectivity",
		Status:    "pass",
		LatencyMs: latencyMs,
		Message:   "代理出口连通正常",
	})
	result.PassedCount++

	client, err := httpclient.GetClient(httpclient.Options{
		ProxyURL:              proxyURL,
		Timeout:               proxyQualityRequestTimeout,
		ResponseHeaderTimeout: proxyQualityResponseHeaderTimeout,
	})
	if err != nil {
		result.Items = append(result.Items, ProxyQualityCheckItem{
			Target:  "http_client",
			Status:  "fail",
			Message: fmt.Sprintf("创建检测客户端失败: %v", err),
		})
		result.FailedCount++
		finalizeProxyQualityResult(result)
		s.saveProxyQualitySnapshot(ctx, id, result, exitInfo)
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
	s.saveProxyQualitySnapshot(ctx, id, result, exitInfo)
	return result, nil
}

func runProxyQualityTarget(ctx context.Context, client *http.Client, target proxyQualityTarget) ProxyQualityCheckItem {
	item := ProxyQualityCheckItem{
		Target: target.Target,
	}

	req, err := http.NewRequestWithContext(ctx, target.Method, target.URL, nil)
	if err != nil {
		item.Status = "fail"
		item.Message = fmt.Sprintf("构建请求失败: %v", err)
		return item
	}
	req.Header.Set("Accept", "application/json,text/html,*/*")
	req.Header.Set("User-Agent", proxyQualityClientUserAgent)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		item.Status = "fail"
		item.LatencyMs = time.Since(start).Milliseconds()
		item.Message = fmt.Sprintf("请求失败: %v", err)
		return item
	}
	defer func() { _ = resp.Body.Close() }()
	item.LatencyMs = time.Since(start).Milliseconds()
	item.HTTPStatus = resp.StatusCode

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, proxyQualityMaxBodyBytes+1))
	if readErr != nil {
		item.Status = "fail"
		item.Message = fmt.Sprintf("读取响应失败: %v", readErr)
		return item
	}
	if int64(len(body)) > proxyQualityMaxBodyBytes {
		body = body[:proxyQualityMaxBodyBytes]
	}

	// Cloudflare challenge 检测
	if httputil.IsCloudflareChallengeResponse(resp.StatusCode, resp.Header, body) {
		item.Status = "challenge"
		item.CFRay = httputil.ExtractCloudflareRayID(resp.Header, body)
		item.Message = "命中 Cloudflare challenge"
		return item
	}

	if _, ok := target.AllowedStatuses[resp.StatusCode]; ok {
		// 白名单内的状态码均代表目标可达：2xx 表示接口直接可用，
		// 401/405 等是无鉴权探测的预期结果，同样视为连通正常，不再扣分。
		item.Status = "pass"
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			item.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		} else {
			item.Message = fmt.Sprintf("HTTP %d（目标可达）", resp.StatusCode)
		}
		return item
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		item.Status = "warn"
		item.Message = "目标返回 429，可能存在频控"
		return item
	}

	item.Status = "fail"
	item.Message = fmt.Sprintf("非预期状态码: %d", resp.StatusCode)
	return item
}

func finalizeProxyQualityResult(result *ProxyQualityCheckResult) {
	if result == nil {
		return
	}
	score := 100 - result.WarnCount*10 - result.FailedCount*22 - result.ChallengeCount*30
	if score < 0 {
		score = 0
	}
	result.Score = score
	result.Grade = proxyQualityGrade(score)
	result.Summary = fmt.Sprintf(
		"通过 %d 项，告警 %d 项，失败 %d 项，挑战 %d 项",
		result.PassedCount,
		result.WarnCount,
		result.FailedCount,
		result.ChallengeCount,
	)
}

func proxyQualityGrade(score int) string {
	switch {
	case score >= 90:
		return "A"
	case score >= 75:
		return "B"
	case score >= 60:
		return "C"
	case score >= 40:
		return "D"
	default:
		return "F"
	}
}

func proxyQualityOverallStatus(result *ProxyQualityCheckResult) string {
	if result == nil {
		return ""
	}
	if result.ChallengeCount > 0 {
		return "challenge"
	}
	if result.FailedCount > 0 {
		// A reachable proxy can still be unable to access one provider's
		// public probe endpoint because of regional policy or upstream rules.
		// Keep that distinct from a proxy that cannot establish the base
		// connection at all.
		if proxyQualityBaseConnectivityPass(result) {
			return "warn"
		}
		return "failed"
	}
	if result.WarnCount > 0 {
		return "warn"
	}
	if result.PassedCount > 0 {
		return "healthy"
	}
	return "failed"
}

func proxyQualityFirstCFRay(result *ProxyQualityCheckResult) string {
	if result == nil {
		return ""
	}
	for _, item := range result.Items {
		if item.CFRay != "" {
			return item.CFRay
		}
	}
	return ""
}

func proxyQualityBaseConnectivityPass(result *ProxyQualityCheckResult) bool {
	if result == nil {
		return false
	}
	for _, item := range result.Items {
		if item.Target == "base_connectivity" {
			return item.Status == "pass"
		}
	}
	return false
}

func (s *adminServiceImpl) saveProxyQualitySnapshot(ctx context.Context, proxyID int64, result *ProxyQualityCheckResult, exitInfo *ProxyExitInfo) {
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
	s.saveProxyLatency(ctx, proxyID, info)
}

func (s *adminServiceImpl) probeProxyLatency(ctx context.Context, proxy *Proxy) {
	if s.proxyProber == nil || proxy == nil {
		return
	}
	proxyURL, cleanup, resolveErr := resolveProxyProbeURL(ctx, s.proxyProbeResolver, proxy)
	if resolveErr != nil {
		s.saveProxyLatency(ctx, proxy.ID, &ProxyLatencyInfo{
			Success:   false,
			Message:   logredact.RedactText(resolveErr.Error()),
			UpdatedAt: time.Now(),
		})
		return
	}
	defer cleanup()
	exitInfo, latencyMs, err := s.proxyProber.ProbeProxy(ctx, proxyURL)
	if err != nil {
		s.saveProxyLatency(ctx, proxy.ID, &ProxyLatencyInfo{
			Success:   false,
			Message:   err.Error(),
			UpdatedAt: time.Now(),
		})
		return
	}

	latency := latencyMs
	s.saveProxyLatency(ctx, proxy.ID, &ProxyLatencyInfo{
		Success:     true,
		LatencyMs:   &latency,
		Message:     "Proxy is accessible",
		IPAddress:   exitInfo.IP,
		Country:     exitInfo.Country,
		CountryCode: exitInfo.CountryCode,
		Region:      exitInfo.Region,
		City:        exitInfo.City,
		UpdatedAt:   time.Now(),
	})
}

func (s *adminServiceImpl) attachProxyLatency(ctx context.Context, proxies []ProxyWithAccountCount) {
	if s.proxyLatencyCache == nil || len(proxies) == 0 {
		return
	}

	ids := make([]int64, 0, len(proxies))
	for i := range proxies {
		ids = append(ids, proxies[i].ID)
	}

	latencies, err := s.proxyLatencyCache.GetProxyLatencies(ctx, ids)
	if err != nil {
		logger.LegacyPrintf("service.admin", "Warning: load proxy latency cache failed: %v", err)
		return
	}

	for i := range proxies {
		info := latencies[proxies[i].ID]
		if info == nil {
			continue
		}
		if info.Success {
			proxies[i].LatencyStatus = "success"
			proxies[i].LatencyMs = info.LatencyMs
		} else {
			proxies[i].LatencyStatus = "failed"
		}
		proxies[i].LatencyMessage = info.Message
		proxies[i].IPAddress = info.IPAddress
		proxies[i].Country = info.Country
		proxies[i].CountryCode = info.CountryCode
		proxies[i].Region = info.Region
		proxies[i].City = info.City
		if hasCurrentProxyQuality(info) {
			proxies[i].QualityStatus = info.QualityStatus
			proxies[i].QualityScore = info.QualityScore
			proxies[i].QualityGrade = info.QualityGrade
			proxies[i].QualitySummary = info.QualitySummary
			proxies[i].QualityChecked = info.QualityCheckedAt
		}
	}
}

func (s *adminServiceImpl) saveProxyLatency(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) {
	if s.proxyLatencyCache == nil || info == nil {
		return
	}

	merged := *info
	if latencies, err := s.proxyLatencyCache.GetProxyLatencies(ctx, []int64{proxyID}); err == nil {
		if existing := latencies[proxyID]; existing != nil {
			if merged.QualityCheckedAt == nil &&
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
	}

	if err := s.proxyLatencyCache.SetProxyLatency(ctx, proxyID, &merged); err != nil {
		logger.LegacyPrintf("service.admin", "Warning: store proxy latency cache failed: %v", err)
	}
}
