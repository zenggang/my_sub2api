package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

func excerptGroupsTestService(t *testing.T, cfg *ContentModerationConfig) (*ContentModerationService, *contentModerationRuntimeSettingRepo, *contentModerationTestRepo) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)
	settings := &contentModerationRuntimeSettingRepo{values: map[string]string{
		SettingKeyRiskControlEnabled:      "true",
		SettingKeyContentModerationConfig: string(raw),
	}}
	repo := &contentModerationTestRepo{}
	return &ContentModerationService{
		settingRepo: settings, repo: repo, runtimeCacheTTL: time.Hour,
		asyncQueue: make(chan contentModerationTask, 1024),
		keyHealth:  make(map[string]*contentModerationKeyHealth),
	}, settings, repo
}

func excerptGroupsTestInput(t *testing.T, userID int64, text string) ContentModerationCheckInput {
	t.Helper()
	body, err := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "user", "content": text}}})
	require.NoError(t, err)
	return ContentModerationCheckInput{
		UserID: userID, APIKeyID: 9, Protocol: ContentModerationProtocolOpenAIChat,
		Endpoint: "/v1/chat/completions", Body: body,
	}
}

func excerptGroupsDequeueAndPersist(t *testing.T, svc *ContentModerationService) {
	t.Helper()
	ctx := context.Background()
	task, ok := svc.dequeueAsyncTask(ctx, time.Millisecond)
	require.True(t, ok)
	snapshot, err := svc.loadRuntimeSnapshot(ctx)
	require.NoError(t, err)
	if task.log != nil {
		svc.persistContentModerationLog(ctx, task.config, task.log, task.inputHash, task.recordHash, task.applySideEffects)
		return
	}
	// 与 worker 一致：出队后的审核使用新配置，摘要长度必须仍由 task.input 的准入快照决定。
	queueDelay := 1
	decision := svc.checkSync(ctx, task.input, snapshot.config, task.content, task.inputHash, &queueDelay, false)
	require.True(t, decision.Allowed)
}

func TestContentModerationExcerptGroupsConfigRoundTrip(t *testing.T) {
	ctx := context.Background()
	cfg := defaultContentModerationConfig()
	svc, settings, _ := excerptGroupsTestService(t, cfg)
	view, err := svc.GetConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, view.ExcerptGroups)
	require.Empty(t, view.ExcerptGroups)
	_, err = svc.loadRuntimeSnapshot(ctx)
	require.NoError(t, err)
	groups := []ContentModerationExcerptGroup{
		{Name: "  1000 字组  ", MaxChars: 1000, UserIDs: []int64{33, 33, 34}},
		{Name: "800 字组", MaxChars: 800, UserIDs: []int64{55}},
		{Name: "预留空组", MaxChars: 1},
		{Name: "最大长度", MaxChars: 12000},
	}
	view, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &groups})
	require.NoError(t, err)
	expected := []ContentModerationExcerptGroup{
		{Name: "1000 字组", MaxChars: 1000, UserIDs: []int64{33, 34}},
		{Name: "800 字组", MaxChars: 800, UserIDs: []int64{55}},
		{Name: "预留空组", MaxChars: 1, UserIDs: []int64{}},
		{Name: "最大长度", MaxChars: 12000, UserIDs: []int64{}},
	}
	require.Equal(t, expected, view.ExcerptGroups)
	raw, err := settings.GetValue(ctx, SettingKeyContentModerationConfig)
	require.NoError(t, err)
	var stored ContentModerationConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &stored))
	require.Equal(t, expected, stored.ExcerptGroups)

	// 请求参数、返回视图和运行时名单分别拥有切片，调用方修改其中一个不能污染快照。
	groups[0].UserIDs[0], groups[0].Name = 999, "changed request"
	view.ExcerptGroups[0].UserIDs[0], view.ExcerptGroups[0].Name = 888, "changed response"
	view, err = svc.GetConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, expected, view.ExcerptGroups)
	snapshot := svc.runtimeSnapshot.Load()
	require.Equal(t, 1000, snapshot.excerptRule(33).maxChars)
	require.Equal(t, 500, snapshot.excerptRule(999).maxChars)
	clone := cloneContentModerationConfig(snapshot.config)
	clone.ExcerptGroups[0].UserIDs[0] = 777
	require.Equal(t, int64(33), snapshot.config.ExcerptGroups[0].UserIDs[0])

	view, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{})
	require.NoError(t, err)
	require.Equal(t, expected, view.ExcerptGroups)
	empty := []ContentModerationExcerptGroup{}
	view, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &empty})
	require.NoError(t, err)
	require.NotNil(t, view.ExcerptGroups)
	require.Empty(t, view.ExcerptGroups)
	require.Equal(t, 500, svc.runtimeSnapshot.Load().excerptRule(33).maxChars)

	legacy, err := parseContentModerationConfig(`{"enabled":true,"chinese_only":true}`)
	require.NoError(t, err)
	require.Empty(t, legacy.ExcerptGroups)
	require.True(t, legacy.ChineseOnly)
}

