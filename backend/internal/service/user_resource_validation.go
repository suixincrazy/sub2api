package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/lib/pq"
)

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

func validateFiniteRange(field string, value, min float64, minInclusive bool) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return invalidUserResourceField(field, "must be finite")
	}
	if (minInclusive && value < min) || (!minInclusive && value <= min) {
		op := ">="
		if !minInclusive {
			op = ">"
		}
		return invalidUserResourceField(field, fmt.Sprintf("must be %s %v", op, min))
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

func normalizeNullableNonNegative(payload map[string]any, keys ...string) error {
	for _, key := range keys {
		value, ok := payload[key]
		if !ok || value == nil || isBlank(value) {
			continue
		}
		number, err := strictFloatValue(value)
		if err != nil {
			return invalidUserResourceField(key, "must be a number")
		}
		if number < 0 {
			payload[key] = nil
		}
	}
	return nil
}

func (s *UserResourceService) normalizeAndValidateGroupPayload(ctx context.Context, ownerID, groupID int64, existing, payload map[string]any) error {
	if err := validateResourcePayloadTypes(payload, groupWritableColumns); err != nil {
		return err
	}
	if err := normalizeNullableNonNegative(payload,
		"daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd",
		"image_price_1k", "image_price_2k", "image_price_4k",
		"video_price_480p", "video_price_720p", "video_price_1080p",
		"web_search_price_per_call",
	); err != nil {
		return err
	}
	for _, key := range []string{"fallback_group_id", "fallback_group_id_on_invalid_request"} {
		if value, ok := payload[key]; ok && urToInt64(value) <= 0 {
			payload[key] = nil
		}
	}

	state := mergeResourceState(existing, payload, groupWritableColumns)
	name := strings.TrimSpace(urAsString(state["name"]))
	if name == "" {
		return invalidUserResourceField("name", "is required")
	}
	payload["name"] = name
	platform := strings.ToLower(strings.TrimSpace(urAsString(state["platform"])))
	if err := validateAllowedValue("platform", platform, PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek); err != nil {
		return err
	}
	if _, ok := payload["platform"]; ok {
		payload["platform"] = platform
	}
	status := strings.ToLower(strings.TrimSpace(urAsString(state["status"])))
	if err := validateAllowedValue("status", status, StatusActive, StatusDisabled); err != nil {
		return err
	}
	if _, ok := payload["status"]; ok {
		payload["status"] = status
	}
	subscriptionType := strings.ToLower(strings.TrimSpace(urAsString(state["subscription_type"])))
	if err := validateAllowedValue("subscription_type", subscriptionType, SubscriptionTypeStandard, SubscriptionTypeSubscription); err != nil {
		return err
	}
	if _, ok := payload["subscription_type"]; ok {
		payload["subscription_type"] = subscriptionType
	}

	checks := []struct {
		field        string
		min          float64
		minInclusive bool
	}{
		{"rate_multiplier", 0, false},
		{"image_rate_multiplier", 0, true},
		{"batch_image_discount_multiplier", 0, true},
		{"batch_image_hold_multiplier", 0, true},
		{"video_rate_multiplier", 0, true},
	}
	for _, check := range checks {
		value, err := strictFloatValue(state[check.field])
		if err != nil {
			return invalidUserResourceField(check.field, "must be a number")
		}
		if err := validateFiniteRange(check.field, value, check.min, check.minInclusive); err != nil {
			return err
		}
	}
	discount, _ := strictFloatValue(state["batch_image_discount_multiplier"])
	hold, _ := strictFloatValue(state["batch_image_hold_multiplier"])
	if hold < discount {
		return invalidUserResourceField("batch_image_hold_multiplier", "must be >= batch_image_discount_multiplier")
	}
	for _, key := range []string{"default_validity_days", "rpm_limit"} {
		value, err := strictInt64Value(state[key])
		if err != nil {
			return invalidUserResourceField(key, "must be an integer")
		}
		if key == "default_validity_days" && value <= 0 {
			return invalidUserResourceField(key, "must be > 0")
		}
		if key == "rpm_limit" && value < 0 {
			return invalidUserResourceField(key, "must be >= 0")
		}
	}

	peakMultiplier, err := strictFloatValue(state["peak_rate_multiplier"])
	if err != nil {
		return invalidUserResourceField("peak_rate_multiplier", "must be a number")
	}
	peakEnabled := toBool(state["peak_rate_enabled"])
	peakStart := strings.TrimSpace(urAsString(state["peak_start"]))
	peakEnd := strings.TrimSpace(urAsString(state["peak_end"]))
	peakEnabled, peakStart, peakEnd, peakMultiplier = NormalizePeakRateConfig(subscriptionType, peakEnabled, peakStart, peakEnd, peakMultiplier)
	if err := ValidatePeakRateConfig(subscriptionType, peakEnabled, peakStart, peakEnd, peakMultiplier); err != nil {
		return invalidUserResourceField("peak_rate", err.Error())
	}
	payload["peak_rate_enabled"] = peakEnabled
	payload["peak_start"] = peakStart
	payload["peak_end"] = peakEnd
	payload["peak_rate_multiplier"] = peakMultiplier

	allowBatch := toBool(state["allow_batch_image_generation"])
	if !toBool(state["allow_image_generation"]) || platform != PlatformGemini {
		allowBatch = false
	}
	payload["allow_batch_image_generation"] = allowBatch

	allowLive := toBool(state["allow_live"])
	if platform != PlatformOpenAI {
		allowLive = false
	}
	payload["allow_live"] = allowLive

	if raw, ok := payload["model_routing"]; ok {
		routing, err := normalizeModelRouting(raw)
		if err != nil {
			return err
		}
		payload["model_routing"] = routing
		state["model_routing"] = routing
	}
	if raw, ok := payload["supported_model_scopes"]; ok {
		scopes, err := strictStringSlice(raw)
		if err != nil {
			return invalidUserResourceField("supported_model_scopes", "must be an array of strings")
		}
		payload["supported_model_scopes"] = scopes
	}
	if raw, ok := payload["messages_dispatch_model_config"]; ok {
		var cfg OpenAIMessagesDispatchModelConfig
		if err := decodeResourceJSON(raw, &cfg); err != nil {
			return invalidUserResourceField("messages_dispatch_model_config", "is malformed")
		}
		payload["messages_dispatch_model_config"] = normalizeOpenAIMessagesDispatchModelConfig(cfg)
	}
	if raw, ok := payload["models_list_config"]; ok {
		var cfg GroupModelsListConfig
		if err := decodeResourceJSON(raw, &cfg); err != nil {
			return invalidUserResourceField("models_list_config", "is malformed")
		}
		payload["models_list_config"] = normalizeGroupModelsListConfig(cfg)
	}

	return s.validateGroupStateReferences(ctx, ownerID, groupID, platform, subscriptionType, state, payload)
}

func (s *UserResourceService) validateGroupStateReferences(ctx context.Context, ownerID, groupID int64, platform, subscriptionType string, state, payload map[string]any) error {
	if err := s.validateGroupReferences(ctx, ownerID, state); err != nil {
		return err
	}
	if fallbackID := urToInt64(state["fallback_group_id"]); fallbackID > 0 {
		if err := s.validateOwnedFallbackChain(ctx, ownerID, groupID, fallbackID); err != nil {
			return err
		}
	}
	if fallbackID := urToInt64(state["fallback_group_id_on_invalid_request"]); fallbackID > 0 {
		if platform != PlatformAnthropic && platform != PlatformAntigravity {
			return invalidUserResourceField("fallback_group_id_on_invalid_request", "is only supported for anthropic or antigravity groups")
		}
		if subscriptionType == SubscriptionTypeSubscription {
			return invalidUserResourceField("fallback_group_id_on_invalid_request", "is not supported for subscription groups")
		}
		if fallbackID == groupID {
			return invalidUserResourceField("fallback_group_id_on_invalid_request", "cannot reference the same group")
		}
		var fallbackPlatform, fallbackType string
		var nestedFallback *int64
		err := s.db.QueryRowContext(ctx, `
SELECT platform, subscription_type, fallback_group_id_on_invalid_request
FROM groups
WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, fallbackID, ownerID).
			Scan(&fallbackPlatform, &fallbackType, &nestedFallback)
		if err == sql.ErrNoRows {
			return infraerrors.Forbidden("GROUP_OWNER_MISMATCH", "fallback group does not belong to current user")
		}
		if err != nil {
			return err
		}
		if fallbackPlatform != PlatformAnthropic || fallbackType == SubscriptionTypeSubscription || nestedFallback != nil {
			return invalidUserResourceField("fallback_group_id_on_invalid_request", "references an incompatible group")
		}
	}

	copyIDs, err := strictPositiveIDSlice(payload["copy_accounts_from_group_ids"], "copy_accounts_from_group_ids")
	if err != nil {
		return err
	}
	if len(copyIDs) == 0 {
		return nil
	}
	for _, id := range copyIDs {
		if id == groupID {
			return invalidUserResourceField("copy_accounts_from_group_ids", "cannot contain the current group")
		}
	}
	if err := s.validateOwnedGroupIDs(ctx, ownerID, copyIDs); err != nil {
		return err
	}
	var matching int
	if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM groups
WHERE owner_user_id = $1 AND id = ANY($2) AND platform = $3 AND deleted_at IS NULL`, ownerID, pq.Array(copyIDs), platform).Scan(&matching); err != nil {
		return err
	}
	if matching != len(copyIDs) {
		return invalidUserResourceField("copy_accounts_from_group_ids", "must use groups from the same platform")
	}
	return nil
}

func (s *UserResourceService) validateOwnedFallbackChain(ctx context.Context, ownerID, groupID, fallbackID int64) error {
	visited := map[int64]struct{}{}
	for nextID := fallbackID; nextID > 0; {
		if nextID == groupID {
			return invalidUserResourceField("fallback_group_id", "creates a cycle")
		}
		if _, ok := visited[nextID]; ok {
			return invalidUserResourceField("fallback_group_id", "creates a cycle")
		}
		visited[nextID] = struct{}{}
		var claudeCodeOnly bool
		var next *int64
		err := s.db.QueryRowContext(ctx, `
SELECT claude_code_only, fallback_group_id
FROM groups
WHERE id = $1 AND owner_user_id = $2 AND deleted_at IS NULL`, nextID, ownerID).Scan(&claudeCodeOnly, &next)
		if err == sql.ErrNoRows {
			return infraerrors.Forbidden("GROUP_OWNER_MISMATCH", "fallback group does not belong to current user")
		}
		if err != nil {
			return err
		}
		if nextID == fallbackID && claudeCodeOnly {
			return invalidUserResourceField("fallback_group_id", "cannot reference a claude_code_only group")
		}
		if next == nil {
			return nil
		}
		nextID = *next
	}
	return nil
}

func (s *UserResourceService) normalizeAndValidateAccountPayload(ctx context.Context, ownerID int64, existing, payload map[string]any) error {
	if err := validateResourcePayloadTypes(payload, accountWritableColumns); err != nil {
		return err
	}
	state := mergeResourceState(existing, payload, accountWritableColumns)
	name := strings.TrimSpace(urAsString(state["name"]))
	if name == "" {
		return invalidUserResourceField("name", "is required")
	}
	payload["name"] = name
	platform := strings.ToLower(strings.TrimSpace(urAsString(state["platform"])))
	if err := validateAllowedValue("platform", platform, PlatformAnthropic, PlatformOpenAI, PlatformGemini, PlatformAntigravity, PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek); err != nil {
		return err
	}
	if _, ok := payload["platform"]; ok {
		payload["platform"] = platform
	}
	accountType := normalizeUserAccountType(urAsString(state["type"]))
	if err := validateAllowedValue("type", accountType, AccountTypeOAuth, AccountTypeSetupToken, AccountTypeAPIKey, AccountTypeUpstream, AccountTypeBedrock, AccountTypeServiceAccount); err != nil {
		return err
	}
	if _, ok := payload["type"]; ok {
		payload["type"] = accountType
	}
	if err := validateUserAccountPlatformType(platform, accountType); err != nil {
		return err
	}
	if err := validateUserAccountCredentials(accountType, state); err != nil {
		return err
	}
	status := strings.ToLower(strings.TrimSpace(urAsString(state["status"])))
	if err := validateAllowedValue("status", status, StatusActive, StatusDisabled, StatusError); err != nil {
		return err
	}
	if _, ok := payload["status"]; ok {
		payload["status"] = status
	}

	concurrency, err := strictInt64Value(state["concurrency"])
	if err != nil || concurrency < 0 {
		return invalidUserResourceField("concurrency", "must be an integer >= 0")
	}
	if platform == PlatformGrok && accountType == AccountTypeOAuth && concurrency == 0 {
		payload["concurrency"] = 1
	}
	priority, err := strictInt64Value(state["priority"])
	if err != nil || priority < 0 {
		return invalidUserResourceField("priority", "must be an integer >= 0")
	}
	rate, err := strictFloatValue(state["rate_multiplier"])
	if err != nil {
		return invalidUserResourceField("rate_multiplier", "must be a number")
	}
	if err := validateFiniteRange("rate_multiplier", rate, 0, true); err != nil {
		return err
	}
	if load, ok := state["load_factor"]; ok && load != nil && !isBlank(load) {
		value, err := strictInt64Value(load)
		if err != nil {
			return invalidUserResourceField("load_factor", "must be an integer")
		}
		if value <= 0 {
			payload["load_factor"] = nil
		} else if value > 10000 {
			return invalidUserResourceField("load_factor", "must be <= 10000")
		}
	}
	if raw, ok := payload["credentials"]; ok {
		credentials, ok := raw.(map[string]any)
		if !ok {
			return invalidUserResourceField("credentials", "must be an object")
		}
		if err := NormalizeHeaderOverrideCredentials(credentials); err != nil {
			return invalidUserResourceField("credentials", err.Error())
		}
		payload["credentials"] = credentials
	}
	if IsCNProvider(platform) {
		credentials, _ := state["credentials"].(map[string]any)
		if credentials == nil {
			credentials = map[string]any{}
		}
		if err := normalizeAndValidateUserCNAccountCredentials(platform, credentials); err != nil {
			return err
		}
		// Persist normalized defaults on create and when an existing CN account is
		// updated, so routing and quota probing do not depend on implicit fallbacks.
		payload["credentials"] = credentials
	}
	if raw, ok := payload["extra"]; ok {
		extra, ok := raw.(map[string]any)
		if !ok {
			return invalidUserResourceField("extra", "must be an object")
		}
		if err := ValidateQuotaResetConfig(extra); err != nil {
			return invalidUserResourceField("extra", err.Error())
		}
		ComputeQuotaResetAt(extra)
		NormalizeFixedQuotaWindows(extra)
		payload["extra"] = extra
	}
	if err := validateUserOwnedAccountURLs(ctx, state); err != nil {
		return err
	}

	groupIDs, err := strictPositiveIDSlice(payload["group_ids"], "group_ids")
	if err != nil {
		return err
	}
	if len(groupIDs) > 0 {
		if err := s.validateOwnedGroupIDs(ctx, ownerID, groupIDs); err != nil {
			return err
		}
		allowedPlatforms := []string{platform}
		if platform == PlatformAntigravity {
			extra, _ := state["extra"].(map[string]any)
			if toBool(extra["mixed_scheduling"]) {
				allowedPlatforms = []string{PlatformAntigravity, PlatformAnthropic, PlatformGemini}
			}
		}
		var matching int
		if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM groups
WHERE owner_user_id = $1 AND id = ANY($2) AND platform = ANY($3) AND deleted_at IS NULL`, ownerID, pq.Array(groupIDs), pq.Array(allowedPlatforms)).Scan(&matching); err != nil {
			return err
		}
		if matching != len(groupIDs) {
			return invalidUserResourceField("group_ids", "must use groups from the account platform")
		}
		if accountType == AccountTypeAPIKey {
			var oauthOnly int
			if err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM groups
WHERE owner_user_id = $1 AND id = ANY($2) AND require_oauth_only = TRUE AND deleted_at IS NULL`, ownerID, pq.Array(groupIDs)).Scan(&oauthOnly); err != nil {
				return err
			}
			if oauthOnly > 0 {
				return invalidUserResourceField("group_ids", "contains a group that only accepts OAuth accounts")
			}
		}
	}
	return s.validateAccountReferences(ctx, ownerID, payload)
}

