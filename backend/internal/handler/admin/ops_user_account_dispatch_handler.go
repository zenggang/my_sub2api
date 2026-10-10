package admin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type openAIUserAccountDispatchService interface {
	OpenAIDispatchEnabled(context.Context) bool
	SetOpenAIDispatchEnabled(context.Context, bool) error
	PreviewOpenAIUserAccountDispatch(context.Context, string, int64, int64) (*service.OpenAIDispatchPreview, error)
	CreateOpenAIUserAccountDispatch(context.Context, string, string, int64, string) (*service.OpenAIDispatchOperation, error)
	StatusOpenAIUserAccountDispatch(context.Context, string, string) (*service.OpenAIDispatchOperation, error)
}

func (h *OpsHandler) dispatchOwner(c *gin.Context) (string, bool) {
	subject, authenticated := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	if !authenticated || subject.UserID <= 0 || role != service.RoleAdmin {
		response.ErrorWithDetails(c, http.StatusForbidden, "需要管理员身份", "FORBIDDEN", nil)
		return "", false
	}
	if h.userAccountDispatch == nil {
		response.ErrorWithDetails(c, http.StatusServiceUnavailable, "用户会话改绑服务不可用", "DISPATCH_STORE_UNAVAILABLE", nil)
		return "", false
	}
	owner := strings.TrimSpace(c.GetHeader("X-Quick-Ops-Owner"))
	if owner != "" {
		decoded, err := hex.DecodeString(owner)
		if err != nil || len(decoded) != 32 || owner != strings.ToLower(owner) {
			response.ErrorWithDetails(c, http.StatusBadRequest, "管理会话标识无效", "INVALID_REQUEST", nil)
			return "", false
		}
	}
	// 此头只缩小幂等命名空间；权限和实际操作者始终来自已认证的管理员。
	sum := sha256.Sum256([]byte(strconv.FormatInt(subject.UserID, 10) + ":" + owner))
	return hex.EncodeToString(sum[:]), true
}

func decodeDispatchRequest(c *gin.Context, target any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		response.ErrorWithDetails(c, http.StatusBadRequest, "请求正文无效", "INVALID_REQUEST", nil)
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		response.ErrorWithDetails(c, http.StatusBadRequest, "请求正文只能包含一个 JSON 对象", "INVALID_REQUEST", nil)
		return false
	}
	return true
}

func dispatchResponseError(c *gin.Context, err error) {
	var detail *service.OpenAIDispatchError
	if errors.As(err, &detail) {
		metadata := map[string]string(nil)
		if detail.OperationID != "" {
			metadata = map[string]string{"operation_id": detail.OperationID}
		}
		middleware.SetAuditExtra(c, map[string]any{"error_code": detail.Code, "operation_id": detail.OperationID})
		response.ErrorWithDetails(c, detail.Status, detail.Message, detail.Code, metadata)
		return
	}
	response.ErrorWithDetails(c, http.StatusInternalServerError, "用户会话改绑处理失败", "INTERNAL_ERROR", nil)
}

func (h *OpsHandler) PreviewUserAccountDispatch(c *gin.Context) {
	owner, ok := h.dispatchOwner(c)
	if !ok {
		return
	}
	var request struct {
		UserID          int64 `json:"user_id"`
		SourceAccountID int64 `json:"source_account_id"`
	}
	if !decodeDispatchRequest(c, &request) {
		return
	}
	preview, err := h.userAccountDispatch.PreviewOpenAIUserAccountDispatch(c.Request.Context(), owner, request.UserID, request.SourceAccountID)
	if err != nil {
		dispatchResponseError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"preview_id": preview.PreviewID, "user_id": request.UserID, "source_account_id": request.SourceAccountID, "requested_count": preview.Counts.Rebindable, "skipped_count": preview.Counts.Skipped})
	response.Success(c, preview)
}

func (h *OpsHandler) CreateUserAccountDispatch(c *gin.Context) {
	owner, ok := h.dispatchOwner(c)
	if !ok {
		return
	}
	var request struct {
		PreviewID       string `json:"preview_id"`
		TargetAccountID int64  `json:"target_account_id"`
		IdempotencyKey  string `json:"idempotency_key"`
	}
	if !decodeDispatchRequest(c, &request) {
		return
	}
	op, err := h.userAccountDispatch.CreateOpenAIUserAccountDispatch(c.Request.Context(), owner, request.PreviewID, request.TargetAccountID, request.IdempotencyKey)
	if err != nil {
		dispatchResponseError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"operation_id": op.OperationID, "preview_id": request.PreviewID, "owner_hash": owner, "user_id": op.UserID, "source_account_id": op.SourceAccountID, "target_account_id": op.TargetAccountID, "rebound_count": op.Counts.Rebound, "skipped_count": op.Counts.Skipped, "unresolved_count": op.Counts.Unresolved, "result": op.State})
	response.Success(c, op)
}

func (h *OpsHandler) GetUserAccountDispatchStatus(c *gin.Context) {
	owner, ok := h.dispatchOwner(c)
	if !ok {
		return
	}
	op, err := h.userAccountDispatch.StatusOpenAIUserAccountDispatch(c.Request.Context(), owner, strings.TrimSpace(c.Query("operation_id")))
	if err != nil {
		dispatchResponseError(c, err)
		return
	}
	response.Success(c, op)
}

func (h *OpsHandler) GetUserAccountDispatchSettings(c *gin.Context) {
	if _, ok := h.dispatchOwner(c); !ok {
		return
	}
	response.Success(c, gin.H{"enabled": h.userAccountDispatch.OpenAIDispatchEnabled(c.Request.Context())})
}

func (h *OpsHandler) UpdateUserAccountDispatchSettings(c *gin.Context) {
	if _, ok := h.dispatchOwner(c); !ok {
		return
	}
	var request struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeDispatchRequest(c, &request) {
		return
	}
	if request.Enabled == nil {
		response.ErrorWithDetails(c, http.StatusBadRequest, "enabled 必须显式指定", "INVALID_REQUEST", nil)
		return
	}
	if err := h.userAccountDispatch.SetOpenAIDispatchEnabled(c.Request.Context(), *request.Enabled); err != nil {
		dispatchResponseError(c, err)
		return
	}
	middleware.SetAuditExtra(c, map[string]any{"enabled": *request.Enabled})
	response.Success(c, gin.H{"enabled": *request.Enabled})
}
