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

func excerptGroupsTestRouter() (*gin.Engine, *moderationEngineSettings) {
	gin.SetMode(gin.TestMode)
	settings := &moderationEngineSettings{value: `{"chinese_only":true,"api_keys":["private-provider-key"]}`}
	svc := service.NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	h := NewContentModerationHandler(svc)
	router := gin.New()
	router.PUT("/config", h.UpdateConfig)
	router.GET("/config", h.GetConfig)
	return router, settings
}

func TestContentModerationHandlerExcerptGroupsRoundTrip(t *testing.T) {
	router, settings := excerptGroupsTestRouter()
	expected := []service.ContentModerationExcerptGroup{
		{Name: "1000 字组", MaxChars: 1000, UserIDs: []int64{33, 34}},
		{Name: "800 字组", MaxChars: 800, UserIDs: []int64{}},
	}
	for _, tc := range []struct {
		name, body string
		groups     []service.ContentModerationExcerptGroup
	}{
		{"Save", `{"excerpt_groups":[{"name":" 1000 字组 ","max_chars":1000,"user_ids":[33,33,34]},{"name":"800 字组","max_chars":800,"user_ids":[]}]}`, expected},
		{"OmittedKeeps", `{"sample_rate":90}`, expected},
		{"NullKeeps", `{"excerpt_groups":null}`, expected},
		{"EmptyClears", `{"excerpt_groups":[]}`, []service.ContentModerationExcerptGroup{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			for _, method := range []string{http.MethodPut, http.MethodGet} {
				if method == http.MethodGet {
					w = httptest.NewRecorder()
					router.ServeHTTP(w, httptest.NewRequest(method, "/config", nil))
					require.Equal(t, http.StatusOK, w.Code)
				}
				var result struct {
					Data service.ContentModerationConfigView `json:"data"`
				}
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
				require.Equal(t, tc.groups, result.Data.ExcerptGroups)
				require.True(t, result.Data.ChineseOnly)
				require.NotContains(t, w.Body.String(), "private-provider-key")
			}
			var stored service.ContentModerationConfig
			require.NoError(t, json.Unmarshal([]byte(settings.value), &stored))
			require.Equal(t, tc.groups, stored.ExcerptGroups)
			require.True(t, stored.ChineseOnly)
		})
	}
}

func TestContentModerationHandlerExcerptGroupsRejectInvalid(t *testing.T) {
	router, settings := excerptGroupsTestRouter()
	before := settings.value
	for _, tc := range []struct{ name, groups, message string }{
		{"BlankName", `[{"name":"  ","max_chars":1000,"user_ids":[33]}]`, "名称不能为空"},
		{"ZeroLength", `[{"name":"A","max_chars":0,"user_ids":[33]}]`, "1-12000"},
		{"TooLong", `[{"name":"A","max_chars":12001,"user_ids":[33]}]`, "1-12000"},
		{"FractionLength", `[{"name":"A","max_chars":800.5,"user_ids":[33]}]`, "Invalid request"},
		{"FractionUID", `[{"name":"A","max_chars":1000,"user_ids":[33.5]}]`, "Invalid request"},
		{"OverflowUID", `[{"name":"A","max_chars":1000,"user_ids":[9223372036854775808]}]`, "Invalid request"},
		{"StringUID", `[{"name":"A","max_chars":1000,"user_ids":["33"]}]`, "Invalid request"},
		{"NegativeUID", `[{"name":"A","max_chars":1000,"user_ids":[-33]}]`, "正整数"},
		{"ZeroUID", `[{"name":"A","max_chars":1000,"user_ids":[0]}]`, "正整数"},
		{"CrossGroupUID", `[{"name":"A","max_chars":1000,"user_ids":[33]},{"name":"B","max_chars":800,"user_ids":[33]}]`, "UID 33"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(`{"chinese_only":false,"excerpt_groups":`+tc.groups+`}`))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), tc.message)
			require.Equal(t, before, settings.value)
		})
	}
}
