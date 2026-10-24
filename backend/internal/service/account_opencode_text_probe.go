package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

type openCodeAccountTextProbe struct {
	AccountHealthProbeResult
	OutputText string
}

// OpenCodeAccountTestModels keeps the account's public aliases and excludes models whose native endpoint is unsupported.
func OpenCodeAccountTestModels(account *Account) []openai.Model {
	if account == nil || !account.IsOpenCodeGo() {
		return nil
	}
	ids := make(map[string]bool)
	mapping := account.GetModelMapping()
	for model := range mapping {
		if !strings.Contains(model, "*") {
			ids[model] = true
		}
	}
	// 显式映射仍是白名单；空映射或通配映射从已同步目录展开，避免将通配符当成可请求模型。
	catalog := DefaultOpenCodeGoModelIDs()
	if snapshot := account.GetUpstreamModelMetadataSnapshot(); snapshot != nil {
		catalog = make([]string, 0, len(snapshot.Models))
		for model := range snapshot.Models {
			catalog = append(catalog, model)
		}
	}
	for _, model := range catalog {
		if account.IsModelSupported(model) {
			ids[model] = true
		}
	}
	models := make([]openai.Model, 0, len(ids))
	for model := range ids {
		if !accountHealthProbeModelID.MatchString(model) || !IsOpenCodeAccountTestTextModel(account.GetMappedModel(model)) {
			continue
		}
		models = append(models, openai.Model{ID: model, Object: "model", Type: "model", DisplayName: model})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

// IsOpenCodeAccountTestTextModel excludes media and provider-specific endpoints from text-only probes.
func IsOpenCodeAccountTestTextModel(model string) bool {
	return isAccountHealthProbeTextModel(model) && !IsOpenCodeUnsupportedModel(model)
}

// testOpenCodeAccountText shares only request construction and stream reading; account recovery, quota reconciliation and scheduling are deliberately absent.
func (s *AccountTestService) testOpenCodeAccountText(ctx context.Context, account *Account, modelID, prompt, effort string, timeout time.Duration, capture bool) (result openCodeAccountTextProbe, err error) {
	if !account.IsModelSupported(modelID) || !IsOpenCodeAccountTestTextModel(account.GetMappedModel(modelID)) {
		return result, infraerrors.BadRequest("UNSUPPORTED_ACCOUNT_TEST_MODEL", "A supported text model is required")
	}
	if account.IsSyntheticUITest() {
		result.Error = "Synthetic test account cannot be probed"
		return result, nil
	}
	if s.httpUpstream == nil {
		result.Error = "Upstream transport is unavailable"
		return result, nil
	}
	authToken := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
	if authToken == "" {
		result.Error = "API key is missing"
		return result, nil
	}
	upstreamModel := account.GetMappedModel(modelID)
	protocol := account.resolveModelRoutedProtocol(upstreamModel)
	if s.openaiGatewayService != nil {
		protocol = s.openaiGatewayService.resolveUpstreamProtocolFor(ctx, account, "", upstreamModel)
	}
	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, requestErr := s.buildOpenCodeAccountTextRequest(testCtx, account, upstreamModel, prompt, effort, protocol, authToken)
	if requestErr != nil {
		result.Error = "Failed to build upstream test request"
		return result, nil
	}
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	startedAt := time.Now()
	defer func() { result.DurationMS = time.Since(startedAt).Milliseconds() }()
	resp, upstreamErr := s.httpUpstream.DoWithTLS(req, proxyURL, account.ID, account.Concurrency, s.tlsFPProfileService.ResolveTLSProfile(account))
	if upstreamErr != nil {
		result.Error = "Upstream transport failed"
		if errors.Is(upstreamErr, context.DeadlineExceeded) || errors.Is(testCtx.Err(), context.DeadlineExceeded) {
			result.Error = "Upstream request timed out"
		} else if errors.Is(upstreamErr, context.Canceled) || testCtx.Err() != nil {
			result.Error = "Test request canceled"
		}
		return result, nil
	}
	if resp == nil || resp.Body == nil {
		result.Error = "Upstream returned no response body"
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()
	result.UpstreamStatusCode = &resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result.Error = accountHealthProbeHTTPError(resp.StatusCode, body)
		return result, nil
	}
	if protocol == APIProtocolResponses {
		if capture {
			result.OutputText, result.StreamErrorStatusCode, result.Error = readAccountIntelligenceStream(resp.Body)
		} else {
			result.FirstTokenMS, result.StreamErrorStatusCode, result.Error = readAccountHealthProbeStream(resp.Body, startedAt)
		}
	} else {
		result.OutputText, result.FirstTokenMS, result.StreamErrorStatusCode, result.Error = readOpenCodeAccountTextStream(resp.Body, protocol, startedAt, capture)
	}
	if errors.Is(testCtx.Err(), context.DeadlineExceeded) {
		result.Error = "Upstream request timed out"
	} else if testCtx.Err() != nil {
		result.Error = "Test request canceled"
	}
	result.Success = result.Error == ""
	if !result.Success {
		result.OutputText = ""
	}
	return result, nil
}

func (s *AccountTestService) buildOpenCodeAccountTextRequest(ctx context.Context, account *Account, model, prompt, effort, protocol, authToken string) (*http.Request, error) {
	input, err := json.Marshal([]map[string]any{{"role": "user", "content": []map[string]string{{"type": "input_text", "text": prompt}}}})
	if err != nil {
		return nil, err
	}
	request := &apicompat.ResponsesRequest{Model: model, Input: input, Stream: true}
	if effort != "" {
		request.Reasoning = &apicompat.ResponsesReasoning{Effort: effort}
	}
	var payload any = request
	baseURL := account.GetOpenAIBaseURL()
	if account.IsAdaptiveAPIProtocol() {
		baseURL = account.GetCNProtocolBaseURL(protocol)
	}
	if protocol == APIProtocolAnthropic {
		baseURL = account.GetAnthropicProtocolBaseURL()
	}
	baseURL, err = s.validateUpstreamBaseURL(baseURL)
	if err != nil {
		return nil, err
	}
	apiURL := buildOpenAIResponsesURLForPlatform(account.Platform, baseURL)
	switch protocol {
	case APIProtocolAnthropic:
		// 沿用正常协议转换的推理强度映射；显式输出预算高于 thinking budget，防止高强度请求被原生端点拒绝。
		maxOutput := 65536
		request.MaxOutputTokens = &maxOutput
		payload, err = apicompat.ResponsesToAnthropicRequest(request)
		apiURL = nativeAnthropicMessagesURL(account, baseURL)
	case APIProtocolChatCompletions:
		payload, err = apicompat.ResponsesToChatCompletionsRequest(request)
		apiURL = buildOpenAIChatCompletionsURL(baseURL)
	}
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if protocol == APIProtocolChatCompletions {
		if normalized, changed := NormalizeGLMOpenAIReasoningEffort(body, model); changed {
			body = normalized
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if protocol == APIProtocolAnthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
		setAnthropicAPIKeyAuthHeader(req.Header, account, authToken, baseURL)
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	applyOpenCodeUpstreamUserAgent(account, apiURL, req.Header)
	account.ApplyHeaderOverrides(req.Header)
	applyOpenCodeSessionHeader(nil, account, apiURL, req.Header, body)
	return req, nil
}

func readOpenCodeAccountTextStream(body io.Reader, protocol string, startedAt time.Time, capture bool) (string, *int64, *int, string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var output strings.Builder
	var firstTokenMS *int64
	stopReason := ""
	appendText := func(text string) bool {
		if firstTokenMS == nil && strings.TrimSpace(text) != "" {
			ms := time.Since(startedAt).Milliseconds()
			firstTokenMS = &ms
		}
		if capture {
			if output.Len()+len(text) > maxIntelligenceOutputBytes {
				return false
			}
			_, _ = output.WriteString(text)
		}
		return true
	}
	completed := func() (string, *int64, *int, string) {
		if firstTokenMS == nil {
			return "", nil, nil, "Upstream completed without text"
		}
		return output.String(), firstTokenMS, nil, ""
	}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			return "", firstTokenMS, nil, "Upstream stream ended before completion"
		}
		if !gjson.ValidBytes(data) {
			return "", firstTokenMS, nil, "Invalid upstream stream event"
		}
		event := gjson.ParseBytes(data)
		if event.Get("error").IsObject() || event.Get("type").String() == "error" {
			return "", firstTokenMS, accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream stream failed", event.Get("error.code").String())
		}
		if protocol == APIProtocolChatCompletions {
			choices := event.Get("choices").Array()
			for _, choice := range choices {
				if choice.Get("index").Int() != 0 {
					continue
				}
				// reasoning_content 是内部思考；首字和结果只认答案 content，不将思考回退成答案。
				content := choice.Get("delta.content")
				if content.Type == gjson.String && !appendText(content.String()) {
					return "", firstTokenMS, nil, "Response text exceeds 1 MiB limit"
				}
				if reason := choice.Get("finish_reason").String(); reason != "" {
					if reason != "stop" {
						return "", firstTokenMS, nil, "Upstream response did not complete"
					}
					return completed()
				}
			}
			continue
		}
		switch event.Get("type").String() {
		case "content_block_start":
			if event.Get("content_block.type").String() == "text" && !appendText(event.Get("content_block.text").String()) {
				return "", firstTokenMS, nil, "Response text exceeds 1 MiB limit"
			}
		case "content_block_delta":
			if event.Get("delta.type").String() == "text_delta" && !appendText(event.Get("delta.text").String()) {
				return "", firstTokenMS, nil, "Response text exceeds 1 MiB limit"
			}
		case "message_delta":
			stopReason = event.Get("delta.stop_reason").String()
		case "message_stop":
			if stopReason != "" && stopReason != "end_turn" && stopReason != "stop_sequence" {
				return "", firstTokenMS, nil, "Upstream response did not complete"
			}
			return completed()
		}
	}
	if scanner.Err() != nil {
		return "", firstTokenMS, nil, "Upstream stream read failed"
	}
	return "", firstTokenMS, nil, "Upstream stream ended before completion"
}
