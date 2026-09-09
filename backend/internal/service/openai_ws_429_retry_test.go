//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"testing"
	"testing/synctest"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type retry429FrameStub struct {
	writes  [][]byte
	reads   [][]byte
	onWrite func([]byte) [][]byte
}

func (s *retry429FrameStub) WriteFrame(_ context.Context, _ coderws.MessageType, p []byte) error {
	s.writes = append(s.writes, append([]byte(nil), p...))
	s.reads = append(s.reads, s.onWrite(p)...)
	return nil
}
func (s *retry429FrameStub) ReadFrame(_ context.Context) (coderws.MessageType, []byte, error) {
	if len(s.reads) == 0 {
		return coderws.MessageText, nil, io.EOF
	}
	p := s.reads[0]
	s.reads = s.reads[1:]
	return coderws.MessageText, p, nil
}
func (s *retry429FrameStub) Close() error { return nil }

func TestWS429RetriesOnlyCurrentTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &anthropicWindowLimitRepo{}
		account := &Account{ID: 904, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(repo, nil, nil, nil, nil)}
		ctx, state, restore := beginUpstream429Retry(context.Background(), nil, account)
		defer restore()
		counts := map[string]int{}
		stub := &retry429FrameStub{onWrite: func(p []byte) [][]byte {
			input := gjson.GetBytes(p, "input").String()
			counts[input]++
			require.Zero(t, repo.rateLimitCalls)
			created := []byte(`{"type":"response.created","response":{"id":"pending"}}`)
			if input == "second" && counts[input] <= 6 {
				return [][]byte{[]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"rate limit reached"}}}`)}
			}
			return [][]byte{created, []byte(`{"type":"response.completed","response":{"id":"done","status":"completed"}}`)}
		}}
		conn := &openAIWS429RetryFrameConn{FrameConn: stub, service: svc, account: account, state: state, headers: http.Header{}}
		for _, input := range []string{"first", "second", "third"} {
			request := []byte(`{"type":"response.create","model":"gpt-5.4","input":"` + input + `"}`)
			require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, request))
			_, created, err := conn.ReadFrame(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.created", gjson.GetBytes(created, "type").String())
			_, completed, err := conn.ReadFrame(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
		}
		require.Equal(t, map[string]int{"first": 1, "second": 7, "third": 1}, counts)
		require.Zero(t, repo.rateLimitCalls)
	})
}

func TestWS429ExhaustionFreezesOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		repo := &anthropicWindowLimitRepo{}
		account := &Account{ID: 905, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
		svc := &OpenAIGatewayService{rateLimitService: NewRateLimitService(repo, nil, nil, nil, nil)}
		ctx, state, restore := beginUpstream429Retry(context.Background(), nil, account)
		defer restore()
		stub := &retry429FrameStub{onWrite: func(p []byte) [][]byte {
			require.Zero(t, repo.rateLimitCalls)
			return [][]byte{[]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit reached"}}`)}
		}}
		conn := &openAIWS429RetryFrameConn{FrameConn: stub, service: svc, account: account, state: state}
		require.NoError(t, conn.WriteFrame(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.4","input":"first"}`)))
		_, _, err := conn.ReadFrame(ctx)
		var failure *UpstreamFailoverError
		require.ErrorAs(t, err, &failure)
		require.Len(t, stub.writes, 7)
		require.Equal(t, 1, repo.rateLimitCalls)
		require.False(t, failure.RetryableOnSameAccount)
		svc.handleOpenAIAccountUpstreamError(ctx, account, 429, nil, failure.ResponseBody)
		require.Equal(t, 1, repo.rateLimitCalls, "relay must not reapply the final freeze")
	})
}
