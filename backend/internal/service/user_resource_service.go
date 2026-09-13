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

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/util/logredact"
	"github.com/lib/pq"
	"gopkg.in/yaml.v3"
)

var (
	ErrUserResourceNotFound  = infraerrors.NotFound("USER_RESOURCE_NOT_FOUND", "resource not found")
	ErrUserResourceForbidden = infraerrors.Forbidden("USER_RESOURCE_FORBIDDEN", "resource is not owned by current user")
	ErrUserResourceInvalid   = infraerrors.BadRequest("USER_RESOURCE_INVALID", "invalid user resource payload")
)

var (
	resourceAuthHeaderPattern      = regexp.MustCompile(`(?i)\b((?:authorization|proxy[-_]?authorization)\s*[:=]\s*)(?:bearer|basic)\s+[^\s,&}]+`)
	proxyImportURLPattern          = regexp.MustCompile(`(?i)\b(?:https?|socks(?:5h?)?|vmess|vless|trojan|ss|hysteria2?|hy2|tuic|anytls|naive(?:\+https|\+quic)?|wireguard|wg)://[^\s,;]+`)
	proxyImportCredentialPattern   = regexp.MustCompile(`(?i)\b[^\s/@:]+:[^\s/@]+@(?:\[[0-9a-f:]+\]|[a-z0-9.-]+)(?::\d{1,5})?`)
	proxyImportEndpointPattern     = regexp.MustCompile(`(?i)\b(?:(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}|(?:\d{1,3}\.){3}\d{1,3})(?::\d{1,5})?\b`)
	proxyImportOpaqueSecretPattern = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)
)

const (
	userResourceBatchMaxItems   = 1000
	userResourceMaxGroups       = 100
	userResourceMaxAccounts     = 1000
	userResourceMaxProxies      = 1000
	userResourceMaxProxySources = 100
	userResourceMaxNotesBytes   = 4096
)

type UserResourceService struct {
	db                   *sql.DB
	subscriptionService  *SubscriptionService
	billingCacheService  *BillingCacheService
	authCacheInvalidator APIKeyAuthCacheInvalidator
	accountTestService   *AccountTestService
	tokenRefreshService  *TokenRefreshService
	oauthService         *OAuthService
	openAIOAuthService   *OpenAIOAuthService
	geminiOAuthService   *GeminiOAuthService
	antigravityOAuth     *AntigravityOAuthService
	grokOAuthService     *GrokOAuthService
	dashboardService     *DashboardService
	groupCapacityService *GroupCapacityService
	userGroupRateRepo    UserGroupRateRepository
	proxyProber          ProxyExitInfoProber
	proxyLatencyCache    ProxyLatencyCache
	proxyProbeResolver   ProxyProbeURLResolver
	oauthSessionMu       sync.Mutex
	oauthSessions        map[string]userResourceOAuthSession
	proxySourceSchedMu   sync.Mutex
	proxySourceCancel    context.CancelFunc
	proxySourceDone      chan struct{}
	proxyQualityMu       sync.Mutex
	proxyQualityCancel   context.CancelFunc
	proxyQualityJobs     chan userProxyQualityJob
	proxyQualityDone     chan struct{}
	proxyQualityRunner   userProxyQualityRunner
}

type UserUpstreamModelsPreviewInput struct {
	Platform    string `json:"platform"`
	Type        string `json:"type"`
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	AccountMode string `json:"account_mode"`
	APIProtocol string `json:"api_protocol"`
}

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

type userResourceOAuthSession struct {
	OwnerID   int64
	Platform  string
	CreatedAt time.Time
}

func NewUserResourceService(
	db *sql.DB,
	subscriptionService *SubscriptionService,
	billingCacheService *BillingCacheService,
	authCacheInvalidator APIKeyAuthCacheInvalidator,
) *UserResourceService {
	return &UserResourceService{
		db:                   db,
		subscriptionService:  subscriptionService,
		billingCacheService:  billingCacheService,
		authCacheInvalidator: authCacheInvalidator,
		proxyProbeResolver:   DefaultProxyProbeRuntimeResolver(),
	}
}

func (s *UserResourceService) SetAccountMaintenanceServices(accountTestService *AccountTestService, tokenRefreshService *TokenRefreshService) {
	if s == nil {
		return
	}
	s.accountTestService = accountTestService
	s.tokenRefreshService = tokenRefreshService
}

func (s *UserResourceService) scheduleOpenAIResponsesProbe(account map[string]any) {
	if s == nil || s.accountTestService == nil || urAsString(account["platform"]) != PlatformOpenAI || urAsString(account["type"]) != AccountTypeAPIKey {
		return
	}
	accountID := urToInt64(account["id"])
	if accountID <= 0 {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				slog.Error("user_openai_responses_probe_panic", "account_id", accountID, "recover", recovered)
			}
		}()
		s.accountTestService.ProbeOpenAIAPIKeyResponsesSupport(context.Background(), accountID)
	}()
}

func (s *UserResourceService) SetOAuthServices(
	oauthService *OAuthService,
	openAIOAuthService *OpenAIOAuthService,
	geminiOAuthService *GeminiOAuthService,
	antigravityOAuth *AntigravityOAuthService,
	grokOAuthService *GrokOAuthService,
) {
	if s == nil {
		return
	}
	s.oauthService = oauthService
	s.openAIOAuthService = openAIOAuthService
	s.geminiOAuthService = geminiOAuthService
	s.antigravityOAuth = antigravityOAuth
	s.grokOAuthService = grokOAuthService
	s.oauthSessions = make(map[string]userResourceOAuthSession)
}

func (s *UserResourceService) SetGroupSupportServices(
	dashboardService *DashboardService,
	groupCapacityService *GroupCapacityService,
	userGroupRateRepo UserGroupRateRepository,
) {
	if s == nil {
		return
	}
	s.dashboardService = dashboardService
	s.groupCapacityService = groupCapacityService
	s.userGroupRateRepo = userGroupRateRepo
}

func (s *UserResourceService) SetProxyObservabilityServices(
	proxyProber ProxyExitInfoProber,
	proxyLatencyCache ProxyLatencyCache,
) {
	if s == nil {
		return
	}
	s.proxyProber = proxyProber
	s.proxyLatencyCache = proxyLatencyCache
}

func (s *UserResourceService) SetProxyProbeResolver(resolver ProxyProbeURLResolver) {
	if s == nil {
		return
	}
	s.proxyProbeResolver = resolver
}

