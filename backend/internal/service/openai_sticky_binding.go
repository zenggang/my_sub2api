package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type openAIStickyRequestContextKey struct{}

type openAIStickyRequestMetadata struct {
	mu                   sync.Mutex
	Hash                 string
	LegacyHash           string
	GroupID              int64
	Fingerprint          string
	Supported            bool
	Text                 bool
	Protocol             string
	Guardian             bool
	Expected             map[string]*OpenAIStickySnapshot
	Blocked              map[string]bool
	Observed             *OpenAIDispatchMarker
	ObservedScope        OpenAIStickyScope
	ObservationAttempted bool
}

// PrepareOpenAIStickyRequest uses an independent ingress marker; upstream protocol selection remains unchanged.
func PrepareOpenAIStickyRequest(c *gin.Context, protocol string) {
	if c == nil || c.Request == nil {
		return
	}
	if md, _ := c.Request.Context().Value(openAIStickyRequestContextKey{}).(*openAIStickyRequestMetadata); md != nil {
		return
	}
	md := &openAIStickyRequestMetadata{Protocol: protocol, Text: true, Expected: make(map[string]*OpenAIStickySnapshot), Blocked: make(map[string]bool)}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIStickyRequestContextKey{}, md))
}

func attachOpenAIStickyRequestIdentity(c *gin.Context, seed, hash string) {
	if c == nil || c.Request == nil {
		return
	}
	md, _ := c.Request.Context().Value(openAIStickyRequestContextKey{}).(*openAIStickyRequestMetadata)
	if md == nil {
		md = &openAIStickyRequestMetadata{Protocol: "unknown", Expected: make(map[string]*OpenAIStickySnapshot), Blocked: make(map[string]bool)}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), openAIStickyRequestContextKey{}, md))
	}
	md.mu.Lock()
	defer md.mu.Unlock()
	md.Hash = hash
	sum := sha256.Sum256([]byte(strings.TrimSpace(seed)))
	md.Fingerprint = hex.EncodeToString(sum[:])
	md.LegacyHash = md.Fingerprint
	_, err := uuid.Parse(strings.TrimSpace(seed))
	// 首版只开放显式 UUID 会话头；长度、User-Agent 都不能当成严格用户隔离证明。
	md.Supported = err == nil && explicitOpenAIHeaderSessionID(c) != ""
}

func stickyRequestMetadata(ctx context.Context) *openAIStickyRequestMetadata {
	if ctx == nil {
		return nil
	}
	md, _ := ctx.Value(openAIStickyRequestContextKey{}).(*openAIStickyRequestMetadata)
	return md
}

func (s *OpenAIGatewayService) stickyBindingScope(ctx context.Context, groupID *int64, hash string) OpenAIStickyScope {
	scope := OpenAIStickyScope{GroupID: derefGroupID(groupID), Hash: hash, Fallback: s.openAISessionHashReadOldFallbackEnabled(), DualWrite: s.openAISessionHashDualWriteOldEnabled(), LegacyHash: openAILegacySessionHashFromContext(ctx)}
	if ctx != nil {
		scope.UserID, _ = ctx.Value(ctxkey.UserID).(int64)
	}
	md := stickyRequestMetadata(ctx)
	if md != nil {
		scope.Own = hash == md.Hash
		scope.Protocol = md.Protocol
		if scope.Own {
			// WS 的 local ctx 早于 SID 生成；从共享入口元数据恢复自身 legacy 引用。
			scope.LegacyHash = md.LegacyHash
			scope.Fingerprint = md.Fingerprint
			scope.Supported = md.Supported && md.Text && !md.Guardian
		}
	}
	return scope
}

func stickyExpectationKey(groupID *int64, hash string) string {
	return fmt.Sprintf("%d:%s", derefGroupID(groupID), hash)
}

