//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func codexHTTPRuntimeBody(t *testing.T, model string, stream bool) []byte {
	t.Helper()
	var root map[string]any
	require.NoError(t, json.Unmarshal([]byte(codexRuntimeToolsBody), &root))
	root["model"] = model
	root["stream"] = stream
	body, err := json.Marshal(root)
	require.NoError(t, err)
	return body
}

func codexHTTPRuntimeAccount(platform string, enabled bool) *Account {
	if platform == PlatformOpenAI {
		account := openaiPlatformDeepSeekAccount()
		account.Extra[ExtraKeyCodexNamespaceTools] = enabled
		return account
	}
	account := openCodeMappedTestAccount()
	account.Credentials["account_mode"] = AccountModeGo
	account.Credentials["base_url"] = DefaultOpenCodeGoBaseURL
	delete(account.Credentials, "api_base_urls")
	account.Extra = map[string]any{ExtraKeyCodexNamespaceTools: enabled}
	return account
}

func codexHTTPRuntimeContext(body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	return c, rec
}

func codexHTTPRuntimeToolResponse(stream bool) *http.Response {
	body := `{"id":"chatcmpl_exec","model":"deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"console.log(1)\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`
	contentType := "application/json"
	if stream {
		contentType = "text/event-stream"
		body = "data: " + `{"id":"chatcmpl_exec","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_exec","type":"function","function":{"name":"exec","arguments":"{\"input\":\"console.log(1)\"}"}}]},"finish_reason":null}]}` + "\n\ndata: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\ndata: [DONE]\n\n"
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func codexHTTPRuntimeCompleted(t *testing.T, body string, stream bool) gjson.Result {
	t.Helper()
	if !stream {
		return gjson.Parse(body)
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		event := gjson.Parse(strings.TrimPrefix(line, "data: "))
		if event.Get("type").String() == "response.completed" {
			return event.Get("response")
		}
	}
	t.Fatalf("missing completed event: %s", body)
	return gjson.Result{}
}

func TestCodexHTTPRuntimeToolsForwardAndReplay(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, platform := range []string{PlatformOpenAI, PlatformOpenCodeGo} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", platform, stream), func(t *testing.T) {
				account := codexHTTPRuntimeAccount(platform, true)
				upstream := &httpUpstreamRecorder{responses: []*http.Response{
					codexHTTPRuntimeToolResponse(stream),
					{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(deepSeekChatFallbackOKBody))},
				}}
				svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
				body := codexHTTPRuntimeBody(t, "deepseek-v4.1-flash", stream)
				c, rec := codexHTTPRuntimeContext(body)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Contains(t, upstream.lastReq.URL.Path, "/chat/completions")
				require.Equal(t, "deepseek-v4.1-flash", gjson.GetBytes(upstream.lastBody, "model").String())
				require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(function.name=="exec")`).Exists())
				require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(function.name=="functions__wait")`).Exists())
				require.Equal(t, "string", gjson.GetBytes(upstream.lastBody, `tools.#(function.name=="exec").function.parameters.properties.input.type`).String())
				completed := codexHTTPRuntimeCompleted(t, rec.Body.String(), stream)
				call := completed.Get(`output.#(type=="custom_tool_call")`)
				require.Equal(t, "exec", call.Get("name").String())
				require.Equal(t, "console.log(1)", call.Get("input").String())
				require.Equal(t, "call_exec", call.Get("call_id").String())
				if stream {
					require.Contains(t, rec.Body.String(), "response.custom_tool_call_input.done")
				}

				var root map[string]any
				require.NoError(t, json.Unmarshal(body, &root))
				var historyCall map[string]any
				require.NoError(t, json.Unmarshal([]byte(call.Raw), &historyCall))
				root["input"] = append(root["input"].([]any), historyCall, map[string]any{"type": "custom_tool_call_output", "call_id": "call_exec", "output": "1"})
				root["stream"] = false
				nextBody, err := json.Marshal(root)
				require.NoError(t, err)
				nextContext, nextRec := codexHTTPRuntimeContext(nextBody)
				_, err = svc.Forward(context.Background(), nextContext, account, nextBody)
				require.NoError(t, err)
				require.Equal(t, "completed", gjson.Get(nextRec.Body.String(), "status").String())
				require.Len(t, upstream.requests, 2)
				foundCall, foundResult := false, false
				for _, message := range gjson.GetBytes(upstream.lastBody, "messages").Array() {
					if message.Get(`tool_calls.#(id=="call_exec").function.name`).String() == "exec" {
						foundCall = true
						require.JSONEq(t, `{"input":"console.log(1)"}`, message.Get(`tool_calls.#(id=="call_exec").function.arguments`).String())
						require.Equal(t, deepSeekChatReasoningPlaceholderText, message.Get("reasoning_content").String())
					}
					if message.Get("role").String() == "tool" && message.Get("tool_call_id").String() == "call_exec" {
						foundResult = true
					}
				}
				require.True(t, foundCall)
				require.True(t, foundResult)
			})
		}
	}
}

