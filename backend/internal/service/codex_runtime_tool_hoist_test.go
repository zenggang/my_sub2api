//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// codexRuntimeToolsBody 复刻 Codex 0.166 的真实形状：工具全部在
// input[].additional_tools 里，exec 是嵌在 namespace "functions" 下的 custom 工具。
const codexRuntimeToolsBody = `{
  "model": "gpt-6.1-sol",
  "stream": true,
  "input": [
    {"type":"additional_tools","role":"developer","tools":[
      {"type":"namespace","name":"functions","tools":[
        {"type":"custom","name":"exec","description":"Run JavaScript code"},
        {"type":"function","name":"wait","parameters":{"type":"object"}}
      ]},
      {"type":"namespace","name":"clock","tools":[
        {"type":"function","name":"sleep","parameters":{"type":"object"}}
      ]}
    ]},
    {"type":"message","role":"user","content":"run date -u"}
  ]
}`

func TestHoistCodexRuntimeToolsLiftsAdditionalToolsAndNamespaceCustom(t *testing.T) {
	out, changed, err := hoistCodexRuntimeTools([]byte(codexRuntimeToolsBody))
	require.NoError(t, err)
	require.True(t, changed)

	require.False(t, gjson.GetBytes(out, `input.#(type=="additional_tools")`).Exists(),
		"additional_tools 不能被原样发给上游")

	tools := gjson.GetBytes(out, "tools").Array()
	require.Len(t, tools, 3, "两个 namespace + 提升上来的 custom exec")

	// custom exec 提到顶层，名字保持原样，Codex 回程把它还原成 custom_tool_call。
	var exec gjson.Result
	var functionsNs gjson.Result
	for _, tool := range tools {
		switch tool.Get("name").String() {
		case "exec":
			exec = tool
		case "functions":
			functionsNs = tool
		}
	}
	require.True(t, exec.Exists(), "custom exec 必须被提升到顶层")
	require.Equal(t, "custom", exec.Get("type").String())
	require.Equal(t, "Run JavaScript code", exec.Get("description").String())

	// namespace 里只应剩下 function 子工具。
	require.True(t, functionsNs.Exists())
	children := functionsNs.Get("tools").Array()
	require.Len(t, children, 1)
	require.Equal(t, "wait", children[0].Get("name").String())
}

func TestHoistCodexRuntimeToolsDedupesRepeatedAdditionalTools(t *testing.T) {
	body := `{
      "input": [
        {"type":"additional_tools","tools":[{"type":"function","name":"wait"}]},
        {"type":"message","role":"user","content":"hi"},
        {"type":"additional_tools","tools":[{"type":"function","name":"wait"},{"type":"namespace","name":"ns","tools":[{"type":"custom","name":"exec"}]}]}
      ]
    }`
	out, changed, err := hoistCodexRuntimeTools([]byte(body))
	require.NoError(t, err)
	require.True(t, changed)

	names := []string{}
	for _, tool := range gjson.GetBytes(out, "tools").Array() {
		names = append(names, tool.Get("name").String())
	}
	require.Equal(t, []string{"wait", "ns", "exec"}, names)
}

func TestHoistCodexRuntimeToolsNoopWithoutCodexShape(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"message","role":"user","content":"hi"}],"tools":[{"type":"function","name":"f"}]}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(out))
}

func TestHoistCodexRuntimeToolsLeavesInvalidJSONAlone(t *testing.T) {
	body := []byte(`{"additional_tools": oops`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, string(body), string(out))
}

// TestHoistedCodexToolsLowerToChatFunction 验证提升后的请求在 CC 回退路径上
// 能把 exec 降级成 function 工具发给上游，并在回程被识别为 custom 工具。
func TestHoistedCodexToolsLowerToChatFunction(t *testing.T) {
	out, changed, err := hoistCodexRuntimeTools([]byte(codexRuntimeToolsBody))
	require.NoError(t, err)
	require.True(t, changed)

	var req apicompat.ResponsesRequest
	require.NoError(t, json.Unmarshal(out, &req))
	effective, err := apicompat.EffectiveResponsesTools(&req)
	require.NoError(t, err)
	require.True(t, apicompat.CustomToolNames(effective)["exec"], "回程需要按 custom 还原 exec")

	chatReq, err := apicompat.ResponsesToChatCompletionsRequest(&req)
	require.NoError(t, err)

	var lowered bool
	for _, tool := range chatReq.Tools {
		if tool.Function == nil || tool.Function.Name != "exec" {
			continue
		}
		lowered = true
		require.Equal(t, "function", tool.Type)
		require.Contains(t, string(tool.Function.Parameters), "input")
	}
	require.True(t, lowered, "exec 必须以 function 工具形式出现在上游请求里")
}

