package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type OpenAIDispatchError struct {
	Status      int
	Code        string
	Message     string
	OperationID string
}

func (e *OpenAIDispatchError) Error() string { return e.Message }
func dispatchError(status int, code, message string) error {
	return &OpenAIDispatchError{Status: status, Code: code, Message: message}
}
func dispatchStoreError(err error, op string) error {
	return &OpenAIDispatchError{Status: 503, Code: "DISPATCH_STORE_UNAVAILABLE", Message: "改绑存储暂不可用，请保留幂等键查询原操作", OperationID: op}
}

func (s *OpenAIGatewayService) OpenAIDispatchEnabled(ctx context.Context) bool {
	if s == nil {
		return false
	}
	repo := s.openAIAdvancedSchedulerSettingRepo()
	if repo == nil {
		return false
	}
	value, err := repo.GetValue(ctx, SettingKeyOpenAIUserAccountDispatchEnabled)
	return err == nil && (value == "true" || value == "1")
}
func (s *OpenAIGatewayService) SetOpenAIDispatchEnabled(ctx context.Context, enabled bool) error {
	repo := s.openAIAdvancedSchedulerSettingRepo()
	if repo == nil {
		return dispatchError(503, "DISPATCH_STORE_UNAVAILABLE", "管理设置不可用")
	}
	value := "false"
	if enabled {
		value = "true"
	}
	return repo.Set(ctx, SettingKeyOpenAIUserAccountDispatchEnabled, value)
}
func (s *OpenAIGatewayService) userDispatchCache() (OpenAIUserDispatchCache, error) {
	if s == nil {
		return nil, dispatchStoreError(nil, "")
	}
	cache, ok := s.cache.(OpenAIUserDispatchCache)
	if !ok {
		return nil, dispatchStoreError(nil, "")
	}
	return cache, nil
}
func (s *OpenAIGatewayService) dispatchBindingTTL() time.Duration {
	if s.cfg != nil && s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds > 0 {
		return time.Duration(s.cfg.Gateway.OpenAIWS.StickySessionTTLSeconds) * time.Second
	}
	return openaiStickySessionTTL
}
func (s *OpenAIGatewayService) dispatchScope(scope OpenAIStickyScope) OpenAIStickyScope {
	scope.Fallback = s.openAISessionHashReadOldFallbackEnabled()
	scope.DualWrite = s.openAISessionHashDualWriteOldEnabled()
	return scope
}
func dispatchSessionReason(snap *OpenAIStickySnapshot, uid, aid int64) string {
	if snap.AccountID != aid {
		return "binding_changed"
	}
	if snap.OwnerConflict || snap.IdentityConflict {
		return "owner_conflict"
	}
	if snap.OwnerID != uid {
		return "owner_unknown"
	}
	if snap.HasWS {
		return "protocol_unsupported"
	}
	if !snap.Supported {
		return "identity_unsupported"
	}
	return ""
}
func (s *OpenAIGatewayService) dispatchTargetReason(ctx context.Context, a *Account, bindings []OpenAIStickySnapshot, source int64) string {
	if a == nil || a.ID == source {
		return "same_or_missing_account"
	}
	if a.Platform != PlatformOpenAI || !a.IsSchedulable() {
		return "not_schedulable"
	}
	if s.isOpenAIAccountRequestRuntimeBlocked(a, "") || s.isOpenAIAccountBlockedBySchedulingThreshold(ctx, a) {
		return "runtime_blocked"
	}
	if paused, _ := shouldAutoPauseOpenAIAccountByQuota(s.withOpenAIQuotaAutoPauseContext(ctx), a); paused {
		return "quota_auto_pause"
	}
	if !parentHealthyForShadow(a, s.parentAccountLookup(ctx)) {
		return "shadow_parent_unhealthy"
	}
	for _, b := range bindings {
		var gid *int64
		if b.Scope.GroupID > 0 {
			g := b.Scope.GroupID
			gid = &g
		}
		if !s.openAIAccountMatchesSchedulingGroup(a, gid) {
			return "group_mismatch"
		}
		if s.openAIGroupRequiresPrivacySet(ctx, gid) && !a.IsPrivacySet() {
			return "privacy_not_set"
		}
	}
	return ""
}

