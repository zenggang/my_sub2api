package admin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func NewOpsHandlerWithAccountUserPauses(ops *service.OpsService, gateway *service.OpenAIGatewayService, pauses *service.AccountUserPauseService) *OpsHandler {
	h := NewOpsHandlerWithDispatch(ops, gateway)
	h.accountUserPauses = pauses
	return h
}

func (h *OpsHandler) accountUserPauseAdmin(c *gin.Context) (int64, bool) {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	role, _ := middleware.GetUserRoleFromContext(c)
	if !ok || subject.UserID <= 0 || role != service.RoleAdmin {
		response.ErrorWithDetails(c, http.StatusForbidden, "需要管理员身份", "FORBIDDEN", nil)
		return 0, false
	}
	if h.accountUserPauses == nil {
		response.ErrorFrom(c, service.ErrAccountUserPauseUnavailable)
		return 0, false
	}
	return subject.UserID, true
}

func accountUserPauseInvalid(c *gin.Context) {
	response.ErrorWithDetails(c, http.StatusBadRequest, "账号、用户、邮箱或分页参数无效", "INVALID_REQUEST", nil)
}

func positivePauseInteger(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	return id, err == nil && id > 0
}

func (h *OpsHandler) ListAccountUserPauses(c *gin.Context) {
	if _, ok := h.accountUserPauseAdmin(c); !ok {
		return
	}
	// ParseQuery 会保留部分有效值；非法转义或分隔符必须整体拒绝，不能静默丢弃输入。
	query, err := url.ParseQuery(c.Request.URL.RawQuery)
	if err != nil {
		accountUserPauseInvalid(c)
		return
	}
	for key, values := range query {
		if len(values) != 1 || (key != "account_id" && key != "page" && key != "page_size") {
			accountUserPauseInvalid(c)
			return
		}
	}
	accountID, ok := positivePauseInteger(query.Get("account_id"))
	if !ok {
		accountUserPauseInvalid(c)
		return
	}
	page, pageSize := int64(1), int64(20)
	if values, present := query["page"]; present {
		page, ok = positivePauseInteger(values[0])
		if !ok {
			accountUserPauseInvalid(c)
			return
		}
	}
	if values, present := query["page_size"]; present {
		pageSize, ok = positivePauseInteger(values[0])
		if !ok || pageSize > 100 {
			accountUserPauseInvalid(c)
			return
		}
	}
	if page > int64(^uint(0)>>1) {
		accountUserPauseInvalid(c)
		return
	}
	result, err := h.accountUserPauses.List(c.Request.Context(), accountID, int(page), int(pageSize))
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, result)
}

func (h *OpsHandler) PauseAccountUser(c *gin.Context) {
	adminID, ok := h.accountUserPauseAdmin(c)
	if !ok {
		return
	}
	if c.Request.URL.RawQuery != "" {
		accountUserPauseInvalid(c)
		return
	}
	var request struct {
		AccountID int64           `json:"account_id"`
		UserID    json.RawMessage `json:"user_id"`
		Email     json.RawMessage `json:"email"`
	}
	if !decodeDispatchRequest(c, &request) {
		return
	}
	if (len(request.UserID) == 0) == (len(request.Email) == 0) {
		accountUserPauseInvalid(c)
		return
	}
	var userID int64
	var email string
	if len(request.UserID) > 0 {
		if json.Unmarshal(request.UserID, &userID) != nil || userID <= 0 {
			accountUserPauseInvalid(c)
			return
		}
	}
	if len(request.Email) > 0 {
		if json.Unmarshal(request.Email, &email) != nil {
			accountUserPauseInvalid(c)
			return
		}
	}
	result, err := h.accountUserPauses.Pause(c.Request.Context(), request.AccountID, userID, email, adminID)
	h.accountUserPauseResponse(c, request.AccountID, userID, result, err)
}

func (h *OpsHandler) ResumeAccountUser(c *gin.Context) {
	adminID, ok := h.accountUserPauseAdmin(c)
	if !ok {
		return
	}
	if c.Request.URL.RawQuery != "" {
		accountUserPauseInvalid(c)
		return
	}
	var request struct {
		AccountID int64 `json:"account_id"`
		UserID    int64 `json:"user_id"`
	}
	if !decodeDispatchRequest(c, &request) {
		return
	}
	result, err := h.accountUserPauses.Resume(c.Request.Context(), request.AccountID, request.UserID, adminID)
	h.accountUserPauseResponse(c, request.AccountID, request.UserID, result, err)
}

func (h *OpsHandler) accountUserPauseResponse(c *gin.Context, accountID, userID int64, result *service.AccountUserPauseResult, err error) {
	fields := map[string]any{"account_id": accountID, "user_id": userID}
	if err != nil {
		fields["error_code"] = infraerrors.Reason(err)
		middleware.SetAuditExtra(c, fields)
		response.ErrorFrom(c, err)
		return
	}
	fields["user_id"], fields["paused"], fields["changed"] = result.UserID, result.Paused, result.Changed
	fields["result"] = "success"
	// 管理路由已记录原始受控请求，extra 只补实际解析的身份和提交结果，不重复打印正文。
	middleware.SetAuditExtra(c, fields)
	response.Success(c, result)
}