func TestContentModerationExcerptGroupsValidationAtomic(t *testing.T) {
	ctx := context.Background()
	cfg := defaultContentModerationConfig()
	cfg.ChineseOnly = true
	cfg.ExcerptGroups = []ContentModerationExcerptGroup{{Name: "原分组", MaxChars: 1000, UserIDs: []int64{33}}}
	svc, settings, _ := excerptGroupsTestService(t, cfg)
	before, err := svc.loadRuntimeSnapshot(ctx)
	require.NoError(t, err)
	beforeRaw, err := settings.GetValue(ctx, SettingKeyContentModerationConfig)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		groups []ContentModerationExcerptGroup
		match  string
	}{
		{"BlankName", []ContentModerationExcerptGroup{{Name: " \n", MaxChars: 1000}}, "名称不能为空"},
		{"ZeroLimit", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 0}}, "1-12000"},
		{"NegativeLimit", []ContentModerationExcerptGroup{{Name: "A", MaxChars: -1}}, "1-12000"},
		{"AboveLimit", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 12001}}, "1-12000"},
		{"ZeroUID", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{0}}}, "正整数"},
		{"NegativeUID", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{-33}}}, "正整数"},
		{"DuplicateUID", []ContentModerationExcerptGroup{
			{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}, {Name: "B", MaxChars: 800, UserIDs: []int64{33}},
		}, `UID 33 同时出现在摘要分组 "A"（第 1 组）和 "B"（第 2 组）`},
		{"SameNameDuplicateUID", []ContentModerationExcerptGroup{
			{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}, {Name: "A", MaxChars: 800, UserIDs: []int64{33}},
		}, `UID 33 同时出现在摘要分组 "A"（第 1 组）和 "A"（第 2 组）`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disabled := false
			_, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ChineseOnly: &disabled, ExcerptGroups: &tc.groups})
			require.Error(t, err)
			require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
			require.Contains(t, infraerrors.Message(err), tc.match)
			raw, readErr := settings.GetValue(ctx, SettingKeyContentModerationConfig)
			require.NoError(t, readErr)
			require.Equal(t, beforeRaw, raw)
			require.Same(t, before, svc.runtimeSnapshot.Load())
			view, readErr := svc.GetConfig(ctx)
			require.NoError(t, readErr)
			require.True(t, view.ChineseOnly)
		})
	}
	groups := []ContentModerationExcerptGroup{
		{Name: "同名", MaxChars: 1000, UserIDs: []int64{33}}, {Name: "同名", MaxChars: 800, UserIDs: []int64{55}},
	}
	view, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &groups})
	require.NoError(t, err)
	require.Equal(t, groups, view.ExcerptGroups)
}

