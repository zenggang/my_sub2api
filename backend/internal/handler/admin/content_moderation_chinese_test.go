package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestContentModerationHandlerChineseOnlyRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{}`}
	svc := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	h := NewContentModerationHandler(svc)
	router := gin.New()
	router.PUT("/config", h.UpdateConfig)
	router.GET("/config", h.GetConfig)
	for _, tc := range []struct {
		name, body string
		enabled    bool
	}{
		{"Enable", `{"chinese_only":true}`, true},
		{"PartialUpdatePreserves", `{}`, true},
		{"Disable", `{"chinese_only":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code)
			var stored service.ContentModerationConfig
			require.NoError(t, json.Unmarshal([]byte(settings.value), &stored))
			require.Equal(t, tc.enabled, stored.ChineseOnly)
			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/config", nil))
			require.Equal(t, http.StatusOK, w.Code)
			var result struct {
				Data service.ContentModerationConfigView `json:"data"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Equal(t, tc.enabled, result.Data.ChineseOnly)
		})
	}
}
