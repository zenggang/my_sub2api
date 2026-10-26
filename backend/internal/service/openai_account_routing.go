package service

import "net/http"

const (
	openAIAccountRoutingUSCREnabledKey = "openai_account_routing_us_cr_enabled"
	openAIAccountRoutingOverrideHeader = "x-openai-account-routing-override"
)

func (a *Account) IsOpenAIAccountRoutingUSCREnabled() bool {
	if !a.IsOpenAIOAuthLike() {
		return false
	}
	// 仅管理员为当前逻辑账号保存的布尔 true 生效；历史缺省值和错误类型均保持关闭。
	enabled, ok := a.Extra[openAIAccountRoutingUSCREnabledKey].(bool)
	return ok && enabled
}

func applyOpenAIAccountRoutingOverride(headers http.Header, account *Account) {
	if headers == nil || !account.IsOpenAIOAuthLike() {
		return
	}
	// 路由策略属于所选逻辑账号，不能从凭据母账号继承，也不能由客户端头覆盖关闭状态。
	headers.Del(openAIAccountRoutingOverrideHeader)
	if account.IsOpenAIAccountRoutingUSCREnabled() {
		headers.Set(openAIAccountRoutingOverrideHeader, "us_cr")
	}
}