type UserResourceOAuthAuthURLInput struct {
	Platform    string `json:"platform"`
	ProxyID     *int64 `json:"proxy_id,omitempty"`
	SetupToken  bool   `json:"setup_token,omitempty"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	OAuthType   string `json:"oauth_type,omitempty"`
	TierID      string `json:"tier_id,omitempty"`
}

type UserResourceOAuthExchangeInput struct {
	Platform    string `json:"platform"`
	ProxyID     *int64 `json:"proxy_id,omitempty"`
	SetupToken  bool   `json:"setup_token,omitempty"`
	SessionID   string `json:"session_id"`
	Code        string `json:"code"`
	State       string `json:"state,omitempty"`
	RedirectURI string `json:"redirect_uri,omitempty"`
	OAuthType   string `json:"oauth_type,omitempty"`
	TierID      string `json:"tier_id,omitempty"`
}

type UserResourceOAuthCookieInput struct {
	ProxyID    *int64 `json:"proxy_id,omitempty"`
	SetupToken bool   `json:"setup_token,omitempty"`
	SessionKey string `json:"session_key"`
}

type UserResourceOAuthCredentialsResult struct {
	Credentials   map[string]any `json:"credentials"`
	Extra         map[string]any `json:"extra,omitempty"`
	SuggestedName string         `json:"suggested_name,omitempty"`
}

func (s *UserResourceService) GenerateAccountOAuthURL(ctx context.Context, ownerID int64, input UserResourceOAuthAuthURLInput) (any, error) {
	platform, err := normalizeUserOAuthPlatform(input.Platform)
	if err != nil {
		return nil, err
	}
	if err := validateUserOAuthTextLengths(input.RedirectURI, input.ProjectID, input.OAuthType, input.TierID); err != nil {
		return nil, err
	}
	if err := s.validateUserOAuthProxy(ctx, ownerID, input.ProxyID); err != nil {
		return nil, err
	}

	var result any
	var sessionID string
	switch platform {
	case PlatformAnthropic:
		if s.oauthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Anthropic OAuth is unavailable")
		}
		var authResult *GenerateAuthURLResult
		if input.SetupToken {
			authResult, err = s.oauthService.GenerateSetupTokenURL(ctx, input.ProxyID)
		} else {
			authResult, err = s.oauthService.GenerateAuthURL(ctx, input.ProxyID)
		}
		if authResult != nil {
			sessionID = authResult.SessionID
		}
		result = authResult
	case PlatformOpenAI:
		if input.SetupToken {
			return nil, infraerrors.BadRequest("USER_OAUTH_TYPE_INVALID", "setup token OAuth is only supported for Anthropic")
		}
		if s.openAIOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "OpenAI OAuth is unavailable")
		}
		authResult, callErr := s.openAIOAuthService.GenerateAuthURL(ctx, input.ProxyID, input.RedirectURI, PlatformOpenAI)
		err = callErr
		if authResult != nil {
			sessionID = authResult.SessionID
		}
		result = authResult
	case PlatformGemini:
		if input.SetupToken {
			return nil, infraerrors.BadRequest("USER_OAUTH_TYPE_INVALID", "setup token OAuth is only supported for Anthropic")
		}
		if s.geminiOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Gemini OAuth is unavailable")
		}
		oauthType := strings.TrimSpace(input.OAuthType)
		if oauthType == "" {
			oauthType = "code_assist"
		}
		authResult, callErr := s.geminiOAuthService.GenerateAuthURL(ctx, input.ProxyID, input.RedirectURI, input.ProjectID, oauthType, input.TierID)
		err = callErr
		if authResult != nil {
			sessionID = authResult.SessionID
		}
		result = authResult
	case PlatformAntigravity:
		if input.SetupToken {
			return nil, infraerrors.BadRequest("USER_OAUTH_TYPE_INVALID", "setup token OAuth is only supported for Anthropic")
		}
		if s.antigravityOAuth == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Antigravity OAuth is unavailable")
		}
		authResult, callErr := s.antigravityOAuth.GenerateAuthURL(ctx, input.ProxyID)
		err = callErr
		if authResult != nil {
			sessionID = authResult.SessionID
		}
		result = authResult
	case PlatformGrok:
		if input.SetupToken {
			return nil, infraerrors.BadRequest("USER_OAUTH_TYPE_INVALID", "setup token OAuth is only supported for Anthropic")
		}
		if s.grokOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Grok OAuth is unavailable")
		}
		authResult, callErr := s.grokOAuthService.GenerateAuthURL(ctx, input.ProxyID, input.RedirectURI)
		err = callErr
		if authResult != nil {
			sessionID = authResult.SessionID
		}
		result = authResult
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, infraerrors.ServiceUnavailable("USER_OAUTH_SESSION_FAILED", "OAuth session was not created")
	}
	s.recordUserOAuthSession(ownerID, platform, sessionID)
	return result, nil
}

func (s *UserResourceService) ExchangeAccountOAuthCode(ctx context.Context, ownerID int64, input UserResourceOAuthExchangeInput) (*UserResourceOAuthCredentialsResult, error) {
	platform, err := normalizeUserOAuthPlatform(input.Platform)
	if err != nil {
		return nil, err
	}
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.Code = strings.TrimSpace(input.Code)
	if input.SessionID == "" || input.Code == "" {
		return nil, infraerrors.BadRequest("USER_OAUTH_INPUT_REQUIRED", "session_id and code are required")
	}
	if len(input.SessionID) > 512 || len(input.Code) > 16384 || len(input.State) > 4096 {
		return nil, infraerrors.BadRequest("USER_OAUTH_INPUT_TOO_LARGE", "OAuth input is too large")
	}
	if err := validateUserOAuthTextLengths(input.RedirectURI, input.OAuthType, input.TierID); err != nil {
		return nil, err
	}
	if err := s.authorizeUserOAuthSession(ownerID, platform, input.SessionID); err != nil {
		return nil, err
	}
	if err := s.validateUserOAuthProxy(ctx, ownerID, input.ProxyID); err != nil {
		return nil, err
	}
	defer s.forgetUserOAuthSession(platform, input.SessionID)

	switch platform {
	case PlatformAnthropic:
		if s.oauthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Anthropic OAuth is unavailable")
		}
		tokenInfo, err := s.oauthService.ExchangeCode(ctx, &ExchangeCodeInput{SessionID: input.SessionID, Code: input.Code, ProxyID: input.ProxyID})
		if err != nil {
			return nil, err
		}
		return userResourceClaudeOAuthResult(tokenInfo), nil
	case PlatformOpenAI:
		if s.openAIOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "OpenAI OAuth is unavailable")
		}
		tokenInfo, err := s.openAIOAuthService.ExchangeCode(ctx, &OpenAIExchangeCodeInput{SessionID: input.SessionID, Code: input.Code, State: input.State, RedirectURI: input.RedirectURI, ProxyID: input.ProxyID})
		if err != nil {
			return nil, err
		}
		return userResourceOpenAIOAuthResult(s.openAIOAuthService, tokenInfo), nil
	case PlatformGemini:
		if s.geminiOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Gemini OAuth is unavailable")
		}
		oauthType := strings.TrimSpace(input.OAuthType)
		if oauthType == "" {
			oauthType = "code_assist"
		}
		tokenInfo, err := s.geminiOAuthService.ExchangeCode(ctx, &GeminiExchangeCodeInput{SessionID: input.SessionID, Code: input.Code, State: input.State, ProxyID: input.ProxyID, OAuthType: oauthType, TierID: input.TierID})
		if err != nil {
			return nil, err
		}
		return &UserResourceOAuthCredentialsResult{Credentials: s.geminiOAuthService.BuildAccountCredentials(tokenInfo)}, nil
	case PlatformAntigravity:
		if s.antigravityOAuth == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Antigravity OAuth is unavailable")
		}
		tokenInfo, err := s.antigravityOAuth.ExchangeCode(ctx, &AntigravityExchangeCodeInput{SessionID: input.SessionID, Code: input.Code, State: input.State, ProxyID: input.ProxyID})
		if err != nil {
			return nil, err
		}
		return userResourceAntigravityOAuthResult(s.antigravityOAuth, tokenInfo), nil
	case PlatformGrok:
		if s.grokOAuthService == nil {
			return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Grok OAuth is unavailable")
		}
		tokenInfo, err := s.grokOAuthService.ExchangeCode(ctx, &GrokExchangeCodeInput{SessionID: input.SessionID, Code: input.Code, State: input.State, RedirectURI: input.RedirectURI, ProxyID: input.ProxyID})
		if err != nil {
			return nil, err
		}
		return userResourceGrokOAuthResult(s.grokOAuthService, tokenInfo), nil
	default:
		return nil, infraerrors.BadRequest("USER_OAUTH_PLATFORM_INVALID", "unsupported OAuth platform")
	}
}

func (s *UserResourceService) ExchangeAccountOAuthCookie(ctx context.Context, ownerID int64, input UserResourceOAuthCookieInput) (*UserResourceOAuthCredentialsResult, error) {
	input.SessionKey = strings.TrimSpace(input.SessionKey)
	if input.SessionKey == "" {
		return nil, infraerrors.BadRequest("USER_OAUTH_COOKIE_REQUIRED", "session_key is required")
	}
	if len(input.SessionKey) > 16384 {
		return nil, infraerrors.BadRequest("USER_OAUTH_INPUT_TOO_LARGE", "session_key is too large")
	}
	if err := s.validateUserOAuthProxy(ctx, ownerID, input.ProxyID); err != nil {
		return nil, err
	}
	if s.oauthService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_OAUTH_UNAVAILABLE", "Anthropic OAuth is unavailable")
	}
	scope := "full"
	if input.SetupToken {
		scope = "inference"
	}
	tokenInfo, err := s.oauthService.CookieAuth(ctx, &CookieAuthInput{SessionKey: input.SessionKey, ProxyID: input.ProxyID, Scope: scope})
	if err != nil {
		return nil, err
	}
	return userResourceClaudeOAuthResult(tokenInfo), nil
}

func userResourceClaudeOAuthResult(tokenInfo *TokenInfo) *UserResourceOAuthCredentialsResult {
	extra := make(map[string]any, 3)
	if tokenInfo.OrgUUID != "" {
		extra["org_uuid"] = tokenInfo.OrgUUID
	}
	if tokenInfo.AccountUUID != "" {
		extra["account_uuid"] = tokenInfo.AccountUUID
	}
	if tokenInfo.EmailAddress != "" {
		extra["email_address"] = tokenInfo.EmailAddress
	}
	return &UserResourceOAuthCredentialsResult{
		Credentials:   BuildClaudeAccountCredentials(tokenInfo),
		Extra:         extra,
		SuggestedName: tokenInfo.EmailAddress,
	}
}

func userResourceOpenAIOAuthResult(service *OpenAIOAuthService, tokenInfo *OpenAITokenInfo) *UserResourceOAuthCredentialsResult {
	extra := make(map[string]any, 2)
	if tokenInfo.Email != "" {
		extra["email"] = tokenInfo.Email
	}
	if tokenInfo.PrivacyMode != "" {
		extra["privacy_mode"] = tokenInfo.PrivacyMode
	}
	return &UserResourceOAuthCredentialsResult{
		Credentials:   service.BuildAccountCredentials(tokenInfo),
		Extra:         extra,
		SuggestedName: tokenInfo.Email,
	}
}

func userResourceAntigravityOAuthResult(service *AntigravityOAuthService, tokenInfo *AntigravityTokenInfo) *UserResourceOAuthCredentialsResult {
	extra := make(map[string]any, 1)
	if tokenInfo.PrivacyMode != "" {
		extra["privacy_mode"] = tokenInfo.PrivacyMode
	}
	return &UserResourceOAuthCredentialsResult{
		Credentials:   service.BuildAccountCredentials(tokenInfo),
		Extra:         extra,
		SuggestedName: tokenInfo.Email,
	}
}

func userResourceGrokOAuthResult(service *GrokOAuthService, tokenInfo *GrokTokenInfo) *UserResourceOAuthCredentialsResult {
	extra := make(map[string]any, 3)
	if tokenInfo.Email != "" {
		extra["email"] = tokenInfo.Email
	}
	if tokenInfo.SubscriptionTier != "" {
		extra["subscription_tier"] = tokenInfo.SubscriptionTier
	}
	if tokenInfo.EntitlementStatus != "" {
		extra["entitlement_status"] = tokenInfo.EntitlementStatus
	}
	return &UserResourceOAuthCredentialsResult{
		Credentials:   service.BuildAccountCredentials(tokenInfo),
		Extra:         extra,
		SuggestedName: tokenInfo.Email,
	}
}

func normalizeUserOAuthPlatform(platform string) (string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	if err := validateAllowedValue("platform", platform, PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok); err != nil {
		return "", infraerrors.BadRequest("USER_OAUTH_PLATFORM_INVALID", "unsupported OAuth platform")
	}
	return platform, nil
}

func validateUserOAuthTextLengths(values ...string) error {
	for _, value := range values {
		if len(value) > 4096 {
			return infraerrors.BadRequest("USER_OAUTH_INPUT_TOO_LARGE", "OAuth input is too large")
		}
	}
	return nil
}

func (s *UserResourceService) validateUserOAuthProxy(ctx context.Context, ownerID int64, proxyID *int64) error {
	if proxyID == nil {
		return nil
	}
	if *proxyID <= 0 {
		return infraerrors.BadRequest("USER_OAUTH_PROXY_INVALID", "proxy_id is invalid")
	}
	return s.validateProxySelectable(ctx, ownerID, *proxyID)
}

func userOAuthSessionKey(platform, sessionID string) string {
	return platform + ":" + sessionID
}

func (s *UserResourceService) recordUserOAuthSession(ownerID int64, platform, sessionID string) {
	now := time.Now()
	s.oauthSessionMu.Lock()
	defer s.oauthSessionMu.Unlock()
	if s.oauthSessions == nil {
		s.oauthSessions = make(map[string]userResourceOAuthSession)
	}
	for key, session := range s.oauthSessions {
		if now.Sub(session.CreatedAt) > 15*time.Minute {
			delete(s.oauthSessions, key)
		}
	}
	s.oauthSessions[userOAuthSessionKey(platform, sessionID)] = userResourceOAuthSession{OwnerID: ownerID, Platform: platform, CreatedAt: now}
}

func (s *UserResourceService) authorizeUserOAuthSession(ownerID int64, platform, sessionID string) error {
	s.oauthSessionMu.Lock()
	defer s.oauthSessionMu.Unlock()
	session, ok := s.oauthSessions[userOAuthSessionKey(platform, sessionID)]
	if !ok || time.Since(session.CreatedAt) > 15*time.Minute {
		return infraerrors.BadRequest("USER_OAUTH_SESSION_NOT_FOUND", "OAuth session not found or expired")
	}
	if session.OwnerID != ownerID || session.Platform != platform {
		return infraerrors.Forbidden("USER_OAUTH_SESSION_FORBIDDEN", "OAuth session belongs to another user")
	}
	return nil
}

func (s *UserResourceService) forgetUserOAuthSession(platform, sessionID string) {
	s.oauthSessionMu.Lock()
	delete(s.oauthSessions, userOAuthSessionKey(platform, sessionID))
	s.oauthSessionMu.Unlock()
}

type UserResourceListOptions struct {
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

type UserResourcePage struct {
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

type UserAccountImportResult struct {
	Created      []map[string]any `json:"created"`
	Errors       []string         `json:"errors"`
	CreatedCount int              `json:"created_count"`
	FailedCount  int              `json:"failed_count"`
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

// ProxySourceSyncAllItem is the per-source line of a "sync every source" run.
// It deliberately carries counts only: node payloads would leak credentials and
// subscription URLs into a response that lists every source at once.
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

type UserSubscriptionAssignInput struct {
	UserID       int64  `json:"user_id"`
	Email        string `json:"email"`
	GroupID      int64  `json:"group_id"`
	ValidityDays int    `json:"validity_days"`
	Notes        string `json:"notes"`
}

type UserSubscriptionBulkAssignInput struct {
	UserIDs      []int64  `json:"user_ids"`
	Emails       []string `json:"emails"`
	GroupID      int64    `json:"group_id"`
	ValidityDays int      `json:"validity_days"`
	Notes        string   `json:"notes"`
}

type UserSubscriptionBulkAssignResult struct {
	SuccessCount int              `json:"success_count"`
	FailedCount  int              `json:"failed_count"`
	Items        []map[string]any `json:"items"`
	Errors       []string         `json:"errors"`
}

type SubscriptionPoolHealth struct {
	GroupID     int64              `json:"group_id"`
	Available   int64              `json:"available"`
	RateLimited int64              `json:"rate_limited"`
	Error       int64              `json:"error"`
	Disabled    int64              `json:"disabled"`
	Total       int64              `json:"total"`
	Reasons     []PoolHealthReason `json:"reasons,omitempty"`
	ByStatus    map[string]int64   `json:"by_status,omitempty"`
}

type PoolHealthReason struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Reason    string `json:"reason"`
}

type columnSpec struct {
	Kind   string
	Create bool
	Update bool
}

const (
	colString = "string"
	colInt    = "int"
	colInt64  = "int64"
	colFloat  = "float"
	colBool   = "bool"
	colTime   = "time"
	colJSON   = "json"
)

var groupWritableColumns = map[string]columnSpec{
	"name":                                 {Kind: colString, Create: true, Update: true},
	"description":                          {Kind: colString, Create: true, Update: true},
	"platform":                             {Kind: colString, Create: true, Update: true},
	"rate_multiplier":                      {Kind: colFloat, Create: true, Update: true},
	"peak_rate_enabled":                    {Kind: colBool, Create: true, Update: true},
	"peak_start":                           {Kind: colString, Create: true, Update: true},
	"peak_end":                             {Kind: colString, Create: true, Update: true},
	"peak_rate_multiplier":                 {Kind: colFloat, Create: true, Update: true},
	"is_exclusive":                         {Kind: colBool, Create: true, Update: true},
	"status":                               {Kind: colString, Create: true, Update: true},
	"subscription_type":                    {Kind: colString, Create: true, Update: true},
	"daily_limit_usd":                      {Kind: colFloat, Create: true, Update: true},
	"weekly_limit_usd":                     {Kind: colFloat, Create: true, Update: true},
	"monthly_limit_usd":                    {Kind: colFloat, Create: true, Update: true},
	"default_validity_days":                {Kind: colInt, Create: true, Update: true},
	"allow_image_generation":               {Kind: colBool, Create: true, Update: true},
	"allow_batch_image_generation":         {Kind: colBool, Create: true, Update: true},
	"image_rate_independent":               {Kind: colBool, Create: true, Update: true},
	"image_rate_multiplier":                {Kind: colFloat, Create: true, Update: true},
	"image_price_1k":                       {Kind: colFloat, Create: true, Update: true},
	"image_price_2k":                       {Kind: colFloat, Create: true, Update: true},
	"image_price_4k":                       {Kind: colFloat, Create: true, Update: true},
	"batch_image_discount_multiplier":      {Kind: colFloat, Create: true, Update: true},
	"batch_image_hold_multiplier":          {Kind: colFloat, Create: true, Update: true},
	"video_rate_independent":               {Kind: colBool, Create: true, Update: true},
	"video_rate_multiplier":                {Kind: colFloat, Create: true, Update: true},
	"video_price_480p":                     {Kind: colFloat, Create: true, Update: true},
	"video_price_720p":                     {Kind: colFloat, Create: true, Update: true},
	"video_price_1080p":                    {Kind: colFloat, Create: true, Update: true},
	"web_search_price_per_call":            {Kind: colFloat, Create: true, Update: true},
	"claude_code_only":                     {Kind: colBool, Create: true, Update: true},
	"fallback_group_id":                    {Kind: colInt64, Create: true, Update: true},
	"fallback_group_id_on_invalid_request": {Kind: colInt64, Create: true, Update: true},
	"model_routing":                        {Kind: colJSON, Create: true, Update: true},
	"model_routing_enabled":                {Kind: colBool, Create: true, Update: true},
	"mcp_xml_inject":                       {Kind: colBool, Create: true, Update: true},
	"supported_model_scopes":               {Kind: colJSON, Create: true, Update: true},
	"sort_order":                           {Kind: colInt, Create: true, Update: true},
	"allow_messages_dispatch":              {Kind: colBool, Create: true, Update: true},
	"allow_live":                           {Kind: colBool, Create: true, Update: true},
	"require_oauth_only":                   {Kind: colBool, Create: true, Update: true},
	"require_privacy_set":                  {Kind: colBool, Create: true, Update: true},
	"default_mapped_model":                 {Kind: colString, Create: true, Update: true},
	"messages_dispatch_model_config":       {Kind: colJSON, Create: true, Update: true},
	"model_allowlist":                      {Kind: colJSON, Create: true, Update: true},
	"rpm_limit":                            {Kind: colInt, Create: true, Update: true},
}

var accountWritableColumns = map[string]columnSpec{
	"name":                  {Kind: colString, Create: true, Update: true},
	"notes":                 {Kind: colString, Create: true, Update: true},
	"platform":              {Kind: colString, Create: true, Update: false},
	"type":                  {Kind: colString, Create: true, Update: true},
	"credentials":           {Kind: colJSON, Create: true, Update: true},
	"extra":                 {Kind: colJSON, Create: true, Update: true},
	"proxy_id":              {Kind: colInt64, Create: true, Update: true},
	"concurrency":           {Kind: colInt, Create: true, Update: true},
	"load_factor":           {Kind: colInt, Create: true, Update: true},
	"priority":              {Kind: colInt, Create: true, Update: true},
	"rate_multiplier":       {Kind: colFloat, Create: true, Update: true},
	"status":                {Kind: colString, Create: true, Update: true},
	"schedulable":           {Kind: colBool, Create: true, Update: true},
	"expires_at":            {Kind: colTime, Create: true, Update: true},
	"auto_pause_on_expired": {Kind: colBool, Create: true, Update: true},
}

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

func (s *UserResourceService) ensureDB() error {
	if s == nil || s.db == nil {
		return infraerrors.ServiceUnavailable("USER_RESOURCE_DB_UNAVAILABLE", "database is not available")
	}
	return nil
}

func (s *UserResourceService) ensureOwnedResourceCapacity(ctx context.Context, table string, ownerID int64, limit int) error {
	return s.ensureResourceCapacityForOwner(ctx, table, userResourceOwner(ownerID), limit)
}

func (s *UserResourceService) ensureResourceCapacityForOwner(ctx context.Context, table string, ownerID *int64, limit int) error {
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

func paged(items []map[string]any, total int64, page, pageSize int) *UserResourcePage {
	pages := int(math.Ceil(float64(total) / float64(pageSize)))
	if pages < 1 {
		pages = 1
	}
	return &UserResourcePage{Items: items, Total: total, Page: page, PageSize: pageSize, Pages: pages}
}

func nextArg(args *[]any, v any) string {
	*args = append(*args, v)
	return fmt.Sprintf("$%d", len(*args))
}

func validateUserResourceNotes(notes string) error {
	if len([]byte(notes)) > userResourceMaxNotesBytes {
		return infraerrors.BadRequest("USER_RESOURCE_NOTES_TOO_LARGE", fmt.Sprintf("notes cannot exceed %d bytes", userResourceMaxNotesBytes))
	}
	return nil
}

func ownedWhere(alias string, ownerID int64, args *[]any) []string {
	return []string{
		alias + ".deleted_at IS NULL",
		alias + ".owner_user_id = " + nextArg(args, ownerID),
	}
}

func (s *UserResourceService) ListGroups(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{}
	where := ownedWhere("g", ownerID, &args)
	if opts.Platform != "" {
		where = append(where, "g.platform = "+nextArg(&args, opts.Platform))
	}
	if opts.Status != "" {
		where = append(where, "g.status = "+nextArg(&args, opts.Status))
	}
	if opts.Search != "" {
		where = append(where, "(g.name ILIKE "+nextArg(&args, "%"+opts.Search+"%")+" OR g.description ILIKE "+nextArg(&args, "%"+opts.Search+"%")+")")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups g WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := resourceOrder(opts.SortBy, opts.SortOrder, map[string]string{
		"name":       "g.name",
		"platform":   "g.platform",
		"status":     "g.status",
		"created_at": "g.created_at",
		"sort_order": "g.sort_order",
	}, "g.sort_order ASC, g.id DESC")
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT
  g.id, g.owner_user_id, g.name, COALESCE(g.description, '') AS description, g.platform,
  g.rate_multiplier::double precision AS rate_multiplier,
  g.peak_rate_enabled, g.peak_start, g.peak_end, g.peak_rate_multiplier::double precision AS peak_rate_multiplier,
  g.is_exclusive, g.status, g.subscription_type,
  g.daily_limit_usd::double precision AS daily_limit_usd,
  g.weekly_limit_usd::double precision AS weekly_limit_usd,
  g.monthly_limit_usd::double precision AS monthly_limit_usd,
  g.default_validity_days,
  g.allow_image_generation, g.allow_batch_image_generation, g.image_rate_independent,
  g.image_rate_multiplier::double precision AS image_rate_multiplier,
  g.batch_image_discount_multiplier::double precision AS batch_image_discount_multiplier,
  g.batch_image_hold_multiplier::double precision AS batch_image_hold_multiplier,
  g.video_rate_independent, g.video_rate_multiplier::double precision AS video_rate_multiplier,
  g.claude_code_only, g.fallback_group_id, g.fallback_group_id_on_invalid_request,
  COALESCE(g.model_routing, '{}'::jsonb)::text AS model_routing,
  g.model_routing_enabled, g.mcp_xml_inject,
  COALESCE(g.supported_model_scopes, '[]'::jsonb)::text AS supported_model_scopes,
  g.sort_order, g.allow_messages_dispatch, g.allow_live, g.require_oauth_only, g.require_privacy_set,
  g.default_mapped_model,
  COALESCE(g.messages_dispatch_model_config, '{}'::jsonb)::text AS messages_dispatch_model_config,
  COALESCE(g.model_allowlist, '{}'::jsonb)::text AS model_allowlist,
  g.rpm_limit, g.created_at, g.updated_at,
  COUNT(ag.account_id)::bigint AS account_count,
  COALESCE(SUM(CASE WHEN a.status = 'active' AND a.schedulable = true AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()) AND (a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()) THEN 1 ELSE 0 END), 0)::bigint AS active_account_count,
  COALESCE(SUM(CASE WHEN a.rate_limit_reset_at > NOW() THEN 1 ELSE 0 END), 0)::bigint AS rate_limited_account_count
FROM groups g
LEFT JOIN account_groups ag ON ag.group_id = g.id
LEFT JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL
  AND a.owner_user_id IS NOT DISTINCT FROM g.owner_user_id
WHERE `+whereSQL+`
GROUP BY g.id
ORDER BY `+order+`
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

func (s *UserResourceService) GetGroup(ctx context.Context, ownerID, groupID int64) (map[string]any, error) {
	args := []any{}
	where := ownedWhere("g", ownerID, &args)
	where = append(where, "g.id = "+nextArg(&args, groupID))
	rows, err := s.db.QueryContext(ctx, `
SELECT
  g.id, g.owner_user_id, g.name, COALESCE(g.description, '') AS description, g.platform,
  g.rate_multiplier::double precision AS rate_multiplier, g.peak_rate_enabled, g.peak_start, g.peak_end,
  g.peak_rate_multiplier::double precision AS peak_rate_multiplier, g.is_exclusive, g.status,
  g.subscription_type, g.daily_limit_usd::double precision AS daily_limit_usd,
  g.weekly_limit_usd::double precision AS weekly_limit_usd, g.monthly_limit_usd::double precision AS monthly_limit_usd,
  g.default_validity_days, g.allow_image_generation, g.allow_batch_image_generation,
  g.image_rate_independent, g.image_rate_multiplier::double precision AS image_rate_multiplier,
  g.image_price_1k::double precision AS image_price_1k,
  g.image_price_2k::double precision AS image_price_2k,
  g.image_price_4k::double precision AS image_price_4k,
  g.batch_image_discount_multiplier::double precision AS batch_image_discount_multiplier,
  g.batch_image_hold_multiplier::double precision AS batch_image_hold_multiplier,
  g.video_rate_independent, g.video_rate_multiplier::double precision AS video_rate_multiplier,
  g.video_price_480p::double precision AS video_price_480p,
  g.video_price_720p::double precision AS video_price_720p,
  g.video_price_1080p::double precision AS video_price_1080p,
  g.claude_code_only, g.fallback_group_id, g.fallback_group_id_on_invalid_request,
  COALESCE(g.model_routing, '{}'::jsonb)::text AS model_routing, g.model_routing_enabled,
  g.mcp_xml_inject, COALESCE(g.supported_model_scopes, '[]'::jsonb)::text AS supported_model_scopes,
  g.sort_order, g.allow_messages_dispatch, g.allow_live, g.require_oauth_only, g.require_privacy_set,
  g.default_mapped_model, COALESCE(g.messages_dispatch_model_config, '{}'::jsonb)::text AS messages_dispatch_model_config,
  COALESCE(g.model_allowlist, '{}'::jsonb)::text AS model_allowlist, g.rpm_limit, g.created_at, g.updated_at,
  (SELECT COUNT(*) FROM account_groups ag JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL
   WHERE ag.group_id = g.id AND a.owner_user_id IS NOT DISTINCT FROM g.owner_user_id)::bigint AS account_count
FROM groups g
WHERE `+strings.Join(where, " AND ")+`
LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUserResourceNotFound
	}
	return items[0], nil
}

