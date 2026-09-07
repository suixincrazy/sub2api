package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIWSOverloadQuotaMerge_LaterTurnPreservesOverload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, priorOverloads := range []int{4, 5} {
		t.Run(fmt.Sprintf("overload_%d", priorOverloads+1), func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			upstream := newStagedPassthroughConn()
			account := passthroughLifecycleAccount()
			groupID := int64(2)
			account.GroupIDs = []int64{groupID}
			fallback := *account
			fallback.ID++
			fallback.Priority = 10
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIFirstOutputTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
			cfg.Gateway.OpenAIWS.IngressInterTurnIdleTimeoutSeconds = 3
			svc := newPassthroughLifecycleService(cfg, upstream)
			svc.accountRepo = schedulerTestOpenAIAccountRepo{accounts: []Account{*account, fallback}}
			svc.cache = &schedulerTestGatewayCache{}
			svc.rateLimitService = newOpenAIAdvancedSchedulerRateLimitService("false")
			const model = "gpt-5.1"
			type turnObservation struct {
				turn   int
				result *OpenAIForwardResult
				err    error
			}
			observations := make(chan turnObservation, 16)
			server, done := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: 11, GroupID: &groupID})
				return &OpenAIWSIngressHooks{AfterTurn: func(turn int, result *OpenAIForwardResult, turnErr error) {
					// Match the handler's adapter-to-observer contract without seeding its state.
					observedErr := turnErr
					if result != nil && result.UpstreamCapacityShed {
						observedErr = ErrOpenAIUpstreamOverloaded
					}
					svc.ObserveOpenAIAccountOverloadResult(&groupID, account, model, observedErr == nil && result.SucceededForScheduling(), observedErr)
					observations <- turnObservation{turn: turn, result: result, err: turnErr}
				}}
			})
			defer server.Close()
			client := dialPassthroughLifecycleClient(t, server)
			defer client.CloseNow()
			readObservation := func() turnObservation {
				t.Helper()
				select {
				case observed := <-observations:
					return observed
				case <-time.After(3 * time.Second):
					t.Fatal("AfterTurn was not called")
					return turnObservation{}
				}
			}
			writeNextTurn := func() {
				t.Helper()
				writeCtx, cancelWrite := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancelWrite()
				require.NoError(t, client.Write(writeCtx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.1","stream":false}`)))
			}
			for turn := 1; turn <= priorOverloads; turn++ {
				if turn > 1 {
					writeNextTurn()
				}
				requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
				upstream.Send(`{"type":"error","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded"}}`)
				upstream.Send(fmt.Sprintf(`{"type":"response.failed","response":{"id":"resp_overload_%d","usage":{"input_tokens":7,"output_tokens":2},"error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded"}}}`, turn))
				for _, eventType := range []string{"error", "response.failed"} {
					event, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
					require.NoError(t, err)
					require.Equal(t, eventType, gjson.GetBytes(event, "type").String())
				}
				observed := readObservation()
				require.Equal(t, turn, observed.turn)
				require.NoError(t, observed.err)
				require.NotNil(t, observed.result)
				require.True(t, observed.result.UpstreamCapacityShed)
				require.False(t, svc.ShouldReselectOpenAIAccountAfterOverload(&groupID, account, model))
			}
			writeNextTurn()
			requirePassthroughUpstreamWrite(t, upstream, 3*time.Second)
			upstream.Send(`{"type":"error","error":{"type":"rate_limit_error","code":"server_is_overloaded","message":"Our servers are currently overloaded"}}`)
			upstream.Send(`{"type":"response.failed","response":{"id":"resp_overload_quota","error":{"type":"rate_limit_error","code":"server_is_overloaded","message":"Our servers are currently overloaded"}}}`)
			_, err := readPassthroughLifecycleFrame(t, client, 3*time.Second)
			var clientClose coderws.CloseError
			require.ErrorAs(t, err, &clientClose)
			require.Equal(t, coderws.StatusTryAgainLater, clientClose.Code)
			require.Equal(t, "upstream rate limit exceeded; please reconnect", clientClose.Reason)
			select {
			case err = <-done:
				var closeErr *OpenAIWSClientCloseError
				require.ErrorAs(t, err, &closeErr)
				require.Equal(t, coderws.StatusTryAgainLater, closeErr.StatusCode())
				var failoverErr *UpstreamFailoverError
				require.NotErrorAs(t, err, &failoverErr, "later turns must reconnect, not replay the initial request")
				require.ErrorIs(t, err, ErrOpenAIUpstreamOverloaded)
			case <-time.After(3 * time.Second):
				t.Fatal("relay did not release the connection")
			}
			select {
			case <-upstream.closed:
			default:
				t.Fatal("upstream connection was not closed")
			}
			require.Empty(t, upstream.writes, "the initial request must not be replayed")
			last := readObservation()
			require.Equal(t, priorOverloads+1, last.turn)
			require.Error(t, last.err)
			require.Empty(t, observations, "error and response.failed must count as one turn")
			require.Equal(t, priorOverloads == 5, svc.ShouldReselectOpenAIAccountAfterOverload(&groupID, account, model), "the sixth overload must trigger reselection")
			require.NotNil(t, last.result, "the rejected quota frame must retain its overload result")
			require.True(t, last.result.UpstreamCapacityShed)
			require.False(t, last.result.SucceededForScheduling())
			require.Equal(t, OpenAIUsage{}, last.result.Usage, "failure metadata must not rebill earlier turns")
			wantAccount := account.ID
			if priorOverloads == 5 {
				wantAccount = fallback.ID
			}
			require.Equal(t, wantAccount, overloadTestPick(t, svc, &groupID, model, ""))
		})
	}
}