func (s *OpenAIGatewayService) PreviewOpenAIUserAccountDispatch(ctx context.Context, owner string, uid, sourceID int64) (*OpenAIDispatchPreview, error) {
	if !s.OpenAIDispatchEnabled(ctx) {
		return nil, dispatchError(409, "FEATURE_DISABLED", "用户会话改绑尚未启用")
	}
	if uid <= 0 || sourceID <= 0 {
		return nil, dispatchError(400, "INVALID_REQUEST", "用户和源账号必须有效")
	}
	cache, err := s.userDispatchCache()
	if err != nil {
		return nil, err
	}
	source, err := s.accountRepo.GetByID(ctx, sourceID)
	if err != nil || source == nil || source.Platform != PlatformOpenAI {
		return nil, dispatchError(404, "SOURCE_NOT_FOUND", "源账号不存在或不支持此功能")
	}
	user, err := s.userRepo.GetByID(ctx, uid)
	if err != nil || user == nil {
		return nil, dispatchError(404, "USER_NOT_FOUND", "用户不存在")
	}
	scopes, truncated, err := cache.ListOpenAIStickyUserBindings(ctx, uid, sourceID)
	if err != nil {
		return nil, dispatchStoreError(err, "")
	}
	now := time.Now().UTC()
	preview := OpenAIDispatchPreview{PreviewID: uuid.NewString(), ExpiresAt: now.Add(OpenAIDispatchPreviewTTL), User: OpenAIDispatchIdentity{ID: uid, Name: user.Email}, SourceAccount: OpenAIDispatchIdentity{ID: sourceID, Name: source.Name}, Sessions: []OpenAIDispatchSession{}, SkippedSessions: []OpenAIDispatchSession{}, Targets: []OpenAIDispatchTarget{}, Coverage: OpenAIDispatchCoverage{Truncated: truncated, Scope: "已登记且仍有效的 UUID HTTP/SSE 自身会话；在途对照来自当前实例"}}
	bindings := make([]OpenAIStickySnapshot, 0, len(scopes))
	for _, scope := range scopes {
		scope = s.dispatchScope(scope)
		snap, e := cache.ReadOpenAIStickySnapshot(ctx, scope, uuid.NewString())
		if e != nil {
			return nil, dispatchStoreError(e, "")
		}
		item := OpenAIDispatchSession{SessionRef: uuid.NewString(), GroupID: scope.GroupID, CurrentBinding: nil, Observation: "not_observed", RebindResult: "not_processed"}
		if snap.AccountID > 0 {
			item.CurrentBinding = snap.AccountID
		}
		reason := dispatchSessionReason(snap, uid, sourceID)
		if reason != "" {
			item.RebindResult = reason
			item.Reason = reason
			preview.SkippedSessions = append(preview.SkippedSessions, item)
			continue
		}
		item.Observation = "awaiting_observation"
		if s.concurrencyService != nil {
			item.InFlight = s.concurrencyService.dispatchSessionInFlight(sourceID, uid, scope.GroupID, scope.Hash)
		}
		preview.Counts.InFlight += item.InFlight
		preview.Sessions = append(preview.Sessions, item)
		bindings = append(bindings, *snap)
	}
	preview.Counts.Rebindable = len(bindings)
	preview.Counts.Skipped = len(preview.SkippedSessions)
	candidates, err := s.accountRepo.ListSchedulableByPlatform(ctx, PlatformOpenAI)
	if err != nil {
		return nil, err
	}
	loadInputs := make([]AccountWithConcurrency, 0, len(candidates))
	for _, a := range candidates {
		loadInputs = append(loadInputs, AccountWithConcurrency{ID: a.ID, MaxConcurrency: a.Concurrency})
	}
	loads := map[int64]*AccountLoadInfo{}
	if s.concurrencyService != nil {
		if values, e := s.concurrencyService.GetAccountsLoadBatch(ctx, loadInputs); e == nil {
			loads = values
		}
	}
	for i := range candidates {
		a := &candidates[i]
		if a.ID == sourceID {
			continue
		}
		reason := s.dispatchTargetReason(ctx, a, bindings, sourceID)
		item := OpenAIDispatchTarget{AccountID: a.ID, Name: a.Name, Compatible: reason == "" && len(bindings) > 0, Reason: reason, MaxConcurrency: a.Concurrency}
		if l := loads[a.ID]; l != nil {
			item.Active = l.CurrentConcurrency
			item.Waiting = l.WaitingCount
		}
		preview.Targets = append(preview.Targets, item)
	}
	record := &OpenAIDispatchPreviewRecord{OwnerHash: owner, Preview: preview, Bindings: bindings}
	if err = cache.SaveOpenAIDispatchPreview(ctx, record); err != nil {
		return nil, dispatchStoreError(err, "")
	}
	return &preview, nil
}

