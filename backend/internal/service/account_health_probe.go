package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const accountHealthProbeTimeout = 90 * time.Second

var accountHealthProbeModelID = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)

type AccountHealthProbeResult struct {
	AccountID             int64  `json:"account_id"`
	ModelID               string `json:"model_id"`
	Success               bool   `json:"success"`
	UpstreamStatusCode    *int   `json:"upstream_status_code"`
	StreamErrorStatusCode *int   `json:"stream_error_status_code"`
	Error                 string `json:"error"`
	FirstTokenMS          *int64 `json:"first_token_ms"`
	DurationMS            int64  `json:"duration_ms"`
}

// ProbeAccountHealth sends one fixed-account text request without changing account runtime state.
func (s *AccountTestService) ProbeAccountHealth(ctx context.Context, accountID int64, modelID string) (result AccountHealthProbeResult, err error) {
	result.AccountID = accountID
	result.ModelID = strings.TrimSpace(modelID)
	if s == nil || s.accountRepo == nil {
		return result, infraerrors.ServiceUnavailable("HEALTH_PROBE_UNAVAILABLE", "Account health probe is unavailable")
	}
	if !accountHealthProbeModelID.MatchString(result.ModelID) {
		return result, infraerrors.BadRequest("INVALID_HEALTH_PROBE_MODEL", "A valid model_id is required")
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if errors.Is(err, ErrAccountNotFound) || account == nil && err == nil {
		return result, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "Account not found")
	}
	if err != nil {
		return result, infraerrors.InternalServer("HEALTH_PROBE_ACCOUNT_LOAD_FAILED", "Failed to load account")
	}
	// A health probe is observational even for disabled or cooling accounts; keep the selected row and never schedule another account.
	defer func() {
		if err == nil {
			status := 0
			if result.UpstreamStatusCode != nil {
				status = *result.UpstreamStatusCode
			}
			streamErrorStatus := 0
			if result.StreamErrorStatusCode != nil {
				streamErrorStatus = *result.StreamErrorStatusCode
			}
			firstTokenMS := int64(0)
			if result.FirstTokenMS != nil {
				firstTokenMS = *result.FirstTokenMS
			}
			slog.Info("account_health_probe_complete", "account_id", accountID, "model_id", result.ModelID,
				"success", result.Success, "upstream_status_code", status, "stream_error_status_code", streamErrorStatus,
				"has_first_token_ms", result.FirstTokenMS != nil,
				"first_token_ms", firstTokenMS,
				"duration_ms", result.DurationMS, "error", result.Error)
		}
	}()
	// Synthetic UI fixtures carry placeholder credentials and cannot establish real upstream health.
	if account.IsSyntheticUITest() {
		result.Error = "Synthetic test account cannot be health-probed"
		return result, nil
	}
	if !account.IsOpenAIOAuthLike() {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_ACCOUNT", "OpenAI OAuth text accounts only")
	}
	if !account.IsModelSupported(result.ModelID) {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_MODEL", "Model is not available on this account")
	}
	// Passthrough ignores legacy model_mapping, matching the model sent by normal account routing.
	upstreamModel := result.ModelID
	if !account.IsOpenAIPassthroughEnabled() {
		upstreamModel = account.GetMappedModel(result.ModelID)
	}

	credentialAccount := account
	if account.IsCredentialShadow() {
		credentialAccount, err = resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			result.Error = "Shadow account credentials are unavailable"
			return result, nil
		}
	}

	var authToken string
	if !credentialAccount.IsOpenAIAgentIdentity() {
		authToken = credentialAccount.GetOpenAIAccessToken()
		if authToken == "" {
			result.Error = "Access token is missing"
			return result, nil
		}
	}
	upstreamModel = normalizeOpenAIModelForUpstream(credentialAccount, upstreamModel)
	if !isAccountHealthProbeTextModel(upstreamModel) {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_MODEL", "Upstream model must be a text model")
	}

	payloadBytes, marshalErr := json.Marshal(createOpenAITestPayload(upstreamModel, true))
	if marshalErr != nil {
		result.Error = "Failed to build test request"
		return result, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, accountHealthProbeTimeout)
	defer cancel()
	req, requestErr := http.NewRequestWithContext(probeCtx, http.MethodPost, chatgptCodexAPIURL, bytes.NewReader(payloadBytes))
	if requestErr != nil {
		result.Error = "Failed to build test request"
		return result, nil
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/json")
	if credentialAccount.IsOpenAIAgentIdentity() {
		// A missing or invalid task must fail the probe; normal authentication may register a task and persist it.
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
	if s.httpUpstream == nil {
		result.Error = "Upstream transport is unavailable"
		return result, nil
	}
	startedAt := time.Now()
	resp, upstreamErr := s.doOpenAIAccountTestUpstream(req, proxyURL, account, true)
	if upstreamErr != nil {
		result.DurationMS = time.Since(startedAt).Milliseconds()
		if errors.Is(upstreamErr, context.DeadlineExceeded) || errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
			result.Error = "Upstream request timed out"
		} else {
			result.Error = "Upstream transport failed"
		}
		return result, nil
	}
	if resp == nil {
		result.DurationMS = time.Since(startedAt).Milliseconds()
		result.Error = "Upstream returned no response"
		return result, nil
	}
	if resp.Body == nil {
		result.DurationMS = time.Since(startedAt).Milliseconds()
		result.Error = "Upstream returned no response body"
		return result, nil
	}
	defer func() { _ = resp.Body.Close() }()
	result.UpstreamStatusCode = &resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		// The upstream body may echo credentials; expose only its bounded error code and HTTP status.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result.DurationMS = time.Since(startedAt).Milliseconds()
		result.Error = accountHealthProbeHTTPError(resp.StatusCode, body)
		return result, nil
	}
	result.FirstTokenMS, result.StreamErrorStatusCode, result.Error = readAccountHealthProbeStream(resp.Body, startedAt)
	result.DurationMS = time.Since(startedAt).Milliseconds()
	if errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		result.Error = "Upstream request timed out"
	}
	result.Success = result.Error == ""
	return result, nil
}

func isAccountHealthProbeTextModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if !accountHealthProbeModelID.MatchString(model) {
		return false
	}
	for _, part := range []string{"image", "video", "audio", "realtime", "compact", "embedding", "moderation", "whisper", "tts", "speech", "sora", "dall-e"} {
		if strings.Contains(model, part) {
			return false
		}
	}
	return true
}

func accountHealthProbeHTTPError(status int, body []byte) string {
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "code").String()
	}
	return accountHealthProbeSafeError(fmt.Sprintf("Upstream returned HTTP %d", status), code)
}

func accountHealthProbeSafeError(message, code string) string {
	// Upstream error.code is untrusted and may contain token-like data despite safe-looking syntax.
	switch code {
	case "server_error", "server_is_overloaded", "slow_down", "invalid_token", "rate_limit_exceeded", "invalid_request_error", "insufficient_quota", "authentication_error", "permission_denied", "model_not_found":
		return message + " (" + code + ")"
	}
	return message
}

func accountHealthProbeExplicitStreamStatus(event gjson.Result) *int {
	// Only an explicit numeric status in a failed event is reportable as 503/504; semantic error names are not HTTP statuses.
	for _, path := range openAIStreamErrorStatusPaths {
		if status := accountHealthProbeNumericStatus(event.Get(path).String()); status != nil {
			return status
		}
	}
	for _, path := range []string{"response.error.code", "error.code", "code"} {
		if status := accountHealthProbeNumericStatus(event.Get(path).String()); status != nil {
			return status
		}
	}
	return nil
}

func accountHealthProbeNumericStatus(raw string) *int {
	raw = strings.TrimSpace(raw)
	status, err := strconv.Atoi(raw)
	if err != nil || status < 400 || status > 599 || strconv.Itoa(status) != raw {
		return nil
	}
	return &status
}

func readAccountHealthProbeStream(body io.Reader, startedAt time.Time) (*int64, *int, string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var firstTokenMS *int64
	completed := false
	markText := func(text string) {
		if firstTokenMS == nil && strings.TrimSpace(text) != "" {
			ms := time.Since(startedAt).Milliseconds()
			firstTokenMS = &ms
		}
	}
	markContent := func(item gjson.Result) {
		for _, content := range item.Get("content").Array() {
			if content.Get("type").String() == "output_text" {
				markText(content.Get("text").String())
			}
		}
	}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			return firstTokenMS, nil, "Upstream stream ended before response.completed"
		}
		// The shared decoder accepts only complete concatenated JSON events; malformed tails fail closed.
		documents, repaired := splitOpenAIConcatenatedJSONDocuments(data)
		if !repaired {
			documents = [][]byte{data}
		}
		for _, document := range documents {
			if !gjson.ValidBytes(document) {
				return firstTokenMS, nil, "Invalid upstream stream event"
			}
			event := gjson.ParseBytes(document)
			if event.Get("error").IsObject() {
				return firstTokenMS, accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream stream failed", event.Get("error.code").String())
			}
			switch event.Get("type").String() {
			case "response.output_text.delta":
				if !completed {
					markText(event.Get("delta").String())
				}
			case "response.output_text.done":
				if !completed {
					markText(event.Get("text").String())
				}
			case "response.content_part.done":
				part := event.Get("part")
				if !completed && part.Get("type").String() == "output_text" {
					markText(part.Get("text").String())
				}
			case "response.output_item.done":
				if !completed {
					markContent(event.Get("item"))
				}
			case "response.completed", "response.done":
				if status := event.Get("response.status").String(); status != "" && status != "completed" {
					return firstTokenMS, accountHealthProbeExplicitStreamStatus(event), "Upstream response did not complete"
				}
				if event.Get("response.error").IsObject() {
					return firstTokenMS, accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream response failed", event.Get("response.error.code").String())
				}
				if !completed {
					for _, item := range event.Get("response.output").Array() {
						markContent(item)
					}
					markText(event.Get("response.output_text").String())
				}
				completed = true
			case "response.failed", "response.incomplete", "error":
				code := event.Get("response.error.code").String()
				if code == "" {
					code = event.Get("error.code").String()
				}
				return firstTokenMS, accountHealthProbeExplicitStreamStatus(event), accountHealthProbeSafeError("Upstream stream failed", code)
			}
		}
		// Completion is terminal after every document on this data line has been checked.
		if completed {
			if firstTokenMS == nil {
				return nil, nil, "Upstream completed without text"
			}
			return firstTokenMS, nil, ""
		}
	}
	if scanner.Err() != nil {
		return firstTokenMS, nil, "Upstream stream read failed"
	}
	return firstTokenMS, nil, "Upstream stream ended before completion"
}