func (s *UserResourceService) ListGroupUsageSummary(ctx context.Context, ownerID int64, todayStart time.Time) ([]usagestats.GroupUsageSummary, error) {
	if s.dashboardService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_GROUP_USAGE_UNAVAILABLE", "group usage summary is unavailable")
	}
	owned, err := s.ownedGroupIDSet(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	all, err := s.dashboardService.GetGroupUsageSummary(ctx, todayStart)
	if err != nil {
		return nil, err
	}
	result := make([]usagestats.GroupUsageSummary, 0, len(owned))
	for _, item := range all {
		if _, ok := owned[item.GroupID]; ok {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *UserResourceService) ListGroupCapacitySummary(ctx context.Context, ownerID int64) ([]GroupCapacitySummary, error) {
	if s.groupCapacityService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_GROUP_CAPACITY_UNAVAILABLE", "group capacity summary is unavailable")
	}
	owned, err := s.ownedGroupIDSet(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	all, err := s.groupCapacityService.GetAllGroupCapacity(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]GroupCapacitySummary, 0, len(owned))
	for _, item := range all {
		if _, ok := owned[item.GroupID]; ok {
			result = append(result, item)
		}
	}
	return result, nil
}

func (s *UserResourceService) GetGroupModelsListCandidates(ctx context.Context, ownerID, groupID int64, platform string) ([]string, error) {
	platform = strings.TrimSpace(platform)
	if groupID > 0 {
		group, err := s.GetGroup(ctx, ownerID, groupID)
		if err != nil {
			return nil, err
		}
		if platform == "" {
			platform = strings.TrimSpace(fmt.Sprint(group["platform"]))
		}
	}
	if platform == "" {
		platform = PlatformAnthropic
	}
	if err := validateAllowedValue("platform", platform, PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek); err != nil {
		return nil, err
	}
	candidates := userResourceDefaultModelsListCandidateIDs(platform)
	if groupID <= 0 {
		return candidates, nil
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT COALESCE(a.credentials, '{}'::jsonb)::text
FROM accounts a
JOIN account_groups ag ON ag.account_id = a.id
WHERE ag.group_id = $1 AND a.owner_user_id = $2 AND a.deleted_at IS NULL
  AND a.platform = $3 AND a.status = 'active' AND a.schedulable = true`, groupID, ownerID, platform)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := make(map[string]struct{}, len(candidates))
	for _, model := range candidates {
		seen[model] = struct{}{}
	}
	var additions []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		credentials := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &credentials); err != nil {
			continue
		}
		mapping, _ := credentials["model_mapping"].(map[string]any)
		for model := range mapping {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if _, ok := seen[model]; ok {
				continue
			}
			seen[model] = struct{}{}
			additions = append(additions, model)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(additions)
	return append(candidates, additions...), nil
}

func userResourceDefaultModelsListCandidateIDs(platform string) []string {
	switch platform {
	case PlatformKimi:
		return []string{"kimi-k2.6", "kimi-k2.5", "kimi-k2-thinking", "kimi-k2", "kimi-k3", "kimi-for-coding", "moonshot-v1-8k", "moonshot-v1-32k", "moonshot-v1-128k"}
	case PlatformZhipu:
		return []string{"glm-5.2", "glm-5.1", "glm-5", "glm-4.7", "glm-4.6", "glm-4.5", "glm-4-flash"}
	case PlatformDeepseek:
		return []string{"deepseek-chat", "deepseek-reasoner", "deepseek-v4-pro", "deepseek-v4-flash"}
	default:
		return defaultModelsListCandidateIDs(platform)
	}
}

func (s *UserResourceService) GetGroupUserOverrides(ctx context.Context, ownerID, groupID int64) ([]UserGroupRateEntry, error) {
	if err := s.ensureOwned(ctx, "groups", ownerID, groupID); err != nil {
		return nil, err
	}
	if s.userGroupRateRepo == nil {
		return []UserGroupRateEntry{}, nil
	}
	allowed, err := s.groupSubscriberIDSet(ctx, ownerID, groupID)
	if err != nil {
		return nil, err
	}
	entries, err := s.userGroupRateRepo.GetByGroupID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	result := make([]UserGroupRateEntry, 0, len(entries))
	for _, entry := range entries {
		if _, ok := allowed[entry.UserID]; ok {
			result = append(result, entry)
		}
	}
	return result, nil
}

func (s *UserResourceService) SetGroupRateMultipliers(ctx context.Context, ownerID, groupID int64, entries []GroupRateMultiplierInput) error {
	if err := s.ensureOwned(ctx, "groups", ownerID, groupID); err != nil {
		return err
	}
	if len(entries) > userResourceBatchMaxItems {
		return infraerrors.BadRequest("USER_GROUP_OVERRIDE_LIMIT", "too many rate multiplier entries")
	}
	userIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		if entry.UserID <= 0 || entry.RateMultiplier <= 0 || entry.RateMultiplier > 1000 {
			return infraerrors.BadRequest("USER_GROUP_OVERRIDE_INVALID", "invalid user rate multiplier")
		}
		userIDs = append(userIDs, entry.UserID)
	}
	if err := s.validateGroupSubscriberIDs(ctx, ownerID, groupID, userIDs); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return infraerrors.ServiceUnavailable("USER_GROUP_OVERRIDE_UNAVAILABLE", "group overrides are unavailable")
	}
	if err := s.userGroupRateRepo.SyncGroupRateMultipliers(ctx, groupID, entries); err != nil {
		return err
	}
	s.invalidateGroupAuthCache(ctx, groupID)
	return nil
}

func (s *UserResourceService) SetGroupRPMOverrides(ctx context.Context, ownerID, groupID int64, entries []GroupRPMOverrideInput) error {
	if err := s.ensureOwned(ctx, "groups", ownerID, groupID); err != nil {
		return err
	}
	if len(entries) > userResourceBatchMaxItems {
		return infraerrors.BadRequest("USER_GROUP_OVERRIDE_LIMIT", "too many RPM override entries")
	}
	userIDs := make([]int64, 0, len(entries))
	for _, entry := range entries {
		if entry.UserID <= 0 || entry.RPMOverride == nil || *entry.RPMOverride < 0 || *entry.RPMOverride > 1000000 {
			return infraerrors.BadRequest("USER_GROUP_OVERRIDE_INVALID", "invalid user RPM override")
		}
		userIDs = append(userIDs, entry.UserID)
	}
	if err := s.validateGroupSubscriberIDs(ctx, ownerID, groupID, userIDs); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return infraerrors.ServiceUnavailable("USER_GROUP_OVERRIDE_UNAVAILABLE", "group overrides are unavailable")
	}
	if err := s.userGroupRateRepo.SyncGroupRPMOverrides(ctx, groupID, entries); err != nil {
		return err
	}
	s.invalidateGroupAuthCache(ctx, groupID)
	return nil
}

func (s *UserResourceService) ClearGroupRateMultipliers(ctx context.Context, ownerID, groupID int64) error {
	if err := s.ensureOwned(ctx, "groups", ownerID, groupID); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return infraerrors.ServiceUnavailable("USER_GROUP_OVERRIDE_UNAVAILABLE", "group overrides are unavailable")
	}
	if err := s.userGroupRateRepo.SyncGroupRateMultipliers(ctx, groupID, nil); err != nil {
		return err
	}
	s.invalidateGroupAuthCache(ctx, groupID)
	return nil
}

func (s *UserResourceService) ClearGroupRPMOverrides(ctx context.Context, ownerID, groupID int64) error {
	if err := s.ensureOwned(ctx, "groups", ownerID, groupID); err != nil {
		return err
	}
	if s.userGroupRateRepo == nil {
		return infraerrors.ServiceUnavailable("USER_GROUP_OVERRIDE_UNAVAILABLE", "group overrides are unavailable")
	}
	if err := s.userGroupRateRepo.ClearGroupRPMOverrides(ctx, groupID); err != nil {
		return err
	}
	s.invalidateGroupAuthCache(ctx, groupID)
	return nil
}

func (s *UserResourceService) CreateGroup(ctx context.Context, ownerID int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	payload = clonePayload(payload)
	defaultPayload(payload, map[string]any{
		"platform":                        PlatformAnthropic,
		"rate_multiplier":                 1.0,
		"peak_rate_enabled":               false,
		"peak_start":                      "",
		"peak_end":                        "",
		"peak_rate_multiplier":            1.0,
		"status":                          StatusActive,
		"subscription_type":               SubscriptionTypeStandard,
		"default_validity_days":           30,
		"rpm_limit":                       0,
		"allow_image_generation":          false,
		"allow_batch_image_generation":    false,
		"image_rate_multiplier":           1.0,
		"batch_image_discount_multiplier": 0.5,
		"batch_image_hold_multiplier":     0.6,
		"video_rate_multiplier":           1.0,
		"allow_live":                      false,
		"supported_model_scopes":          []string{"claude", "gemini_text", "gemini_image"},
		"messages_dispatch_model_config":  map[string]any{},
		"model_allowlist":                 map[string]any{},
	})
	if err := s.normalizeAndValidateGroupPayload(ctx, ownerID, 0, nil, payload); err != nil {
		return nil, err
	}
	if err := s.ensureOwnedResourceCapacity(ctx, "groups", ownerID, userResourceMaxGroups); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	id, err := s.insertOwnedWith(ctx, tx, "groups", ownerID, groupWritableColumns, payload, []string{"name"})
	if err != nil {
		return nil, err
	}
	if ids := urParseInt64Slice(payload["copy_accounts_from_group_ids"]); len(ids) > 0 {
		if err := copyGroupAccountsWith(ctx, tx, ownerID, id, ids); err != nil {
			return nil, err
		}
	}
	if err := enqueueGroupChangesWith(ctx, tx, []int64{id}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateGroupAuthCache(ctx, id)
	return s.GetGroup(ctx, ownerID, id)
}

func (s *UserResourceService) UpdateGroup(ctx context.Context, ownerID, groupID int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	existing, err := s.GetGroup(ctx, ownerID, groupID)
	if err != nil {
		return nil, err
	}
	payload = clonePayload(payload)
	if err := s.normalizeAndValidateGroupPayload(ctx, ownerID, groupID, existing, payload); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.updateOwnedWith(ctx, tx, "groups", ownerID, groupID, groupWritableColumns, payload); err != nil {
		return nil, err
	}
	if _, ok := payload["copy_accounts_from_group_ids"]; ok {
		ids := urParseInt64Slice(payload["copy_accounts_from_group_ids"])
		if err := replaceGroupAccountsFromGroupsWith(ctx, tx, ownerID, groupID, ids); err != nil {
			return nil, err
		}
	}
	if err := enqueueGroupChangesWith(ctx, tx, []int64{groupID}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.invalidateGroupAuthCache(ctx, groupID)
	return s.GetGroup(ctx, ownerID, groupID)
}

func (s *UserResourceService) DeleteGroup(ctx context.Context, ownerID, groupID int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
UPDATE groups SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, groupID, ownerID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrUserResourceNotFound
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_groups WHERE group_id = $1", groupID); err != nil {
		return fmt.Errorf("delete group account bindings: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
UPDATE user_subscriptions
SET deleted_at = NOW(), revoked_by_user_id = $2, updated_at = NOW()
WHERE group_id = $1 AND deleted_at IS NULL
RETURNING user_id`, groupID, ownerID)
	if err != nil {
		return fmt.Errorf("revoke group subscriptions: %w", err)
	}
	affectedUsers := map[int64]struct{}{}
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan revoked subscription user: %w", err)
		}
		affectedUsers[userID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate revoked subscription users: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close revoked subscription users: %w", err)
	}
	if err := enqueueGroupChangesWith(ctx, tx, []int64{groupID}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	s.invalidateGroupAuthCache(ctx, groupID)
	for userID := range affectedUsers {
		if err := s.invalidateSubscription(ctx, userID, groupID); err != nil {
			return err
		}
	}
	return nil
}

func (s *UserResourceService) ListAccounts(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{}
	where := ownedWhere("a", ownerID, &args)
	if opts.Platform != "" {
		where = append(where, "a.platform = "+nextArg(&args, opts.Platform))
	}
	if opts.Type != "" {
		where = append(where, "a.type = "+nextArg(&args, opts.Type))
	}
	if opts.Status != "" {
		where = append(where, "a.status = "+nextArg(&args, opts.Status))
	}
	if opts.GroupID > 0 {
		where = append(where, "EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = "+nextArg(&args, opts.GroupID)+")")
	}
	if opts.Search != "" {
		where = append(where, "(a.name ILIKE "+nextArg(&args, "%"+opts.Search+"%")+" OR COALESCE(a.notes, '') ILIKE "+nextArg(&args, "%"+opts.Search+"%")+")")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts a WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := resourceOrder(opts.SortBy, opts.SortOrder, map[string]string{
		"name": "a.name", "platform": "a.platform", "type": "a.type", "status": "a.status",
		"priority": "a.priority", "last_used_at": "a.last_used_at", "created_at": "a.created_at",
	}, "a.priority ASC, a.id DESC")
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, accountSelectSQL("a", true)+`
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
WHERE `+whereSQL+`
ORDER BY `+order+`
LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachAccountGroups(ctx, items); err != nil {
		return nil, err
	}
	return paged(items, total, page, pageSize), nil
}

func (s *UserResourceService) GetAccount(ctx context.Context, ownerID, accountID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, accountSelectSQL("a", true)+`
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
WHERE a.id = $1 AND a.owner_user_id = $2 AND a.deleted_at IS NULL
LIMIT 1`, accountID, ownerID)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUserResourceNotFound
	}
	if err := s.attachAccountGroups(ctx, items); err != nil {
		return nil, err
	}
	return items[0], nil
}

// SyncAccountUpstreamModels loads a complete account through the shared account
// repository only after proving that the authenticated user owns it.
func (s *UserResourceService) SyncAccountUpstreamModels(ctx context.Context, ownerID, accountID int64) ([]string, error) {
	if err := s.ensureOwned(ctx, "accounts", ownerID, accountID); err != nil {
		return nil, err
	}
	if s.accountTestService == nil || s.accountTestService.accountRepo == nil {
		return nil, infraerrors.ServiceUnavailable("USER_MODEL_SYNC_UNAVAILABLE", "upstream model sync is unavailable")
	}

	account, err := s.accountTestService.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		if errors.Is(err, ErrAccountNotFound) {
			return nil, ErrUserResourceNotFound
		}
		return nil, err
	}
	if account == nil || account.OwnerUserID == nil || *account.OwnerUserID != ownerID {
		return nil, ErrUserResourceNotFound
	}

	return s.accountTestService.FetchUpstreamSupportedModels(ctx, account)
}

// SyncAccountUpstreamModelsPreview probes temporary API-key credentials without
// persisting them. The owner marker enables socket-level public-network policy.
func (s *UserResourceService) SyncAccountUpstreamModelsPreview(ctx context.Context, ownerID int64, input UserUpstreamModelsPreviewInput) ([]string, error) {
	if ownerID <= 0 {
		return nil, ErrUserResourceForbidden
	}
	if s.accountTestService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_MODEL_SYNC_UNAVAILABLE", "upstream model sync is unavailable")
	}

	platform := strings.ToLower(strings.TrimSpace(input.Platform))
	switch platform {
	case PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek:
	default:
		return nil, infraerrors.BadRequest("USER_MODEL_SYNC_PLATFORM_INVALID", "platform does not support upstream model sync")
	}

	accountType := normalizeUserAccountType(input.Type)
	if accountType != AccountTypeAPIKey {
		return nil, infraerrors.BadRequest("USER_MODEL_SYNC_TYPE_INVALID", "temporary model sync only supports API key accounts")
	}

	apiKey := strings.TrimSpace(input.APIKey)
	if apiKey == "" || len(apiKey) > 64*1024 {
		return nil, infraerrors.BadRequest("USER_MODEL_SYNC_API_KEY_INVALID", "api_key is required and must not exceed 64 KiB")
	}
	baseURL := strings.TrimSpace(input.BaseURL)
	if len(baseURL) > 2048 {
		return nil, infraerrors.BadRequest("USER_MODEL_SYNC_BASE_URL_INVALID", "base_url must not exceed 2048 bytes")
	}

	credentials := map[string]any{"api_key": apiKey}
	if IsCNProvider(platform) {
		accountMode := strings.ToLower(strings.TrimSpace(input.AccountMode))
		if accountMode == "" {
			accountMode = AccountModePayG
		}
		if err := validateAllowedValue("account_mode", accountMode, AccountModePayG, AccountModeCoding); err != nil {
			return nil, err
		}
		if platform == PlatformDeepseek && accountMode == AccountModeCoding {
			return nil, infraerrors.BadRequest("USER_MODEL_SYNC_ACCOUNT_MODE_INVALID", "DeepSeek does not support coding account mode")
		}

		apiProtocol := strings.ToLower(strings.TrimSpace(input.APIProtocol))
		if apiProtocol == "" {
			apiProtocol = APIProtocolChatCompletions
		}
		if err := validateAllowedValue("api_protocol", apiProtocol, APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses); err != nil {
			return nil, err
		}
		if apiProtocol == APIProtocolResponses && platform != PlatformDeepseek {
			return nil, infraerrors.BadRequest("USER_MODEL_SYNC_API_PROTOCOL_INVALID", "responses protocol is only supported by DeepSeek")
		}
		credentials["account_mode"] = accountMode
		credentials["api_protocol"] = apiProtocol
	}
	if baseURL != "" {
		credentials["base_url"] = baseURL
		if err := validateUserOwnedAccountURLs(ctx, map[string]any{"credentials": credentials}); err != nil {
			return nil, err
		}
	}

	owner := ownerID
	temporaryAccount := &Account{
		OwnerUserID: &owner,
		Platform:    platform,
		Type:        accountType,
		Credentials: credentials,
		Extra:       map[string]any{},
		Concurrency: 1,
	}
	return s.accountTestService.FetchUpstreamSupportedModels(ctx, temporaryAccount)
}

func (s *UserResourceService) GetGeminiOAuthCapabilities() (*GeminiOAuthCapabilities, error) {
	if s == nil || s.geminiOAuthService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_GEMINI_CAPABILITIES_UNAVAILABLE", "Gemini OAuth capabilities are unavailable")
	}
	capabilities := s.geminiOAuthService.GetOAuthConfig()
	if capabilities == nil {
		return nil, infraerrors.ServiceUnavailable("USER_GEMINI_CAPABILITIES_UNAVAILABLE", "Gemini OAuth capabilities are unavailable")
	}
	return &GeminiOAuthCapabilities{
		AIStudioOAuthEnabled: capabilities.AIStudioOAuthEnabled,
		RequiredRedirectURIs: append([]string(nil), capabilities.RequiredRedirectURIs...),
	}, nil
}

func (s *UserResourceService) GetAntigravityDefaultModelMapping() map[string]string {
	mapping := make(map[string]string, len(domain.DefaultAntigravityModelMapping))
	for from, to := range domain.DefaultAntigravityModelMapping {
		mapping[from] = to
	}
	return mapping
}

func (s *UserResourceService) CreateAccount(ctx context.Context, ownerID int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	payload = clonePayload(payload)
	defaultPayload(payload, map[string]any{
		"concurrency":           3,
		"priority":              50,
		"rate_multiplier":       1.0,
		"status":                StatusActive,
		"schedulable":           true,
		"auto_pause_on_expired": true,
		"credentials":           map[string]any{},
		"extra":                 map[string]any{},
	})
	prepareUserResourceCodexExtraForCreate(payload)
	if err := s.normalizeAndValidateAccountPayload(ctx, ownerID, nil, payload); err != nil {
		return nil, err
	}
	if err := s.ensureOwnedResourceCapacity(ctx, "accounts", ownerID, userResourceMaxAccounts); err != nil {
		return nil, err
	}
	groupIDs := urParseInt64Slice(payload["group_ids"])
	delete(payload, "group_ids")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	id, err := s.insertOwnedWith(ctx, tx, "accounts", ownerID, accountWritableColumns, payload, []string{"name", "platform", "type"})
	if err != nil {
		return nil, err
	}
	if err := replaceAccountGroupsWith(ctx, tx, id, groupIDs); err != nil {
		return nil, err
	}
	groupIDs = uniquePositiveInt64s(groupIDs)
	if err := enqueueAccountChangeWith(ctx, tx, id, groupIDs); err != nil {
		return nil, err
	}
	if err := enqueueGroupChangesWith(ctx, tx, groupIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, groupID := range groupIDs {
		s.invalidateGroupAuthCache(ctx, groupID)
	}
	item, err := s.GetAccount(ctx, ownerID, id)
	if err == nil {
		s.scheduleOpenAIResponsesProbe(item)
	}
	return item, err
}

func (s *UserResourceService) UpdateAccount(ctx context.Context, ownerID, accountID int64, payload map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	existing, err := s.GetAccount(ctx, ownerID, accountID)
	if err != nil {
		return nil, err
	}
	payload = clonePayload(payload)
	_, credentialsChanged := payload["credentials"]
	_, proxyChanged := payload["proxy_id"]
	if incoming, ok := payload["credentials"].(map[string]any); ok {
		existingCredentials, _ := existing["credentials"].(map[string]any)
		payload["credentials"] = MergePreservingSensitiveCreds(existingCredentials, incoming)
	}
	prepareUserResourceCodexExtraForUpdate(existing, payload)
	if err := s.normalizeAndValidateAccountPayload(ctx, ownerID, existing, payload); err != nil {
		return nil, err
	}
	oldGroupIDs := urParseInt64Slice(existing["group_ids"])
	groupIDsRaw, hasGroupIDs := payload["group_ids"]
	delete(payload, "group_ids")
	affectedGroupIDs := append([]int64{}, oldGroupIDs...)
	if hasGroupIDs {
		affectedGroupIDs = append(affectedGroupIDs, urParseInt64Slice(groupIDsRaw)...)
	}
	affectedGroupIDs = uniquePositiveInt64s(affectedGroupIDs)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := s.updateOwnedWith(ctx, tx, "accounts", ownerID, accountID, accountWritableColumns, payload); err != nil {
		return nil, err
	}
	if hasGroupIDs {
		if err := replaceAccountGroupsWith(ctx, tx, accountID, urParseInt64Slice(groupIDsRaw)); err != nil {
			return nil, err
		}
	}
	if err := enqueueAccountChangeWith(ctx, tx, accountID, affectedGroupIDs); err != nil {
		return nil, err
	}
	if err := enqueueGroupChangesWith(ctx, tx, affectedGroupIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, groupID := range affectedGroupIDs {
		s.invalidateGroupAuthCache(ctx, groupID)
	}
	item, err := s.GetAccount(ctx, ownerID, accountID)
	if err == nil && (credentialsChanged || proxyChanged) {
		s.scheduleOpenAIResponsesProbe(item)
	}
	return item, err
}

func (s *UserResourceService) DeleteAccount(ctx context.Context, ownerID, accountID int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var groupIDs []int64
	rows, err := tx.QueryContext(ctx, "SELECT group_id FROM account_groups WHERE account_id = $1", accountID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		groupIDs = append(groupIDs, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `
UPDATE accounts SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, accountID, ownerID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrUserResourceNotFound
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", accountID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM scheduled_test_plans WHERE account_id = $1", accountID); err != nil {
		return err
	}
	groupIDs = uniquePositiveInt64s(groupIDs)
	if err := enqueueAccountChangeWith(ctx, tx, accountID, groupIDs); err != nil {
		return err
	}
	if err := enqueueGroupChangesWith(ctx, tx, groupIDs); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, gid := range groupIDs {
		s.invalidateGroupAuthCache(ctx, gid)
	}
	return nil
}

func (s *UserResourceService) ClearAccountError(ctx context.Context, ownerID, accountID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, "UPDATE accounts SET error_message = NULL, status = 'active', schedulable = true, updated_at = NOW() WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL", accountID, ownerID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceNotFound
	}
	if err := enqueueAccountChangeWith(ctx, tx, accountID, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, ownerID, accountID)
}

