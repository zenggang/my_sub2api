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
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/tidwall/gjson"
)

const accountHealthProbeTimeout = 90 * time.Second

var accountHealthProbeModelID = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,160}$`)
var accountHealthProbeErrorCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type AccountHealthProbeResult struct {
	AccountID          int64  `json:"account_id"`
	ModelID            string `json:"model_id"`
	Success            bool   `json:"success"`
	UpstreamStatusCode *int   `json:"upstream_status_code"`
	Error              string `json:"error"`
	FirstTokenMS       *int64 `json:"first_token_ms"`
	DurationMS         int64  `json:"duration_ms"`
}

// ProbeAccountHealth sends one fixed-account text request without changing account runtime state.
func (s *AccountTestService) ProbeAccountHealth(ctx context.Context, accountID int64, modelID string) (result AccountHealthProbeResult, err error) {
	result.AccountID = accountID
	result.ModelID = strings.TrimSpace(modelID)
	if s == nil || s.accountRepo == nil {
		return result, infraerrors.ServiceUnavailable("HEALTH_PROBE_UNAVAILABLE", "Account health probe is unavailable")
	}
	if !accountHealthProbeModelID.MatchString(result.ModelID) || !isAccountHealthProbeTextModel(result.ModelID) {
		return result, infraerrors.BadRequest("INVALID_HEALTH_PROBE_MODEL", "A text model_id is required")
	}

	account, err := s.accountRepo.GetByID(ctx, accountID)
	if errors.Is(err, ErrAccountNotFound) || account == nil && err == nil {
		return result, infraerrors.NotFound("ACCOUNT_NOT_FOUND", "Account not found")
	}
	if err != nil {
		return result, infraerrors.InternalServer("HEALTH_PROBE_ACCOUNT_LOAD_FAILED", "Failed to load account")
	}
	if !account.IsOpenAIOAuthLike() {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_ACCOUNT", "OpenAI OAuth text accounts only")
	}
	if !account.IsModelSupported(result.ModelID) {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_MODEL", "Model is not available on this account")
	}
	upstreamModel := account.GetMappedModel(result.ModelID)
	if !isAccountHealthProbeTextModel(upstreamModel) {
		return result, infraerrors.BadRequest("UNSUPPORTED_HEALTH_PROBE_MODEL", "Mapped model must be a text model")
	}

	// A health probe is observational even for disabled or cooling accounts; keep the selected row and never schedule another account.
	defer func() {
		if err == nil {
			status := 0
			if result.UpstreamStatusCode != nil {
				status = *result.UpstreamStatusCode
			}
			firstTokenMS := int64(0)
			if result.FirstTokenMS != nil {
				firstTokenMS = *result.FirstTokenMS
			}
			slog.Info("account_health_probe_complete", "account_id", accountID, "model_id", result.ModelID,
				"success", result.Success, "upstream_status_code", status, "has_first_token_ms", result.FirstTokenMS != nil,
				"first_token_ms", firstTokenMS,
				"duration_ms", result.DurationMS, "error", result.Error)
		}
	}()

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
	result.FirstTokenMS, result.Error = readAccountHealthProbeStream(resp.Body, startedAt)
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
	if accountHealthProbeErrorCode.MatchString(code) {
		return message + " (" + code + ")"
	}
	return message
}

func readAccountHealthProbeStream(body io.Reader, startedAt time.Time) (*int64, string) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var firstTokenMS *int64
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
			return firstTokenMS, "Upstream stream ended before response.completed"
		}
		if !gjson.ValidBytes(data) {
			return firstTokenMS, "Invalid upstream stream event"
		}
		event := gjson.ParseBytes(data)
		if event.Get("error").IsObject() {
			return firstTokenMS, accountHealthProbeSafeError("Upstream stream failed", event.Get("error.code").String())
		}
		switch event.Get("type").String() {
		case "response.output_text.delta":
			markText(event.Get("delta").String())
		case "response.output_text.done":
			markText(event.Get("text").String())
		case "response.content_part.done":
			part := event.Get("part")
			if part.Get("type").String() == "output_text" {
				markText(part.Get("text").String())
			}
		case "response.output_item.done":
			markContent(event.Get("item"))
		case "response.completed", "response.done":
			if status := event.Get("response.status").String(); status != "" && status != "completed" {
				return firstTokenMS, "Upstream response did not complete"
			}
			if event.Get("response.error").IsObject() {
				return firstTokenMS, accountHealthProbeSafeError("Upstream response failed", event.Get("response.error.code").String())
			}
			for _, item := range event.Get("response.output").Array() {
				markContent(item)
			}
			markText(event.Get("response.output_text").String())
			if firstTokenMS == nil {
				return nil, "Upstream completed without text"
			}
			return firstTokenMS, ""
		case "response.failed", "response.incomplete", "error":
			code := event.Get("response.error.code").String()
			if code == "" {
				code = event.Get("error.code").String()
			}
			return firstTokenMS, accountHealthProbeSafeError("Upstream stream failed", code)
		}
	}
	if scanner.Err() != nil {
		return firstTokenMS, "Upstream stream read failed"
	}
	return firstTokenMS, "Upstream stream ended before completion"
}
