package service

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

const (
	openAIAttestationHeader         = "x-oai-attestation"
	openAIAttestationMaxBytes       = 64 * 1024
	openAIAttestationCodexPath      = "/backend-api/codex/responses"
	openAIAttestationFailoverReason = GatewayFailureReason("openai_attestation_auth_failure")
	openAIAttestationSwitchCountKey = "openai_attestation_switch_count"
)

// SetOpenAIAttestationSwitchCount exposes request-local account-switch state
// to the service layer. It is used only to cap a proof-bearing request at one
// new OAuth account attempt; it is never persisted.
func SetOpenAIAttestationSwitchCount(c *gin.Context, count int) {
	if c == nil {
		return
	}
	if count < 0 {
		count = 0
	}
	c.Set(openAIAttestationSwitchCountKey, count)
}

// applyOpenAIAttestationHTTPForwarding forwards only a client-provided
// attestation to the ChatGPT OAuth/SetupToken Responses endpoint. It never
// creates, normalizes, or persists an attestation value.
func (s *OpenAIGatewayService) applyOpenAIAttestationHTTPForwarding(c *gin.Context, req *http.Request, account *Account, targetURL string) error {
	if req == nil || account == nil {
		return nil
	}
	// The header is opt-in and target-gated. Remove any value that may have
	// entered through a generic override before evaluating the forwarding mode.
	deleteOpenAIAttestationHeader(req.Header)
	if s == nil || s.cfg == nil || c == nil || c.Request == nil {
		return nil
	}
	mode := strings.ToLower(strings.TrimSpace(s.cfg.Gateway.OpenAIAttestation.Mode))
	bridgeContext, _ := c.Get("openai_ws_http_bridge")
	if bridge, ok := bridgeContext.(bool); ok && bridge && mode != config.OpenAIAttestationModeAll {
		return nil
	}
	if mode == config.OpenAIAttestationModeObserve {
		observeOpenAIAttestationHTTP(c, account, targetURL)
		return nil
	}
	if mode != config.OpenAIAttestationModeHTTP && mode != config.OpenAIAttestationModeAll {
		return nil
	}
	if !account.IsOpenAIOAuthLike() || !isOpenAIAttestationCodexTarget(targetURL) {
		return nil
	}
	values := incomingOpenAIAttestationValues(c.Request.Header)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		logOpenAIAttestationHTTP(c, account, targetURL, values, false, true)
		return fmt.Errorf("malformed %s: duplicate values", openAIAttestationHeader)
	}
	value := values[0]
	if err := validateOpenAIAttestationValue(value); err != nil {
		logOpenAIAttestationHTTP(c, account, targetURL, values, false, true)
		return err
	}
	req.Header.Set(openAIAttestationHeader, value)
	logOpenAIAttestationHTTP(c, account, targetURL, values, true, false)
	return nil
}

// guardOpenAIAttestationFailover prevents an authentication/risk response from
// being replayed against another OAuth account with the same client proof.
// Capacity/503 errors retain the existing account-pool behavior.
func (s *OpenAIGatewayService) guardOpenAIAttestationFailover(c *gin.Context, account *Account, failoverErr *UpstreamFailoverError) *UpstreamFailoverError {
	if failoverErr == nil || s == nil || s.cfg == nil || account == nil || !account.IsOpenAIOAuthLike() {
		return failoverErr
	}
	mode := strings.ToLower(strings.TrimSpace(s.cfg.Gateway.OpenAIAttestation.Mode))
	if mode != config.OpenAIAttestationModeHTTP && mode != config.OpenAIAttestationModeAll || c == nil || c.Request == nil {
		return failoverErr
	}
	values := incomingOpenAIAttestationValues(c.Request.Header)
	if len(values) != 1 || validateOpenAIAttestationValue(values[0]) != nil {
		return failoverErr
	}
	authFailure := failoverErr.StatusCode == http.StatusUnauthorized || failoverErr.StatusCode == http.StatusForbidden
	switchCount := 0
	if raw, ok := c.Get(openAIAttestationSwitchCountKey); ok {
		if count, ok := raw.(int); ok && count > 0 {
			switchCount = count
		}
	}
	if !authFailure && switchCount < 1 {
		return failoverErr
	}
	failoverErr.RetryableOnSameAccount = false
	failoverErr.SameAccountRetryDelay = 0
	failoverErr.SameAccountRetryDeadline = time.Time{}
	failoverErr.SameAccountRetryMax = 0
	failoverErr.NextAccountAction = NextAccountStop
	failoverErr.Scope = GatewayFailureScopeRequest
	failoverErr.Reason = openAIAttestationFailoverReason
	return failoverErr
}

