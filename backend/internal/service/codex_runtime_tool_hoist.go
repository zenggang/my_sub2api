package service

// Adapted from Wei-Shaw/sub2api PR #8592 (7c05ee4f8), HTTP tool hoisting only.
// Codex 的 input[].additional_tools 可以在 namespace 下声明 custom exec。
// Chat Completions 桥原本只展开 namespace function，会丢失这类 custom 工具。
// 这里按账号将其提升到顶层，复用既有 custom 降级、回程还原及续聊逻辑。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// ExtraKeyCodexNamespaceTools 是账号级开关：启用后对 Codex 0.166 形状的
// additional_tools / namespace custom 工具做提升。默认 false。
const ExtraKeyCodexNamespaceTools = "codex_namespace_tools"

// CodexNamespaceToolsEnabled 只允许 API Key 兼容上游按账号启用工具提升。
// OpenCode 的 DeepSeek 等模型共用 HTTP Chat Completions 桥；官方 OAuth 保留原生工具协议。
func (a *Account) CodexNamespaceToolsEnabled() bool {
	if a == nil || a.Type != AccountTypeAPIKey || (!a.IsOpenAI() && !a.IsOpenCodeGo()) {
		return false
	}
	enabled, ok := a.Extra[ExtraKeyCodexNamespaceTools].(bool)
	return ok && enabled
}

// hasCodexNamespaceCustomTools 限定实际改写范围；普通 function 和顶层 custom
// 已受现有桥支持，无需重新编码可能包含长上下文的整个请求。
func hasCodexNamespaceCustomTools(tools []apicompat.ResponsesTool) bool {
	for _, tool := range tools {
		if tool.Type != "namespace" {
			continue
		}
		children := tool.Tools
		if len(children) == 0 {
			children = tool.Children
		}
		for _, child := range children {
			if child.Type == "custom" && child.Name != "" {
				return true
			}
		}
	}
	return false
}

// hoistCodexRuntimeTools 把 input[].additional_tools 里的工具提升到顶层
// tools，并把 namespace 内的 custom 子工具提到顶层。
//
// changed 表示请求体是否被改写；未改写时返回原切片，调用方无需替换。
// 无效 JSON 保留原请求供入口报错；非法工具声明和无法区分的同名 custom 工具
// 返回参数错误，避免把调用发给错误的工具或静默丢失声明。
func hoistCodexRuntimeTools(body []byte) ([]byte, bool, error) {
	if len(body) == 0 || !bodyHasCodexRuntimeTools(body) {
		return body, false, nil
	}
	var root map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &root); err != nil {
		return body, false, nil
	}
	if root == nil {
		return body, false, nil
	}

	changed := false
	tools, _ := root["tools"].([]any)
	lifted, inputChanged, err := liftCodexAdditionalTools(root)
	if err != nil {
		return body, false, err
	}
	if inputChanged {
		changed = true
	}
	if len(lifted) > 0 {
		tools = appendDedupeTools(tools, lifted)
		changed = true
	}
	// 顶层和增量声明都可能重复同一个 namespace，先合并子工具再检查 custom 名称。
	tools = appendDedupeTools(nil, tools)
	hoistedTools, hoisted, err := hoistNamespaceCustomTools(tools)
	if err != nil {
		return body, false, err
	}
	if hoisted {
		tools = hoistedTools
		changed = true
	}
	if !changed {
		return body, false, nil
	}
	if len(tools) > 0 {
		root["tools"] = tools
	}
	rebuilt, err := marshalOpenAIUpstreamJSON(root)
	if err != nil {
		return body, false, nil
	}
	return rebuilt, true, nil
}

// bodyHasCodexRuntimeTools 用廉价字节判断挡掉绝大多数无关请求。
func bodyHasCodexRuntimeTools(body []byte) bool {
	return bytes.Contains(body, []byte(`"additional_tools"`)) ||
		bytes.Contains(body, []byte(`"namespace"`))
}

// liftCodexAdditionalTools 把 input 里的 additional_tools 项搬到顶层，
// 并从 input 中移除声明项；它们不属于用户消息或工具结果正文。
func liftCodexAdditionalTools(root map[string]any) ([]any, bool, error) {
	input, ok := root["input"].([]any)
	if !ok || len(input) == 0 {
		return nil, false, nil
	}
	var lifted []any
	kept := make([]any, 0, len(input))
	changed := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		if strings.TrimSpace(stringValue(item["type"])) != "additional_tools" {
			kept = append(kept, raw)
			continue
		}
		// 移除 input item 前按原协议类型验证，不能吞掉 tools 非数组等参数错误。
		encoded, err := json.Marshal(item["tools"])
		if err != nil {
			return nil, false, fmt.Errorf("encode responses additional tools: %w", err)
		}
		var declarations []apicompat.ResponsesTool
		if err := json.Unmarshal(encoded, &declarations); err != nil {
			return nil, false, fmt.Errorf("parse responses additional tools item: %w", err)
		}
		if children, ok := item["tools"].([]any); ok {
			lifted = append(lifted, children...)
		}
		changed = true
	}
	// 只有声明项时保留原 input，避免兼容层制造空 input；后续 Chat Completions
	// 转换仍按既有规则跳过该声明项，不把它当成消息正文。
	if changed && len(kept) > 0 {
		root["input"] = kept
	}
	return lifted, changed, nil
}

