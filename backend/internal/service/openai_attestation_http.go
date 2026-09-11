package service

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
)

const (
	openAIAttestationHeader    = "x-oai-attestation"
	openAIAttestationMaxBytes  = 64 * 1024
	openAIAttestationCodexPath = "/backend-api/codex/responses"
)

// applyOpenAIAttestationHTTPForwarding forwards only a client-provided
// attestation to the ChatGPT OAuth/SetupToken Responses endpoint. It never
// creates, normalizes, or persists an attestation value.
func (s *OpenAIGatewayService) applyOpenAIAttestationHTTPForwarding(c *gin.Context, req *http.Request, account *Account, targetURL string) error {
	if s == nil || s.cfg == nil || c == nil || c.Request == nil || req == nil || account == nil {
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
		return fmt.Errorf("malformed %s: duplicate values", openAIAttestationHeader)
	}
	value := values[0]
	if err := validateOpenAIAttestationValue(value); err != nil {
		return err
	}
	req.Header.Set(openAIAttestationHeader, value)
	return nil
}

func observeOpenAIAttestationHTTP(c *gin.Context, account *Account, targetURL string) {
	if c == nil || c.Request == nil || account == nil || !account.IsOpenAIOAuthLike() || !isOpenAIAttestationCodexTarget(targetURL) {
		return
	}
	values := incomingOpenAIAttestationValues(c.Request.Header)
	fields := []any{"transport", "http", "target", "codex", "present", len(values) > 0, "value_count", len(values)}
	if len(values) == 1 {
		fields = append(fields, "length", len(values[0]), "malformed", validateOpenAIAttestationValue(values[0]) != nil)
	}
	slog.Debug("openai_attestation_observed", fields...)
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

func validateOpenAIAttestationValue(value string) error {
	if len(value) == 0 || len(value) > openAIAttestationMaxBytes {
		return fmt.Errorf("malformed %s: invalid length", openAIAttestationHeader)
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || r == unicode.ReplacementChar }) >= 0 {
		return fmt.Errorf("malformed %s: control character", openAIAttestationHeader)
	}
	return nil
}
