package service

import (
	"context"
	"errors"
	"net/http"
	"net/mail"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const AccountUserPauseScope = "openai_http_sse"

var (
	ErrAccountUserPauseUnavailable   = infraerrors.New(http.StatusServiceUnavailable, "PAUSE_STORE_UNAVAILABLE", "暂停调度规则暂不可用")
	ErrAccountUserPauseResultUnknown = infraerrors.New(http.StatusServiceUnavailable, "RESULT_UNKNOWN", "提交结果暂无法确认，请重试确认")
	ErrAccountUserPausePlatform      = infraerrors.New(http.StatusUnprocessableEntity, "ACCOUNT_PLATFORM_UNSUPPORTED", "仅支持 OpenAI 账号的 HTTP/SSE 暂停调度")
	ErrUserEmailAmbiguous            = infraerrors.Conflict("EMAIL_AMBIGUOUS", "邮箱匹配到多个用户，请使用用户 ID")
)

type AccountUserPausePair struct {
	AccountID int64
	UserID    int64
}

type AccountUserPauseEntry struct {
	UserID      int64     `json:"user_id"`
	Email       string    `json:"email"`
	Username    string    `json:"username"`
	UserStatus  string    `json:"user_status"`
	UserDeleted bool      `json:"user_deleted"`
	PausedAt    time.Time `json:"paused_at"`
}

type AccountUserPausePage struct {
	AccountID int64                   `json:"account_id"`
	Scope     string                  `json:"scope"`
	Items     []AccountUserPauseEntry `json:"items"`
	Total     int64                   `json:"total"`
	Page      int                     `json:"page"`
	PageSize  int                     `json:"page_size"`
	Pages     int64                   `json:"pages"`
}

type AccountUserPauseResult struct {
	AccountID   int64      `json:"account_id"`
	UserID      int64      `json:"user_id"`
	Email       string     `json:"email"`
	Paused      bool       `json:"paused"`
	PausedAt    *time.Time `json:"paused_at"`
	Changed     bool       `json:"changed"`
	PausedCount int64      `json:"paused_count"`
	Scope       string     `json:"scope"`
}

type AccountUserPauseSummary struct {
	Available         bool              `json:"available"`
	Counts            map[int64]int64   `json:"counts,omitempty"`
	ActivePausedUsers map[int64][]int64 `json:"active_paused_users,omitempty"`
}

// AccountUserPauseRepository 的写入必须在账号行锁内修改并回读；空组合也不能跳过串行化。
type AccountUserPauseRepository interface {
	PausedAccountIDs(context.Context, int64) (map[int64]struct{}, error)
	IsPaused(context.Context, int64, int64) (bool, error)
	List(context.Context, int64, int, int) (*AccountUserPausePage, error)
	SetPaused(context.Context, int64, int64, int64, bool) (*AccountUserPauseResult, error)
	Summary(context.Context, []AccountUserPausePair) (*AccountUserPauseSummary, error)
}

type AccountUserPauseService struct {
	repo  AccountUserPauseRepository
	users UserRepository
}

func NewAccountUserPauseService(repo AccountUserPauseRepository, users UserRepository) *AccountUserPauseService {
	return &AccountUserPauseService{repo: repo, users: users}
}

func accountUserPauseInvalid() error {
	return infraerrors.BadRequest("INVALID_REQUEST", "账号、用户、邮箱或分页参数无效")
}

func accountUserPauseError(err error) error {
	if err == nil {
		return nil
	}
	var appErr *infraerrors.ApplicationError
	if errors.As(err, &appErr) {
		return err
	}
	return ErrAccountUserPauseUnavailable.WithCause(err)
}

func (s *AccountUserPauseService) PausedAccountIDs(ctx context.Context, userID int64) (map[int64]struct{}, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	if userID <= 0 {
		return nil, accountUserPauseInvalid()
	}
	ids, err := s.repo.PausedAccountIDs(ctx, userID)
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	if ids == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	return ids, nil
}

func (s *AccountUserPauseService) IsPaused(ctx context.Context, accountID, userID int64) (bool, error) {
	if s == nil || s.repo == nil {
		return false, ErrAccountUserPauseUnavailable
	}
	if accountID <= 0 || userID <= 0 {
		return false, accountUserPauseInvalid()
	}
	paused, err := s.repo.IsPaused(ctx, accountID, userID)
	return paused, accountUserPauseError(err)
}

func (s *AccountUserPauseService) List(ctx context.Context, accountID int64, page, pageSize int) (*AccountUserPausePage, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	if accountID <= 0 || page < 1 || pageSize < 1 || pageSize > 100 || int64(page-1) > int64(^uint64(0)>>1)/int64(pageSize) {
		return nil, accountUserPauseInvalid()
	}
	result, err := s.repo.List(ctx, accountID, page, pageSize)
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	if result == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	result.Scope = AccountUserPauseScope
	return result, nil
}

func (s *AccountUserPauseService) Pause(ctx context.Context, accountID, userID int64, email string, adminID int64) (*AccountUserPauseResult, error) {
	if s == nil || s.repo == nil || s.users == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if accountID <= 0 || adminID <= 0 || userID < 0 || (userID > 0) == (email != "") {
		return nil, accountUserPauseInvalid()
	}
	var user *User
	var err error
	if email != "" {
		address, parseErr := mail.ParseAddress(email)
		if parseErr != nil || address.Address != email || len(email) > 255 {
			return nil, accountUserPauseInvalid()
		}
		user, err = s.users.GetByEmail(ctx, email)
	} else {
		user, err = s.users.GetByID(ctx, userID)
	}
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	if user == nil || user.ID <= 0 {
		return nil, ErrUserNotFound
	}
	// 邮箱只解析身份，事务内仓储还会复核该用户未软删，避免解析与写入之间的删除竞态。
	result, err := s.repo.SetPaused(ctx, accountID, user.ID, adminID, true)
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	return accountUserPauseResult(result)
}

func (s *AccountUserPauseService) Resume(ctx context.Context, accountID, userID, adminID int64) (*AccountUserPauseResult, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	if accountID <= 0 || userID <= 0 || adminID <= 0 {
		return nil, accountUserPauseInvalid()
	}
	// 恢复直接删除持久组合，不依赖当前用户查询，已停用或软删的历史规则也能清理。
	result, err := s.repo.SetPaused(ctx, accountID, userID, adminID, false)
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	return accountUserPauseResult(result)
}

func accountUserPauseResult(result *AccountUserPauseResult) (*AccountUserPauseResult, error) {
	if result == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	result.Scope = AccountUserPauseScope
	return result, nil
}

func (s *AccountUserPauseService) Summary(ctx context.Context, loads map[int64]AccountSlotObservation) (*AccountUserPauseSummary, error) {
	if s == nil || s.repo == nil {
		return nil, ErrAccountUserPauseUnavailable
	}
	pairs := make([]AccountUserPausePair, 0)
	for accountID, load := range loads {
		for rawID := range load.Users {
			userID, err := strconv.ParseInt(rawID, 10, 64)
			if err == nil && accountID > 0 && userID > 0 {
				pairs = append(pairs, AccountUserPausePair{AccountID: accountID, UserID: userID})
			}
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].AccountID != pairs[j].AccountID {
			return pairs[i].AccountID < pairs[j].AccountID
		}
		return pairs[i].UserID < pairs[j].UserID
	})
	result, err := s.repo.Summary(ctx, pairs)
	if err != nil {
		return nil, accountUserPauseError(err)
	}
	if result == nil || !result.Available {
		return nil, ErrAccountUserPauseUnavailable
	}
	return result, nil
}