func (s *UserResourceService) SetAccountSchedulable(ctx context.Context, ownerID, accountID int64, schedulable bool) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, "UPDATE accounts SET schedulable = $1, updated_at = NOW() WHERE id = $2 AND owner_user_id = $3 AND deleted_at IS NULL", schedulable, accountID, ownerID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceNotFound
	}
	if err := enqueueAccountChangeWith(ctx, tx, accountID, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetAccount(ctx, ownerID, accountID)
}

func (s *UserResourceService) ExportAccounts(ctx context.Context, ownerID int64, ids []int64, includeProxies bool) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ids = uniquePositiveInt64s(ids)
	args := []any{ownerID}
	where := []string{"a.owner_user_id = $1", "a.deleted_at IS NULL"}
	if len(ids) > 0 {
		where = append(where, "a.id = ANY("+nextArg(&args, pq.Array(ids))+")")
	}
	rows, err := s.db.QueryContext(ctx, accountSelectSQL("a", true)+`
FROM accounts a
LEFT JOIN proxies p ON p.id = a.proxy_id AND p.deleted_at IS NULL
WHERE `+strings.Join(where, " AND ")+`
ORDER BY a.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	accounts, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if err := s.attachAccountGroups(ctx, accounts); err != nil {
		return nil, err
	}

	proxies := []map[string]any{}
	if includeProxies {
		proxyIDs := []int64{}
		for _, account := range accounts {
			if id := urToInt64(account["proxy_id"]); id > 0 {
				proxyIDs = append(proxyIDs, id)
			}
		}
		proxyIDs = uniquePositiveInt64s(proxyIDs)
		if len(proxyIDs) > 0 {
			proxyRows, err := s.db.QueryContext(ctx, `
SELECT id, owner_user_id, is_public, kind, name, protocol, host, port, username, password,
       status, expires_at, fallback_mode, backup_proxy_id, expiry_warn_days,
       COALESCE(extra, '{}'::jsonb)::text AS extra, created_at, updated_at
FROM proxies
WHERE owner_user_id = $1 AND id = ANY($2) AND deleted_at IS NULL
ORDER BY id ASC`, ownerID, pq.Array(proxyIDs))
			if err != nil {
				return nil, err
			}
			proxies, err = scanRowsToMaps(proxyRows)
			if err != nil {
				return nil, err
			}
		}
	}

	return map[string]any{
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    accounts,
		"proxies":     proxies,
	}, nil
}

func (s *UserResourceService) ImportAccounts(ctx context.Context, ownerID int64, payload map[string]any) (*UserAccountImportResult, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	result := &UserAccountImportResult{Created: []map[string]any{}, Errors: []string{}}
	proxyPayloads := mapSliceFromAny(payload["proxies"])
	accounts := mapSliceFromAny(payload["accounts"])
	if len(accounts) == 0 {
		accounts = mapSliceFromAny(payload["items"])
	}
	if len(proxyPayloads) > userResourceBatchMaxItems || len(accounts) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "imports cannot exceed 1000 items per resource type")
	}
	proxyIDMap := map[int64]int64{}
	createdProxyIDs := make([]int64, 0, len(proxyPayloads))
	for i, proxyPayload := range proxyPayloads {
		oldID := urToInt64(proxyPayload["id"])
		clean := sanitizeImportPayload(proxyPayload)
		clean["is_public"] = toBool(proxyPayload["is_public"])
		created, err := s.CreateProxy(ctx, ownerID, clean)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("proxy %d: %v", i+1, err))
			result.FailedCount++
			continue
		}
		if oldID > 0 {
			proxyIDMap[oldID] = urToInt64(created["id"])
		}
		createdProxyIDs = append(createdProxyIDs, urToInt64(created["id"]))
	}
	s.enqueueImportedProxyQualityChecks(ownerID, createdProxyIDs)

	if len(accounts) == 0 {
		return result, nil
	}
	for i, accountPayload := range accounts {
		clean := sanitizeImportPayload(accountPayload)
		if oldProxyID := urToInt64(clean["proxy_id"]); oldProxyID > 0 {
			if newProxyID := proxyIDMap[oldProxyID]; newProxyID > 0 {
				clean["proxy_id"] = newProxyID
			}
		}
		created, err := s.CreateAccount(ctx, ownerID, clean)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("account %d: %v", i+1, err))
			result.FailedCount++
			continue
		}
		result.Created = append(result.Created, created)
		result.CreatedCount++
	}
	return result, nil
}

func prepareUserResourceCodexExtraForCreate(payload map[string]any) {
	if payload == nil {
		return
	}
	extra, _ := payload["extra"].(map[string]any)
	payload["extra"] = prepareCodexFingerprintExtraForCreate(
		strings.ToLower(strings.TrimSpace(urAsString(payload["platform"]))),
		normalizeUserAccountType(urAsString(payload["type"])),
		extra,
	)
}

func prepareUserResourceCodexExtraForUpdate(existing, payload map[string]any) {
	if payload == nil {
		return
	}
	desiredPlatform := strings.ToLower(strings.TrimSpace(urAsString(existing["platform"])))
	if value, ok := payload["platform"]; ok {
		desiredPlatform = strings.ToLower(strings.TrimSpace(urAsString(value)))
	}
	desiredType := normalizeUserAccountType(urAsString(existing["type"]))
	if value, ok := payload["type"]; ok {
		desiredType = normalizeUserAccountType(urAsString(value))
	}
	existingExtra, _ := existing["extra"].(map[string]any)
	desiredExtra := existingExtra
	if value, ok := payload["extra"].(map[string]any); ok {
		desiredExtra = value
	}
	payload["extra"] = prepareCodexFingerprintExtraForUpdate(&Account{
		Platform: desiredPlatform,
		Type:     desiredType,
		Extra:    existingExtra,
	}, desiredExtra)
}

func (s *UserResourceService) BatchUpdateAccounts(ctx context.Context, ownerID int64, ids []int64, fields map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ids = uniquePositiveInt64s(ids)
	if len(ids) == 0 {
		return nil, infraerrors.BadRequest("ACCOUNT_IDS_REQUIRED", "ids are required")
	}
	if len(ids) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "batch updates cannot exceed 1000 accounts")
	}
	if err := s.validateOwnedAccountIDs(ctx, ownerID, ids); err != nil {
		return nil, err
	}
	fields = clonePayload(fields)
	_, credentialsChanged := fields["credentials"]
	_, proxyChanged := fields["proxy_id"]
	delete(fields, "id")
	delete(fields, "owner_user_id")
	type accountBatchUpdate struct {
		id          int64
		fields      map[string]any
		oldGroupIDs []int64
		groupIDs    []int64
		hasGroupIDs bool
	}
	updates := make([]accountBatchUpdate, 0, len(ids))
	for _, id := range ids {
		existing, err := s.GetAccount(ctx, ownerID, id)
		if err != nil {
			return nil, err
		}
		normalized := clonePayload(fields)
		if incoming, ok := normalized["credentials"].(map[string]any); ok {
			existingCredentials, _ := existing["credentials"].(map[string]any)
			normalized["credentials"] = MergePreservingSensitiveCreds(existingCredentials, incoming)
		}
		prepareUserResourceCodexExtraForUpdate(existing, normalized)
		if err := s.normalizeAndValidateAccountPayload(ctx, ownerID, existing, normalized); err != nil {
			return nil, err
		}
		groupIDsRaw, hasGroupIDs := normalized["group_ids"]
		delete(normalized, "group_ids")
		updates = append(updates, accountBatchUpdate{
			id:          id,
			fields:      normalized,
			oldGroupIDs: urParseInt64Slice(existing["group_ids"]),
			groupIDs:    urParseInt64Slice(groupIDsRaw),
			hasGroupIDs: hasGroupIDs,
		})
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	for _, update := range updates {
		if err := s.updateOwnedWith(ctx, tx, "accounts", ownerID, update.id, accountWritableColumns, update.fields); err != nil {
			return nil, err
		}
		if update.hasGroupIDs {
			if err := replaceAccountGroupsWith(ctx, tx, update.id, update.groupIDs); err != nil {
				return nil, err
			}
		}
	}
	affectedGroupIDs := make([]int64, 0)
	for _, update := range updates {
		affectedGroupIDs = append(affectedGroupIDs, update.oldGroupIDs...)
		if update.hasGroupIDs {
			affectedGroupIDs = append(affectedGroupIDs, update.groupIDs...)
		}
	}
	affectedGroupIDs = uniquePositiveInt64s(affectedGroupIDs)
	if err := enqueueAccountBulkChangeWith(ctx, tx, ids, affectedGroupIDs); err != nil {
		return nil, err
	}
	if err := enqueueGroupChangesWith(ctx, tx, affectedGroupIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	for _, groupID := range affectedGroupIDs {
		s.invalidateGroupAuthCache(ctx, groupID)
	}
	for _, update := range updates {
		if credentialsChanged || proxyChanged {
			if item, err := s.GetAccount(ctx, ownerID, update.id); err == nil {
				s.scheduleOpenAIResponsesProbe(item)
			}
		}
	}
	return map[string]any{"updated": len(updates)}, nil
}

func (s *UserResourceService) TestAccount(ctx context.Context, ownerID, accountID int64, modelID string) (map[string]any, error) {
	account, err := s.GetAccount(ctx, ownerID, accountID)
	if err != nil {
		return nil, err
	}
	if s.accountTestService != nil {
		result, err := s.accountTestService.RunTestBackground(ctx, accountID, strings.TrimSpace(modelID))
		if err != nil {
			return nil, err
		}
		if result == nil {
			return map[string]any{
				"success":    false,
				"status":     "failed",
				"message":    "account test did not return a result",
				"account_id": accountID,
				"platform":   account["platform"],
				"type":       account["type"],
			}, nil
		}
		return map[string]any{
			"success":       result.Status == "success",
			"status":        result.Status,
			"message":       testAccountResultMessage(result),
			"account_id":    accountID,
			"platform":      account["platform"],
			"type":          account["type"],
			"latency_ms":    result.LatencyMs,
			"response_text": result.ResponseText,
			"error_message": logredact.RedactText(result.ErrorMessage),
			"started_at":    result.StartedAt,
			"finished_at":   result.FinishedAt,
		}, nil
	}
	credentials, _ := account["credentials"].(map[string]any)
	if len(credentials) == 0 {
		return map[string]any{"success": false, "message": "account has no credentials"}, nil
	}
	if proxyID := urToInt64(account["proxy_id"]); proxyID > 0 {
		proxyResult, err := s.TestProxy(ctx, ownerID, proxyID)
		if err != nil {
			return nil, err
		}
		if !toBool(proxyResult["success"]) {
			return map[string]any{"success": false, "message": "proxy check failed", "proxy": proxyResult}, nil
		}
	}
	return map[string]any{
		"success":    true,
		"message":    "account configuration is valid",
		"account_id": accountID,
		"platform":   account["platform"],
		"type":       account["type"],
	}, nil
}

func (s *UserResourceService) RefreshAccount(ctx context.Context, ownerID, accountID int64) (map[string]any, error) {
	if err := s.ensureOwned(ctx, "accounts", ownerID, accountID); err != nil {
		return nil, err
	}
	if s.tokenRefreshService != nil {
		if _, err := s.tokenRefreshService.RefreshAccountNow(ctx, accountID); err != nil {
			if infraerrors.Reason(err) != "ACCOUNT_NOT_REFRESHABLE" {
				return nil, err
			}
		} else {
			item, err := s.GetAccount(ctx, ownerID, accountID)
			if err != nil {
				return nil, err
			}
			item["refresh_status"] = "refreshed"
			item["refresh_message"] = "account credentials refreshed"
			return item, nil
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
UPDATE accounts
SET error_message = NULL,
    status = CASE WHEN status = 'error' THEN 'active' ELSE status END,
    schedulable = true,
    rate_limit_reset_at = NULL,
    temp_unschedulable_until = NULL,
    temp_unschedulable_reason = '',
    updated_at = NOW()
WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, accountID, ownerID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceNotFound
	}
	if err := enqueueAccountChangeWith(ctx, tx, accountID, nil); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	item, err := s.GetAccount(ctx, ownerID, accountID)
	if err != nil {
		return nil, err
	}
	item["refresh_status"] = "cleared"
	item["refresh_message"] = "account scheduling state refreshed; OAuth token refresh requires platform-specific credentials"
	return item, nil
}

func testAccountResultMessage(result *ScheduledTestResult) string {
	if result == nil {
		return "account test did not return a result"
	}
	if result.Status == "success" {
		return "account test succeeded"
	}
	if result.ErrorMessage != "" {
		return logredact.RedactText(result.ErrorMessage)
	}
	return "account test failed"
}

func (s *UserResourceService) ListProxies(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{ownerID}
	scope := "(p.owner_user_id = $1 OR p.is_public = true)"
	if opts.OwnedOnly {
		scope = "p.owner_user_id = $1"
	}
	where := []string{"p.deleted_at IS NULL", scope}
	if opts.Protocol != "" {
		where = append(where, "p.protocol = "+nextArg(&args, opts.Protocol))
	}
	if opts.Status != "" {
		where = append(where, "p.status = "+nextArg(&args, opts.Status))
	}
	if opts.Type != "" {
		where = append(where, "p.kind = "+nextArg(&args, opts.Type))
	}
	if opts.SourceID > 0 {
		// Narrows the existing owner/public scope to one subscription source; it can
		// never widen visibility because the scope predicate above still applies.
		where = append(where, "p.extra->>'source_id' = "+nextArg(&args, strconv.FormatInt(opts.SourceID, 10)))
	}
	if opts.Search != "" {
		where = append(where, "(p.name ILIKE "+nextArg(&args, "%"+opts.Search+"%")+" OR (p.owner_user_id = $1 AND p.host ILIKE "+nextArg(&args, "%"+opts.Search+"%")+"))")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxies p WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := resourceOrder(opts.SortBy, opts.SortOrder, map[string]string{
		"name": "p.name", "protocol": "p.protocol", "status": "p.status", "created_at": "p.created_at", "expires_at": "p.expires_at",
	}, "p.id DESC")
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT
  p.id, p.owner_user_id, p.is_public, p.kind, p.name, p.protocol, p.host, p.port,
  p.username, p.password, (COALESCE(p.username, '') <> '' OR COALESCE(p.password, '') <> '') AS has_auth,
  p.status, p.expires_at, p.fallback_mode, p.backup_proxy_id, p.expiry_warn_days,
  COALESCE(p.extra, '{}'::jsonb)::text AS extra, p.created_at, p.updated_at,
  (SELECT COUNT(*) FROM accounts a WHERE a.proxy_id = p.id AND a.deleted_at IS NULL)::bigint AS account_count
FROM proxies p
WHERE `+whereSQL+`
ORDER BY `+order+`
LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	s.attachProxyObservability(ctx, items)
	for _, item := range items {
		redactPublicProxy(item, ownerID)
	}
	return paged(items, total, page, pageSize), nil
}

func (s *UserResourceService) GetProxy(ctx context.Context, ownerID, proxyID int64) (map[string]any, error) {
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
  AND p.deleted_at IS NULL
  AND (p.owner_user_id = $2 OR p.is_public = true)
LIMIT 1`, proxyID, ownerID)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUserResourceNotFound
	}
	item := items[0]
	s.attachProxyObservability(ctx, items)
	redactPublicProxy(item, ownerID)
	return item, nil
}

func (s *UserResourceService) getProxyForExactOwner(ctx context.Context, ownerID *int64, proxyID int64) (map[string]any, error) {
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
		return nil, ErrUserResourceNotFound
	}
	s.attachProxyObservability(ctx, items)
	return items[0], nil
}

func (s *UserResourceService) CreateProxy(ctx context.Context, ownerID int64, payload map[string]any) (map[string]any, error) {
	return s.createProxy(ctx, ownerID, payload, false)
}

func (s *UserResourceService) createProxy(ctx context.Context, ownerID int64, payload map[string]any, preserveSourceMetadata bool) (map[string]any, error) {
	return s.createProxyForOwner(ctx, userResourceOwner(ownerID), payload, preserveSourceMetadata)
}

