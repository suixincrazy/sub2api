package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	openAITeamLinkedErrorDedupTTL      = 60 * time.Second
	openAITeamLinkedErrorFanoutTimeout = 30 * time.Second
	openAITeamLinkedErrorBlockReason   = "team_linked_error"
)

// maybeHandleOpenAITeamLinkedError 在 OpenAI OAuth 账户收到 402 deactivated_workspace
// （ChatGPT Team 工作区被停用）时，把同一 Team（credentials.chatgpt_account_id 相同）
// 的其余 active 账户一并置为 error 并立即熔断。触发账户自身不在 fan-out 范围内，
// 仍由常规 402 处理标记。
func (s *RateLimitService) maybeHandleOpenAITeamLinkedError(ctx context.Context, account *Account, statusCode int, responseBody []byte) {
	if s == nil || s.accountRepo == nil || statusCode != http.StatusPaymentRequired || !isOpenAIOAuthAccount(account) {
		return
	}
	if gjson.GetBytes(responseBody, "detail.code").String() != "deactivated_workspace" {
		return
	}
	teamID := strings.TrimSpace(account.GetChatGPTAccountID())
	if teamID == "" {
		return
	}
	if !s.markOpenAITeamLinkedFired(account.OwnerUserID, teamID) {
		return
	}
	// 上游报错场景请求 ctx 往往已被取消，落库需要独立生命周期。
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAITeamLinkedErrorFanoutTimeout)
	defer cancel()

	accounts, err := s.accountRepo.ListByPlatform(opCtx, PlatformOpenAI)
	if err != nil {
		slog.Warn("openai_team_linked_error_list_failed", "trigger_account_id", account.ID, "error", err)
		return
	}
	var targets []*Account
	for i := range accounts {
		acc := &accounts[i]
		if acc.ID == account.ID || acc.IsShadow() || !isOpenAIOAuthAccount(acc) || !sameResourceOwner(account.OwnerUserID, acc.OwnerUserID) || strings.TrimSpace(acc.GetChatGPTAccountID()) != teamID {
			continue
		}
		targets = append(targets, acc)
	}
	if len(targets) == 0 {
		return
	}
	// 先全部进程内熔断（微秒级生效），再逐个落库，避免后面的账户等待前面的 DB 写入。
	for _, acc := range targets {
		s.notifyAccountSchedulingBlocked(acc, time.Time{}, openAITeamLinkedErrorBlockReason)
	}
	errorMsg := fmt.Sprintf("Workspace deactivated (402): team-linked error triggered by account #%d", account.ID)
	marked := 0
	for _, acc := range targets {
		// 单账户写入失败不中断其余账户；进程内熔断已先行，且该账户仍为 active，
		// 下一个 402 在去重 TTL 过期后会重新触发 fan-out。
		if err := s.accountRepo.SetError(opCtx, acc.ID, errorMsg); err != nil {
			slog.Warn("openai_team_linked_error_set_error_failed", "account_id", acc.ID, "error", err)
			continue
		}
		marked++
	}
	slog.Warn("openai_team_linked_error_fanout",
		"trigger_account_id", account.ID,
		"team_fingerprint", openAITeamIDFingerprint(teamID),
		"affected", marked,
		"targets", len(targets),
	)
}

// markOpenAITeamLinkedFired 按资源归属和 teamID 做进程内去重。
func (s *RateLimitService) markOpenAITeamLinkedFired(ownerUserID *int64, teamID string) bool {
	now := time.Now()
	key := openAITeamLinkedDedupKey(ownerUserID, teamID)
	s.openaiTeamLinkedMu.Lock()
	defer s.openaiTeamLinkedMu.Unlock()
	if expiry, ok := s.openaiTeamLinkedRecent[key]; ok && expiry.After(now) {
		return false
	}
	if s.openaiTeamLinkedRecent == nil {
		s.openaiTeamLinkedRecent = make(map[string]time.Time)
	}
	for k, v := range s.openaiTeamLinkedRecent {
		if !v.After(now) {
			delete(s.openaiTeamLinkedRecent, k)
		}
	}
	s.openaiTeamLinkedRecent[key] = now.Add(openAITeamLinkedErrorDedupTTL)
	return true
}

func openAITeamLinkedDedupKey(ownerUserID *int64, teamID string) string {
	if ownerUserID == nil {
		return "system:" + teamID
	}
	return fmt.Sprintf("user:%d:%s", *ownerUserID, teamID)
}

func openAITeamIDFingerprint(teamID string) string {
	normalized := strings.ToLower(strings.TrimSpace(teamID))
	sum := sha256.Sum256([]byte(normalized))
	return fmt.Sprintf("%x", sum[:6])
}