func dispatchBodyHash(previewID string, target int64) string {
	b, _ := json.Marshal(struct {
		PreviewID string `json:"preview_id"`
		Target    int64  `json:"target_account_id"`
	}{previewID, target})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *OpenAIGatewayService) CreateOpenAIUserAccountDispatch(ctx context.Context, owner, previewID string, targetID int64, idempotency string) (*OpenAIDispatchOperation, error) {
	if _, e := uuid.Parse(previewID); e != nil {
		return nil, dispatchError(400, "INVALID_REQUEST", "preview_id无效")
	}
	if _, e := uuid.Parse(idempotency); e != nil || targetID <= 0 {
		return nil, dispatchError(400, "INVALID_REQUEST", "幂等键或目标账号无效")
	}
	cache, err := s.userDispatchCache()
	if err != nil {
		return nil, err
	}
	bodyHash := dispatchBodyHash(previewID, targetID)
	record, err := cache.FindOpenAIDispatchOperation(ctx, owner, idempotency)
	if err != nil {
		return nil, dispatchStoreError(err, "")
	}
	if record == nil {
		if !s.OpenAIDispatchEnabled(ctx) {
			return nil, dispatchError(409, "FEATURE_DISABLED", "用户会话改绑尚未启用")
		}
		preview, e := cache.GetOpenAIDispatchPreview(ctx, previewID)
		if e == redis.Nil {
			return nil, dispatchError(409, "PREVIEW_EXPIRED", "预览已过期，请重新预览")
		}
		if e != nil {
			return nil, dispatchStoreError(e, "")
		}
		if preview.OwnerHash != owner {
			return nil, dispatchError(403, "FORBIDDEN", "预览不属于当前管理会话")
		}
		if preview.Preview.Coverage.Truncated || len(preview.Bindings) > 200 {
			return nil, dispatchError(409, "COVERAGE_INCOMPLETE", "索引截断或超过单批200个会话，请缩小范围")
		}
		if len(preview.Bindings) == 0 {
			return nil, dispatchError(404, "NO_REBINDABLE_SESSIONS", "没有可改绑会话，请查看跳过原因")
		}
		target, e := s.accountRepo.GetByID(ctx, targetID)
		if e != nil || s.dispatchTargetReason(ctx, target, preview.Bindings, preview.Preview.SourceAccount.ID) != "" {
			return nil, dispatchError(422, "TARGET_INELIGIBLE", "目标账号当前不满足整批基础资格")
		}
		now := time.Now().UTC()
		op := OpenAIDispatchOperation{OperationID: uuid.NewString(), UserID: preview.Preview.User.ID, SourceAccountID: preview.Preview.SourceAccount.ID, TargetAccountID: targetID, CreatedAt: now, ExecutionDeadline: now.Add(OpenAIDispatchPreviewTTL), ObservationDeadline: now.Add(OpenAIDispatchObservationTTL), State: "processing", Sessions: preview.Preview.Sessions}
		for i := range op.Sessions {
			op.Sessions[i].RebindResult = "not_processed"
			op.Sessions[i].Observation = "awaiting_observation"
		}
		record, e = cache.ReserveOpenAIDispatchOperation(ctx, idempotency, &OpenAIDispatchOperationRecord{OwnerHash: owner, BodyHash: bodyHash, Operation: op, Bindings: preview.Bindings})
		if e != nil {
			return nil, dispatchStoreError(e, op.OperationID)
		}
	}
	if record.OwnerHash != owner || record.BodyHash != bodyHash {
		return nil, dispatchError(409, "IDEMPOTENCY_CONFLICT", "同一幂等键对应不同请求")
	}
	// 已处理结果先恢复；新目标资格改变不应让客户端丢失原操作事实。
	for i, item := range record.Operation.Sessions {
		if item.RebindResult != "not_processed" && item.RebindResult != "result_unknown" {
			continue
		}
		if !s.OpenAIDispatchEnabled(ctx) {
			break
		}
		record.Bindings[i].Scope = s.dispatchScope(record.Bindings[i].Scope)
		target, e := s.accountRepo.GetByID(ctx, record.Operation.TargetAccountID)
		eligible := e == nil && s.dispatchTargetReason(ctx, target, record.Bindings, record.Operation.SourceAccountID) == ""
		ttl := s.dispatchBindingTTL()
		if e = cache.RebindOpenAIDispatchSession(ctx, record, i, ttl, s.openAIStickyLegacyTTL(ttl), uuid.NewString(), eligible); e != nil {
			slog.Warn("openai.dispatch_result_unknown", "operation_id", record.Operation.OperationID, "session_ref", item.SessionRef, "error", e)
			// 响应丢失无法等价为未执行；读取成功时保留既有结果，否则明确未知。
			if recovered, readErr := cache.GetOpenAIDispatchOperation(ctx, record.Operation.OperationID); readErr == nil {
				record = recovered
			} else {
				record.Operation.Sessions[i].RebindResult = "result_unknown"
				record.Operation.Sessions[i].Reason = "result_unknown"
			}
			return s.dispatchOperationView(ctx, cache, record), nil
		}
	}
	stored, err := cache.GetOpenAIDispatchOperation(ctx, record.Operation.OperationID)
	if err != nil {
		return nil, dispatchStoreError(err, record.Operation.OperationID)
	}
	slog.Info("openai.dispatch_created", "operation_id", stored.Operation.OperationID, "user_id", stored.Operation.UserID, "source_account_id", stored.Operation.SourceAccountID, "target_account_id", stored.Operation.TargetAccountID, "session_count", len(stored.Operation.Sessions))
	return s.dispatchOperationView(ctx, cache, stored), nil
}

