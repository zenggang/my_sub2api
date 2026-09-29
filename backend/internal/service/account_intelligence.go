package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const (
	accountIntelligenceTestTimeout = 120 * time.Second
	maxIntelligencePromptRunes     = 10000
	maxIntelligenceOutputBytes     = 1 << 20
)

type AccountIntelligenceTestResult struct {
	AccountID             int64  `json:"account_id"`
	ModelID               string `json:"model_id"`
	Success               bool   `json:"success"`
	OutputText            string `json:"output_text"`
	DurationMS            int64  `json:"duration_ms"`
	UpstreamStatusCode    *int   `json:"upstream_status_code"`
	StreamErrorStatusCode *int   `json:"stream_error_status_code"`
	Error                 string `json:"error"`
}

// TestAccountIntelligence calls only the selected OpenAI OAuth-like account and never updates its state.
func (s *AccountTestService) TestAccountIntelligence(ctx context.Context, accountID int64, modelID, prompt, effort string) (result AccountIntelligenceTestResult, err error) {
	result.AccountID = accountID
	result.ModelID = modelID
	if s == nil || s.accountRepo == nil {
		return result, infraerrors.ServiceUnavailable("INTELLIGENCE_TEST_UNAVAILABLE", "Account intelligence test is unavailable")
	}
	if !accountHealthProbeModelID.MatchString(modelID) {
		return result, infraerrors.BadRequest("INVALID_INTELLIGENCE_TEST_MODEL", "A valid model_id is required")
	}
	if strings.TrimSpace(prompt) == "" || !utf8.ValidString(prompt) || utf8.RuneCountInString(prompt) > maxIntelligencePromptRunes {
		return result, infraerrors.BadRequest("INVALID_INTELLIGENCE_TEST_PROMPT", "Prompt must contain 1 to 10000 characters")
	}
	if effort == "" {
		effort = "medium"
	}
	switch effort {
	case "low", "medium", "high", "xhigh", "max":
	default:
		return result, infraerrors.BadRequest("INVALID_INTELLIGENCE_TEST_EFFORT", "Unsupported reasoning effort")
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if errors.Is(err, ErrAccountNotFound) || account == nil && err == nil {
		return result, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "Account not found")
	}
	if err != nil {
		return result, infraerrors.InternalServer("INTELLIGENCE_TEST_ACCOUNT_LOAD_FAILED", "Failed to load account")
	}
	if !account.IsOpenAIOAuthLike() {
		return result, infraerrors.BadRequest("UNSUPPORTED_INTELLIGENCE_TEST_ACCOUNT", "OpenAI OAuth text accounts only")
	}
	if !account.IsModelSupported(modelID) {
		return result, infraerrors.BadRequest("UNSUPPORTED_INTELLIGENCE_TEST_MODEL", "Model is not available on this account")
	}
	// Match normal account routing: the public alias chooses the model, but passthrough keeps it unchanged.
	upstreamModel := modelID
	if !account.IsOpenAIPassthroughEnabled() {
		upstreamModel = account.GetMappedModel(modelID)
	}
	credentialAccount := account
	if account.IsCredentialShadow() {
		credentialAccount, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			result.Error = "Shadow account credentials are unavailable"
			return result, nil
		}
	}
	upstreamModel = normalizeOpenAIModelForUpstream(credentialAccount, upstreamModel)
	if !isAccountHealthProbeTextModel(upstreamModel) {
		return result, infraerrors.BadRequest("UNSUPPORTED_INTELLIGENCE_TEST_MODEL", "Upstream model must be a text model")
	}

	defer func() {
		if err == nil {
			status := 0
			if result.UpstreamStatusCode != nil {
				status = *result.UpstreamStatusCode
			}
			streamStatus := 0
			if result.StreamErrorStatusCode != nil {
				streamStatus = *result.StreamErrorStatusCode
			}
			slog.Info("account_intelligence_test_complete", "account_id", accountID, "model_id", modelID,
				"reasoning_effort", effort, "success", result.Success, "upstream_status_code", status, "stream_error_status_code", streamStatus,
				"output_bytes", len(result.OutputText), "duration_ms", result.DurationMS, "error", result.Error)
		}
	}()
	if account.IsSyntheticUITest() {
		result.Error = "Synthetic test account cannot be intelligence-tested"
		return result, nil
	}
	if s.httpUpstream == nil {
		result.Error = "Upstream transport is unavailable"
		return result, nil
	}
	var authToken string
	if !credentialAccount.IsOpenAIAgentIdentity() {
		authToken = credentialAccount.GetOpenAIAccessToken()
		if authToken == "" {
			result.Error = "Access token is missing"
			return result, nil
		}
	}
	// The authored prompt is the sole instruction input; do not inherit connectivity-test instructions or add a hidden prompt.
	payload, marshalErr := json.Marshal(map[string]any{
		"model":     upstreamModel,
		"input":     []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": prompt}}}},
		"reasoning": map[string]string{"effort": effort},
		"stream":    true,
		"store":     false,
	})
	if marshalErr != nil {
		result.Error = "Failed to build test request"
		return result, nil
	}

	testCtx, cancel := context.WithTimeout(ctx, accountIntelligenceTestTimeout)
	defer cancel()
	req, requestErr := http.NewRequestWithContext(testCtx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(payload))
	if requestErr != nil {
		result.Error = "Failed to build test request"
		return result, nil
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	if credentialAccount.IsOpenAIAgentIdentity() {
		key, keyErr := agentIdentityKeyFromAccount(credentialAccount)
		if keyErr != nil || key.taskID == "" {
			result.Error = "Agent Identity task or key is unavailable"
			return result, nil
		}
		assertion, signErr := buildAgentAssertion(key, time.Now())
		if signErr != nil {
			result.Error = "Agent Identity signing failed"
			return result, nil
		}
		req.Header.Set("Authorization", assertion)
	} else {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	req.Host = "chatgpt.com"
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	canonical := resolveCodexOutboundIdentity("")
	req.Header.Set("Originator", canonical.originator)
	if customUA := strings.TrimSpace(credentialAccount.GetOpenAIUserAgent()); customUA != "" {
		req.Header.Set("User-Agent", customUA)
	} else {
		req.Header.Set("User-Agent", canonical.userAgent)
	}
	setOpenAIChatGPTAccountHeaders(req.Header, credentialAccount)
	enforceCodexIdentityHeadersWithUA(req.Header, credentialAccount.GetOpenAIUserAgent())
	credentialAccount.ApplyHeaderOverrides(req.Header)

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	startedAt := time.Now()
	resp, upstreamErr := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if upstreamErr != nil {
		result.DurationMS = time.Since(startedAt).Milliseconds()
		if errors.Is(upstreamErr, context.DeadlineExceeded) || errors.Is(testCtx.Err(), context.DeadlineExceeded) {
			result.Error = "Upstream request timed out"
		} else if errors.Is(upstreamErr, context.Canceled) || testCtx.Err() != nil {
			result.Error = "Test request canceled"
		} else {
			result.Error = "Upstream transport failed"
		}
		return result, nil
	}
	if resp == nil || resp.Body == nil {
		result.DurationMS = time.Since(startedAt).Milliseconds()
		result.Error = "Upstream returned no response body"
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()
	result.UpstreamStatusCode = &resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result.DurationMS = time.Since(startedAt).Milliseconds()
		result.Error = accountHealthProbeHTTPError(resp.StatusCode, body)
		return result, nil
	}
	output, streamStatus, streamErr := readAccountIntelligenceStream(resp.Body)
	result.DurationMS = time.Since(startedAt).Milliseconds()
	result.StreamErrorStatusCode = streamStatus
	if errors.Is(testCtx.Err(), context.DeadlineExceeded) {
		result.Error = "Upstream request timed out"
	} else if testCtx.Err() != nil {
		result.Error = "Test request canceled"
	} else {
		result.Error = streamErr
	}
	if result.Error == "" {
		result.Success = true
		result.OutputText = output
	}
	return result, nil
}

func readAccountIntelligenceStream(body io.Reader) (string, *int, string) {
	scanner := bufio.NewScanner(body)
	// A 1 MiB answer can occupy more than 1 MiB in one JSON event after character escaping.
	scanner.Buffer(make([]byte, 4096), 8<<20)
	var deltas, doneText, partText, itemText, completedText strings.Builder
	eventTextTooLong := false
	appendBounded := func(dst *strings.Builder, text string) bool {
		if dst.Len()+len(text) > maxIntelligenceOutputBytes {
			return false
		}
		_, _ = dst.WriteString(text)
		return true
	}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			return "", nil, "Upstream stream ended before response.completed"
		}
		documents, repaired := splitOpenAIConcatenatedJSONDocuments(data)
		if !repaired {
			documents = [][]byte{data}
		}
		completed := false
		for _, document := range documents {
			if !gjson.ValidBytes(document) {
				return "", nil, "Invalid upstream stream event"
			}
			event := gjson.ParseBytes(document)
			if event.Get("error").IsObject() {
				return "", accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream stream failed", event.Get("error.code").String())
			}
			switch event.Get("type").String() {
			case "response.output_text.delta":
				if !appendBounded(&deltas, event.Get("delta").String()) {
					eventTextTooLong = true
				}
			case "response.output_text.done":
				if deltas.Len() == 0 && !appendBounded(&doneText, event.Get("text").String()) {
					eventTextTooLong = true
				}
			case "response.content_part.done":
				part := event.Get("part")
				if deltas.Len() == 0 && doneText.Len() == 0 && part.Get("type").String() == "output_text" && !appendBounded(&partText, part.Get("text").String()) {
					eventTextTooLong = true
				}
			case "response.output_item.done":
				if deltas.Len() == 0 && doneText.Len() == 0 && partText.Len() == 0 {
					for _, content := range event.Get("item.content").Array() {
						if content.Get("type").String() == "output_text" && !appendBounded(&itemText, content.Get("text").String()) {
							eventTextTooLong = true
						}
					}
				}
			case "response.completed":
				if status := event.Get("response.status").String(); status != "" && status != "completed" {
					return "", accountHealthProbeExplicitStreamStatus(event), "Upstream response did not complete"
				}
				if event.Get("response.error").IsObject() {
					return "", accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream response failed", event.Get("response.error.code").String())
				}
				// Completed output is authoritative when the stream carries partial or revised deltas.
				for _, item := range event.Get("response.output").Array() {
					for _, content := range item.Get("content").Array() {
						if content.Get("type").String() == "output_text" && !appendBounded(&completedText, content.Get("text").String()) {
							return "", nil, "Response text exceeds 1 MiB limit"
						}
					}
				}
				if completedText.Len() == 0 && !appendBounded(&completedText, event.Get("response.output_text").String()) {
					return "", nil, "Response text exceeds 1 MiB limit"
				}
				completed = true
			case "response.failed", "response.incomplete", "error":
				code := event.Get("response.error.code").String()
				if code == "" {
					code = event.Get("error.code").String()
				}
				return "", accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream stream failed", code)
			}
		}
		if completed {
			output := completedText.String()
			if output == "" && eventTextTooLong {
				return "", nil, "Response text exceeds 1 MiB limit"
			}
			if output == "" {
				output = deltas.String()
			}
			if output == "" {
				output = doneText.String()
			}
			if output == "" {
				output = partText.String()
			}
			if output == "" {
				output = itemText.String()
			}
			if strings.TrimSpace(output) == "" {
				return "", nil, "Upstream completed without text"
			}
			return output, nil, ""
		}
	}
	if scanner.Err() != nil {
		return "", nil, "Upstream stream read failed"
	}
	return "", nil, "Upstream stream ended before completion"
}