// TestHoistCodexRuntimeToolsKeepsInputNonEmpty 覆盖 Codex 首包只带
// additional_tools 的场景：移除后 input 不能变成空数组，否则上游 400
// "Input items array must not be empty"。
func TestHoistCodexRuntimeToolsKeepsInputNonEmpty(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"exec"}]}]}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, gjson.GetBytes(out, "input").Array(), 1, "input 不能被清空")
	require.Len(t, gjson.GetBytes(out, "tools").Array(), 1, "工具仍要提升到顶层")
}

// 同名 namespace 出现在顶层 tools 与 input[].additional_tools 时，必须按子工具
// 合并而不是整体丢弃（review 反馈：增量下发的常见形态）。
func TestHoistMergesSameNamespaceAcrossSources(t *testing.T) {
	body := []byte(`{
	  "model":"compat-model","stream":false,
	  "tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}],
	  "input":[
	    {"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"write","parameters":{"type":"object"}}]}]},
	    {"type":"message","role":"user","content":"hi"}
	  ]
	}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"read", "write"}, namespaceChildren(t, out, "functions"))
}

// 多个 additional_tools item 声明同一个 namespace 时同样要合并。
func TestHoistMergesNamespaceAcrossAdditionalToolsItems(t *testing.T) {
	body := []byte(`{
	  "input":[
	    {"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read"}]}]},
	    {"type":"message","role":"user","content":"hi"},
	    {"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"write"}]}]}
	  ]
	}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"read", "write"}, namespaceChildren(t, out, "functions"))
}

// 后声明的同名 namespace 里的 custom 工具（如 exec）也必须被提升到顶层，
// 否则本 PR 想修的场景在这类形状下依然丢工具。
func TestHoistLiftsCustomFromMergedNamespace(t *testing.T) {
	body := []byte(`{
	  "tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"read"}]}],
	  "input":[
	    {"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]},
	    {"type":"message","role":"user","content":"hi"}
	  ]
	}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"read"}, namespaceChildren(t, out, "functions"))
	require.True(t, hasTopLevelTool(out, "custom", "exec"), "exec 必须提升到顶层")
}

func hasTopLevelTool(body []byte, typ, name string) bool {
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("type").String() == typ && tool.Get("name").String() == name {
			return true
		}
	}
	return false
}

func namespaceChildren(t *testing.T, body []byte, ns string) []string {
	t.Helper()
	var names []string
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		if tool.Get("name").String() != ns || tool.Get("type").String() != "namespace" {
			continue
		}
		for _, child := range tool.Get("tools").Array() {
			names = append(names, child.Get("name").String())
		}
	}
	return names
}
func TestCodexNamespaceToolsEnabledForHTTPAPIKeyAccounts(t *testing.T) {
	require.False(t, (*Account)(nil).CodexNamespaceToolsEnabled())
	for _, platform := range []string{PlatformOpenAI, PlatformOpenCodeGo, PlatformAnthropic} {
		for _, kind := range []string{AccountTypeAPIKey, AccountTypeOAuth, AccountTypeSetupToken} {
			for _, value := range []any{nil, false, true, "true"} {
				account := &Account{Platform: platform, Type: kind, Extra: map[string]any{ExtraKeyCodexNamespaceTools: value}}
				require.Equal(t, (platform == PlatformOpenAI || platform == PlatformOpenCodeGo) && kind == AccountTypeAPIKey && value == true, account.CodexNamespaceToolsEnabled())
			}
		}
	}
}
func TestHoistCodexRuntimeToolsRejectsAmbiguousCustomNames(t *testing.T) {
	for _, body := range []string{
		`{"tools":[{"type":"function","name":"exec"},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`,
		`{"tools":[{"type":"custom","name":"exec"},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}`,
		`{"tools":[{"type":"namespace","name":"a","tools":[{"type":"custom","name":"exec"}]},{"type":"namespace","name":"b","tools":[{"type":"custom","name":"exec"}]}]}`,
	} {
		out, changed, err := hoistCodexRuntimeTools([]byte(body))
		require.ErrorContains(t, err, "cannot disambiguate")
		require.False(t, changed)
		require.Equal(t, body, string(out))
	}
}
func TestHoistCodexRuntimeToolsRejectsMalformedAdditionalTools(t *testing.T) {
	for _, tools := range []string{`"invalid"`, `{}`, `[123]`, `[{"type":"namespace","name":"functions","tools":"invalid"}]`} {
		body := `{"input":[{"type":"additional_tools","tools":` + tools + `},{"role":"user","content":"hi"}]}`
		out, changed, err := hoistCodexRuntimeTools([]byte(body))
		require.ErrorContains(t, err, "additional tools")
		require.False(t, changed)
		require.Equal(t, body, string(out))
	}
}
func TestHoistCodexRuntimeToolsSupportsChildrenAliasAndTopLevelMerge(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"functions","children":[{"type":"function","name":"read"}]},{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}],"input":[{"role":"user","content":"hi"}]}`)
	out, changed, err := hoistCodexRuntimeTools(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, []string{"read"}, namespaceChildren(t, out, "functions"))
	require.True(t, hasTopLevelTool(out, "custom", "exec"))
}