func validateUserAccountCredentials(accountType string, state map[string]any) error {
	credentials, _ := state["credentials"].(map[string]any)
	credential := func(key string) string {
		return strings.TrimSpace(urAsString(credentials[key]))
	}

	switch accountType {
	case AccountTypeAPIKey:
		apiKey := credential("api_key")
		if apiKey == "" {
			return invalidUserResourceField("credentials.api_key", "is required for API key accounts")
		}
		if len(apiKey) > 64*1024 {
			return invalidUserResourceField("credentials.api_key", "must not exceed 64 KiB")
		}
	case AccountTypeUpstream:
		if credential("base_url") == "" {
			return invalidUserResourceField("credentials.base_url", "is required for upstream accounts")
		}
		if credential("api_key") == "" {
			return invalidUserResourceField("credentials.api_key", "is required for upstream accounts")
		}
	case AccountTypeBedrock:
		if credential("api_key") == "" && (credential("aws_access_key_id") == "" || credential("aws_secret_access_key") == "") {
			return invalidUserResourceField("credentials", "must include a Bedrock API key or AWS access keys")
		}
	case AccountTypeServiceAccount:
		if credential("service_account_json") == "" && (credential("client_email") == "" || credential("private_key") == "") {
			return invalidUserResourceField("credentials.service_account_json", "is required for service account credentials")
		}
	}

	return nil
}

