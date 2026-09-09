package service

import (
	"context"
	"errors"
	"net/http"
	"sync"

	openaiwsv2 "github.com/Wei-Shaw/sub2api/internal/service/openai_ws_v2"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

// Retry a rejected response.create on its existing connection. Once a frame
// reaches the client, preserve the established stream and never replay it.
type openAIWS429RetryFrameConn struct {
	openaiwsv2.FrameConn
	service     *OpenAIGatewayService
	account     *Account
	headers     http.Header
	state       *upstream429RetryState
	reconnect   func(context.Context) (openaiwsv2.FrameConn, error)
	mu          sync.Mutex
	connMu      sync.RWMutex
	closed      bool
	request     []byte
	requestType coderws.MessageType
	turn        int
	wrote       bool
}

func (c *openAIWS429RetryFrameConn) WriteFrame(ctx context.Context, kind coderws.MessageType, payload []byte) error {
	if (kind == coderws.MessageText || kind == coderws.MessageBinary) && gjson.GetBytes(payload, "type").String() == "response.create" {
		c.mu.Lock()
		if c.turn > 0 {
			c.state.reset()
		}
		c.turn++
		c.request = append([]byte(nil), payload...)
		c.requestType = kind
		c.wrote = false
		c.mu.Unlock()
	}
	return c.connection().WriteFrame(ctx, kind, payload)
}

func (c *openAIWS429RetryFrameConn) connection() openaiwsv2.FrameConn {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.FrameConn
}

func (c *openAIWS429RetryFrameConn) Close() error {
	c.connMu.Lock()
	c.closed = true
	conn := c.FrameConn
	c.connMu.Unlock()
	return conn.Close()
}

func (c *openAIWS429RetryFrameConn) ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error) {
	dropFailedAfterError := false
	pendingRetry := false
	for {
		kind, payload, err := c.connection().ReadFrame(ctx)
		if err != nil {
			if pendingRetry && ctx.Err() == nil {
				c.mu.Lock()
				canReconnect := c.turn == 1 && !c.wrote && c.reconnect != nil
				request, requestType := c.request, c.requestType
				c.mu.Unlock()
				if canReconnect {
					if reconnectErr := c.reconnectFirstTurn(ctx, requestType, request); reconnectErr != nil {
						return kind, nil, reconnectErr
					}
					pendingRetry, dropFailedAfterError = false, false
					continue
				}
			}
			return kind, payload, err
		}
		event := gjson.GetBytes(payload, "type").String()
		// Some providers close one rejected turn with error + response.failed.
		// The second frame is not another failed attempt.
		if dropFailedAfterError && event == "response.failed" {
			dropFailedAfterError = false
			continue
		}
		dropFailedAfterError = false
		c.mu.Lock()
		wrote, request, turn, requestType := c.wrote, c.request, c.turn, c.requestType
		c.mu.Unlock()
		if kind == coderws.MessageText && !wrote && len(request) > 0 && (event == "error" || event == "response.failed") && openAIStreamFailedEventSemanticStatus(payload, "") == http.StatusTooManyRequests {
			message := extractOpenAISSEErrorMessage(payload)
			model := gjson.GetBytes(request, "model").String()
			c.service.persistOpenAIWSRateLimitSignal(ctx, c.account, c.headers, payload, "rate_limit_exceeded", "rate_limit_error", message, model)
			failure := c.service.newOpenAIWSRateLimitFailoverError(c.account, c.headers, payload, message)
			again, finalErr := c.state.retry(ctx, nil, c.account, failure)
			if !again {
				if turn > 1 {
					cause := errors.New("later passthrough turn exhausted rate-limit retries")
					if failure.IsOpenAICapacityShed() {
						cause = ErrOpenAIUpstreamOverloaded
					}
					finalErr = NewOpenAIWSClientCloseError(coderws.StatusTryAgainLater, "upstream rate limit exceeded; please reconnect", cause)
				}
				return kind, nil, finalErr
			}
			if err := c.connection().WriteFrame(ctx, requestType, request); err != nil {
				if turn != 1 || c.reconnect == nil || ctx.Err() != nil {
					return kind, nil, err
				}
				if err := c.reconnectFirstTurn(ctx, requestType, request); err != nil {
					return kind, nil, err
				}
			}
			pendingRetry = true
			dropFailedAfterError = event == "error"
			continue
		}
		c.mu.Lock()
		c.wrote = true
		c.mu.Unlock()
		return kind, payload, nil
	}
}

func (c *openAIWS429RetryFrameConn) reconnectFirstTurn(ctx context.Context, kind coderws.MessageType, request []byte) error {
	_ = c.connection().Close()
	conn, err := c.reconnect(ctx)
	if err != nil {
		return err
	}
	c.connMu.Lock()
	if c.closed || ctx.Err() != nil {
		c.connMu.Unlock()
		_ = conn.Close()
		return context.Canceled
	}
	c.FrameConn = conn
	c.connMu.Unlock()
	return conn.WriteFrame(ctx, kind, request)
}