func (s *UserResourceService) createProxyForOwner(ctx context.Context, ownerID *int64, payload map[string]any, preserveSourceMetadata bool) (map[string]any, error) {
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

func (s *UserResourceService) UpdateProxy(ctx context.Context, ownerID, proxyID int64, payload map[string]any) (map[string]any, error) {
	return s.updateProxy(ctx, ownerID, proxyID, payload, false)
}

func (s *UserResourceService) updateProxy(ctx context.Context, ownerID, proxyID int64, payload map[string]any, preserveSourceMetadata bool) (map[string]any, error) {
	existing, err := s.GetProxy(ctx, ownerID, proxyID)
	if err != nil {
		return nil, err
	}
	if urToInt64(existing["owner_user_id"]) != ownerID {
		return nil, ErrUserResourceNotFound
	}
	payload = clonePayload(payload)
	if !preserveSourceMetadata {
		stripProxySourceMetadata(payload)
	}
	if err := s.normalizeAndValidateProxyPayload(ctx, ownerID, proxyID, existing, payload); err != nil {
		return nil, err
	}
	if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
		return nil, fmt.Errorf("stop previous proxy runtime: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.updateOwnedWith(ctx, tx, "proxies", ownerID, proxyID, proxyWritableColumns, payload); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb) - 'upstream_billing_probe', updated_at = NOW()
WHERE deleted_at IS NULL
  AND (
    proxy_id = $1
    OR proxy_id IN (
      SELECT id FROM proxies WHERE backup_proxy_id = $1 AND deleted_at IS NULL
    )
  )
RETURNING id`, proxyID)
	if err != nil {
		return nil, err
	}
	accountIDs := make([]int64, 0)
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if len(accountIDs) > 0 {
		payloadJSON, err := json.Marshal(map[string]any{"account_ids": accountIDs})
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, created_at)
VALUES ($1, NULL, NULL, $2::jsonb, NOW())`, SchedulerOutboxEventAccountBulkChanged, payloadJSON); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetProxy(ctx, ownerID, proxyID)
}

func (s *UserResourceService) DeleteProxy(ctx context.Context, ownerID, proxyID int64) error {
	if err := s.ensureOwned(ctx, "proxies", ownerID, proxyID); err != nil {
		return err
	}
	var count int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE proxy_id = $1 AND deleted_at IS NULL", proxyID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return infraerrors.Conflict("PROXY_IN_USE", "proxy is used by accounts")
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proxies WHERE backup_proxy_id = $1 AND deleted_at IS NULL", proxyID).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return infraerrors.Conflict("PROXY_IN_USE", "proxy is used as a fallback by other proxies")
	}
	if err := stopProxyRuntimesWithRetry(proxyID); err != nil {
		return fmt.Errorf("stop proxy runtime before delete: %w", err)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE proxies SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL", proxyID, ownerID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrUserResourceNotFound
	}
	return nil
}

