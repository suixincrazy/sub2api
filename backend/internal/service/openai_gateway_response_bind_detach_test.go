package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// HTTP 续链绑定必须与请求生命周期脱钩。
//
// 背景：bindHTTPResponseAccount 在响应体写完之后才落 response_id -> account_id 与
// response_id -> owner 两笔记账，而此时请求 ctx 常常已被取消（客户端拿到结果就断开）。
// 沿用请求 ctx 会让 Redis 写入以 context canceled 失败、绑定静默丢失，下一轮带
// previous_response_id 的请求就选不回同一个账号。线上日志里实测到过
// openai.ws_bind_response_account_failed / openai.http_bind_response_owner_failed
// 各一次，error 都是 "context canceled"。
//
// 本文件刻意不带 //go:build unit：默认 go test 就要跑到。

// bindCtxRecorderStore 记录真正递给 store 的 ctx 状态。
// 只覆写两个 Bind 方法，其余 12 个接口方法由内嵌的真实 store 兜住。
type bindCtxRecorderStore struct {
	OpenAIWSStateStore

	accountCalls      int
	accountCtxErr     error
	accountDeadline   bool
	accountDeadlineAt time.Time

	ownerCalls      int
	ownerCtxErr     error
	ownerDeadline   bool
	ownerDeadlineAt time.Time
}

func (s *bindCtxRecorderStore) BindResponseAccount(ctx context.Context, groupID int64, responseID string, accountID int64, ttl time.Duration) error {
	s.accountCalls++
	s.accountCtxErr = ctx.Err()
	s.accountDeadlineAt, s.accountDeadline = ctx.Deadline()
	return s.OpenAIWSStateStore.BindResponseAccount(ctx, groupID, responseID, accountID, ttl)
}

func (s *bindCtxRecorderStore) BindHTTPResponseOwner(ctx context.Context, groupID int64, responseID string, userID, apiKeyID int64, ttl time.Duration) error {
	s.ownerCalls++
	s.ownerCtxErr = ctx.Err()
	s.ownerDeadlineAt, s.ownerDeadline = ctx.Deadline()
	return s.OpenAIWSStateStore.BindHTTPResponseOwner(ctx, groupID, responseID, userID, apiKeyID, ttl)
}

func TestBindHTTPResponseAccountSurvivesCanceledRequestContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	groupID := int64(4202)
	c.Set("api_key", &APIKey{ID: 511, GroupID: &groupID})
	SetOpenAIHTTPResponseOwner(c, 611, 511)

	store := &bindCtxRecorderStore{OpenAIWSStateStore: NewOpenAIWSStateStore(nil)}
	svc := &OpenAIGatewayService{openaiWSStateStore: store}
	account := &Account{ID: 37002, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	// 模拟真实收尾时序：响应已写完，请求 ctx 已经取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, ctx.Err(), context.Canceled, "前置条件：调用方的 ctx 必须已取消")

	svc.bindHTTPResponseAccount(ctx, c, account, "resp_http_canceled")

	// 断言落在「递给 store 的 ctx」上而不是只看绑定结果：cache 为 nil 时 store 走纯内存
	// map，无论 ctx 死活都会成功，只有这里能证明脱钩真的发生了。
	require.Equal(t, 1, store.accountCalls)
	require.NoError(t, store.accountCtxErr, "账号绑定拿到的 ctx 不得是已取消的请求 ctx")
	require.True(t, store.accountDeadline, "必须带短超时，避免 Redis 卡住时无限等待")

	require.Equal(t, 1, store.ownerCalls)
	require.NoError(t, store.ownerCtxErr, "owner 绑定拿到的 ctx 不得是已取消的请求 ctx")
	require.True(t, store.ownerDeadline)

	got, err := svc.getOpenAIWSStateStore().GetResponseAccount(context.Background(), groupID, "resp_http_canceled")
	require.NoError(t, err)
	require.Equal(t, account.ID, got)

	owned, err := svc.ValidateOpenAIHTTPResponseOwner(context.Background(), groupID, "resp_http_canceled", 611, 511)
	require.NoError(t, err)
	require.True(t, owned)
}

// 脱钩只针对取消，不是把超时也一并丢掉：绑定动作自己的封顶必须早于调用方给的长期限，
// 否则 Redis 卡住时会把 gin 的收尾路径拖住。
func TestBindHTTPResponseAccountCapsItsOwnDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/openai/v1/responses", nil)
	groupID := int64(4203)
	c.Set("api_key", &APIKey{ID: 512, GroupID: &groupID})

	store := &bindCtxRecorderStore{OpenAIWSStateStore: NewOpenAIWSStateStore(nil)}
	svc := &OpenAIGatewayService{openaiWSStateStore: store}
	account := &Account{ID: 37003, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	before := time.Now()
	svc.bindHTTPResponseAccount(ctx, c, account, "resp_http_deadline")

	require.Equal(t, 1, store.accountCalls)
	require.True(t, store.accountDeadline)
	require.Zero(t, store.ownerCalls, "没有 owner 上下文时不该发起 owner 绑定")

	// 调用方给了 1 小时，绑定动作必须自己收到 openAIWSStateStoreRedisTimeout 这个量级，
	// 而不是继承那 1 小时。
	require.Greater(t, store.accountDeadlineAt.Sub(before), time.Duration(0))
	require.LessOrEqual(t, store.accountDeadlineAt.Sub(before), openAIWSStateStoreRedisTimeout+time.Second,
		"封顶取 store 侧同一个常量，不得继承调用方的长期限")
}
