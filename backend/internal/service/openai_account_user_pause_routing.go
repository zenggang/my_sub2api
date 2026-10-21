package service

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

var (
	ErrOpenAIAccountUserPaused             = errors.New("OpenAI account scheduling is paused for this user")
	ErrOpenAIAccountPausePolicyUnavailable = infraerrors.New(http.StatusServiceUnavailable, "PAUSE_POLICY_UNAVAILABLE", "Account scheduling policy is temporarily unavailable")
)

type openAIHTTPAccountPauseKey struct{}
type openAIAccountPauseAttemptKey struct{}

type openAIAccountPauseAttempt struct {
	userID int64
	paused map[int64]struct{}
}

type openAIAccountPauseReselection struct {
	groupID int64
	allowed map[int64]struct{}
	denied  map[int64]struct{}
}

type openAIHTTPAccountPauseScope struct {
	mu          sync.Mutex
	groupID     int64
	groupKnown  bool
	reselection *openAIAccountPauseReselection
}

// WithOpenAIHTTPAccountPauseScope 只标记客户端 HTTP 准入，不改变上游 HTTP/WS 协议选择。
func WithOpenAIHTTPAccountPauseScope(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Value(openAIHTTPAccountPauseKey{}).(*openAIHTTPAccountPauseScope); ok {
		return ctx
	}
	return context.WithValue(ctx, openAIHTTPAccountPauseKey{}, &openAIHTTPAccountPauseScope{})
}

func openAIHTTPAccountPauseScopeFromContext(ctx context.Context) *openAIHTTPAccountPauseScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(openAIHTTPAccountPauseKey{}).(*openAIHTTPAccountPauseScope)
	return scope
}

func openAIAccountPauseUserID(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	userID, _ := ctx.Value(ctxkey.UserID).(int64)
	return userID
}

func IsOpenAIAccountPausePolicyError(err error) bool {
	return errors.Is(err, ErrOpenAIAccountPausePolicyUnavailable)
}

// withOpenAIAccountUserPauseExclusions 的快照仅在一次选号栈内复用，下一次尝试重新读已提交规则。
func (s *OpenAIGatewayService) withOpenAIAccountUserPauseExclusions(ctx context.Context, groupID *int64, platform string, excluded map[int64]struct{}) (context.Context, map[int64]struct{}, error) {
	scope := openAIHTTPAccountPauseScopeFromContext(ctx)
	if scope == nil || NormalizeOpenAICompatiblePlatform(platform) != PlatformOpenAI {
		return ctx, excluded, nil
	}
	userID := openAIAccountPauseUserID(ctx)
	if userID <= 0 || s == nil || s.accountUserPauses == nil {
		return ctx, nil, ErrOpenAIAccountPausePolicyUnavailable
	}
	attempt, _ := ctx.Value(openAIAccountPauseAttemptKey{}).(*openAIAccountPauseAttempt)
	if attempt == nil || attempt.userID != userID {
		paused, err := s.accountUserPauses.PausedAccountIDs(ctx, userID)
		if err != nil {
			return ctx, nil, ErrOpenAIAccountPausePolicyUnavailable.WithCause(err)
		}
		attempt = &openAIAccountPauseAttempt{userID: userID, paused: paused}
		ctx = context.WithValue(ctx, openAIAccountPauseAttemptKey{}, attempt)
	}
	merged := cloneExcludedAccountIDs(excluded)
	if merged == nil {
		merged = make(map[int64]struct{}, len(attempt.paused))
	}
	for id := range attempt.paused {
		merged[id] = struct{}{}
	}
	scope.mu.Lock()
	scope.groupID, scope.groupKnown = derefGroupID(groupID), true
	if scope.reselection != nil {
		if scope.reselection.groupID != derefGroupID(groupID) {
			scope.mu.Unlock()
			return ctx, nil, ErrNoAvailableAccounts
		}
		for id := range scope.reselection.denied {
			merged[id] = struct{}{}
		}
	}
	scope.mu.Unlock()
	return ctx, merged, nil
}