func (s *UserResourceService) attachProxyObservability(ctx context.Context, items []map[string]any) {
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

func (s *UserResourceService) saveProxyObservation(ctx context.Context, proxyID int64, info *ProxyLatencyInfo) {
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

func (s *UserResourceService) TestProxy(ctx context.Context, ownerID, proxyID int64) (map[string]any, error) {
	proxyItem, err := s.getSelectableProxyRaw(ctx, ownerID, proxyID)
	if err != nil {
		return nil, err
	}
	hideDetails := urToInt64(proxyItem["owner_user_id"]) != ownerID
	proxyURL, cleanup, resolveErr := resolveProxyProbeURL(ctx, s.proxyProbeResolver, proxyFromResourceMap(proxyItem))
	if resolveErr != nil {
		message := logredact.RedactText(resolveErr.Error())
		s.saveProxyObservation(ctx, proxyID, &ProxyLatencyInfo{
			Success:   false,
			Message:   message,
			UpdatedAt: time.Now(),
		})
		if hideDetails {
			message = "public proxy test failed"
		}
		return map[string]any{"success": false, "message": message}, nil
	}
	defer cleanup()
	if s.proxyProber != nil {
		exitInfo, latency, probeErr := s.proxyProber.ProbeProxy(ctx, proxyURL)
		if probeErr != nil {
			message := logredact.RedactText(probeErr.Error())
			s.saveProxyObservation(ctx, proxyID, &ProxyLatencyInfo{
				Success:   false,
				Message:   message,
				UpdatedAt: time.Now(),
			})
			if hideDetails {
				message = "public proxy test failed"
			}
			return map[string]any{"success": false, "message": message}, nil
		}
		latencyValue := latency
		info := &ProxyLatencyInfo{
			Success:   true,
			LatencyMs: &latencyValue,
			Message:   "proxy is accessible",
			UpdatedAt: time.Now(),
		}
		result := map[string]any{
			"success":    true,
			"message":    info.Message,
			"latency_ms": latency,
		}
		if exitInfo != nil {
			info.IPAddress = exitInfo.IP
			info.Country = exitInfo.Country
			info.CountryCode = exitInfo.CountryCode
			info.Region = exitInfo.Region
			info.City = exitInfo.City
			if !hideDetails {
				result["ip_address"] = exitInfo.IP
			}
			result["country"] = exitInfo.Country
			result["country_code"] = exitInfo.CountryCode
			result["region"] = exitInfo.Region
			result["city"] = exitInfo.City
		}
		s.saveProxyObservation(ctx, proxyID, info)
		return result, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil || parsed.Hostname() == "" || portFromURL(parsed) <= 0 {
		return map[string]any{"success": false, "message": "proxy endpoint is not reachable"}, nil
	}
	host := parsed.Hostname()
	port := portFromURL(parsed)
	start := time.Now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	latency := time.Since(start).Milliseconds()
	if conn != nil {
		_ = conn.Close()
	}
	if err != nil {
		message := logredact.RedactText(err.Error())
		s.saveProxyObservation(ctx, proxyID, &ProxyLatencyInfo{Success: false, Message: message, UpdatedAt: time.Now()})
		return map[string]any{"success": false, "message": message, "latency_ms": latency}, nil
	}
	latencyValue := latency
	s.saveProxyObservation(ctx, proxyID, &ProxyLatencyInfo{Success: true, LatencyMs: &latencyValue, Message: "proxy endpoint reachable", UpdatedAt: time.Now()})
	return map[string]any{"success": true, "message": "proxy endpoint reachable", "latency_ms": latency}, nil
}

func (s *UserResourceService) ImportProxyNodes(ctx context.Context, ownerID int64, namePrefix, raw string, isPublic bool) (*ProxyImportResult, error) {
	result, err := s.importProxyNodesForOwner(ctx, userResourceOwner(ownerID), namePrefix, raw, isPublic)
	if err == nil {
		s.enqueueImportedProxyQualityChecks(ownerID, proxyIDsFromResourceItems(result.Created))
	}
	return result, err
}

func (s *UserResourceService) ImportSystemProxyNodes(ctx context.Context, namePrefix, raw string, isPublic bool) (*ProxyImportResult, error) {
	result, err := s.importProxyNodesForOwner(ctx, nil, namePrefix, raw, isPublic)
	if err == nil {
		s.enqueueSystemProxyQualityChecks(proxyIDsFromResourceItems(result.Created))
	}
	return result, err
}

func (s *UserResourceService) importProxyNodesForOwner(ctx context.Context, ownerID *int64, namePrefix, raw string, isPublic bool) (*ProxyImportResult, error) {
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

func (s *UserResourceService) ExportProxies(ctx context.Context, ownerID int64, ids []int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ids = uniquePositiveInt64s(ids)
	args := []any{ownerID}
	where := []string{"owner_user_id = $1", "deleted_at IS NULL"}
	if len(ids) > 0 {
		where = append(where, "id = ANY("+nextArg(&args, pq.Array(ids))+")")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, owner_user_id, is_public, kind, name, protocol, host, port, username, password,
       status, expires_at, fallback_mode, backup_proxy_id, expiry_warn_days,
       COALESCE(extra, '{}'::jsonb)::text AS extra, created_at, updated_at
FROM proxies
WHERE `+strings.Join(where, " AND ")+`
ORDER BY id ASC`, args...)
	if err != nil {
		return nil, err
	}
	proxies, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"proxies":     proxies,
	}, nil
}

func (s *UserResourceService) QualityCheckProxy(ctx context.Context, ownerID, proxyID int64) (*ProxyQualityCheckResult, error) {
	return s.qualityCheckProxyForOwner(ctx, userResourceOwner(ownerID), proxyID)
}

func (s *UserResourceService) qualityCheckProxyForOwner(ctx context.Context, ownerID *int64, proxyID int64) (*ProxyQualityCheckResult, error) {
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

func (s *UserResourceService) saveUserProxyQualitySnapshot(
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

// proxySourceSelectColumns is shared by the list and get queries so the two can
// never drift. `next_sync_at` mirrors the scheduler's due predicate exactly
// (NULL while auto-sync is paused, NOW() when the source has never synced), and
// the node counts are derived from the proxies table instead of a stored counter
// so they cannot go stale.
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

func (s *UserResourceService) ListProxySources(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	return s.listProxySourcesForOwner(ctx, userResourceOwner(ownerID), opts)
}

func (s *UserResourceService) ListSystemProxySources(ctx context.Context, opts UserResourceListOptions) (*UserResourcePage, error) {
	return s.listProxySourcesForOwner(ctx, nil, opts)
}

func (s *UserResourceService) listProxySourcesForOwner(ctx context.Context, ownerID *int64, opts UserResourceListOptions) (*UserResourcePage, error) {
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

func (s *UserResourceService) CreateProxySource(ctx context.Context, ownerID int64, payload map[string]any) (map[string]any, error) {
	return s.createProxySourceForOwner(ctx, userResourceOwner(ownerID), payload)
}

func (s *UserResourceService) CreateSystemProxySource(ctx context.Context, payload map[string]any) (map[string]any, error) {
	return s.createProxySourceForOwner(ctx, nil, payload)
}

func (s *UserResourceService) createProxySourceForOwner(ctx context.Context, ownerID *int64, payload map[string]any) (map[string]any, error) {
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

func (s *UserResourceService) GetProxySource(ctx context.Context, ownerID, sourceID int64) (map[string]any, error) {
	return s.getProxySourceForOwner(ctx, userResourceOwner(ownerID), sourceID)
}

func (s *UserResourceService) getProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) (map[string]any, error) {
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
		return nil, ErrUserResourceNotFound
	}
	return items[0], nil
}

func (s *UserResourceService) UpdateProxySource(ctx context.Context, ownerID, sourceID int64, payload map[string]any) (map[string]any, error) {
	return s.updateProxySourceForOwner(ctx, userResourceOwner(ownerID), sourceID, payload)
}

func (s *UserResourceService) UpdateSystemProxySource(ctx context.Context, sourceID int64, payload map[string]any) (map[string]any, error) {
	return s.updateProxySourceForOwner(ctx, nil, sourceID, payload)
}

func (s *UserResourceService) updateProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64, payload map[string]any) (map[string]any, error) {
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
		return nil, ErrUserResourceNotFound
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

func (s *UserResourceService) DeleteProxySource(ctx context.Context, ownerID, sourceID int64) error {
	return s.deleteProxySourceForOwner(ctx, userResourceOwner(ownerID), sourceID)
}

func (s *UserResourceService) DeleteSystemProxySource(ctx context.Context, sourceID int64) error {
	return s.deleteProxySourceForOwner(ctx, nil, sourceID)
}

func (s *UserResourceService) deleteProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) error {
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
		return ErrUserResourceNotFound
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

func (s *UserResourceService) SyncProxySource(ctx context.Context, ownerID, sourceID int64) (*ProxySourceSyncResult, error) {
	return s.syncProxySourceForOwner(ctx, userResourceOwner(ownerID), sourceID)
}

func (s *UserResourceService) SyncSystemProxySource(ctx context.Context, sourceID int64) (*ProxySourceSyncResult, error) {
	return s.syncProxySourceForOwner(ctx, nil, sourceID)
}

func (s *UserResourceService) syncProxySourceForOwner(ctx context.Context, ownerID *int64, sourceID int64) (*ProxySourceSyncResult, error) {
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

const (
	proxySourceSyncStatusSkipped  = "skipped"
	proxySourceSyncStatusDeferred = "deferred"
	// proxySourceSyncAllBudget bounds one "sync every source" request. A single
	// source may spend up to 30s on its own HTTP fetch, so an owner near the
	// 100-source cap could otherwise hold the request open for most of an hour.
	// Sources not reached inside the budget come back as `deferred`; they are
	// already due, so the background scheduler picks them up unattended.
	proxySourceSyncAllBudget = 100 * time.Second
	// proxySourceSyncAllErrorLimit keeps the aggregate response small: without it
	// 100 sources could each contribute a 64 KiB error blob.
	proxySourceSyncAllErrorLimit = 1000
)

func (s *UserResourceService) SyncAllProxySources(ctx context.Context, ownerID int64) (*ProxySourceSyncAllResult, error) {
	return s.syncAllProxySourcesForOwner(ctx, userResourceOwner(ownerID))
}

func (s *UserResourceService) SyncAllSystemProxySources(ctx context.Context) (*ProxySourceSyncAllResult, error) {
	return s.syncAllProxySourcesForOwner(ctx, nil)
}

func (s *UserResourceService) syncAllProxySourcesForOwner(ctx context.Context, ownerID *int64) (*ProxySourceSyncAllResult, error) {
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

func (s *UserResourceService) listProxySourceSyncTargets(ctx context.Context, ownerID *int64) ([]proxySourceSyncTarget, error) {
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

func (s *UserResourceService) syncProxySourceNodes(ctx context.Context, ownerID *int64, sourceID int64, sourceName, raw string, isPublic bool) (*ProxyImportResult, error) {
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
			return nil, ErrUserResourceNotFound
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
			updatedRuntimeIDs = append(updatedRuntimeIDs, proxyID)
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
		return nil, ErrUserResourceNotFound
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

func (s *UserResourceService) recordProxySourceSyncError(ctx context.Context, ownerID *int64, sourceID int64, syncErr error) error {
	res, err := s.db.ExecContext(ctx, `
UPDATE proxy_sources
SET last_synced_at = NOW(), last_sync_status = 'error', last_sync_error = $1, updated_at = NOW()
WHERE id = $2 AND owner_user_id IS NOT DISTINCT FROM $3 AND deleted_at IS NULL`, safeSyncError(syncErr), sourceID, userResourceOwnerValue(ownerID))
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrUserResourceNotFound
	}
	return nil
}

func (s *UserResourceService) disableMissingProxySourceNodesWith(ctx context.Context, db userResourceDBTX, ownerID *int64, sourceID int64, activeKeys []string) ([]int64, []int64, error) {
	rows, err := db.QueryContext(ctx, `
UPDATE proxies
SET status = 'disabled', updated_at = NOW()
WHERE owner_user_id IS NOT DISTINCT FROM $1 AND deleted_at IS NULL AND status <> 'disabled'
  AND extra->>'source_id' = $2
  AND NOT (extra->>'source_node_key' = ANY($3))
RETURNING id`, userResourceOwnerValue(ownerID), strconv.FormatInt(sourceID, 10), pq.Array(activeKeys))
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

func (s *UserResourceService) ListRedeemCodes(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{ownerID}
	where := []string{"r.owner_user_id = $1"}
	if opts.Status != "" {
		if opts.Status == StatusExpired {
			where = append(where, "(r.status = 'expired' OR (r.status = 'unused' AND r.expires_at IS NOT NULL AND r.expires_at <= NOW()))")
		} else {
			where = append(where, "r.status = "+nextArg(&args, opts.Status))
		}
	}
	if opts.Search != "" {
		where = append(where, "(r.code ILIKE "+nextArg(&args, "%"+opts.Search+"%")+" OR COALESCE(r.notes, '') ILIKE "+nextArg(&args, "%"+opts.Search+"%")+")")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM redeem_codes r WHERE "+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := resourceOrder(opts.SortBy, opts.SortOrder, map[string]string{
		"created_at": "r.created_at", "expires_at": "r.expires_at", "status": "r.status", "code": "r.code",
	}, "r.id DESC")
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.owner_user_id, r.code, r.type, r.value::double precision AS value, r.status,
	       r.used_by, r.used_at, COALESCE(r.notes, '') AS notes, r.created_at, r.expires_at,
	       r.group_id, r.validity_days, r.max_uses, r.used_count, (r.max_uses > 1) AS repeatable,
	       g.name AS group_name, g.platform AS group_platform,
	       u.email AS used_by_email, u.username AS used_by_username
FROM redeem_codes r
LEFT JOIN groups g ON g.id = r.group_id
LEFT JOIN users u ON u.id = r.used_by
WHERE `+whereSQL+`
ORDER BY `+order+`
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

func (s *UserResourceService) ListRedeemCodeUsages(ctx context.Context, ownerID, redeemCodeID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	var total int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM redeem_code_usages rcu
JOIN redeem_codes r ON r.id = rcu.redeem_code_id
WHERE rcu.redeem_code_id = $1 AND r.owner_user_id = $2`, redeemCodeID, ownerID).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT rcu.id, rcu.redeem_code_id, rcu.user_id, u.email AS user_email,
       u.username AS username, rcu.used_at
FROM redeem_code_usages rcu
JOIN redeem_codes r ON r.id = rcu.redeem_code_id
JOIN users u ON u.id = rcu.user_id
WHERE rcu.redeem_code_id = $1 AND r.owner_user_id = $2
ORDER BY rcu.used_at DESC, rcu.id DESC
LIMIT $3 OFFSET $4`, redeemCodeID, ownerID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if total == 0 {
		var exists bool
		if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM redeem_codes WHERE id = $1 AND owner_user_id = $2)", redeemCodeID, ownerID).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, ErrUserResourceNotFound
		}
	}
	return paged(items, total, page, pageSize), nil
}

func (s *UserResourceService) GenerateRedeemCodes(ctx context.Context, ownerID int64, payload map[string]any) ([]map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	groupID := urToInt64(payload["group_id"])
	if groupID <= 0 {
		return nil, infraerrors.BadRequest("REDEEM_GROUP_REQUIRED", "group_id is required")
	}
	if err := s.validateOwnedSubscriptionGroup(ctx, ownerID, groupID); err != nil {
		return nil, err
	}
	count := toInt(payload["count"])
	if count <= 0 {
		count = 1
	}
	if count > 500 {
		return nil, infraerrors.BadRequest("REDEEM_COUNT_TOO_LARGE", "count cannot exceed 500")
	}
	validityDays := toInt(payload["validity_days"])
	if validityDays <= 0 {
		validityDays = 30
	}
	if validityDays > MaxValidityDays {
		return nil, infraerrors.BadRequest("REDEEM_VALIDITY_INVALID", "validity_days exceeds the maximum")
	}
	maxUses := 1
	if toBool(payload["repeatable"]) {
		maxUses = toInt(payload["max_uses"])
		if maxUses < 2 || maxUses > 10000 {
			return nil, infraerrors.BadRequest("REDEEM_MAX_USES_INVALID", "max_uses must be between 2 and 10000 for repeatable codes")
		}
	}
	notes := urAsString(payload["notes"])
	if err := validateUserResourceNotes(notes); err != nil {
		return nil, err
	}
	expiresAt, _ := coerceTime(payload["expires_at"])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	out := make([]map[string]any, 0, count)
	for i := 0; i < count; i++ {
		code, err := GenerateRedeemCode()
		if err != nil {
			return nil, err
		}
		var id int64
		err = tx.QueryRowContext(ctx, `
INSERT INTO redeem_codes (code, owner_user_id, type, value, status, notes, created_at, expires_at, group_id, validity_days, max_uses, used_count)
VALUES ($1, $2, 'subscription', 0, 'unused', $3, NOW(), $4, $5, $6, $7, 0)
RETURNING id`, code, ownerID, notes, expiresAt, groupID, validityDays, maxUses).Scan(&id)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "code": code, "type": RedeemTypeSubscription, "status": StatusUnused, "group_id": groupID, "validity_days": validityDays, "expires_at": expiresAt, "max_uses": maxUses, "used_count": 0, "repeatable": maxUses > 1})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *UserResourceService) DeleteRedeemCode(ctx context.Context, ownerID, id int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM redeem_codes WHERE id = $1 AND owner_user_id = $2 AND status != 'used' AND used_count = 0", id, ownerID)
	if err != nil {
		return err
	}
	if affected(res) == 0 {
		return ErrUserResourceNotFound
	}
	return nil
}

func (s *UserResourceService) ExpireRedeemCode(ctx context.Context, ownerID, id int64) (map[string]any, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE redeem_codes SET status = 'expired' WHERE id = $1 AND owner_user_id = $2 AND status = 'unused'", id, ownerID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceNotFound
	}
	page, err := s.ListRedeemCodes(ctx, ownerID, UserResourceListOptions{Page: 1, PageSize: 1, Search: ""})
	if err != nil {
		return nil, err
	}
	for _, item := range page.Items {
		if urToInt64(item["id"]) == id {
			return item, nil
		}
	}
	return map[string]any{"id": id, "status": StatusExpired}, nil
}

func (s *UserResourceService) RedeemCodeStats(ctx context.Context, ownerID int64) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	var total, unused, used, expired int64
	err := s.db.QueryRowContext(ctx, `
SELECT
  COUNT(*)::bigint,
  COUNT(*) FILTER (WHERE status = 'unused' AND (expires_at IS NULL OR expires_at > NOW()))::bigint,
  COUNT(*) FILTER (WHERE status = 'used')::bigint,
  COUNT(*) FILTER (WHERE status = 'expired' OR (status = 'unused' AND expires_at IS NOT NULL AND expires_at <= NOW()))::bigint
FROM redeem_codes
WHERE owner_user_id = $1`, ownerID).Scan(&total, &unused, &used, &expired)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT type, COUNT(*)::bigint
FROM redeem_codes
WHERE owner_user_id = $1
GROUP BY type`, ownerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	byType := map[string]int64{}
	for rows.Next() {
		var typ string
		var count int64
		if err := rows.Scan(&typ, &count); err != nil {
			return nil, err
		}
		byType[typ] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{
		"total_codes":   total,
		"active_codes":  unused,
		"used_codes":    used,
		"expired_codes": expired,
		"by_type":       byType,
	}, nil
}

func (s *UserResourceService) ExportRedeemCodes(ctx context.Context, ownerID int64, opts UserResourceListOptions, ids []int64) ([]map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ids = uniquePositiveInt64s(ids)
	args := []any{ownerID}
	where := []string{"r.owner_user_id = $1"}
	if len(ids) > 0 {
		where = append(where, "r.id = ANY("+nextArg(&args, pq.Array(ids))+")")
	}
	if opts.Status != "" {
		if opts.Status == StatusExpired {
			where = append(where, "(r.status = 'expired' OR (r.status = 'unused' AND r.expires_at IS NOT NULL AND r.expires_at <= NOW()))")
		} else {
			where = append(where, "r.status = "+nextArg(&args, opts.Status))
		}
	}
	if opts.Type != "" {
		where = append(where, "r.type = "+nextArg(&args, opts.Type))
	}
	if opts.Search != "" {
		p := "%" + opts.Search + "%"
		where = append(where, "(r.code ILIKE "+nextArg(&args, p)+" OR COALESCE(r.notes, '') ILIKE "+nextArg(&args, p)+")")
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT r.id, r.code, r.type, r.status, r.group_id, g.name AS group_name, r.validity_days,
       r.expires_at, r.used_by, u.email AS used_by_email, r.used_at, COALESCE(r.notes, '') AS notes,
       r.created_at
FROM redeem_codes r
LEFT JOIN groups g ON g.id = r.group_id
LEFT JOIN users u ON u.id = r.used_by
WHERE `+strings.Join(where, " AND ")+`
ORDER BY r.id DESC`, args...)
	if err != nil {
		return nil, err
	}
	return scanRowsToMaps(rows)
}

func (s *UserResourceService) BatchUpdateRedeemCodes(ctx context.Context, ownerID int64, ids []int64, fields map[string]any) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	ids = uniquePositiveInt64s(ids)
	if len(ids) == 0 {
		return nil, infraerrors.BadRequest("REDEEM_IDS_REQUIRED", "ids are required")
	}
	if len(ids) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "batch updates cannot exceed 1000 redeem codes")
	}
	fields = clonePayload(fields)
	delete(fields, "type")
	delete(fields, "value")
	delete(fields, "owner_user_id")
	delete(fields, "used_by")
	delete(fields, "used_at")
	if groupID := urToInt64(fields["group_id"]); groupID > 0 {
		if err := s.validateOwnedSubscriptionGroup(ctx, ownerID, groupID); err != nil {
			return nil, err
		}
	}
	assignments := []string{}
	args := []any{}
	for _, key := range urSortedKeys(fields) {
		switch key {
		case "status":
			status := urAsString(fields[key])
			if status != StatusUnused && status != StatusExpired {
				return nil, infraerrors.BadRequest("REDEEM_STATUS_INVALID", "user redeem codes can only be set to unused or expired")
			}
			args = append(args, status)
			assignments = append(assignments, fmt.Sprintf("status = $%d", len(args)))
		case "notes":
			notes := urAsString(fields[key])
			if err := validateUserResourceNotes(notes); err != nil {
				return nil, err
			}
			args = append(args, notes)
			assignments = append(assignments, fmt.Sprintf("notes = $%d", len(args)))
		case "expires_at":
			v, err := coerceTime(fields[key])
			if err != nil {
				return nil, err
			}
			args = append(args, v)
			assignments = append(assignments, fmt.Sprintf("expires_at = $%d", len(args)))
		case "validity_days":
			days := toInt(fields[key])
			if days <= 0 || days > MaxValidityDays {
				return nil, infraerrors.BadRequest("REDEEM_VALIDITY_INVALID", "validity_days is invalid")
			}
			args = append(args, days)
			assignments = append(assignments, fmt.Sprintf("validity_days = $%d", len(args)))
		case "group_id":
			groupID := urToInt64(fields[key])
			if groupID <= 0 {
				return nil, infraerrors.BadRequest("REDEEM_GROUP_REQUIRED", "group_id is required")
			}
			args = append(args, groupID)
			assignments = append(assignments, fmt.Sprintf("group_id = $%d", len(args)))
		}
	}
	if len(assignments) == 0 {
		return map[string]any{"updated": 0}, nil
	}
	args = append(args, ownerID, pq.Array(ids))
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`
UPDATE redeem_codes
SET %s
WHERE owner_user_id = $%d AND id = ANY($%d) AND type = 'subscription' AND status != 'used' AND used_count = 0`,
		strings.Join(assignments, ", "), len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	return map[string]any{"updated": affected(res)}, nil
}

func (s *UserResourceService) ListAssignedSubscriptions(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{ownerID}
	where := []string{"(us.managed_by_user_id = $1 OR g.owner_user_id = $1)"}
	if opts.Status != "" {
		if opts.Status == SubscriptionStatusRevoked {
			where = append(where, "us.deleted_at IS NOT NULL")
		} else {
			where = append(where, "us.deleted_at IS NULL", "us.status = "+nextArg(&args, opts.Status))
		}
	} else {
		where = append(where, "us.deleted_at IS NULL")
	}
	if opts.GroupID > 0 {
		where = append(where, "us.group_id = "+nextArg(&args, opts.GroupID))
	}
	if opts.Platform != "" {
		where = append(where, "g.platform = "+nextArg(&args, opts.Platform))
	}
	if opts.Search != "" {
		p := "%" + opts.Search + "%"
		where = append(where, "(u.email ILIKE "+nextArg(&args, p)+" OR u.username ILIKE "+nextArg(&args, p)+" OR COALESCE(us.notes, '') ILIKE "+nextArg(&args, p)+")")
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
JOIN users u ON u.id = us.user_id
WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	order := resourceOrder(opts.SortBy, opts.SortOrder, map[string]string{"expires_at": "us.expires_at", "created_at": "us.created_at", "status": "us.status"}, "us.created_at DESC")
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT us.id, us.user_id, u.email AS user_email, u.username AS username, us.group_id, g.name AS group_name,
       g.platform AS group_platform, us.starts_at, us.expires_at, us.status, us.daily_usage_usd::double precision AS daily_usage_usd,
       us.weekly_usage_usd::double precision AS weekly_usage_usd, us.monthly_usage_usd::double precision AS monthly_usage_usd,
       us.assigned_by, us.managed_by_user_id, us.source_type, us.source_redeem_code_id, us.revoked_by_user_id, us.assigned_at,
       COALESCE(us.notes, '') AS notes, us.created_at, us.updated_at, us.deleted_at
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
JOIN users u ON u.id = us.user_id
WHERE `+whereSQL+`
ORDER BY `+order+`
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

func (s *UserResourceService) AssignSubscription(ctx context.Context, ownerID int64, input UserSubscriptionAssignInput) (map[string]any, error) {
	if err := validateUserResourceNotes(input.Notes); err != nil {
		return nil, err
	}
	userID := input.UserID
	var err error
	if userID <= 0 && input.Email != "" {
		userID, err = s.lookupUserIDByEmail(ctx, input.Email)
		if err != nil {
			return nil, err
		}
	}
	if userID <= 0 {
		return nil, infraerrors.BadRequest("ASSIGN_USER_REQUIRED", "user_id or email is required")
	}
	if err := s.validateOwnedSubscriptionGroup(ctx, ownerID, input.GroupID); err != nil {
		return nil, err
	}
	sub, err := s.assignOrExtendSubscription(ctx, ownerID, userID, input.GroupID, input.ValidityDays, input.Notes, "manual", nil)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

func (s *UserResourceService) BulkAssignSubscription(ctx context.Context, ownerID int64, input UserSubscriptionBulkAssignInput) (*UserSubscriptionBulkAssignResult, error) {
	if len(input.UserIDs)+len(input.Emails) > userResourceBatchMaxItems {
		return nil, infraerrors.BadRequest("USER_RESOURCE_BATCH_TOO_LARGE", "bulk assignment cannot exceed 1000 users")
	}
	if err := validateUserResourceNotes(input.Notes); err != nil {
		return nil, err
	}
	if err := s.validateOwnedSubscriptionGroup(ctx, ownerID, input.GroupID); err != nil {
		return nil, err
	}
	result := &UserSubscriptionBulkAssignResult{Items: []map[string]any{}, Errors: []string{}}
	userIDs := append([]int64{}, input.UserIDs...)
	for _, email := range input.Emails {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		uid, err := s.lookupUserIDByEmail(ctx, email)
		if err != nil {
			result.FailedCount++
			result.Errors = append(result.Errors, fmt.Sprintf("email %s: %v", email, err))
			continue
		}
		userIDs = append(userIDs, uid)
	}
	userIDs = uniquePositiveInt64s(userIDs)
	for _, uid := range userIDs {
		item, err := s.assignOrExtendSubscription(ctx, ownerID, uid, input.GroupID, input.ValidityDays, input.Notes, "manual", nil)
		if err != nil {
			result.FailedCount++
			result.Errors = append(result.Errors, fmt.Sprintf("user %d: %v", uid, err))
			continue
		}
		result.SuccessCount++
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func (s *UserResourceService) ExtendAssignedSubscription(ctx context.Context, ownerID, subscriptionID int64, days int) (map[string]any, error) {
	if days <= 0 {
		return nil, infraerrors.BadRequest("EXTEND_DAYS_REQUIRED", "days must be greater than 0")
	}
	if days > MaxValidityDays {
		return nil, infraerrors.BadRequest("EXTEND_DAYS_INVALID", "days exceeds the maximum")
	}
	if err := s.ensureManagedSubscription(ctx, ownerID, subscriptionID); err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE user_subscriptions
SET expires_at = CASE WHEN expires_at > NOW() THEN expires_at ELSE NOW() END + ($1::int * INTERVAL '1 day'),
	status = 'active', revoked_by_user_id = NULL, updated_at = NOW(), deleted_at = NULL
WHERE id = $2
  AND (revoked_by_user_id IS NULL OR revoked_by_user_id IS DISTINCT FROM user_id)`, days, subscriptionID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceForbidden
	}
	sub, err := s.getAssignedSubscription(ctx, ownerID, subscriptionID, true)
	if err == nil {
		err = s.invalidateSubscription(ctx, urToInt64(sub["user_id"]), urToInt64(sub["group_id"]))
	}
	return sub, err
}

func (s *UserResourceService) RevokeAssignedSubscription(ctx context.Context, ownerID, subscriptionID int64) error {
	sub, err := s.getAssignedSubscription(ctx, ownerID, subscriptionID, false)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE user_subscriptions SET deleted_at = NOW(), revoked_by_user_id = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL", subscriptionID, ownerID)
	if err != nil {
		return err
	}
	return s.invalidateSubscription(ctx, urToInt64(sub["user_id"]), urToInt64(sub["group_id"]))
}

func (s *UserResourceService) RestoreAssignedSubscription(ctx context.Context, ownerID, subscriptionID int64) (map[string]any, error) {
	if err := s.ensureManagedSubscription(ctx, ownerID, subscriptionID); err != nil {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, `
UPDATE user_subscriptions SET deleted_at = NULL,
  status = CASE WHEN expires_at > NOW() THEN 'active' ELSE 'expired' END,
	revoked_by_user_id = NULL, updated_at = NOW()
WHERE id = $1
  AND (revoked_by_user_id IS NULL OR revoked_by_user_id IS DISTINCT FROM user_id)`, subscriptionID)
	if err != nil {
		return nil, err
	}
	if affected(res) == 0 {
		return nil, ErrUserResourceForbidden
	}
	sub, err := s.getAssignedSubscription(ctx, ownerID, subscriptionID, true)
	if err == nil {
		err = s.invalidateSubscription(ctx, urToInt64(sub["user_id"]), urToInt64(sub["group_id"]))
	}
	return sub, err
}

func (s *UserResourceService) ResetAssignedSubscriptionUsage(ctx context.Context, ownerID, subscriptionID int64) (map[string]any, error) {
	if err := s.ensureManagedSubscription(ctx, ownerID, subscriptionID); err != nil {
		return nil, err
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE user_subscriptions
SET daily_usage_usd = 0, weekly_usage_usd = 0, monthly_usage_usd = 0,
    daily_window_start = NOW(), weekly_window_start = NOW(), monthly_window_start = NOW(), updated_at = NOW()
WHERE id = $1`, subscriptionID)
	if err != nil {
		return nil, err
	}
	sub, err := s.getAssignedSubscription(ctx, ownerID, subscriptionID, true)
	if err == nil {
		err = s.invalidateSubscription(ctx, urToInt64(sub["user_id"]), urToInt64(sub["group_id"]))
	}
	return sub, err
}

func (s *UserResourceService) ListAccountUsageLogs(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args, where, err := userAccountUsageWhere(ownerID, opts)
	if err != nil {
		return nil, err
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM usage_logs ul
JOIN accounts a ON a.id = ul.account_id
WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT ul.id, ul.user_id, ul.api_key_id, ul.account_id, a.name AS account_name, ul.group_id,
       g.name AS group_name, ul.subscription_id, ul.request_id, ul.model, ul.requested_model, ul.upstream_model,
       ul.input_tokens, ul.output_tokens, ul.cache_creation_tokens, ul.cache_read_tokens,
       ul.total_cost::double precision AS total_cost, ul.actual_cost::double precision AS actual_cost,
       ul.rate_multiplier::double precision AS rate_multiplier, ul.account_rate_multiplier::double precision AS account_rate_multiplier,
       ul.billing_type, ul.stream, ul.duration_ms, ul.first_token_ms, ul.user_agent, ul.ip_address, ul.created_at
FROM usage_logs ul
JOIN accounts a ON a.id = ul.account_id
LEFT JOIN groups g ON g.id = ul.group_id
WHERE `+whereSQL+`
ORDER BY ul.created_at DESC
LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	redactUsageLogItemsForUser(items, ownerID)
	return paged(items, total, page, pageSize), nil
}

func (s *UserResourceService) GetAccountUsageStats(ctx context.Context, ownerID int64, opts UserResourceListOptions) (map[string]any, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	args, where, err := userAccountUsageWhere(ownerID, opts)
	if err != nil {
		return nil, err
	}
	var (
		requests      int64
		inputTokens   int64
		outputTokens  int64
		cacheTokens   int64
		totalCost     float64
		actualCost    float64
		averageMillis float64
	)
	err = s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(ul.input_tokens), 0),
       COALESCE(SUM(ul.output_tokens), 0),
       COALESCE(SUM(ul.cache_creation_tokens + ul.cache_read_tokens), 0),
       COALESCE(SUM(ul.total_cost), 0)::double precision,
       COALESCE(SUM(ul.actual_cost), 0)::double precision,
       COALESCE(AVG(ul.duration_ms), 0)::double precision
FROM usage_logs ul
JOIN accounts a ON a.id = ul.account_id
WHERE `+strings.Join(where, " AND "), args...).Scan(
		&requests, &inputTokens, &outputTokens, &cacheTokens, &totalCost, &actualCost, &averageMillis,
	)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"requests": requests, "input_tokens": inputTokens, "output_tokens": outputTokens,
		"cache_tokens": cacheTokens, "total_cost": totalCost, "actual_cost": actualCost,
		"average_duration_ms": averageMillis,
	}, nil
}

func userAccountUsageWhere(ownerID int64, opts UserResourceListOptions) ([]any, []string, error) {
	args := []any{ownerID}
	where := []string{"a.owner_user_id = $1", "a.deleted_at IS NULL"}
	if opts.Search != "" {
		p := "%" + opts.Search + "%"
		where = append(where, "(ul.request_id ILIKE "+nextArg(&args, p)+" OR ul.model ILIKE "+nextArg(&args, p)+" OR a.name ILIKE "+nextArg(&args, p)+")")
	}
	if opts.Platform != "" {
		where = append(where, "a.platform = "+nextArg(&args, opts.Platform))
	}
	if opts.UserID > 0 {
		if opts.UserID != ownerID {
			where = append(where, "1 = 0")
		} else {
			where = append(where, "ul.user_id = "+nextArg(&args, ownerID))
		}
	}
	if opts.APIKeyID > 0 {
		where = append(where, "ul.api_key_id = "+nextArg(&args, opts.APIKeyID))
		where = append(where, "EXISTS (SELECT 1 FROM api_keys own_key WHERE own_key.id = ul.api_key_id AND own_key.user_id = "+nextArg(&args, ownerID)+" AND own_key.deleted_at IS NULL)")
	}
	if opts.AccountID > 0 {
		where = append(where, "ul.account_id = "+nextArg(&args, opts.AccountID))
	}
	start, end, err := userResourceDateRange(opts.StartDate, opts.EndDate, opts.Timezone)
	if err != nil {
		return nil, nil, err
	}
	if start != nil {
		where = append(where, "ul.created_at >= "+nextArg(&args, *start))
	}
	if end != nil {
		where = append(where, "ul.created_at < "+nextArg(&args, *end))
	}
	return args, where, nil
}

func userResourceDateRange(startRaw, endRaw, timezoneName string) (*time.Time, *time.Time, error) {
	location := time.UTC
	if timezoneName = strings.TrimSpace(timezoneName); timezoneName != "" {
		parsed, err := time.LoadLocation(timezoneName)
		if err != nil {
			return nil, nil, infraerrors.BadRequest("USER_USAGE_TIMEZONE_INVALID", "timezone is invalid")
		}
		location = parsed
	}
	parse := func(raw string, endOfDay bool) (*time.Time, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return nil, nil
		}
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			return &parsed, nil
		}
		parsed, err := time.ParseInLocation("2006-01-02", raw, location)
		if err != nil {
			return nil, infraerrors.BadRequest("USER_USAGE_DATE_INVALID", "usage date range is invalid")
		}
		if endOfDay {
			parsed = parsed.AddDate(0, 0, 1)
		}
		return &parsed, nil
	}
	start, err := parse(startRaw, false)
	if err != nil {
		return nil, nil, err
	}
	end, err := parse(endRaw, true)
	if err != nil {
		return nil, nil, err
	}
	if start != nil && end != nil {
		if !end.After(*start) {
			return nil, nil, infraerrors.BadRequest("USER_USAGE_DATE_INVALID", "usage end date must be after start date")
		}
		if end.Sub(*start) > 366*24*time.Hour {
			return nil, nil, infraerrors.BadRequest("USER_USAGE_DATE_RANGE_TOO_LARGE", "usage date range cannot exceed 366 days")
		}
	}
	return start, end, nil
}

func (s *UserResourceService) ListUpstreamErrors(ctx context.Context, ownerID int64, opts UserResourceListOptions) (*UserResourcePage, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	page, pageSize := normalizeResourcePage(opts.Page, opts.PageSize)
	args := []any{ownerID}
	where := []string{"(a.owner_user_id = $1 OR g.owner_user_id = $1)", "e.error_phase = 'upstream'"}
	if opts.Search != "" {
		p := "%" + opts.Search + "%"
		where = append(where, "(e.request_id ILIKE "+nextArg(&args, p)+" OR e.client_request_id ILIKE "+nextArg(&args, p)+" OR COALESCE(e.error_message, '') ILIKE "+nextArg(&args, p)+")")
	}
	if opts.Platform != "" {
		where = append(where, "e.platform = "+nextArg(&args, opts.Platform))
	}
	if opts.UserID > 0 {
		if opts.UserID != ownerID {
			where = append(where, "1 = 0")
		} else {
			where = append(where, "e.user_id = "+nextArg(&args, ownerID))
		}
	}
	if opts.APIKeyID > 0 {
		where = append(where, "e.api_key_id = "+nextArg(&args, opts.APIKeyID))
		where = append(where, "EXISTS (SELECT 1 FROM api_keys own_key WHERE own_key.id = e.api_key_id AND own_key.user_id = "+nextArg(&args, ownerID)+" AND own_key.deleted_at IS NULL)")
	}
	if opts.AccountID > 0 {
		where = append(where, "e.account_id = "+nextArg(&args, opts.AccountID))
	}
	start, end, err := userResourceDateRange(opts.StartDate, opts.EndDate, opts.Timezone)
	if err != nil {
		return nil, err
	}
	if start != nil {
		where = append(where, "e.created_at >= "+nextArg(&args, *start))
	}
	if end != nil {
		where = append(where, "e.created_at < "+nextArg(&args, *end))
	}
	whereSQL := strings.Join(where, " AND ")
	var total int64
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM ops_error_logs e
LEFT JOIN accounts a ON a.id = e.account_id
LEFT JOIN groups g ON g.id = e.group_id
WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return nil, err
	}
	limitArg := nextArg(&args, pageSize)
	offsetArg := nextArg(&args, (page-1)*pageSize)
	rows, err := s.db.QueryContext(ctx, `
SELECT e.id, e.created_at, e.request_id, e.client_request_id, e.user_id, e.api_key_id,
       e.account_id, a.name AS account_name, e.group_id, g.name AS group_name, e.platform, e.model,
       e.requested_model, e.upstream_model, e.error_phase AS phase, e.error_type AS type,
       e.error_owner, e.error_source, e.severity, e.status_code,
       e.upstream_status_code, e.upstream_error_message, e.error_message AS message,
       e.request_path, e.stream, e.user_agent, COALESCE(e.upstream_errors, '[]'::jsonb)::text AS upstream_errors
FROM ops_error_logs e
LEFT JOIN accounts a ON a.id = e.account_id
LEFT JOIN groups g ON g.id = e.group_id
WHERE `+whereSQL+`
ORDER BY e.created_at DESC
LIMIT `+limitArg+` OFFSET `+offsetArg, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	redactUpstreamErrorItemsForUser(items, ownerID)
	return paged(items, total, page, pageSize), nil
}

func (s *UserResourceService) validateGroupReferences(ctx context.Context, ownerID int64, payload map[string]any) error {
	for _, key := range []string{"fallback_group_id", "fallback_group_id_on_invalid_request"} {
		id := urToInt64(payload[key])
		if id > 0 {
			if err := s.validateOwnedGroupIDs(ctx, ownerID, []int64{id}); err != nil {
				return err
			}
		}
	}
	if ids := modelRoutingAccountIDs(payload["model_routing"]); len(ids) > 0 {
		if err := s.validateOwnedAccountIDs(ctx, ownerID, ids); err != nil {
			return err
		}
	}
	return nil
}

func (s *UserResourceService) validateAccountReferences(ctx context.Context, ownerID int64, payload map[string]any) error {
	if proxyID := urToInt64(payload["proxy_id"]); proxyID > 0 {
		if err := s.validateProxySelectable(ctx, ownerID, proxyID); err != nil {
			return err
		}
	}
	if ids := urParseInt64Slice(payload["group_ids"]); len(ids) > 0 {
		if err := s.validateOwnedGroupIDs(ctx, ownerID, ids); err != nil {
			return err
		}
	}
	return nil
}

func (s *UserResourceService) validateOwnedGroupIDs(ctx context.Context, ownerID int64, ids []int64) error {
	ids = uniquePositiveInt64s(ids)
	if len(ids) == 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM groups WHERE owner_user_id = $1 AND id = ANY($2) AND deleted_at IS NULL", ownerID, pq.Array(ids)).Scan(&count); err != nil {
		return err
	}
	if count != len(ids) {
		return infraerrors.Forbidden("GROUP_OWNER_MISMATCH", "group does not belong to current user")
	}
	return nil
}

func (s *UserResourceService) validateOwnedAccountIDs(ctx context.Context, ownerID int64, ids []int64) error {
	ids = uniquePositiveInt64s(ids)
	if len(ids) == 0 {
		return nil
	}
	var count int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts WHERE owner_user_id = $1 AND id = ANY($2) AND deleted_at IS NULL", ownerID, pq.Array(ids)).Scan(&count); err != nil {
		return err
	}
	if count != len(ids) {
		return infraerrors.Forbidden("ACCOUNT_OWNER_MISMATCH", "account does not belong to current user")
	}
	return nil
}