// SetAccountUserPauseService 仅供初始化装配；不改变现有公开构造器和各协议的调用范围。
func (s *OpenAIGatewayService) SetAccountUserPauseService(pauses *AccountUserPauseService) {
	if s != nil {
		s.accountUserPauses = pauses
	}
}

func ProvideOpenAIGatewayServiceWithAccountUserPauses(
	accountRepo AccountRepository, usageLogRepo UsageLogRepository, usageBillingRepo UsageBillingRepository,
	userRepo UserRepository, userSubRepo UserSubscriptionRepository, userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache, cfg *config.Config, schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService, billingService *BillingService, rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService, httpUpstream HTTPUpstream, deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider, grokTokenProvider *GrokTokenProvider, resolver *ModelPricingResolver,
	channelService *ChannelService, balanceNotifyService *BalanceNotifyService, settingService *SettingService,
	userPlatformQuotaRepo UserPlatformQuotaRepository, pauses *AccountUserPauseService,
) *OpenAIGatewayService {
	svc := NewOpenAIGatewayService(accountRepo, usageLogRepo, usageBillingRepo, userRepo, userSubRepo, userGroupRateRepo, cache, cfg, schedulerSnapshot, concurrencyService, billingService, rateLimitService, billingCacheService, httpUpstream, deferredService, openAITokenProvider, grokTokenProvider, resolver, channelService, balanceNotifyService, settingService, userPlatformQuotaRepo)
	svc.SetAccountUserPauseService(pauses)
	return svc
}