// openAIAccountPauseReselectionAllows 仅约束人工暂停否决阶段，不限制正常动态候选与上游重试。
func openAIAccountPauseReselectionAllows(ctx context.Context, account *Account) bool {
	scope := openAIHTTPAccountPauseScopeFromContext(ctx)
	if scope == nil || account == nil || account.Platform != PlatformOpenAI {
		return true
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.reselection == nil {
		return true
	}
	_, allowed := scope.reselection.allowed[account.ID]
	_, denied := scope.reselection.denied[account.ID]
	return allowed && !denied
}

// CheckOpenAIAccountUserPause 在实际准入前重新读取组合状态，不能复用选号时的名单快照。
func (s *OpenAIGatewayService) CheckOpenAIAccountUserPause(ctx context.Context, account *Account) error {
	if openAIHTTPAccountPauseScopeFromContext(ctx) == nil || account == nil || account.Platform != PlatformOpenAI {
		return nil
	}
	userID := openAIAccountPauseUserID(ctx)
	if userID <= 0 || s == nil || s.accountUserPauses == nil {
		return ErrOpenAIAccountPausePolicyUnavailable
	}
	paused, err := s.accountUserPauses.IsPaused(ctx, account.ID, userID)
	if err != nil {
		return ErrOpenAIAccountPausePolicyUnavailable.WithCause(err)
	}
	if paused {
		return ErrOpenAIAccountUserPaused
	}
	return nil
}

// BeginOpenAIAccountPauseReselection 冻结当前有效组的一轮有限候选，避免管理并发导致反复排同一账号。
func (s *OpenAIGatewayService) BeginOpenAIAccountPauseReselection(ctx context.Context, groupID *int64, account *Account) error {
	scope := openAIHTTPAccountPauseScopeFromContext(ctx)
	if scope == nil || account == nil || account.Platform != PlatformOpenAI {
		return ErrNoAvailableAccounts
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope.mu.Lock()
	actualGroupID := derefGroupID(groupID)
	if scope.groupKnown {
		actualGroupID = scope.groupID
	}
	needsSnapshot := scope.reselection == nil
	scope.mu.Unlock()
	var allowed map[int64]struct{}
	if needsSnapshot {
		var actualGroup *int64
		if actualGroupID > 0 {
			actualGroup = &actualGroupID
		}
		accounts, err := s.listSchedulableAccounts(ctx, actualGroup, PlatformOpenAI)
		if err != nil {
			return ErrOpenAIAccountPausePolicyUnavailable.WithCause(err)
		}
		allowed = make(map[int64]struct{}, len(accounts))
		for _, candidate := range accounts {
			allowed[candidate.ID] = struct{}{}
		}
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.reselection == nil {
		scope.reselection = &openAIAccountPauseReselection{groupID: actualGroupID, allowed: allowed, denied: make(map[int64]struct{})}
	}
	if _, repeated := scope.reselection.denied[account.ID]; repeated {
		return ErrNoAvailableAccounts
	}
	scope.reselection.denied[account.ID] = struct{}{}
	return nil
}

// CompleteOpenAIAccountPauseAdmission 结束人工重选阶段，后续真正上游重试继续使用原有动态账号池。
func CompleteOpenAIAccountPauseAdmission(ctx context.Context) {
	if scope := openAIHTTPAccountPauseScopeFromContext(ctx); scope != nil {
		scope.mu.Lock()
		scope.reselection = nil
		scope.mu.Unlock()
	}
}

// selectOpenAIAccountWithPauseAdmission supplies the same final check for HTTP helpers that do not acquire generation slots.
func (s *OpenAIGatewayService) selectOpenAIAccountWithPauseAdmission(ctx context.Context, groupID *int64, selectAccount func(context.Context) (*Account, error)) (*Account, error) {
	for {
		account, err := selectAccount(ctx)
		if err != nil || account == nil {
			return account, err
		}
		err = s.CheckOpenAIAccountUserPause(ctx, account)
		if errors.Is(err, ErrOpenAIAccountUserPaused) {
			if err = s.BeginOpenAIAccountPauseReselection(ctx, groupID, account); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		CompleteOpenAIAccountPauseAdmission(ctx)
		return account, nil
	}
}
