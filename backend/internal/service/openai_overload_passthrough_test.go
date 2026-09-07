package service

import (
	"context"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOverloadPassthrough_PairedErrorReportsOneFailedTurn(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded"}}`)
	upstream.Send(`{"type":"response.failed","response":{"id":"resp_overloaded","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded"}}}`)
	results := make(chan *OpenAIForwardResult, 4)
	server, done := startPassthroughLifecycleServerWithHooks(t, ctx, newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream), passthroughLifecycleAccount(), func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) { results <- result }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer client.CloseNow()
	for i := 0; i < 2; i++ {
		_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
		require.NoError(t, err)
	}
	select {
	case result := <-results:
		require.NotNil(t, result)
		require.True(t, result.UpstreamCapacityShed)
		require.False(t, result.SucceededForScheduling())
	case <-time.After(3 * time.Second):
		t.Fatal("overload terminal result was not reported")
	}
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough did not exit")
	}
	require.Empty(t, results, "paired error and response.failed must not double count")
}

func TestOpenAIOverloadPassthrough_SuccessAfterErrorClearsCapacitySignal(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	upstream := newStagedPassthroughConn()
	upstream.Send(`{"type":"response.created","response":{"id":"resp_ok"}}`)
	upstream.Send(`{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded"}}`)
	upstream.Send(`{"type":"response.completed","response":{"id":"resp_ok","status":"completed","output":[]}}`)
	results := make(chan *OpenAIForwardResult, 4)
	server, done := startPassthroughLifecycleServerWithHooks(t, ctx, newPassthroughLifecycleService(passthroughLifecycleConfig(), upstream), passthroughLifecycleAccount(), func(*gin.Context) *OpenAIWSIngressHooks {
		return &OpenAIWSIngressHooks{AfterTurn: func(_ int, result *OpenAIForwardResult, _ error) { results <- result }}
	})
	defer server.Close()
	client := dialPassthroughLifecycleClient(t, server)
	defer client.CloseNow()
	for i := 0; i < 3; i++ {
		_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
		require.NoError(t, err)
	}
	select {
	case result := <-results:
		require.NotNil(t, result)
		require.True(t, result.UpstreamCapacityShed)
	case <-time.After(3 * time.Second):
		t.Fatal("error terminal result was not reported")
	}
	select {
	case result := <-results:
		require.NotNil(t, result)
		require.False(t, result.UpstreamCapacityShed)
		require.True(t, result.SucceededForScheduling())
	case <-time.After(3 * time.Second):
		t.Fatal("successful terminal result was not reported")
	}
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("passthrough did not exit")
	}
	require.Empty(t, results)
}