func validateUserAccountPlatformType(platform, accountType string) error {
	if IsCNProvider(platform) {
		if accountType == AccountTypeAPIKey {
			return nil
		}
		return invalidUserResourceField("type", "only API key accounts are supported for the selected platform")
	}
	switch accountType {
	case AccountTypeOAuth, AccountTypeAPIKey:
		return nil
	case AccountTypeSetupToken:
		if platform == PlatformAnthropic {
			return nil
		}
	case AccountTypeServiceAccount:
		if platform == PlatformAnthropic || platform == PlatformGemini {
			return nil
		}
	case AccountTypeBedrock:
		if platform == PlatformAnthropic {
			return nil
		}
	case AccountTypeUpstream:
		return nil
	}
	return invalidUserResourceField("type", "is not supported for the selected platform")
}

func normalizeAndValidateUserCNAccountCredentials(platform string, credentials map[string]any) error {
	mode := strings.ToLower(strings.TrimSpace(urAsString(credentials["account_mode"])))
	if mode == "" {
		mode = AccountModePayG
	}
	if err := validateAllowedValue("credentials.account_mode", mode, AccountModePayG, AccountModeCoding); err != nil {
		return err
	}
	if platform == PlatformDeepseek && mode == AccountModeCoding {
		return invalidUserResourceField("credentials.account_mode", "DeepSeek does not support coding mode")
	}

	protocol := strings.ToLower(strings.TrimSpace(urAsString(credentials["api_protocol"])))
	if protocol == "" {
		protocol = APIProtocolChatCompletions
	}
	if err := validateAllowedValue("credentials.api_protocol", protocol, APIProtocolChatCompletions, APIProtocolAnthropic, APIProtocolResponses); err != nil {
		return err
	}
	if protocol == APIProtocolResponses && platform != PlatformDeepseek {
		return invalidUserResourceField("credentials.api_protocol", "responses protocol is only supported by DeepSeek")
	}

	credentials["account_mode"] = mode
	credentials["api_protocol"] = protocol
	return nil
}