func TestCodexHTTPRuntimeToolsScopeAndDisabledControl(t *testing.T) {
	for _, platform := range []string{PlatformOpenAI, PlatformOpenCodeGo} {
		for _, transport := range []OpenAIClientTransport{OpenAIClientTransportHTTP, OpenAIClientTransportWS} {
			for _, enabled := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/enabled=%t", platform, transport, enabled), func(t *testing.T) {
					account := codexHTTPRuntimeAccount(platform, enabled)
					upstream := newOKChatCompletionsUpstream("codex_scope", deepSeekChatFallbackOKBody)
					svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
					body := codexHTTPRuntimeBody(t, "deepseek-v4.1-flash", false)
					c, _ := codexHTTPRuntimeContext(body)
					SetOpenAIClientTransport(c, transport)
					_, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, account, body, time.Now())
					require.NoError(t, err)
					require.Equal(t, enabled && transport == OpenAIClientTransportHTTP, gjson.GetBytes(upstream.lastBody, `tools.#(function.name=="exec")`).Exists())
				})
			}
		}
	}
}

func TestCodexHTTPRuntimeToolsRejectBeforeUpstream(t *testing.T) {
	for name, body := range map[string]string{
		"malformed": `{"model":"deepseek-v4.1-flash","input":[{"type":"additional_tools","tools":"bad"},{"role":"user","content":"hi"}]}`,
		"collision": `{"model":"deepseek-v4.1-flash","input":"hi","tools":[{"type":"namespace","name":"a","tools":[{"type":"custom","name":"exec"}]},{"type":"namespace","name":"b","tools":[{"type":"custom","name":"exec"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			upstream := &httpUpstreamRecorder{}
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
			c, rec := codexHTTPRuntimeContext([]byte(body))
			_, err := svc.Forward(context.Background(), c, codexHTTPRuntimeAccount(PlatformOpenCodeGo, true), []byte(body))
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Empty(t, upstream.requests)
			require.Equal(t, "invalid_request_error", gjson.Get(rec.Body.String(), "error.type").String())
		})
	}
}

type codexRuntimeLocalHTTPUpstream struct {
	client *http.Client
	host   string
}

func (u *codexRuntimeLocalHTTPUpstream) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	// 仿真只允许本地 fixture，避免测试意外访问真实供应商。
	if req.URL.Host != u.host {
		return nil, fmt.Errorf("unexpected upstream %s", req.URL.Host)
	}
	return u.client.Do(req)
}
func (u *codexRuntimeLocalHTTPUpstream) DoWithTLS(req *http.Request, p string, id int64, n int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(req, p, id, n)
}
func TestCodexHTTPRuntimeToolsOpenCodeLocalHTTP(t *testing.T) {
	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		response := codexHTTPRuntimeToolResponse(false)
		defer response.Body.Close()
		_, _ = io.Copy(w, response.Body)
	}))
	defer server.Close()
	account := codexHTTPRuntimeAccount(PlatformOpenCodeGo, true)
	account.Credentials["base_url"] = server.URL + "/v1"
	upstream := &codexRuntimeLocalHTTPUpstream{client: server.Client(), host: strings.TrimPrefix(server.URL, "http://")}
	svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
	body := codexHTTPRuntimeBody(t, "deepseek-v4.1-flash", false)
	c, rec := codexHTTPRuntimeContext(body)
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	received := <-bodies
	require.True(t, gjson.GetBytes(received, `tools.#(function.name=="exec")`).Exists())
	require.Equal(t, "exec", gjson.Get(rec.Body.String(), `output.#(type=="custom_tool_call").name`).String())
}

func TestCodexHTTPRuntimeToolsPreservesOpenCodeModelMapping(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "glm-5.3-flash", "kimi-k2.7-code"} {
		t.Run(model, func(t *testing.T) {
			account := codexHTTPRuntimeAccount(PlatformOpenCodeGo, true)
			account.Credentials["model_mapping"] = map[string]any{"coding-alias": model}
			upstream := newOKChatCompletionsUpstream("codex_mapping", deepSeekChatFallbackOKBody)
			svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
			body := codexHTTPRuntimeBody(t, "coding-alias", false)
			c, _ := codexHTTPRuntimeContext(body)
			_, err := svc.Forward(context.Background(), c, account, body)
			require.NoError(t, err)
			require.Equal(t, model, gjson.GetBytes(upstream.lastBody, "model").String())
			require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(function.name=="exec")`).Exists())
		})
	}
}

func TestCodexHTTPRuntimeToolsPreservesOrdinaryRequests(t *testing.T) {
	for name, tools := range map[string]string{
		"plain":         "[]",
		"function":      `[{"type":"function","name":"read","parameters":{"type":"object"}}]`,
		"legacy_custom": `[{"type":"custom","name":"exec"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(`{"model":"deepseek-v4.1-flash","input":"hello","tools":` + tools + `}`)
			var observed [][]byte
			for _, enabled := range []bool{false, true} {
				upstream := newOKChatCompletionsUpstream("codex_ordinary", deepSeekChatFallbackOKBody)
				svc := &OpenAIGatewayService{cfg: deepSeekChatFallbackTestConfig(), httpUpstream: upstream}
				c, _ := codexHTTPRuntimeContext(body)
				_, err := svc.Forward(context.Background(), c, codexHTTPRuntimeAccount(PlatformOpenCodeGo, enabled), body)
				require.NoError(t, err)
				observed = append(observed, upstream.lastBody)
			}
			require.JSONEq(t, string(observed[0]), string(observed[1]))
		})
	}
}