// appendDedupeTools 按 (type, name) 去重追加；同名 namespace 不整体丢弃，而是
// 按子工具身份合并——顶层 tools 与 input[].additional_tools 各自声明同一个
// namespace 是 Codex 增量下发的常见形态，整体去重会把后声明的子工具一起丢掉。
func appendDedupeTools(tools, extra []any) []any {
	if len(extra) == 0 {
		return tools
	}
	out := tools
	index := make(map[string]int, len(tools)+len(extra))
	for i, raw := range out {
		if key, ok := toolDeclKey(raw); ok {
			index[key] = i
		}
	}
	for _, raw := range extra {
		key, ok := toolDeclKey(raw)
		if !ok {
			out = append(out, raw)
			continue
		}
		if at, seen := index[key]; seen {
			if merged, mergedOK := mergeNamespaceChildren(out[at], raw); mergedOK {
				out[at] = merged
			}
			continue
		}
		index[key] = len(out)
		out = append(out, raw)
	}
	return out
}

// mergeNamespaceChildren 合并同名 namespace 的子工具（按 (type, name) 去重），
// 保留两侧新增的声明。非 namespace 或没有新增子工具时返回 false，调用方按原逻辑
// 保留先出现的声明。
func mergeNamespaceChildren(dst, src any) (any, bool) {
	d, ok := dst.(map[string]any)
	if !ok || strings.TrimSpace(stringValue(d["type"])) != "namespace" {
		return nil, false
	}
	s, ok := src.(map[string]any)
	if !ok || strings.TrimSpace(stringValue(s["type"])) != "namespace" {
		return nil, false
	}
	srcChildren := codexNamespaceChildren(s)
	if len(srcChildren) == 0 {
		return nil, false
	}
	dstChildren := codexNamespaceChildren(d)
	seen := make(map[string]bool, len(dstChildren)+len(srcChildren))
	for _, raw := range dstChildren {
		if key, ok := toolDeclKey(raw); ok {
			seen[key] = true
		}
	}
	merged := append([]any(nil), dstChildren...)
	added := false
	for _, raw := range srcChildren {
		if key, ok := toolDeclKey(raw); ok {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		merged = append(merged, raw)
		added = true
	}
	if !added {
		return nil, false
	}
	out := make(map[string]any, len(d)+1)
	for key, value := range d {
		out[key] = value
	}
	out["tools"] = merged
	delete(out, "children")
	return out, true
}

func toolDeclKey(raw any) (string, bool) {
	tool, ok := raw.(map[string]any)
	if !ok {
		return "", false
	}
	name := strings.TrimSpace(stringValue(tool["name"]))
	if name == "" {
		return "", false
	}
	return strings.TrimSpace(stringValue(tool["type"])) + "\x00" + name, true
}

// codexNamespaceChildren 与 Responses namespace 转换保持一致，兼容 children 别名。
func codexNamespaceChildren(namespace map[string]any) []any {
	children, _ := namespace["tools"].([]any)
	if len(children) == 0 {
		children, _ = namespace["children"].([]any)
	}
	return children
}

// hoistNamespaceCustomTools 把 namespace custom 提到顶层，供既有双向 custom 转换使用。
// custom 回程只携带工具名，无法区分不同 namespace 或顶层的同名工具，必须在出站前拒绝。
func hoistNamespaceCustomTools(tools []any) ([]any, bool, error) {
	if len(tools) == 0 {
		return tools, false, nil
	}
	owners := make(map[string]string, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(stringValue(tool["type"]))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name != "" && (typ == "function" || typ == "custom") {
			owners[name] = "a top-level tool"
		}
	}

	changed := false
	hoisted := make([]any, 0, 4)
	for _, raw := range tools {
		namespace, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(namespace["type"])) != "namespace" {
			continue
		}
		children := codexNamespaceChildren(namespace)
		kept := make([]any, 0, len(children))
		for _, rawChild := range children {
			child, ok := rawChild.(map[string]any)
			if !ok {
				kept = append(kept, rawChild)
				continue
			}
			name := strings.TrimSpace(stringValue(child["name"]))
			if strings.TrimSpace(stringValue(child["type"])) != "custom" || name == "" {
				kept = append(kept, rawChild)
				continue
			}
			namespaceName := strings.TrimSpace(stringValue(namespace["name"]))
			if namespaceName == "" {
				return tools, false, fmt.Errorf("custom tool %q requires a named namespace", name)
			}
			if owner, exists := owners[name]; exists {
				return tools, false, fmt.Errorf("namespace custom tool %q/%q conflicts with %s; this upstream cannot disambiguate them, rename one of the tools", namespaceName, name, owner)
			}
			owners[name] = fmt.Sprintf("namespace %q", namespaceName)
			hoisted = append(hoisted, child)
			changed = true
		}
		if len(kept) != len(children) {
			namespace["tools"] = kept
			delete(namespace, "children")
		}
	}
	if !changed {
		return tools, false, nil
	}
	return append(tools, hoisted...), true, nil
}