func normalizeUserAccountType(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "api_key":
		return AccountTypeAPIKey
	case "setup_token":
		return AccountTypeSetupToken
	default:
		return normalized
	}
}

func validateUserOwnedAccountURLs(ctx context.Context, state map[string]any) error {
	credentials, _ := state["credentials"].(map[string]any)
	if baseURL := strings.TrimSpace(urAsString(credentials["base_url"])); baseURL != "" {
		if err := validateExternalHTTPURL(ctx, baseURL); err != nil {
			return invalidUserResourceField("credentials.base_url", "must resolve to a public HTTP(S) endpoint")
		}
	}
	extra, _ := state["extra"].(map[string]any)
	if customURL := strings.TrimSpace(urAsString(extra["custom_base_url"])); customURL != "" {
		if err := validateExternalHTTPURL(ctx, customURL); err != nil {
			return invalidUserResourceField("extra.custom_base_url", "must resolve to a public HTTP(S) endpoint")
		}
	}
	return nil
}

func (s *UserResourceService) normalizeAndValidateProxyPayload(ctx context.Context, ownerID, proxyID int64, existing, payload map[string]any) error {
	return s.normalizeAndValidateProxyPayloadForOwner(ctx, userResourceOwner(ownerID), proxyID, existing, payload)
}

