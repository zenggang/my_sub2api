//go:build unit

package handler

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func queuedCapacityHandlerResponse(outputEvent string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_failed_attempt\"}}\n\n" +
				"data: {\"type\":\"response.queued\",\"sequence_number\":1,\"response\":{\"id\":\"resp_failed_attempt\",\"status\":\"queued\"}}\n\n" +
				"data: {\"type\":\"response.in_progress\",\"sequence_number\":2,\"response\":{\"id\":\"resp_failed_attempt\"}}\n\n" + outputEvent +
				"data: {\"type\":\"response.metadata\",\"sequence_number\":2,\"response_id\":\"resp_failed_attempt\",\"metadata\":{\"openai_chatgpt_moderation_metadata\":{\"prompt\":{\"omnimod\":{\"outputs\":[{\"is_blocked\":false}]}}}}}\n\n" +
				"data: {\"type\":\"error\",\"sequence_number\":3,\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}\n\n" +
				"data: {\"type\":\"response.failed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_failed_attempt\",\"status\":\"failed\",\"error\":{\"code\":\"server_is_overloaded\",\"message\":\"Our servers are currently overloaded. Please try again later.\"}}}\n\n")),
	}
}

func queuedCapacityHandlerSuccess() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_healthy\"}}\n\n" +
				"data: {\"type\":\"response.queued\",\"response\":{\"id\":\"resp_healthy\",\"status\":\"queued\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_healthy\",\"status\":\"completed\",\"model\":\"gpt-6.1-sol\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"recovered\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")),
	}
}

func waitForQueuedCapacityTestSlot(t *testing.T, c *gin.Context) {
	t.Helper()
	cache := &helperConcurrencyCacheStub{accountSeq: []bool{false, true}}
	helper := NewConcurrencyHelper(service.NewConcurrencyService(cache), SSEPingFormatComment, 10*time.Millisecond)
	streamStarted := false
	release, err := helper.waitForSlotWithPingTimeout(c, "account", 1, 1, time.Second, true, &streamStarted, true)
	require.NoError(t, err)
	require.NotNil(t, release)
	t.Cleanup(release)
	require.True(t, gatewayStreamHasOnlyHeartbeats(c), "必须经过实际排队心跳写出路径，且尚无模型输出")
}

func TestOpenAIResponsesQueuedCapacityRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name           string
		failures       int
		wantCalls      []int64
		wantStatus     int
		queueHeartbeat bool
	}{
		{name: "same account retry succeeds", failures: 1, wantCalls: []int64{1, 1}, wantStatus: http.StatusOK},
		{name: "retry budget then switch account", failures: 4, wantCalls: []int64{1, 1, 1, 1, 2}, wantStatus: http.StatusOK},
		{name: "all accounts overloaded stays bounded", failures: 8, wantCalls: []int64{1, 1, 1, 1, 2, 2, 2, 2}, wantStatus: http.StatusServiceUnavailable},
		{name: "queue heartbeat allows same account retry", failures: 1, wantCalls: []int64{1, 1}, wantStatus: http.StatusOK, queueHeartbeat: true},
		{name: "queue heartbeat allows account switch", failures: 4, wantCalls: []int64{1, 1, 1, 1, 2}, wantStatus: http.StatusOK, queueHeartbeat: true},
		{name: "queue heartbeat exhaustion stays bounded", failures: 8, wantCalls: []int64{1, 1, 1, 1, 2, 2, 2, 2}, wantStatus: http.StatusOK, queueHeartbeat: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := make([]*http.Response, 0, tt.failures+1)
			for i := 0; i < tt.failures; i++ {
				answers = append(answers, queuedCapacityHandlerResponse(""))
			}
			answers = append(answers, queuedCapacityHandlerSuccess())
			upstream := newAstraProCapturedUpstream(answers...)
			handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
			handler.maxAccountSwitches = 1
			c, rec := newAstraProFailoverContext(t, `{"model":"gpt-6.1-sol","stream":true,"input":"hello"}`)
			if tt.queueHeartbeat {
				waitForQueuedCapacityTestSlot(t, c)
			}

			handler.Responses(c)

			_, accountIDs, _ := upstream.snapshot()
			require.Equal(t, tt.wantCalls, accountIDs, "必须经过真实 Handler 的同号重试和选号循环")
			require.Equal(t, tt.wantStatus, rec.Code)
			body := rec.Body.String()
			require.NotContains(t, body, "resp_failed_attempt", "失败尝试的生命周期帧不能混入最终响应")
			if tt.failures < 8 {
				require.Contains(t, body, "resp_healthy")
				require.Contains(t, body, "recovered")
				require.Equal(t, 1, strings.Count(body, `"type":"response.completed"`))
				require.NotContains(t, body, "response.failed")
			} else if tt.queueHeartbeat {
				require.Equal(t, 1, strings.Count(body, `"type":"response.failed"`), "心跳已提交 HTTP 200，耗尽后只能写一次失败终态")
				require.NotContains(t, body, "response.completed")
			}
		})
	}
}

func TestOpenAIResponsesQueuedCapacityAfterOutputDoesNotReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, outputEvent := range []string{
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"command\":\"run\"}"}`,
		`{"type":"response.output_item.added","item":{"type":"reasoning","encrypted_content":"ciphertext"}}`,
	} {
		upstream := newAstraProCapturedUpstream(
			queuedCapacityHandlerResponse("data: "+outputEvent+"\n\n"), queuedCapacityHandlerSuccess(),
		)
		handler := newOpenAIResponsesFailoverTestHandler(t, upstream)
		c, rec := newAstraProFailoverContext(t, `{"model":"gpt-6.1-sol","stream":true,"input":"hello"}`)
		waitForQueuedCapacityTestSlot(t, c)

		handler.Responses(c)

		_, accountIDs, _ := upstream.snapshot()
		require.Equal(t, []int64{1}, accountIDs, "已提交文本、工具参数或加密 reasoning 后禁止重放")
		require.Contains(t, rec.Body.String(), "response.failed")
		require.Contains(t, rec.Body.String(), `"code":"server_error"`)
		require.NotContains(t, rec.Body.String(), "resp_healthy")
	}
}
