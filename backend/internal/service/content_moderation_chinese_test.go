package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestContentModerationCheck_ChineseExcerptFilter(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		chinese bool
	}{
		{name: "English", text: "Please review this code"},
		{name: "NumbersAndSymbols", text: "12345 !@#$ 😀"},
		{name: "Mixed", text: "Please review 中文 code", chinese: true},
		{name: "Traditional", text: "請檢查訂單", chinese: true},
		{name: "ExtendedHan", text: "𠀀", chinese: true},
		{name: "ChineseAt500", text: strings.Repeat("a", 499) + "中", chinese: true},
		{name: "ChineseAt501", text: strings.Repeat("a", 500) + "中"},
		{name: "ChineseAfter240", text: strings.Repeat("a", 300) + "中文", chinese: true},
	}
	for _, mode := range []string{ContentModerationModePreBlock, ContentModerationModeObserve} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				var calls atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{}}})
				}))
				defer server.Close()
				cfg := defaultContentModerationConfig()
				cfg.Enabled, cfg.ChineseOnly, cfg.RecordNonHits, cfg.PreHashCheckEnabled = true, true, true, true
				cfg.Mode, cfg.BaseURL, cfg.APIKeys = mode, server.URL, []string{"sk-test"}
				raw, err := json.Marshal(cfg)
				require.NoError(t, err)
				cache := &contentModerationTestHashCache{}
				// 不启动后台 worker，直接核对是否入队，避免等待时序掩盖范围外请求的审计工作。
				svc := &ContentModerationService{
					settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
						SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
					}},
					repo: &contentModerationTestRepo{}, hashCache: cache,
					asyncQueue: make(chan contentModerationTask, 1), httpClient: server.Client(),
					keyHealth: make(map[string]*contentModerationKeyHealth),
				}
				body, err := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": tc.text}}})
				require.NoError(t, err)
				decision, err := svc.Check(context.Background(), ContentModerationCheckInput{Protocol: ContentModerationProtocolOpenAIChat, Body: body})
				require.NoError(t, err)
				require.True(t, decision.Allowed)
				if !tc.chinese {
					require.Zero(t, calls.Load())
					require.Empty(t, svc.asyncQueue)
					require.Empty(t, cache.snapshotChecked())
					return
				}
				require.Len(t, cache.snapshotChecked(), 1)
				require.Len(t, svc.asyncQueue, 1)
				task := <-svc.asyncQueue
				if mode == ContentModerationModeObserve {
					require.Zero(t, calls.Load())
					require.Equal(t, tc.text, task.content.Text)
				} else {
					require.EqualValues(t, 1, calls.Load())
					require.Equal(t, trimRunes(tc.text, 500), task.log.InputExcerpt)
				}
			})
		}
	}
}

func TestContentModerationCheck_ChineseFilterSkipsKeywordAndHash(t *testing.T) {
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.ChineseOnly, cfg.PreHashCheckEnabled = true, true, true
	cfg.BlockedKeywords = []string{"secret-token"}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	cache := &contentModerationTestHashCache{}
	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
			SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
		}},
		repo: &contentModerationTestRepo{}, hashCache: cache,
		asyncQueue: make(chan contentModerationTask, 1),
	}
	decision, err := svc.Check(context.Background(), ContentModerationCheckInput{
		Protocol: ContentModerationProtocolOpenAIChat,
		Body:     []byte(`{"messages":[{"role":"user","content":"secret-token"}]}`),
	})
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	require.Empty(t, svc.asyncQueue)
	require.Empty(t, cache.snapshotChecked())
}

func TestContentModerationCheck_ChineseFilterUsesCurrentUserAcrossProtocols(t *testing.T) {
	cases := []struct{ protocol, body string }{
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"system","content":"中文规则"},{"role":"user","content":"历史中文"},{"role":"user","content":"English"}]}`},
		{ContentModerationProtocolAnthropicMessages, `{"system":"中文规则","messages":[{"role":"user","content":"历史中文"},{"role":"user","content":"English"}]}`},
		{ContentModerationProtocolOpenAIResponses, `{"instructions":"中文规则","input":[{"role":"user","content":"历史中文"},{"role":"user","content":"English"}]}`},
		{ContentModerationProtocolGemini, `{"systemInstruction":{"parts":[{"text":"中文规则"}]},"contents":[{"role":"user","parts":[{"text":"历史中文"}]},{"role":"user","parts":[{"text":"English"}]}]}`},
		{ContentModerationProtocolOpenAIImages, `{"prompt":"English"}`},
		{ContentModerationProtocolTypeSafeSystemOne, `{"questions":{"q":{"type":"boolean","claim":"English"}},"state":{}}`},
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.png"}}]}]}`},
		{ContentModerationProtocolOpenAIChat, `{"messages":[{"role":"user","content":"中文任务"},{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","content":"中文结果"}]}`},
	}
	for i, tc := range cases {
		t.Run(tc.protocol+"/"+string(rune('A'+i)), func(t *testing.T) {
			cfg := defaultContentModerationConfig()
			cfg.Enabled, cfg.ChineseOnly = true, true
			cfg.BlockedKeywords = []string{"English"}
			raw, err := json.Marshal(cfg)
			require.NoError(t, err)
			svc := &ContentModerationService{
				settingRepo: &contentModerationTestSettingRepo{values: map[string]string{
					SettingKeyRiskControlEnabled: "true", SettingKeyContentModerationConfig: string(raw),
				}},
				repo: &contentModerationTestRepo{}, asyncQueue: make(chan contentModerationTask, 1),
			}
			decision, err := svc.Check(context.Background(), ContentModerationCheckInput{Protocol: tc.protocol, Body: []byte(tc.body)})
			require.NoError(t, err)
			require.True(t, decision.Allowed)
			require.Empty(t, svc.asyncQueue)
		})
	}
}

func TestContentModerationUpdateConfig_ChineseOnly(t *testing.T) {
	settings := &contentModerationTestSettingRepo{values: map[string]string{}}
	svc := NewContentModerationService(settings, nil, nil, nil, nil, nil, nil, nil)
	view, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.False(t, view.ChineseOnly)
	for _, enabled := range []bool{true, false} {
		view, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{ChineseOnly: &enabled})
		require.NoError(t, err)
		require.Equal(t, enabled, view.ChineseOnly)
		var stored ContentModerationConfig
		require.NoError(t, json.Unmarshal([]byte(settings.values[SettingKeyContentModerationConfig]), &stored))
		require.Equal(t, enabled, stored.ChineseOnly)
		view, err = svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{})
		require.NoError(t, err)
		require.Equal(t, enabled, view.ChineseOnly)
	}
}