func TestContentModerationExcerptGroupsCheckModesAndBoundaries(t *testing.T) {
	for _, mode := range []string{ContentModerationModeObserve, ContentModerationModePreBlock, ContentModerationModeLocalRecord} {
		for _, user := range []struct {
			id    int64
			limit int
		}{{10, 500}, {33, 1000}, {55, 800}} {
			for _, inside := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/UID%d/inside=%t", mode, user.id, inside), func(t *testing.T) {
					var submitted []string
					var submittedMu sync.Mutex
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var request struct {
							Input string `json:"input"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}
						submittedMu.Lock()
						submitted = append(submitted, request.Input)
						submittedMu.Unlock()
						_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{}}})
					}))
					defer server.Close()
					cfg := defaultContentModerationConfig()
					cfg.Enabled, cfg.ChineseOnly, cfg.RecordNonHits = true, true, true
					cfg.Mode, cfg.BaseURL, cfg.APIKeys = mode, server.URL, []string{"test-excerpt-key"}
					cfg.ExcerptGroups = []ContentModerationExcerptGroup{
						{Name: "1000", MaxChars: 1000, UserIDs: []int64{33}}, {Name: "800", MaxChars: 800, UserIDs: []int64{55}},
					}
					svc, _, repo := excerptGroupsTestService(t, cfg)
					svc.httpClient = server.Client()
					prefixLength := user.limit - 1
					if !inside {
						prefixLength++
					}
					text := strings.Repeat("😀", prefixLength) + "𠀀" + strings.Repeat("a", 100)
					decision, err := svc.Check(context.Background(), excerptGroupsTestInput(t, user.id, text))
					require.NoError(t, err)
					require.True(t, decision.Allowed)
					if !inside {
						require.Empty(t, svc.asyncQueue)
						require.Empty(t, repo.snapshotLogs())
						submittedMu.Lock()
						require.Empty(t, submitted)
						submittedMu.Unlock()
						return
					}
					excerptGroupsDequeueAndPersist(t, svc)
					logs := repo.snapshotLogs()
					require.Len(t, logs, 1)
					require.Equal(t, strings.Repeat("😀", user.limit-1)+"𠀀", logs[0].InputExcerpt)
					require.Equal(t, user.limit, utf8.RuneCountInString(logs[0].InputExcerpt))
					submittedMu.Lock()
					defer submittedMu.Unlock()
					if mode == ContentModerationModeLocalRecord {
						require.Empty(t, submitted)
					} else {
						require.Equal(t, []string{text}, submitted)
					}
				})
			}
		}
	}
}

func TestContentModerationExcerptGroupsRecordBranches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "audit provider failed", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, action := range []string{ContentModerationActionKeywordBlock, ContentModerationActionHashBlock, ContentModerationActionError} {
		t.Run(action, func(t *testing.T) {
			cfg := defaultContentModerationConfig()
			cfg.Enabled, cfg.RecordNonHits = true, true
			cfg.BaseURL, cfg.APIKeys, cfg.RetryCount = server.URL, []string{"test-excerpt-key"}, 0
			cfg.ExcerptGroups = []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}}
			// 中文过滤关闭时仍按 UID 留档；英文、标点和补充平面字符都各算一个 rune。
			text := "risk!" + strings.Repeat("😀", 1100)
			svc, _, repo := excerptGroupsTestService(t, cfg)
			svc.httpClient = server.Client()
			switch action {
			case ContentModerationActionKeywordBlock:
				keywords := []string{"risk"}
				_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{BlockedKeywords: &keywords})
				require.NoError(t, err)
			case ContentModerationActionHashBlock:
				enabled := true
				_, err := svc.UpdateConfig(context.Background(), UpdateContentModerationConfigInput{PreHashCheckEnabled: &enabled})
				require.NoError(t, err)
				svc.hashCache = &contentModerationTestHashCache{hasResultUsed: true, hasResult: true}
			}
			decision, err := svc.Check(context.Background(), excerptGroupsTestInput(t, 33, text))
			require.NoError(t, err)
			if action == ContentModerationActionError {
				require.True(t, decision.Allowed)
				require.Empty(t, svc.asyncQueue)
			} else {
				require.True(t, decision.Blocked)
				excerptGroupsDequeueAndPersist(t, svc)
			}
			logs := repo.snapshotLogs()
			require.Len(t, logs, 1)
			require.Equal(t, action, logs[0].Action)
			require.Equal(t, "risk!"+strings.Repeat("😀", 995), logs[0].InputExcerpt)
		})
	}
}

func TestContentModerationExcerptGroupsHotUpdatesAndRecovery(t *testing.T) {
	ctx := context.Background()
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.ChineseOnly, cfg.Mode = true, true, ContentModerationModeLocalRecord
	svc, settings, repo := excerptGroupsTestService(t, cfg)
	text := strings.Repeat("a", 699) + "中" + strings.Repeat("😀", 500)
	check := func(limit int) {
		t.Helper()
		_, err := svc.Check(ctx, excerptGroupsTestInput(t, 33, text))
		require.NoError(t, err)
		if limit == 500 {
			require.Empty(t, svc.asyncQueue)
			return
		}
		excerptGroupsDequeueAndPersist(t, svc)
		logs := repo.snapshotLogs()
		require.Equal(t, trimRunes(text, limit), logs[len(logs)-1].InputExcerpt)
	}
	check(500)
	for _, tc := range []struct {
		name   string
		groups []ContentModerationExcerptGroup
		limit  int
	}{
		{"AddUID", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}}, 1000},
		{"ChangeLength", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 800, UserIDs: []int64{33}}}, 800},
		{"RemoveUID", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 800, UserIDs: []int64{}}}, 500},
		{"ReAddUID", []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}}, 1000},
		{"DeleteGroup", []ContentModerationExcerptGroup{}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &tc.groups})
			require.NoError(t, err)
			check(tc.limit)
		})
	}
	groups := []ContentModerationExcerptGroup{{Name: "恢复后仍生效", MaxChars: 800, UserIDs: []int64{33}}}
	_, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &groups})
	require.NoError(t, err)
	restarted := &ContentModerationService{settingRepo: settings, repo: repo, asyncQueue: make(chan contentModerationTask, 1)}
	_, err = restarted.Check(ctx, excerptGroupsTestInput(t, 33, text))
	require.NoError(t, err)
	excerptGroupsDequeueAndPersist(t, restarted)
	logs := repo.snapshotLogs()
	require.Equal(t, trimRunes(text, 800), logs[len(logs)-1].InputExcerpt)

	// 摘要配置摘要值未变化时的缓存刷新必须继续复用 UID 映射。
	before := restarted.runtimeSnapshot.Load()
	restarted.runtimeRefreshMu.Lock()
	after, err := restarted.refreshRuntimeSnapshot(ctx)
	restarted.runtimeRefreshMu.Unlock()
	require.NoError(t, err)
	require.Equal(t, before.configDigest, after.configDigest)
	require.Equal(t, 800, after.excerptRule(33).maxChars)
}

func TestContentModerationExcerptGroupsAsyncAdmissionSnapshot(t *testing.T) {
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(moderationAPIResponse{Results: []moderationAPIResult{{}}})
	}))
	defer server.Close()
	for _, change := range []string{"ChangeLength", "RemoveUID", "DeleteGroup"} {
		t.Run(change, func(t *testing.T) {
			cfg := defaultContentModerationConfig()
			cfg.Enabled, cfg.ChineseOnly, cfg.RecordNonHits = true, true, true
			cfg.Mode, cfg.BaseURL, cfg.APIKeys = ContentModerationModeObserve, server.URL, []string{"test-excerpt-key"}
			cfg.ExcerptGroups = []ContentModerationExcerptGroup{{Name: "A", MaxChars: 1000, UserIDs: []int64{33}}}
			svc, _, repo := excerptGroupsTestService(t, cfg)
			svc.httpClient = server.Client()
			text := strings.Repeat("a", 899) + "中" + strings.Repeat("b", 200)
			_, err := svc.Check(ctx, excerptGroupsTestInput(t, 33, text))
			require.NoError(t, err)
			require.Len(t, svc.asyncQueue, 1)
			groups := []ContentModerationExcerptGroup{{Name: "A", MaxChars: 800, UserIDs: []int64{33}}}
			if change == "RemoveUID" {
				groups[0].UserIDs = []int64{}
			}
			if change == "DeleteGroup" {
				groups = []ContentModerationExcerptGroup{}
			}
			_, err = svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &groups})
			require.NoError(t, err)
			excerptGroupsDequeueAndPersist(t, svc)
			logs := repo.snapshotLogs()
			require.Len(t, logs, 1)
			require.Equal(t, trimRunes(text, 1000), logs[0].InputExcerpt)
			// 只有已经准入的请求保留 1000；修改后新请求按 800/500 的窗口被中文条件排除。
			_, err = svc.Check(ctx, excerptGroupsTestInput(t, 33, text))
			require.NoError(t, err)
			require.Empty(t, svc.asyncQueue)
			require.Len(t, repo.snapshotLogs(), 1)
		})
	}
}

func TestContentModerationExcerptGroupsConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	cfg := defaultContentModerationConfig()
	cfg.Enabled, cfg.ChineseOnly, cfg.Mode = true, true, ContentModerationModeLocalRecord
	cfg.ExcerptGroups = []ContentModerationExcerptGroup{{Name: "A", MaxChars: 800, UserIDs: []int64{33}}}
	svc, _, _ := excerptGroupsTestService(t, cfg)
	before, err := svc.loadRuntimeSnapshot(ctx)
	require.NoError(t, err)
	input := excerptGroupsTestInput(t, 33, "中"+strings.Repeat("😀", 1199))
	errs := make(chan error, 5)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 30 {
				if _, err := svc.Check(ctx, input); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 30 {
			groups := []ContentModerationExcerptGroup{{Name: "A", MaxChars: 800 + 200*(i%2), UserIDs: []int64{33}}}
			if _, err := svc.UpdateConfig(ctx, UpdateContentModerationConfigInput{ExcerptGroups: &groups}); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 800, before.excerptRule(33).maxChars)
	require.Len(t, svc.asyncQueue, 120)
	for len(svc.asyncQueue) > 0 {
		task := <-svc.asyncQueue
		require.NotNil(t, task.log)
		length := utf8.RuneCountInString(task.log.InputExcerpt)
		require.Contains(t, []int{800, 1000}, length)
		require.Equal(t, trimRunes("中"+strings.Repeat("😀", 1199), length), task.log.InputExcerpt)
	}
}
