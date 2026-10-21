package handler

import (
	"context"
	"errors"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func prepareOpenAIHTTPAccountPause(c *gin.Context) {
	if c != nil && c.Request != nil {
		ctx := c.Request.Context()
		// 身份仅取认证中间件写入的服务端上下文，兼容直接进入 handler 的内部调用。
		if id, _ := ctx.Value(ctxkey.UserID).(int64); id <= 0 {
			if subject, ok := middleware2.GetAuthSubjectFromContext(c); ok && subject.UserID > 0 {
				ctx = context.WithValue(ctx, ctxkey.UserID, subject.UserID)
			}
		}
		c.Request = c.Request.WithContext(service.WithOpenAIHTTPAccountPauseScope(ctx))
	}
}

func markOpenAIAccountPausePolicyError(c *gin.Context) {
	// 保留真实上游尝试历史，但最终失败来自本地规则存储，不能沿用前次上游错误归因。
	c.Set(service.OpsUpstreamStatusCodeKey, 0)
	c.Set(service.OpsUpstreamErrorMessageKey, "")
	c.Set(service.OpsUpstreamErrorDetailKey, "")
	service.ClearOpsUpstreamModel(c)
	service.SetOpsLocalErrorType(c, "api_error", "PAUSE_POLICY_UNAVAILABLE")
}

func (h *OpenAIGatewayHandler) handleOpenAIAccountPausePolicyError(c *gin.Context, err error, streamStarted bool, anthropic bool) bool {
	if !service.IsOpenAIAccountPausePolicyError(err) {
		return false
	}
	markOpenAIAccountPausePolicyError(c)
	requestLogger(c, "handler.openai_account_pause").Warn("openai.account_pause_policy_unavailable", zap.Error(err))
	message := "Account scheduling policy is temporarily unavailable"
	if anthropic {
		h.anthropicStreamingAwareError(c, http.StatusServiceUnavailable, "api_error", message, streamStarted)
	} else {
		h.handleStreamingAwareErrorWithCode(c, http.StatusServiceUnavailable, "api_error", "PAUSE_POLICY_UNAVAILABLE", message, streamStarted, false)
	}
	return true
}

// checkOpenAIAccountPauseAdmission 释放已经取得的账号槽位后才重选，人工暂停不进入提供方失败计数。
func (h *OpenAIGatewayHandler) checkOpenAIAccountPauseAdmission(c *gin.Context, ctx context.Context, groupID *int64, selection *service.AccountSelectionResult, release func(), reqLog *zap.Logger, writeError openAISlotErrorWriter) openAISlotAcquireResult {
	err := h.gatewayService.CheckOpenAIAccountUserPause(ctx, selection.Account)
	if err == nil {
		return openAISlotAcquireOK
	}
	if release != nil {
		release()
	}
	if errors.Is(err, service.ErrOpenAIAccountUserPaused) {
		if beginErr := h.gatewayService.BeginOpenAIAccountPauseReselection(ctx, groupID, selection.Account); beginErr == nil {
			reqLog.Debug("openai.account_slot_user_pause_vetoed", zap.Int64("account_id", selection.Account.ID))
			return openAISlotAcquirePauseVetoed
		} else {
			err = beginErr
		}
	}
	if service.IsOpenAIAccountPausePolicyError(err) {
		markOpenAIAccountPausePolicyError(c)
		reqLog.Warn("openai.account_pause_policy_unavailable", zap.Int64("account_id", selection.Account.ID), zap.Error(err))
		writeError(http.StatusServiceUnavailable, "api_error", "PAUSE_POLICY_UNAVAILABLE", "Account scheduling policy is temporarily unavailable")
	} else {
		writeError(http.StatusServiceUnavailable, "api_error", "", "No available accounts")
	}
	return openAISlotAcquireFailed
}