func (s *OpenAIGatewayService) captureStickyBinding(ctx context.Context, groupID *int64, hash string) (*OpenAIStickySnapshot, error) {
	cache, ok := s.cache.(OpenAIUserDispatchCache)
	if !ok || hash == "" {
		return nil, nil
	}
	md := stickyRequestMetadata(ctx)
	if md != nil {
		md.mu.Lock()
		defer md.mu.Unlock()
	}
	if md != nil {
		if _, guardian := openAIGuardianParentAffinityFromContext(ctx); guardian {
			md.Guardian = true
		}
		if md.Blocked[stickyExpectationKey(groupID, hash)] {
			return nil, fmt.Errorf("sticky snapshot unavailable for this request")
		}
		if snapshot := md.Expected[stickyExpectationKey(groupID, hash)]; snapshot != nil {
			return snapshot, nil
		}
	}
	scope := s.stickyBindingScope(ctx, groupID, hash)
	snapshot, err := cache.ReadOpenAIStickySnapshot(ctx, scope, uuid.NewString())
	if err != nil {
		if md != nil {
			md.Blocked[stickyExpectationKey(groupID, hash)] = true
		}
		return nil, err
	}
	if md != nil {
		md.Expected[stickyExpectationKey(groupID, hash)] = snapshot
		if scope.Own {
			md.GroupID = scope.GroupID
		}
		// 已见 SID 碰撞只能继续普通推理，不能领取其他用户的管理观察身份。
		if scope.Own && scope.Supported && scope.Protocol == "http" && !md.Guardian && md.Observed == nil && snapshot.Marker != nil && snapshot.OwnerID == scope.UserID && !snapshot.OwnerConflict && !snapshot.IdentityConflict && !snapshot.HasWS {
			marker := *snapshot.Marker
			md.Observed = &marker
			md.ObservedScope = scope
		}
	}
	return snapshot, nil
}

func (s *OpenAIGatewayService) mutateStickyBinding(ctx context.Context, groupID *int64, hash, action string, accountID int64, ttl time.Duration) error {
	cache, ok := s.cache.(OpenAIUserDispatchCache)
	if !ok {
		return fmt.Errorf("versioned sticky cache unavailable")
	}
	md := stickyRequestMetadata(ctx)
	if md == nil {
		// 缺失快照只能继续推理，不能用当前新版本重新授权旧请求的缓存覆盖。
		slog.Debug("openai.sticky_mutation_skipped", "reason", "missing_snapshot", "group_id", derefGroupID(groupID), "action", action)
		return nil
	}
	md.mu.Lock()
	defer md.mu.Unlock()
	expected := md.Expected[stickyExpectationKey(groupID, hash)]
	if expected == nil && hash != md.Hash && !md.Blocked[stickyExpectationKey(groupID, hash)] {
		// 一次性号池 hash 在选号后生成，只补读这个新的辅助键，不重新授权自身旧快照。
		var err error
		expected, err = cache.ReadOpenAIStickySnapshot(ctx, s.stickyBindingScope(ctx, groupID, hash), uuid.NewString())
		if err != nil {
			return err
		}
		md.Expected[stickyExpectationKey(groupID, hash)] = expected
	}
	if expected == nil {
		return nil
	}
	scope := s.stickyBindingScope(ctx, groupID, hash)
	mutation := OpenAIStickyMutation{Action: action, AccountID: accountID, TTLMillis: ttl.Milliseconds(), LegacyTTLMillis: s.openAIStickyLegacyTTL(ttl).Milliseconds(), NewRevision: uuid.NewString()}
	snapshot, applied, err := cache.MutateOpenAIStickyBinding(ctx, scope, *expected, mutation)
	if err != nil {
		return err
	}
	if applied {
		md.Expected[stickyExpectationKey(groupID, hash)] = snapshot
	} else {
		slog.Debug("openai.sticky_mutation_skipped", "reason", "stale_snapshot", "group_id", scope.GroupID, "action", action, "expected_account_id", expected.AccountID)
	}
	return nil
}

// ObserveOpenAIDispatchAdmission records account admission, never inference completion.
func (s *OpenAIGatewayService) ObserveOpenAIDispatchAdmission(ctx context.Context, accountID int64, queued bool) {
	cache, ok := s.cache.(OpenAIUserDispatchCache)
	if !ok {
		return
	}
	md := stickyRequestMetadata(ctx)
	if md == nil {
		return
	}
	md.mu.Lock()
	if md.Guardian || md.Observed == nil || md.ObservationAttempted {
		md.mu.Unlock()
		return
	}
	marker := *md.Observed
	scope := md.ObservedScope
	md.ObservationAttempted = true
	md.mu.Unlock()
	if err := cache.ObserveOpenAIDispatchAdmission(ctx, scope, marker, accountID, queued); err != nil {
		slog.Warn("openai.dispatch_observation_failed", "operation_id", marker.OperationID, "account_id", accountID, "error", err)
	}
}
