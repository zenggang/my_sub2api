package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenAIStickySpilloverWaitPlanCarriesAdmissionIntent(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "advanced"}[advanced], func(t *testing.T) {
			groupID := int64(91)
			accounts := []Account{
				{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 10, GroupIDs: []int64{groupID}},
				{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Schedulable: true, Concurrency: 1, Priority: 0, GroupIDs: []int64{groupID}},
			}
			cache := &schedulerTestGatewayCache{sessionBindings: map[string]int64{"openai:spill": 1}}
			cfg := &config.Config{}
			cfg.Gateway.Scheduling.LoadBatchEnabled = true
			cfg.Gateway.Scheduling.StickySessionMaxWaiting = 1
			cfg.Gateway.Scheduling.StickySessionWaitTimeout = time.Second
			cfg.Gateway.Scheduling.FallbackMaxWaiting = 10
			cfg.Gateway.Scheduling.FallbackWaitTimeout = time.Second
			cfg.Gateway.OpenAIScheduler.StickyEscapeEnabled = true
			concurrency := schedulerTestConcurrencyCache{
				acquireResults: map[int64]bool{1: false, 2: false},
				waitCounts:     map[int64]int{1: 1},
				loadMap: map[int64]*AccountLoadInfo{
					1: {AccountID: 1, CurrentConcurrency: 1, WaitingCount: 1, LoadRate: 200},
					2: {AccountID: 2, CurrentConcurrency: 1, LoadRate: 100},
				},
			}
			svc := &OpenAIGatewayService{cache: cache, cfg: cfg, accountRepo: schedulerTestOpenAIAccountRepo{accounts: accounts}, concurrencyService: NewConcurrencyService(concurrency)}
			var selection *AccountSelectionResult
			var err error
			if advanced {
				scheduler := newDefaultOpenAIAccountScheduler(svc, newOpenAIAccountRuntimeStats())
				selection, _, err = scheduler.Select(context.Background(), OpenAIAccountScheduleRequest{GroupID: &groupID, Platform: PlatformOpenAI, SessionHash: "spill", StickyAccountID: 1})
			} else {
				selection, err = svc.selectAccountWithLoadAwareness(context.Background(), &groupID, PlatformOpenAI, "spill", "", nil, false, "", false)
			}
			require.NoError(t, err)
			require.NotNil(t, selection)
			require.NotNil(t, selection.WaitPlan)
			require.True(t, selection.PreserveStickyBinding(), "等待准入的临时账号也不得覆盖原粘性")
			require.Equal(t, int64(1), cache.sessionBindings["openai:spill"])
		})
	}
}
