package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const userCodexImportMaxBytes = 8 << 20

type UserCodexSessionImportInput struct {
	Content            string         `json:"content"`
	Contents           []string       `json:"contents"`
	Name               string         `json:"name"`
	Notes              string         `json:"notes"`
	GroupIDs           []int64        `json:"group_ids"`
	ProxyID            *int64         `json:"proxy_id"`
	Concurrency        *int           `json:"concurrency"`
	Priority           *int           `json:"priority"`
	RateMultiplier     *float64       `json:"rate_multiplier"`
	LoadFactor         *int           `json:"load_factor"`
	ExpiresAt          *int64         `json:"expires_at"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired"`
	CredentialExtras   map[string]any `json:"credential_extras"`
	Extra              map[string]any `json:"extra"`
}

type UserCodexPATImportInput struct {
	AccessToken        string         `json:"access_token"`
	Name               string         `json:"name"`
	Notes              string         `json:"notes"`
	GroupIDs           []int64        `json:"group_ids"`
	ProxyID            *int64         `json:"proxy_id"`
	Concurrency        *int           `json:"concurrency"`
	Priority           *int           `json:"priority"`
	RateMultiplier     *float64       `json:"rate_multiplier"`
	LoadFactor         *int           `json:"load_factor"`
	ExpiresAt          *int64         `json:"expires_at"`
	AutoPauseOnExpired *bool          `json:"auto_pause_on_expired"`
	CredentialExtras   map[string]any `json:"credential_extras"`
	Extra              map[string]any `json:"extra"`
}

type userCodexSession struct {
	Name         string
	AccessToken  string
	RefreshToken string
	IDToken      string
	Email        string
	AccountID    string
	UserID       string
	PlanType     string
	Organization string
	ExpiresAt    *time.Time
}

func (s *UserResourceService) ImportCodexSessions(ctx context.Context, ownerID int64, input UserCodexSessionImportInput) (*UserAccountImportResult, error) {
	if err := validateUserCodexImportOptions(input.Concurrency, input.Priority, input.RateMultiplier, input.LoadFactor); err != nil {
		return nil, err
	}
	contents := make([]string, 0, 1+len(input.Contents))
	if strings.TrimSpace(input.Content) != "" {
		contents = append(contents, input.Content)
	}
	contents = append(contents, input.Contents...)
	values := make([]any, 0)
	for _, content := range contents {
		parsed, err := parseUserCodexContent(content)
		if err != nil {
			return nil, err
		}
		values = append(values, parsed...)
		if len(values) > userResourceBatchMaxItems {
			return nil, infraerrors.BadRequest("USER_CODEX_IMPORT_LIMIT", "Codex imports cannot exceed 1000 entries")
		}
	}
	if len(values) == 0 {
		return nil, infraerrors.BadRequest("USER_CODEX_IMPORT_EMPTY", "Codex session content is required")
	}

	result := &UserAccountImportResult{Created: []map[string]any{}, Errors: []string{}}
	for index, value := range values {
		session, err := normalizeUserCodexSession(value)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("entry %d: invalid Codex session", index+1))
			continue
		}
		credentials := sanitizeUserCodexCredentialExtras(input.CredentialExtras)
		credentials["access_token"] = session.AccessToken
		if session.RefreshToken != "" {
			credentials["refresh_token"] = session.RefreshToken
		}
		if session.IDToken != "" {
			credentials["id_token"] = session.IDToken
		}
		if session.Email != "" {
			credentials["email"] = session.Email
		}
		if session.AccountID != "" {
			credentials["chatgpt_account_id"] = session.AccountID
		}
		if session.UserID != "" {
			credentials["chatgpt_user_id"] = session.UserID
		}
		if session.PlanType != "" {
			credentials["plan_type"] = session.PlanType
		}
		if session.Organization != "" {
			credentials["organization_id"] = session.Organization
		}
		if session.ExpiresAt != nil {
			credentials["expires_at"] = session.ExpiresAt.UTC().Format(time.RFC3339)
		}
		extra := clonePayload(input.Extra)
		extra["import_source"] = "codex_session"
		extra["imported_at"] = time.Now().UTC().Format(time.RFC3339)
		extra["access_token_sha256"] = userCodexTokenFingerprint(session.AccessToken)

		name := userCodexAccountName(input.Name, session, index, len(values))
		item, err := s.CreateAccount(ctx, ownerID, userCodexAccountPayload(name, input.Notes, input.GroupIDs, input.ProxyID, input.Concurrency, input.Priority, input.RateMultiplier, input.LoadFactor, input.ExpiresAt, input.AutoPauseOnExpired, credentials, extra))
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("entry %d: account import failed", index+1))
			continue
		}
		result.Created = append(result.Created, item)
	}
	result.CreatedCount = len(result.Created)
	result.FailedCount = len(result.Errors)
	return result, nil
}

