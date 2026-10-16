package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const queuedCapacityPreamble = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_queued\"}}\n\n" +
	"event: response.queued\ndata: {\"type\":\"response.queued\",\"sequence_number\":1,\"response\":{\"id\":\"resp_queued\",\"status\":\"queued\"}}\n\n" +
	"event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"sequence_number\":2,\"response\":{\"id\":\"resp_queued\"}}\n\n"

// 2026-10-16 的真实 hello 探针收到审核元数据；失败请求中该事件也可能先于任何模型输出到达。
const capacityModerationMetadataEvent = "event: response.metadata\ndata: {\"type\":\"response.metadata\",\"response_id\":\"resp_queued\",\"sequence_number\":2,\"metadata\":{\"openai_chatgpt_moderation_metadata\":{\"generation\":{},\"prompt\":{\"omnimod\":{\"outputs\":[{\"is_blocked\":false}],\"protection_type\":\"omnimod\"}},\"tool_call\":{},\"tool_response\":{}}}}\n\n"

const queuedCapacityFailure = "event: error\ndata: {\"type\":\"error\",\"sequence_number\":3,\"error\":{\"type\":\"service_unavailable_error\",\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}\n\n" +
	"event: response.failed\ndata: {\"type\":\"response.failed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_queued\",\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n"

func runOpenAIQueuedCapacityStream(svc *OpenAIGatewayService, c *gin.Context, resp *http.Response, passthrough bool) (*int, error) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	if passthrough {
		result, err := svc.handleStreamingResponsePassthrough(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
		if result != nil {
			return result.firstTokenMs, err
		}
		return nil, err
	}
	result, err := svc.handleStreamingResponse(c.Request.Context(), resp, c, account, time.Now(), "model", "model")
	if result != nil {
		return result.firstTokenMs, err
	}
	return nil, err
}

func TestOpenAIStreamQueuedBeforeOverloadFailsOver(t *testing.T) {
	testOpenAIStreamPreambleBeforeOverloadFailsOver(t, queuedCapacityPreamble)
}

func TestOpenAIStreamMetadataBeforeOverloadFailsOver(t *testing.T) {
	testOpenAIStreamPreambleBeforeOverloadFailsOver(t, capacityModerationMetadataEvent)
}

func testOpenAIStreamPreambleBeforeOverloadFailsOver(t *testing.T, preamble string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			for _, eventLines := range []bool{false, true} {
				stream := preamble + queuedCapacityFailure
				if !eventLines {
					var lines []string
					for _, line := range strings.Split(stream, "\n") {
						if !strings.HasPrefix(line, "event: ") {
							lines = append(lines, line)
						}
					}
					stream = strings.Join(lines, "\n")
				}
				svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}

				firstTokenMs, err := runOpenAIQueuedCapacityStream(svc, c, resp, passthrough)

				var failoverErr *UpstreamFailoverError
				require.ErrorAs(t, err, &failoverErr, "event lines: %t", eventLines)
				require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
				require.True(t, failoverErr.RetryableOnSameAccount)
				require.True(t, failoverErr.RequestScopedTransient)
				require.Nil(t, firstTokenMs, "排队状态不应计为首 token")
				require.False(t, c.Writer.Written())
				require.Empty(t, rec.Body.String(), "失败账号的生命周期事件必须保持在本次尝试内")
			}
		})
	}
}

func TestOpenAIStreamQueuedKeepaliveBeforeOverloadFailsOver(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			rec := newOpenAIResponseFlushRecorder()
			c, _ := gin.CreateTestContext(rec)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
				MaxLineSize: defaultMaxLineSize, StreamKeepaliveInterval: 1,
			}}}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: reader}
			finished := make(chan error, 1)
			go func() {
				_, err := runOpenAIQueuedCapacityStream(svc, c, resp, passthrough)
				finished <- err
			}()
			_, err := io.WriteString(writer, queuedCapacityPreamble+capacityModerationMetadataEvent)
			require.NoError(t, err)
			select {
			case <-rec.flushEvents:
			case <-ctx.Done():
				t.Fatal("排队期间没有收到下游心跳")
			}
			_, err = io.WriteString(writer, queuedCapacityFailure)
			require.NoError(t, err)
			require.NoError(t, writer.Close())
			select {
			case err = <-finished:
			case <-ctx.Done():
				t.Fatal("降载后未结束本次尝试")
			}

			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusServiceUnavailable, failoverErr.StatusCode)
			body, _ := rec.snapshot()
			heartbeat := ":\n\n"
			if passthrough {
				heartbeat = ": keepalive\n\n"
			}
			require.Contains(t, body, heartbeat)
			require.NotContains(t, body, "resp_queued", "心跳不能提交失败账号的排队帧")
			require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c))
		})
	}
}

func TestOpenAINativeQueuedDoesNotDisarmFirstOutputTimeout(t *testing.T) {
	testOpenAINativePreambleDoesNotDisarmFirstOutputTimeout(t, queuedCapacityPreamble)
}

func TestOpenAINativeMetadataDoesNotDisarmFirstOutputTimeout(t *testing.T) {
	testOpenAINativePreambleDoesNotDisarmFirstOutputTimeout(t, capacityModerationMetadataEvent)
}

func testOpenAINativePreambleDoesNotDisarmFirstOutputTimeout(t *testing.T, preamble string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reader, writer := io.Pipe()
	body := &firstOutputCloseTrackingBody{ReadCloser: reader, closed: make(chan struct{})}
	defer body.Close()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		_, _ = io.WriteString(writer, preamble)
		select {
		case <-body.closed:
		case <-ctx.Done():
		}
		_ = writer.Close()
	}()
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		MaxLineSize: defaultMaxLineSize, OpenAIFirstOutputTimeoutSeconds: 1,
	}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body}

	firstTokenMs, err := runOpenAIQueuedCapacityStream(svc, c, resp, false)

	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.Equal(t, http.StatusGatewayTimeout, failoverErr.StatusCode)
	require.True(t, failoverErr.SafeToFailoverAfterWrite)
	require.Nil(t, firstTokenMs)
	require.Empty(t, rec.Body.String())
	select {
	case <-writerDone:
	case <-time.After(time.Second):
		t.Fatal("首输出超时未关闭排队中的上游流")
	}
}

func TestOpenAIStreamQueuedUnknownEventPreservesReplayBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			logSink, restore := captureStructuredLog(t)
			defer restore()
			svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			stream := queuedCapacityPreamble +
				"data: {\"type\":\"response.future_progress\",\"sequence_number\":27,\"private\":\"must-not-log-request-content\"}\n\n" + queuedCapacityFailure
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(stream))}

			_, err := runOpenAIQueuedCapacityStream(svc, c, resp, passthrough)

			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "未知输出事件继续采用禁止重放的保守边界")
			require.True(t, logSink.ContainsFieldValue("first_output_event_type", "response.future_progress"))
			require.True(t, logSink.ContainsFieldValue("first_output_sequence_number", "27"))
			require.True(t, logSink.ContainsField("first_output_elapsed_ms"))
			logSink.mu.Lock()
			defer logSink.mu.Unlock()
			suppressed := 0
			for _, event := range logSink.events {
				if event.Message == "gateway.failover_suppressed_after_semantic_output" {
					suppressed++
				}
				require.NotContains(t, fmt.Sprint(event.Fields), "must-not-log-request-content")
			}
			require.Equal(t, 1, suppressed, "error 和 response.failed 只记录一次禁止切号原因")
		})
	}
}