func (s *UserResourceService) validateProxySelectable(ctx context.Context, ownerID, proxyID int64) error {
	return s.validateProxySelectableForOwner(ctx, userResourceOwner(ownerID), proxyID)
}

func (s *UserResourceService) validateProxySelectableForOwner(ctx context.Context, ownerID *int64, proxyID int64) error {
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

func (s *UserResourceService) validateOwnedSubscriptionGroup(ctx context.Context, ownerID, groupID int64) error {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1 FROM groups
  WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL
    AND status = 'active' AND subscription_type = 'subscription'
)`, groupID, ownerID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return infraerrors.Forbidden("SUBSCRIPTION_GROUP_OWNER_MISMATCH", "subscription group does not belong to current user")
	}
	return nil
}

func (s *UserResourceService) ownedGroupIDSet(ctx context.Context, ownerID int64) (map[int64]struct{}, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id FROM groups
WHERE owner_user_id = $1 AND deleted_at IS NULL`, ownerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int64]struct{})
	for rows.Next() {
		var groupID int64
		if err := rows.Scan(&groupID); err != nil {
			return nil, err
		}
		result[groupID] = struct{}{}
	}
	return result, rows.Err()
}

func (s *UserResourceService) groupSubscriberIDSet(ctx context.Context, ownerID, groupID int64) (map[int64]struct{}, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT us.user_id
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
WHERE us.group_id = $1 AND g.owner_user_id = $2 AND g.deleted_at IS NULL`, groupID, ownerID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := map[int64]struct{}{ownerID: {}}
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		result[userID] = struct{}{}
	}
	return result, rows.Err()
}

func (s *UserResourceService) validateGroupSubscriberIDs(ctx context.Context, ownerID, groupID int64, userIDs []int64) error {
	if len(userIDs) == 0 {
		return nil
	}
	allowed, err := s.groupSubscriberIDSet(ctx, ownerID, groupID)
	if err != nil {
		return err
	}
	for _, userID := range userIDs {
		if _, ok := allowed[userID]; !ok {
			return infraerrors.Forbidden("USER_GROUP_OVERRIDE_FORBIDDEN", "group overrides are limited to assigned subscribers")
		}
	}
	return nil
}

func (s *UserResourceService) insertOwnedWith(ctx context.Context, db userResourceDBTX, table string, ownerID int64, specs map[string]columnSpec, payload map[string]any, required []string) (int64, error) {
	return s.insertForOwnerWith(ctx, db, table, userResourceOwner(ownerID), specs, payload, required)
}

func (s *UserResourceService) insertForOwnerWith(ctx context.Context, db userResourceDBTX, table string, ownerID *int64, specs map[string]columnSpec, payload map[string]any, required []string) (int64, error) {
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

func (s *UserResourceService) updateOwnedWith(ctx context.Context, db userResourceDBTX, table string, ownerID, id int64, specs map[string]columnSpec, payload map[string]any) error {
	return s.updateForOwnerWith(ctx, db, table, userResourceOwner(ownerID), id, specs, payload)
}

func (s *UserResourceService) updateForOwnerWith(ctx context.Context, db userResourceDBTX, table string, ownerID *int64, id int64, specs map[string]columnSpec, payload map[string]any) error {
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
		return ErrUserResourceNotFound
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

func (s *UserResourceService) ensureOwned(ctx context.Context, table string, ownerID, id int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	var ok bool
	query := fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL)", table)
	if err := s.db.QueryRowContext(ctx, query, id, ownerID).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return ErrUserResourceNotFound
	}
	return nil
}

func replaceAccountGroupsWith(ctx context.Context, db userResourceDBTX, accountID int64, groupIDs []int64) error {
	groupIDs = uniquePositiveInt64s(groupIDs)
	if _, err := db.ExecContext(ctx, "DELETE FROM account_groups WHERE account_id = $1", accountID); err != nil {
		return err
	}
	for _, gid := range groupIDs {
		if _, err := db.ExecContext(ctx, "INSERT INTO account_groups (account_id, group_id, priority, created_at) VALUES ($1, $2, 50, NOW()) ON CONFLICT (account_id, group_id) DO NOTHING", accountID, gid); err != nil {
			return err
		}
	}
	return nil
}

func copyGroupAccountsWith(ctx context.Context, db userResourceDBTX, ownerID, targetGroupID int64, sourceGroupIDs []int64) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO account_groups (account_id, group_id, priority, created_at)
SELECT DISTINCT ag.account_id, $1, 50, NOW()
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id AND a.owner_user_id = $2 AND a.deleted_at IS NULL
WHERE ag.group_id = ANY($3)
ON CONFLICT (account_id, group_id) DO NOTHING`, targetGroupID, ownerID, pq.Array(sourceGroupIDs))
	return err
}

func replaceGroupAccountsFromGroupsWith(ctx context.Context, db userResourceDBTX, ownerID, targetGroupID int64, sourceGroupIDs []int64) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM account_groups WHERE group_id = $1", targetGroupID); err != nil {
		return err
	}
	if len(sourceGroupIDs) > 0 {
		if _, err := db.ExecContext(ctx, `
INSERT INTO account_groups (account_id, group_id, priority, created_at)
SELECT DISTINCT ag.account_id, $1, 50, NOW()
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id AND a.owner_user_id = $2 AND a.deleted_at IS NULL
WHERE ag.group_id = ANY($3)
ON CONFLICT (account_id, group_id) DO NOTHING`, targetGroupID, ownerID, pq.Array(sourceGroupIDs)); err != nil {
			return err
		}
	}
	return nil
}

func (s *UserResourceService) attachAccountGroups(ctx context.Context, items []map[string]any) error {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, urToInt64(item["id"]))
		item["groups"] = []map[string]any{}
		item["group_ids"] = []int64{}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT ag.account_id, g.id, g.name, g.platform, g.status, g.subscription_type
FROM account_groups ag
JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL
JOIN groups g ON g.id = ag.group_id AND g.deleted_at IS NULL
  AND g.owner_user_id IS NOT DISTINCT FROM a.owner_user_id
WHERE ag.account_id = ANY($1)
ORDER BY g.sort_order ASC, g.id ASC`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	byID := map[int64]map[string]any{}
	for _, item := range items {
		byID[urToInt64(item["id"])] = item
	}
	for rows.Next() {
		var accountID, groupID int64
		var name, platform, status, subType string
		if err := rows.Scan(&accountID, &groupID, &name, &platform, &status, &subType); err != nil {
			return err
		}
		item := byID[accountID]
		if item == nil {
			continue
		}
		groups, groupsOK := item["groups"].([]map[string]any)
		groupIDs, groupIDsOK := item["group_ids"].([]int64)
		if !groupsOK || !groupIDsOK {
			return errors.New("account group projection has an invalid shape")
		}
		item["groups"] = append(groups, map[string]any{"id": groupID, "name": name, "platform": platform, "status": status, "subscription_type": subType})
		item["group_ids"] = append(groupIDs, groupID)
	}
	return rows.Err()
}

func (s *UserResourceService) assignOrExtendSubscription(ctx context.Context, managerID, userID, groupID int64, validityDays int, notes, sourceType string, sourceRedeemCodeID *int64) (map[string]any, error) {
	if err := validateUserResourceNotes(notes); err != nil {
		return nil, err
	}
	if validityDays <= 0 {
		validityDays = 30
	}
	if validityDays > MaxValidityDays {
		validityDays = MaxValidityDays
	}
	now := time.Now()
	expiresAt := now.AddDate(0, 0, validityDays)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var existingID int64
	var existingExpires time.Time
	var existingNotes string
	var existingDeletedAt sql.NullTime
	var existingRevokedBy sql.NullInt64
	err = tx.QueryRowContext(ctx, `
SELECT id, expires_at, COALESCE(notes, ''), deleted_at, revoked_by_user_id
FROM user_subscriptions
WHERE user_id = $1 AND group_id = $2
ORDER BY deleted_at NULLS FIRST, id DESC
LIMIT 1
FOR UPDATE`, userID, groupID).Scan(&existingID, &existingExpires, &existingNotes, &existingDeletedAt, &existingRevokedBy)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if existingID > 0 {
		if existingDeletedAt.Valid && sourceType == "manual" && managerID != userID &&
			(!existingRevokedBy.Valid || existingRevokedBy.Int64 == userID) {
			return nil, infraerrors.Conflict("SUBSCRIPTION_USER_UNSUBSCRIBED", "subscriber opted out of this group")
		}
		updatedNotes := existingNotes
		if notes != "" {
			if updatedNotes != "" {
				updatedNotes += "\n"
			}
			updatedNotes += notes
		}
		if err := validateUserResourceNotes(updatedNotes); err != nil {
			return nil, err
		}
		base := now
		if existingExpires.After(now) {
			base = existingExpires
		}
		expiresAt = base.AddDate(0, 0, validityDays)
		_, err = tx.ExecContext(ctx, `
UPDATE user_subscriptions
SET starts_at = CASE WHEN expires_at > NOW() THEN starts_at ELSE NOW() END,
		expires_at = $1, status = 'active', assigned_by = $2, managed_by_user_id = $3,
		source_type = $4, source_redeem_code_id = $5, assigned_at = NOW(),
		notes = $6,
		deleted_at = NULL, revoked_by_user_id = NULL, updated_at = NOW()
WHERE id = $7`, expiresAt, managerID, managerID, sourceType, sourceRedeemCodeID, updatedNotes, existingID)
	} else {
		_, err = tx.ExecContext(ctx, `
INSERT INTO user_subscriptions (
  user_id, group_id, starts_at, expires_at, status,
  daily_window_start, weekly_window_start, monthly_window_start,
  daily_usage_usd, weekly_usage_usd, monthly_usage_usd,
  assigned_by, managed_by_user_id, source_type, source_redeem_code_id, assigned_at, notes, created_at, updated_at
) VALUES ($1, $2, $3, $4, 'active', $3, $3, $3, 0, 0, 0, $5, $6, $7, $8, NOW(), $9, NOW(), NOW())`,
			userID, groupID, now, expiresAt, managerID, managerID, sourceType, sourceRedeemCodeID, notes)
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := s.invalidateSubscription(ctx, userID, groupID); err != nil {
		return nil, err
	}
	return s.getAssignedSubscriptionByUserGroup(ctx, managerID, userID, groupID)
}

func (s *UserResourceService) lookupUserIDByEmail(ctx context.Context, email string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, "SELECT id FROM users WHERE email = $1 AND deleted_at IS NULL LIMIT 1", strings.TrimSpace(email)).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, infraerrors.NotFound("USER_NOT_FOUND", "user not found")
	}
	return id, err
}

func (s *UserResourceService) ensureManagedSubscription(ctx context.Context, ownerID, subscriptionID int64) error {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
SELECT EXISTS (
  SELECT 1
  FROM user_subscriptions us
  JOIN groups g ON g.id = us.group_id
	WHERE us.id = $1 AND g.deleted_at IS NULL AND g.status = 'active'
    AND (us.managed_by_user_id = $2 OR g.owner_user_id = $2)
)`, subscriptionID, ownerID).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrUserResourceForbidden
	}
	return nil
}

func (s *UserResourceService) getAssignedSubscription(ctx context.Context, ownerID, subscriptionID int64, includeDeleted bool) (map[string]any, error) {
	args := []any{subscriptionID, ownerID}
	deleted := "us.deleted_at IS NULL AND "
	if includeDeleted {
		deleted = ""
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT us.id, us.user_id, u.email AS user_email, u.username AS username, us.group_id, g.name AS group_name,
       g.platform AS group_platform, us.starts_at, us.expires_at, us.status,
       us.daily_usage_usd::double precision AS daily_usage_usd, us.weekly_usage_usd::double precision AS weekly_usage_usd,
       us.monthly_usage_usd::double precision AS monthly_usage_usd,
       us.assigned_by, us.managed_by_user_id, us.source_type, us.source_redeem_code_id, us.revoked_by_user_id, us.assigned_at,
       COALESCE(us.notes, '') AS notes, us.created_at, us.updated_at, us.deleted_at
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
JOIN users u ON u.id = us.user_id
WHERE `+deleted+`us.id = $1 AND (us.managed_by_user_id = $2 OR g.owner_user_id = $2)
LIMIT 1`, args...)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUserResourceNotFound
	}
	return items[0], nil
}

func (s *UserResourceService) getAssignedSubscriptionByUserGroup(ctx context.Context, ownerID, userID, groupID int64) (map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT us.id
FROM user_subscriptions us
JOIN groups g ON g.id = us.group_id
WHERE us.user_id = $1 AND us.group_id = $2 AND us.deleted_at IS NULL
  AND (us.managed_by_user_id = $3 OR g.owner_user_id = $3)
ORDER BY us.id DESC LIMIT 1`, userID, groupID, ownerID)
	if err != nil {
		return nil, err
	}
	items, err := scanRowsToMaps(rows)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrUserResourceNotFound
	}
	return s.getAssignedSubscription(ctx, ownerID, urToInt64(items[0]["id"]), false)
}