func (s *UserResourceService) ImportCodexPAT(ctx context.Context, ownerID int64, input UserCodexPATImportInput) (map[string]any, error) {
	if s.openAIOAuthService == nil {
		return nil, infraerrors.ServiceUnavailable("USER_CODEX_PAT_UNAVAILABLE", "Codex PAT validation is unavailable")
	}
	if err := validateUserCodexImportOptions(input.Concurrency, input.Priority, input.RateMultiplier, input.LoadFactor); err != nil {
		return nil, err
	}
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	if input.AccessToken == "" || len(input.AccessToken) > 65536 {
		return nil, infraerrors.BadRequest("USER_CODEX_PAT_INVALID", "Codex personal access token is invalid")
	}
	proxyURL := ""
	if input.ProxyID != nil {
		proxyItem, err := s.getSelectableProxyRaw(ctx, ownerID, *input.ProxyID)
		if err != nil {
			return nil, err
		}
		proxyURL = proxyFromResourceMap(proxyItem).URL()
		if strings.HasPrefix(proxyURL, "xray://unavailable/") {
			return nil, infraerrors.BadRequest("USER_CODEX_PAT_PROXY_UNAVAILABLE", "selected proxy is unavailable")
		}
	}
	tokenInfo, err := s.openAIOAuthService.ValidateCodexPersonalAccessToken(ctx, input.AccessToken, proxyURL)
	if err != nil {
		return nil, err
	}
	credentials := sanitizeUserCodexCredentialExtras(input.CredentialExtras)
	for key, value := range s.openAIOAuthService.BuildAccountCredentials(tokenInfo) {
		credentials[key] = value
	}
	extra := clonePayload(input.Extra)
	extra["import_source"] = "codex_personal_access_token"
	extra["auth_provider"] = "codex_personal_access_token"
	extra["imported_at"] = time.Now().UTC().Format(time.RFC3339)
	extra["access_token_sha256"] = userCodexTokenFingerprint(input.AccessToken)
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = strings.TrimSpace(tokenInfo.Email)
	}
	if name == "" {
		name = "Codex PAT Account"
	}
	return s.CreateAccount(ctx, ownerID, userCodexAccountPayload(name, input.Notes, input.GroupIDs, input.ProxyID, input.Concurrency, input.Priority, input.RateMultiplier, input.LoadFactor, input.ExpiresAt, input.AutoPauseOnExpired, credentials, extra))
}

func validateUserCodexImportOptions(concurrency, priority *int, rateMultiplier *float64, loadFactor *int) error {
	if concurrency != nil && (*concurrency < 0 || *concurrency > 100000) {
		return infraerrors.BadRequest("USER_CODEX_IMPORT_INVALID", "concurrency is invalid")
	}
	if priority != nil && (*priority < 0 || *priority > 1000000) {
		return infraerrors.BadRequest("USER_CODEX_IMPORT_INVALID", "priority is invalid")
	}
	if rateMultiplier != nil && (*rateMultiplier < 0 || *rateMultiplier > 1000) {
		return infraerrors.BadRequest("USER_CODEX_IMPORT_INVALID", "rate_multiplier is invalid")
	}
	if loadFactor != nil && (*loadFactor < 0 || *loadFactor > 10000) {
		return infraerrors.BadRequest("USER_CODEX_IMPORT_INVALID", "load_factor is invalid")
	}
	return nil
}

func parseUserCodexContent(content string) ([]any, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, nil
	}
	if len(trimmed) > userCodexImportMaxBytes {
		return nil, infraerrors.BadRequest("USER_CODEX_IMPORT_TOO_LARGE", "Codex session content is too large")
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		decoder := json.NewDecoder(strings.NewReader(trimmed))
		decoder.UseNumber()
		var values []any
		for {
			var value any
			err := decoder.Decode(&value)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, infraerrors.BadRequest("USER_CODEX_IMPORT_JSON_INVALID", "Codex session JSON is invalid")
			}
			values = appendUserCodexValue(values, value)
		}
		return values, nil
	}
	values := make([]any, 0)
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "{") || strings.HasPrefix(line, "[") {
			parsed, err := parseUserCodexContent(line)
			if err != nil {
				return nil, err
			}
			values = append(values, parsed...)
			continue
		}
		values = append(values, line)
	}
	return values, nil
}

func appendUserCodexValue(values []any, value any) []any {
	if items, ok := value.([]any); ok {
		for _, item := range items {
			values = appendUserCodexValue(values, item)
		}
		return values
	}
	return append(values, value)
}