func observeOpenAIAttestationHTTP(c *gin.Context, account *Account, targetURL string) {
	if c == nil || c.Request == nil || account == nil || !account.IsOpenAIOAuthLike() || !isOpenAIAttestationCodexTarget(targetURL) {
		return
	}
	values := incomingOpenAIAttestationValues(c.Request.Header)
	logOpenAIAttestationHTTP(c, account, targetURL, values, false, len(values) != 1 && len(values) > 0)
}

// logOpenAIAttestationHTTP records only safe forwarding metadata. The opaque
// token and any stable digest are deliberately excluded from production logs.
func logOpenAIAttestationHTTP(c *gin.Context, account *Account, targetURL string, values []string, forwarded, malformed bool) {
	if c == nil || c.Request == nil || account == nil || !isOpenAIAttestationCodexTarget(targetURL) || len(values) == 0 {
		return
	}
	fields := []any{"transport", "http", "target", "codex", "present", len(values) > 0, "value_count", len(values)}
	if len(values) == 1 {
		valueMalformed := validateOpenAIAttestationValue(values[0]) != nil
		fields = append(fields, "length", len(values[0]), "malformed", malformed || valueMalformed, "forwarded", forwarded)
		if !valueMalformed {
			var envelope struct {
				Version *int `json:"v"`
				Status  *int `json:"s"`
			}
			if err := json.Unmarshal([]byte(values[0]), &envelope); err == nil {
				if envelope.Version != nil {
					fields = append(fields, "v", *envelope.Version)
				}
				if envelope.Status != nil {
					fields = append(fields, "s", *envelope.Status)
				}
			}
		}
	} else {
		fields = append(fields, "malformed", malformed, "forwarded", false)
	}
	requestID := c.GetString("request_id")
	if requestID == "" {
		requestID = c.GetHeader("x-client-request-id")
	}
	if requestID != "" {
		fields = append(fields, "request_id", requestID)
	}
	slog.Info("openai_attestation_forwarding", fields...)
}

func isOpenAIAttestationCodexTarget(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(u.Scheme, "https") || !strings.EqualFold(u.Hostname(), "chatgpt.com") {
		return false
	}
	path := u.EscapedPath()
	return path == openAIAttestationCodexPath || strings.HasPrefix(path, openAIAttestationCodexPath+"/")
}

func incomingOpenAIAttestationValues(headers http.Header) []string {
	var values []string
	for key, entries := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), openAIAttestationHeader) {
			continue
		}
		values = append(values, entries...)
	}
	return values
}

// deleteOpenAIAttestationHeader removes every casing variant. Header.Del only
// canonicalizes its argument, while account/header override paths may have
// constructed a map entry with a non-canonical key.
func deleteOpenAIAttestationHeader(headers http.Header) {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), openAIAttestationHeader) {
			delete(headers, key)
		}
	}
}

func validateOpenAIAttestationValue(value string) error {
	if len(value) == 0 || len(value) > openAIAttestationMaxBytes {
		return fmt.Errorf("malformed %s: invalid length", openAIAttestationHeader)
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || r == unicode.ReplacementChar }) >= 0 {
		return fmt.Errorf("malformed %s: control character", openAIAttestationHeader)
	}
	return nil
}

// validateOpenAIAttestationForWS applies the same hard gate as HTTP before a
// WebSocket handshake is attempted. The raw value is deliberately not copied
// into the reusable handshake header map; callers materialize it only for the
// individual dial.
func validateOpenAIAttestationForWS(cfg *config.Config, account *Account, headers http.Header) error {
	if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg.Gateway.OpenAIAttestation.Mode), config.OpenAIAttestationModeAll) || account == nil || !account.IsOpenAIOAuthLike() {
		return nil
	}
	values := incomingOpenAIAttestationValues(headers)
	if len(values) == 0 {
		return nil
	}
	if len(values) != 1 {
		return fmt.Errorf("malformed %s: duplicate values", openAIAttestationHeader)
	}
	return validateOpenAIAttestationValue(values[0])
}