func (s *UserResourceService) normalizeAndValidateProxyPayloadForOwner(ctx context.Context, ownerID *int64, proxyID int64, existing, payload map[string]any) error {
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

func normalizeModelRouting(raw any) (map[string][]int64, error) {
	if raw == nil {
		return map[string][]int64{}, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, invalidUserResourceField("model_routing", "is malformed")
	}
	var decoded map[string][]int64
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, invalidUserResourceField("model_routing", "must map model names to account ID arrays")
	}
	out := make(map[string][]int64, len(decoded))
	for model, ids := range decoded {
		model = strings.TrimSpace(model)
		if model == "" {
			return nil, invalidUserResourceField("model_routing", "contains an empty model name")
		}
		ids = uniquePositiveInt64s(ids)
		if len(ids) == 0 {
			return nil, invalidUserResourceField("model_routing", "contains an empty account list")
		}
		out[model] = ids
	}
	return out, nil
}

func strictPositiveIDSlice(raw any, field string) ([]int64, error) {
	if raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, invalidUserResourceField(field, "must be an array of positive IDs")
	}
	var values []any
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, invalidUserResourceField(field, "must be an array of positive IDs")
	}
	out := make([]int64, 0, len(values))
	seen := map[int64]struct{}{}
	for _, value := range values {
		id, err := strictInt64Value(value)
		if err != nil || id <= 0 {
			return nil, invalidUserResourceField(field, "must contain only positive IDs")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func strictStringSlice(raw any) ([]string, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var values []string
	if err := json.Unmarshal(encoded, &values); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func decodeResourceJSON(raw any, target any) error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
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