func normalizeUserCodexSession(value any) (userCodexSession, error) {
	item := userCodexSession{}
	switch raw := value.(type) {
	case string:
		item.AccessToken = strings.TrimSpace(raw)
	case map[string]any:
		item.AccessToken = userCodexFirstString(raw, []string{"tokens", "access_token"}, []string{"tokens", "accessToken"}, []string{"access_token"}, []string{"accessToken"}, []string{"token"})
		item.RefreshToken = userCodexFirstString(raw, []string{"tokens", "refresh_token"}, []string{"tokens", "refreshToken"}, []string{"refresh_token"}, []string{"refreshToken"})
		item.IDToken = userCodexFirstString(raw, []string{"tokens", "id_token"}, []string{"tokens", "idToken"}, []string{"id_token"}, []string{"idToken"})
		item.Email = userCodexFirstString(raw, []string{"email"}, []string{"user", "email"})
		item.Name = userCodexFirstString(raw, []string{"name"}, []string{"user", "name"})
		item.AccountID = userCodexFirstString(raw, []string{"chatgpt_account_id"}, []string{"chatgptAccountId"}, []string{"account_id"}, []string{"accountId"}, []string{"account", "id"})
		item.UserID = userCodexFirstString(raw, []string{"chatgpt_user_id"}, []string{"chatgptUserId"}, []string{"user_id"}, []string{"userId"}, []string{"user", "id"})
		item.PlanType = userCodexFirstString(raw, []string{"plan_type"}, []string{"planType"}, []string{"account", "plan_type"})
		item.Organization = userCodexFirstString(raw, []string{"organization_id"}, []string{"organizationId"}, []string{"org_id"}, []string{"orgId"})
		for _, path := range [][]string{{"tokens", "expires_at"}, {"tokens", "expiresAt"}, {"expires_at"}, {"expiresAt"}} {
			if value, ok := userCodexNested(raw, path...); ok {
				if parsed, err := coerceTime(value); err == nil && parsed != nil {
					item.ExpiresAt = parsed
					break
				}
			}
		}
	default:
		return item, errors.New("unsupported Codex session value")
	}
	if item.AccessToken == "" || len(item.AccessToken) > 65536 || len(item.RefreshToken) > 65536 || len(item.IDToken) > 65536 {
		return item, errors.New("invalid Codex token")
	}
	return item, nil
}

func userCodexFirstString(value map[string]any, paths ...[]string) string {
	for _, path := range paths {
		if candidate, ok := userCodexNested(value, path...); ok {
			if text := strings.TrimSpace(fmt.Sprint(candidate)); text != "" && text != "<nil>" {
				return text
			}
		}
	}
	return ""
}

func userCodexNested(value map[string]any, path ...string) (any, bool) {
	var current any = value
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func sanitizeUserCodexCredentialExtras(extras map[string]any) map[string]any {
	result := clonePayload(extras)
	for _, key := range []string{"access_token", "refresh_token", "id_token", "expires_at", "auth_mode", "openai_auth_mode"} {
		delete(result, key)
	}
	return result
}

func userCodexAccountName(base string, session userCodexSession, index, total int) string {
	base = strings.TrimSpace(base)
	if base == "" {
		for _, candidate := range []string{session.Name, session.Email, session.AccountID, session.UserID} {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				base = candidate
				break
			}
		}
	}
	if base == "" {
		base = "Codex Session"
	}
	if total > 1 {
		return fmt.Sprintf("%s #%d", base, index+1)
	}
	return base
}

func userCodexAccountPayload(name, notes string, groupIDs []int64, proxyID *int64, concurrency, priority *int, rateMultiplier *float64, loadFactor *int, expiresAt *int64, autoPause *bool, credentials, extra map[string]any) map[string]any {
	payload := map[string]any{
		"name": name, "notes": notes, "platform": PlatformOpenAI, "type": AccountTypeOAuth,
		"credentials": credentials, "extra": extra, "group_ids": groupIDs, "proxy_id": proxyID,
	}
	if concurrency != nil {
		payload["concurrency"] = *concurrency
	}
	if priority != nil {
		payload["priority"] = *priority
	}
	if rateMultiplier != nil {
		payload["rate_multiplier"] = *rateMultiplier
	}
	if loadFactor != nil {
		payload["load_factor"] = *loadFactor
	}
	if expiresAt != nil && *expiresAt > 0 {
		payload["expires_at"] = time.Unix(*expiresAt, 0).UTC()
	}
	if autoPause != nil {
		payload["auto_pause_on_expired"] = *autoPause
	}
	return payload
}

func userCodexTokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