func (s *OpenAIGatewayService) StatusOpenAIUserAccountDispatch(ctx context.Context, owner, id string) (*OpenAIDispatchOperation, error) {
	if _, e := uuid.Parse(id); e != nil {
		return nil, dispatchError(400, "INVALID_REQUEST", "operation_id无效")
	}
	cache, err := s.userDispatchCache()
	if err != nil {
		return nil, err
	}
	record, err := cache.GetOpenAIDispatchOperation(ctx, id)
	if err == redis.Nil {
		return nil, dispatchError(404, "OPERATION_EXPIRED", "操作不存在或已过查询期限")
	}
	if err != nil {
		return nil, dispatchStoreError(err, id)
	}
	if record.OwnerHash != owner {
		return nil, dispatchError(403, "FORBIDDEN", "操作不属于当前管理会话")
	}
	return s.dispatchOperationView(ctx, cache, record), nil
}
func (s *OpenAIGatewayService) dispatchOperationView(ctx context.Context, cache OpenAIUserDispatchCache, record *OpenAIDispatchOperationRecord) *OpenAIDispatchOperation {
	op := record.Operation
	op.Counts = OpenAIDispatchCounts{Rebindable: len(op.Sessions)}
	op.Sessions = append([]OpenAIDispatchSession(nil), op.Sessions...)
	now := time.Now().UTC()
	for i := range op.Sessions {
		item := &op.Sessions[i]
		op.Counts.InFlight += item.InFlight
		if item.RebindResult == "not_processed" && now.After(op.ExecutionDeadline) {
			item.RebindResult = "execution_expired"
			item.Reason = "execution_expired"
		}
		switch item.RebindResult {
		case "rebound":
			op.Counts.Rebound++
		case "not_processed", "result_unknown":
			op.Counts.Unresolved++
		default:
			op.Counts.Skipped++
		}
		if item.Observation == "awaiting_observation" && now.After(op.ObservationDeadline) {
			item.Observation = "not_observed"
		}
		switch item.Observation {
		case "assigned_target":
			op.Counts.AssignedTarget++
		case "assigned_other":
			op.Counts.AssignedOther++
		case "awaiting_observation":
			op.Counts.Awaiting++
		}
		scope := s.dispatchScope(record.Bindings[i].Scope)
		snapshot, err := cache.ReadOpenAIStickySnapshot(ctx, scope, uuid.NewString())
		item.BindingCheckedAt = &now
		if err != nil {
			item.CurrentBinding = "unknown"
		} else if snapshot.AccountID == 0 {
			item.CurrentBinding = nil
		} else {
			item.CurrentBinding = snapshot.AccountID
		}
	}
	op.Accepted = op.Counts.Rebound > 0
	switch {
	case op.Counts.Unresolved > 0:
		op.State = "processing"
	case op.Counts.Rebound > 0 && op.Counts.Skipped > 0:
		op.State = "partially_rebound"
	case op.Counts.Rebound > 0:
		op.State = "rebound"
	default:
		op.State = "failed"
	}
	return &op
}