func (s *UserResourceService) GetPoolHealth(ctx context.Context, groupID int64) (*SubscriptionPoolHealth, error) {
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, a.name, a.status, a.schedulable, COALESCE(a.error_message, '') AS error_message,
       a.rate_limit_reset_at, a.temp_unschedulable_until, a.expires_at
FROM groups g
JOIN account_groups ag ON ag.group_id = g.id
JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL
  AND a.owner_user_id IS NOT DISTINCT FROM g.owner_user_id
WHERE g.id = $1 AND g.deleted_at IS NULL
ORDER BY ag.priority ASC, a.priority ASC, a.id ASC`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	now := time.Now()
	health := &SubscriptionPoolHealth{
		GroupID:  groupID,
		Reasons:  []PoolHealthReason{},
		ByStatus: map[string]int64{},
	}
	for rows.Next() {
		var (
			id                       int64
			name, status, errMessage string
			schedulable              bool
			rateLimitResetAt         sql.NullTime
			tempUnschedulableUntil   sql.NullTime
			expiresAt                sql.NullTime
		)
		if err := rows.Scan(&id, &name, &status, &schedulable, &errMessage, &rateLimitResetAt, &tempUnschedulableUntil, &expiresAt); err != nil {
			return nil, err
		}
		health.Total++
		if status == "" {
			status = StatusActive
		}
		health.ByStatus[status]++

		reasonStatus := ""
		reason := ""
		switch {
		case status != StatusActive:
			health.Disabled++
			reasonStatus = status
			reason = "account status is " + status
		case !schedulable:
			health.Disabled++
			reasonStatus = "disabled"
			reason = "account scheduling is disabled"
		case expiresAt.Valid && expiresAt.Time.Before(now):
			health.Disabled++
			reasonStatus = "expired"
			reason = "account is expired"
		case rateLimitResetAt.Valid && rateLimitResetAt.Time.After(now):
			health.RateLimited++
			reasonStatus = "rate_limited"
			reason = "rate limit reset at " + rateLimitResetAt.Time.Format(time.RFC3339)
		case tempUnschedulableUntil.Valid && tempUnschedulableUntil.Time.After(now):
			health.RateLimited++
			reasonStatus = "temporarily_unschedulable"
			reason = "temporarily unschedulable until " + tempUnschedulableUntil.Time.Format(time.RFC3339)
		case strings.TrimSpace(errMessage) != "":
			health.Error++
			reasonStatus = "error"
			reason = redactResourceText(errMessage)
		default:
			health.Available++
		}
		if reason != "" {
			health.Reasons = append(health.Reasons, PoolHealthReason{
				AccountID: id,
				Name:      name,
				Status:    reasonStatus,
				Reason:    reason,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return health, nil
}

func (s *UserResourceService) GetPoolHealthMap(ctx context.Context, groupIDs []int64) (map[int64]*SubscriptionPoolHealth, error) {
	groupIDs = uniquePositiveInt64s(groupIDs)
	out := make(map[int64]*SubscriptionPoolHealth, len(groupIDs))
	for _, groupID := range groupIDs {
		health, err := s.GetPoolHealth(ctx, groupID)
		if err != nil {
			return nil, err
		}
		out[groupID] = health
	}
	return out, nil
}

func RedactPoolHealthForSubscriber(health *SubscriptionPoolHealth) *SubscriptionPoolHealth {
	if health == nil {
		return nil
	}
	out := *health
	if health.ByStatus != nil {
		out.ByStatus = make(map[string]int64, len(health.ByStatus))
		for key, value := range health.ByStatus {
			out.ByStatus[key] = value
		}
	}
	if len(health.Reasons) > 0 {
		out.Reasons = make([]PoolHealthReason, 0, len(health.Reasons))
		for _, reason := range health.Reasons {
			reason.AccountID = 0
			reason.Name = ""
			reason.Reason = redactResourceText(reason.Reason)
			out.Reasons = append(out.Reasons, reason)
		}
	}
	return &out
}

func (s *UserResourceService) UnsubscribeOwnSubscription(ctx context.Context, userID, subscriptionID int64) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID int64
	err = tx.QueryRowContext(ctx, `
UPDATE user_subscriptions
SET deleted_at = NOW(), revoked_by_user_id = $2, updated_at = NOW()
WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL
RETURNING group_id`, subscriptionID, userID).Scan(&groupID)
	if err == sql.ErrNoRows {
		return ErrUserResourceNotFound
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.invalidateSubscription(ctx, userID, groupID)
}

func (s *UserResourceService) invalidateGroupAuthCache(ctx context.Context, groupID int64) {
	if groupID > 0 && s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByGroupID(ctx, groupID)
	}
}

func (s *UserResourceService) invalidateSubscription(ctx context.Context, userID, groupID int64) error {
	if userID <= 0 || groupID <= 0 {
		return nil
	}
	cacheCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.authCacheInvalidator != nil {
		s.authCacheInvalidator.InvalidateAuthCacheByUserID(cacheCtx, userID)
	}
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if s.subscriptionService != nil {
			lastErr = s.subscriptionService.InvalidateSubscriptionCaches(cacheCtx, userID, groupID)
		} else if s.billingCacheService != nil {
			lastErr = s.billingCacheService.InvalidateSubscription(cacheCtx, userID, groupID)
		} else {
			return nil
		}
		if lastErr == nil {
			return nil
		}
		if attempt < 3 {
			timer := time.NewTimer(time.Duration(attempt) * 50 * time.Millisecond)
			select {
			case <-cacheCtx.Done():
				timer.Stop()
				attempt = 3
			case <-timer.C:
			}
		}
	}
	if lastErr != nil {
		slog.Error("subscription cache invalidation failed after commit", "user_id", userID, "group_id", groupID, "error", lastErr)
	}
	return nil
}

func enqueueSchedulerOutboxWith(ctx context.Context, db userResourceDBTX, eventType string, accountID, groupID *int64, payload any) error {
	if db == nil {
		return errors.New("scheduler outbox database is unavailable")
	}
	var payloadText *string
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		v := string(raw)
		payloadText = &v
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload, created_at)
VALUES ($1, $2, $3, $4::jsonb, NOW())`, eventType, accountID, groupID, payloadText)
	return err
}

func enqueueAccountChangeWith(ctx context.Context, db userResourceDBTX, accountID int64, groupIDs []int64) error {
	if accountID <= 0 {
		return errors.New("scheduler account id is invalid")
	}
	groupIDs = uniquePositiveInt64s(groupIDs)
	var payload any
	if len(groupIDs) > 0 {
		payload = map[string]any{"group_ids": groupIDs}
	}
	return enqueueSchedulerOutboxWith(ctx, db, SchedulerOutboxEventAccountChanged, &accountID, nil, payload)
}

func enqueueAccountBulkChangeWith(ctx context.Context, db userResourceDBTX, accountIDs, groupIDs []int64) error {
	accountIDs = uniquePositiveInt64s(accountIDs)
	if len(accountIDs) == 0 {
		return nil
	}
	payload := map[string]any{"account_ids": accountIDs}
	if groupIDs = uniquePositiveInt64s(groupIDs); len(groupIDs) > 0 {
		payload["group_ids"] = groupIDs
	}
	return enqueueSchedulerOutboxWith(ctx, db, SchedulerOutboxEventAccountBulkChanged, nil, nil, payload)
}

func enqueueGroupChangesWith(ctx context.Context, db userResourceDBTX, groupIDs []int64) error {
	for _, groupID := range uniquePositiveInt64s(groupIDs) {
		if err := enqueueSchedulerOutboxWith(ctx, db, SchedulerOutboxEventGroupChanged, nil, &groupID, nil); err != nil {
			return err
		}
	}
	return nil
}

func accountSelectSQL(alias string, includeProxy bool) string {
	proxyCols := "NULL::bigint AS proxy_id, NULL::text AS proxy_name, NULL::text AS proxy_protocol, NULL::text AS proxy_host, NULL::int AS proxy_port, NULL::bool AS proxy_is_public, NULL::text AS proxy_kind, "
	if includeProxy {
		proxyCols = "p.id AS proxy_id, p.name AS proxy_name, p.protocol AS proxy_protocol, p.host AS proxy_host, p.port AS proxy_port, p.is_public AS proxy_is_public, p.kind AS proxy_kind, "
	}
	return `
SELECT
  ` + alias + `.id, ` + alias + `.owner_user_id, ` + alias + `.name, ` + alias + `.notes, ` + alias + `.platform, ` + alias + `.type,
  COALESCE(` + alias + `.credentials, '{}'::jsonb)::text AS credentials,
  COALESCE(` + alias + `.extra, '{}'::jsonb)::text AS extra,
  ` + alias + `.concurrency, ` + alias + `.load_factor, ` + alias + `.priority,
  ` + alias + `.rate_multiplier::double precision AS rate_multiplier,
  ` + alias + `.status, ` + alias + `.error_message, ` + alias + `.last_used_at, ` + alias + `.expires_at,
  ` + alias + `.auto_pause_on_expired, ` + alias + `.schedulable, ` + alias + `.rate_limited_at,
  ` + alias + `.rate_limit_reset_at, ` + alias + `.overload_until,
  ` + alias + `.temp_unschedulable_until, ` + alias + `.temp_unschedulable_reason,
  ` + alias + `.session_window_start, ` + alias + `.session_window_end, ` + alias + `.session_window_status,
  ` + alias + `.created_at, ` + alias + `.updated_at,
  ` + proxyCols + `
  (SELECT COUNT(*) FROM usage_logs ul WHERE ul.account_id = ` + alias + `.id AND ul.created_at >= date_trunc('day', NOW()))::bigint AS today_request_count
`
}

func resourceOrder(sortBy, sortOrder string, allowed map[string]string, fallback string) string {
	field := allowed[strings.ToLower(strings.TrimSpace(sortBy))]
	if field == "" {
		return fallback
	}
	order := "DESC"
	if strings.EqualFold(sortOrder, "asc") {
		order = "ASC"
	}
	return field + " " + order
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

func urParseInt64Slice(v any) []int64 {
	switch t := v.(type) {
	case []int64:
		return uniquePositiveInt64s(t)
	case []int:
		out := make([]int64, 0, len(t))
		for _, x := range t {
			out = append(out, int64(x))
		}
		return uniquePositiveInt64s(out)
	case []any:
		out := make([]int64, 0, len(t))
		for _, x := range t {
			out = append(out, urToInt64(x))
		}
		return uniquePositiveInt64s(out)
	case string:
		parts := strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' || r == '\t' })
		out := make([]int64, 0, len(parts))
		for _, p := range parts {
			out = append(out, urToInt64(p))
		}
		return uniquePositiveInt64s(out)
	default:
		id := urToInt64(v)
		if id > 0 {
			return []int64{id}
		}
		return nil
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

func modelRoutingAccountIDs(v any) []int64 {
	raw, ok := v.(map[string]any)
	if !ok {
		if typed, ok := v.(map[string][]int64); ok {
			out := []int64{}
			for _, ids := range typed {
				out = append(out, ids...)
			}
			return uniquePositiveInt64s(out)
		}
		return nil
	}
	out := []int64{}
	for _, ids := range raw {
		out = append(out, urParseInt64Slice(ids)...)
	}
	return uniquePositiveInt64s(out)
}

func affected(res sql.Result) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func redactUsageLogItemsForUser(items []map[string]any, ownerID int64) {
	for _, item := range items {
		if item == nil {
			continue
		}
		if urToInt64(item["user_id"]) != ownerID {
			item["user_id"] = nil
			item["api_key_id"] = nil
			item["ip_address"] = ""
			item["user_agent"] = ""
		} else {
			item["user_agent"] = redactResourceText(urAsString(item["user_agent"]))
		}
	}
}

func redactUpstreamErrorItemsForUser(items []map[string]any, ownerID int64) {
	for _, item := range items {
		if item == nil {
			continue
		}
		if urToInt64(item["user_id"]) != ownerID {
			item["user_id"] = nil
			item["api_key_id"] = nil
			item["client_request_id"] = ""
			item["request_path"] = ""
			item["user_agent"] = ""
			// Upstream messages can echo prompts, email addresses or request
			// fragments. A group owner may see the operational row, but not the
			// subscriber's request contents.
			item["message"] = "upstream request failed"
			item["upstream_error_message"] = ""
			item["upstream_errors"] = []any{}
		} else {
			item["user_agent"] = redactResourceText(urAsString(item["user_agent"]))
			for _, key := range []string{
				"message", "upstream_error_message", "request_path", "client_request_id",
			} {
				if value, ok := item[key]; ok && value != nil {
					item[key] = redactResourceText(urAsString(value))
				}
			}
			if value, ok := item["upstream_errors"]; ok {
				item["upstream_errors"] = redactResourceValue(value)
			}
		}
	}
}

func redactResourceValue(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case string:
		return redactResourceText(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = redactResourceValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			lower := strings.ToLower(key)
			switch lower {
			case "authorization", "cookie", "set-cookie", "proxy-authorization":
				out[key] = ""
			default:
				out[key] = redactResourceValue(item)
			}
		}
		return out
	default:
		return v
	}
}

func redactResourceText(value string) string {
	value = resourceAuthHeaderPattern.ReplaceAllString(value, `$1***`)
	return logredact.RedactText(value,
		"api_key", "apikey", "x_api_key", "x-api-key",
		"authorization", "proxy_authorization", "proxy-authorization",
		"cookie", "set_cookie", "set-cookie",
	)
}

func redactPublicProxy(item map[string]any, ownerID int64) {
	if item == nil {
		return
	}
	owned := urToInt64(item["owner_user_id"]) == ownerID
	item["is_owned"] = owned
	if owned {
		return
	}

	// Public proxy responses are an explicit metadata contract, not a blacklist.
	// Proxy node formats evolve quickly and arbitrary extra fields can contain a
	// complete share URI, credentials or private routing details.
	allowed := map[string]struct{}{
		"id": {}, "is_public": {}, "kind": {}, "name": {}, "protocol": {},
		"status": {}, "expires_at": {}, "account_count": {},
		"latency_status": {}, "latency_ms": {},
		"country": {}, "country_code": {}, "region": {}, "city": {},
		"quality_status": {}, "quality_score": {}, "quality_grade": {},
		"quality_summary": {}, "quality_checked": {},
	}
	for key := range item {
		if _, ok := allowed[key]; !ok {
			delete(item, key)
		}
	}
	item["is_owned"] = false
	item["details_hidden"] = true
}

func RedactProxyPageForUserResponse(page *UserResourcePage) {
	if page == nil {
		return
	}
	for _, item := range page.Items {
		RedactProxyForUserResponse(item)
	}
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

func RedactAccountPageForUserResponse(page *UserResourcePage) {
	if page == nil {
		return
	}
	for _, item := range page.Items {
		RedactAccountForUserResponse(item)
	}
}

func RedactAccountImportResultForUserResponse(result *UserAccountImportResult) {
	if result == nil {
		return
	}
	for _, item := range result.Created {
		RedactAccountForUserResponse(item)
	}
}

func RedactAccountForUserResponse(item map[string]any) {
	if item == nil {
		return
	}
	credentials, _ := item["credentials"].(map[string]any)
	item["has_credentials"] = hasNonEmptyCredentialValue(credentials)
	status := make(map[string]bool)
	redacted := make(map[string]any, len(credentials))
	for key, value := range credentials {
		if IsSensitiveCredentialKey(key) && hasNonEmptyCredentialValue(value) {
			status["has_"+key] = true
			continue
		}
		redacted[key] = value
	}
	if len(status) > 0 {
		item["credentials_status"] = status
	}
	item["credentials"] = redacted
	item["credentials_redacted"] = true
}

func hasNonEmptyCredentialValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case map[string]any:
		for _, value := range t {
			if hasNonEmptyCredentialValue(value) {
				return true
			}
		}
		return false
	case []any:
		for _, value := range t {
			if hasNonEmptyCredentialValue(value) {
				return true
			}
		}
		return false
	default:
		return true
	}
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

func sanitizeImportPayload(payload map[string]any) map[string]any {
	out := clonePayload(payload)
	for _, key := range []string{
		"id", "owner_user_id", "is_public", "created_at", "updated_at", "deleted_at",
		"account_count", "today_request_count", "proxy_name", "proxy_protocol",
		"proxy_host", "proxy_port", "proxy_is_public", "proxy_kind",
	} {
		delete(out, key)
	}
	return out
}

func (s *UserResourceService) getSelectableProxyRaw(ctx context.Context, ownerID, proxyID int64) (map[string]any, error) {
	return s.getSelectableProxyRawForOwner(ctx, userResourceOwner(ownerID), proxyID)
}

func (s *UserResourceService) getSelectableProxyRawForOwner(ctx context.Context, ownerID *int64, proxyID int64) (map[string]any, error) {
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
		return nil, ErrUserResourceNotFound
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

// proxySubscriptionUserInfo is the parsed `subscription-userinfo` header. Each
// Has* flag records whether the upstream actually reported that value, so a
// panel that omits a field cannot overwrite a previously known snapshot.
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
func (s *UserResourceService) recordProxySourceSubscriptionInfo(ctx context.Context, ownerID *int64, sourceID int64, info proxySubscriptionUserInfo) error {
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
		out = append(out, parseSingBoxProxyNode(outbound))
	}
	if len(out) == 0 {
		return []parsedProxyNode{{Err: "sing-box config contains no supported proxy outbounds"}}, true
	}
	return out, true
}

func singBoxOutboundItems(document any) []map[string]any {
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
		"name":     urAsString(outbound["tag"]),
		"type":     urAsString(outbound["type"]),
		"server":   urAsString(outbound["server"]),
		"port":     toInt(outbound["server_port"]),
		"username": urAsString(outbound["username"]),
		"password": urAsString(outbound["password"]),
		"uuid":     urAsString(outbound["uuid"]),
		"cipher":   urAsString(outbound["method"]),
		"alterId":  toInt(outbound["alter_id"]),
		"flow":     urAsString(outbound["flow"]),
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
	if strings.Contains(decoded, "://") || strings.Contains(decoded, "proxies:") || strings.Contains(decoded, "\n- name:") {
		return decoded, true
	}
	return "", false
}

type clashProxyDocument struct {
	Proxies []map[string]any `yaml:"proxies"`
}

func parseClashProxyNodes(raw string) ([]parsedProxyNode, bool) {
	if !strings.Contains(raw, "proxies:") {
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
	case "http", "https", "socks5":
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
		upMbps := clashString(proxy, "up", "up-mbps", "up_mbps")
		downMbps := clashString(proxy, "down", "down-mbps", "down_mbps")
		if auth == "" || upMbps == "" || downMbps == "" {
			return parsedProxyNode{Name: name, Err: "hysteria node missing authentication or bandwidth"}
		}
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
		"v":    "2",
		"ps":   name,
		"add":  host,
		"port": strconv.Itoa(port),
		"id":   uuid,
		"aid":  toInt(proxy["alterId"]),
		"scy":  clashString(proxy, "cipher"),
		"net":  network,
		"type": clashString(proxy, "header-type", "headerType"),
		"host": transportHost,
		"path": path,
		"tls":  clashClashTLS(proxy),
		"sni":  clashString(proxy, "servername", "sni"),
		"fp":   clashString(proxy, "client-fingerprint", "fingerprint"),
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
	if toBool(proxy["tls"]) {
		return "tls"
	}
	if reality := proxy["reality-opts"]; reality != nil {
		return "reality"
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
