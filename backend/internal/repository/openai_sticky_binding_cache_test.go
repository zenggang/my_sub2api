package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newDispatchCacheTest(t *testing.T) (*gatewayCache, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(time.Now().UTC())
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &gatewayCache{rdb: client}, mr
}

func dispatchTestScope(hash string) service.OpenAIStickyScope {
	return service.OpenAIStickyScope{GroupID: 91, UserID: 42, Hash: hash, LegacyHash: "legacy-" + hash, Fingerprint: "fingerprint-" + hash, Protocol: "http", Own: true, Supported: true, Fallback: true, DualWrite: true}
}

func readDispatchTest(t *testing.T, cache *gatewayCache, scope service.OpenAIStickyScope) *service.OpenAIStickySnapshot {
	t.Helper()
	snapshot, err := cache.ReadOpenAIStickySnapshot(context.Background(), scope, uuid.NewString())
	require.NoError(t, err)
	return snapshot
}

func mutateDispatchTest(t *testing.T, cache *gatewayCache, scope service.OpenAIStickyScope, expected *service.OpenAIStickySnapshot, action string, accountID int64) (*service.OpenAIStickySnapshot, bool) {
	t.Helper()
	snapshot, applied, err := cache.MutateOpenAIStickyBinding(context.Background(), scope, *expected, service.OpenAIStickyMutation{Action: action, AccountID: accountID, TTLMillis: int64(time.Hour / time.Millisecond), LegacyTTLMillis: int64(10 * time.Minute / time.Millisecond), NewRevision: uuid.NewString()})
	require.NoError(t, err)
	return snapshot, applied
}

func seedDispatchTest(t *testing.T, cache *gatewayCache, scope service.OpenAIStickyScope, accountID int64) *service.OpenAIStickySnapshot {
	t.Helper()
	snapshot, applied := mutateDispatchTest(t, cache, scope, readDispatchTest(t, cache, scope), "set", accountID)
	require.True(t, applied)
	return snapshot
}

func reserveDispatchTest(t *testing.T, cache *gatewayCache, expected *service.OpenAIStickySnapshot, targetID int64) *service.OpenAIDispatchOperationRecord {
	t.Helper()
	now := time.Now().UTC()
	record := &service.OpenAIDispatchOperationRecord{OwnerHash: "admin-test", BodyHash: "body-test", Bindings: []service.OpenAIStickySnapshot{*expected}, Operation: service.OpenAIDispatchOperation{OperationID: uuid.NewString(), UserID: expected.Scope.UserID, SourceAccountID: expected.AccountID, TargetAccountID: targetID, CreatedAt: now, ExecutionDeadline: now.Add(2 * time.Minute), ObservationDeadline: now.Add(15 * time.Minute), Sessions: []service.OpenAIDispatchSession{{SessionRef: uuid.NewString(), GroupID: expected.Scope.GroupID, RebindResult: "not_processed", Observation: "awaiting_observation"}}}}
	stored, err := cache.ReserveOpenAIDispatchOperation(context.Background(), uuid.NewString(), record)
	require.NoError(t, err)
	return stored
}

func rebindDispatchTest(t *testing.T, cache *gatewayCache, record *service.OpenAIDispatchOperationRecord) {
	t.Helper()
	require.NoError(t, cache.RebindOpenAIDispatchSession(context.Background(), record, 0, time.Hour, 10*time.Minute, uuid.NewString(), true))
}

func TestOpenAIStickyRebindRejectsAllOldMutations(t *testing.T) {
	for _, action := range []string{"set", "refresh", "delete"} {
		t.Run(action, func(t *testing.T) {
			cache, _ := newDispatchCacheTest(t)
			scope := dispatchTestScope("old-" + action)
			old := seedDispatchTest(t, cache, scope, 1)
			op := reserveDispatchTest(t, cache, old, 2)
			rebindDispatchTest(t, cache, op)
			rebound := readDispatchTest(t, cache, scope)
			_, applied := mutateDispatchTest(t, cache, scope, old, action, 3)
			require.False(t, applied)
			latest := readDispatchTest(t, cache, scope)
			require.Equal(t, int64(2), latest.AccountID)
			require.Equal(t, rebound.Revision, latest.Revision)
			require.Equal(t, "2", cache.rdb.Get(context.Background(), openAIStickyKeys(scope)[1]).Val())
			bindings, truncated, err := cache.ListOpenAIStickyUserBindings(context.Background(), 42, 2)
			require.NoError(t, err)
			require.False(t, truncated)
			require.Len(t, bindings, 1, "管理 CAS 必须立即登记目标空闲会话")
		})
	}
}

func TestOpenAIDispatchRetryPreservesReverseOperationAndFirstAdmission(t *testing.T) {
	cache, _ := newDispatchCacheTest(t)
	scope := dispatchTestScope("reverse")
	first := reserveDispatchTest(t, cache, seedDispatchTest(t, cache, scope, 1), 2)
	rebindDispatchTest(t, cache, first)
	firstPoint := readDispatchTest(t, cache, scope)
	second := reserveDispatchTest(t, cache, firstPoint, 1)
	rebindDispatchTest(t, cache, second)
	// O1 的客户端没有拿到结果；其重试只能恢复原结果，不能覆盖 O2 的反向改绑。
	rebindDispatchTest(t, cache, first)
	require.Equal(t, int64(1), readDispatchTest(t, cache, scope).AccountID)
	oldResult, err := cache.GetOpenAIDispatchOperation(context.Background(), first.Operation.OperationID)
	require.NoError(t, err)
	require.Equal(t, "rebound", oldResult.Operation.Sessions[0].RebindResult)
	require.Equal(t, "superseded", oldResult.Operation.Sessions[0].Observation)
	require.Equal(t, firstPoint.Revision, oldResult.Operation.Sessions[0].RebindRevision)
	require.NotNil(t, oldResult.Operation.Sessions[0].ReboundAt)
	latest := readDispatchTest(t, cache, scope)
	changed, applied := mutateDispatchTest(t, cache, scope, latest, "set", 3)
	require.True(t, applied)
	require.Equal(t, latest.Marker, changed.Marker, "普通换号保留独立观察身份")
	require.NoError(t, cache.ObserveOpenAIDispatchAdmission(context.Background(), scope, *firstPoint.Marker, 2, false))
	require.NoError(t, cache.ObserveOpenAIDispatchAdmission(context.Background(), scope, *latest.Marker, 3, true))
	require.NoError(t, cache.ObserveOpenAIDispatchAdmission(context.Background(), scope, *latest.Marker, 1, false))
	result, err := cache.GetOpenAIDispatchOperation(context.Background(), second.Operation.OperationID)
	require.NoError(t, err)
	require.Equal(t, "assigned_other", result.Operation.Sessions[0].Observation)
	require.Equal(t, int64(3), *result.Operation.Sessions[0].AssignedAccountID)
	require.True(t, result.Operation.Sessions[0].Queued)
}

func TestOpenAIStickyLegacyEffectiveBindingAndResidualCleanup(t *testing.T) {
	cache, mr := newDispatchCacheTest(t)
	scope := dispatchTestScope("legacy-only")
	scope.DualWrite = false
	require.NoError(t, cache.rdb.Set(context.Background(), openAIStickyKeys(scope)[1], 1, 10*time.Minute).Err())
	legacy := readDispatchTest(t, cache, scope)
	require.Equal(t, int64(1), legacy.AccountID)
	op := reserveDispatchTest(t, cache, legacy, 2)
	rebindDispatchTest(t, cache, op)
	require.Equal(t, int64(2), readDispatchTest(t, cache, scope).AccountID)
	require.False(t, mr.Exists(openAIStickyKeys(scope)[1]), "关闭双写后必须删残留，避免主键过期又读回 A")
	mr.FastForward(time.Hour + time.Second)
	require.Zero(t, readDispatchTest(t, cache, scope).AccountID)

	other := dispatchTestScope("primary-wins")
	old := seedDispatchTest(t, cache, other, 1)
	require.NoError(t, cache.rdb.Set(context.Background(), openAIStickyKeys(other)[0], 3, time.Hour).Err())
	require.Equal(t, int64(3), readDispatchTest(t, cache, other).AccountID)
	rebindDispatchTest(t, cache, reserveDispatchTest(t, cache, old, 2))
	require.Equal(t, int64(3), readDispatchTest(t, cache, other).AccountID)
}

func TestOpenAIStickyRevisionProtectsABAAndFixedSnapshotAge(t *testing.T) {
	cache, mr := newDispatchCacheTest(t)
	scope := dispatchTestScope("aba")
	empty := readDispatchTest(t, cache, scope)
	first, applied := mutateDispatchTest(t, cache, scope, empty, "set", 1)
	require.True(t, applied)
	deleted, applied := mutateDispatchTest(t, cache, scope, first, "delete", 0)
	require.True(t, applied)
	require.Zero(t, deleted.AccountID)
	require.GreaterOrEqual(t, mr.TTL(openAIStickyKeys(scope)[2]), service.OpenAIStickyVersionMinTTL)
	_, applied = mutateDispatchTest(t, cache, scope, empty, "set", 3)
	require.False(t, applied, "无绑定 tombstone 也必须防 ABA")
	restored, applied := mutateDispatchTest(t, cache, scope, deleted, "set", 1)
	require.True(t, applied)
	_, applied = mutateDispatchTest(t, cache, scope, first, "delete", 0)
	require.False(t, applied, "账号值回到 A 不能授权旧版本")
	require.Equal(t, empty.CapturedAtMS, restored.CapturedAtMS, "本请求成功 CAS 只推进版本，不重新计算快照年龄")
	mr.SetTime(time.UnixMilli(empty.CapturedAtMS).Add(time.Hour + time.Second))
	_, applied = mutateDispatchTest(t, cache, scope, restored, "refresh", 0)
	require.False(t, applied, "超过 H 的请求只跳过缓存操作")
}

func TestOpenAIStickyConcurrentCASHasOneWinner(t *testing.T) {
	cache, _ := newDispatchCacheTest(t)
	scope := dispatchTestScope("concurrent")
	old := seedDispatchTest(t, cache, scope, 1)
	ops := []*service.OpenAIDispatchOperationRecord{reserveDispatchTest(t, cache, old, 2), reserveDispatchTest(t, cache, old, 3)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, op := range ops {
		wg.Add(1)
		go func(op *service.OpenAIDispatchOperationRecord) {
			defer wg.Done()
			<-start
			if err := cache.RebindOpenAIDispatchSession(context.Background(), op, 0, time.Hour, 10*time.Minute, uuid.NewString(), true); err != nil {
				t.Error(err)
			}
		}(op)
	}
	close(start)
	wg.Wait()
	results := map[string]int{}
	for _, op := range ops {
		stored, err := cache.GetOpenAIDispatchOperation(context.Background(), op.Operation.OperationID)
		require.NoError(t, err)
		results[stored.Operation.Sessions[0].RebindResult]++
	}
	require.Equal(t, map[string]int{"rebound": 1, "binding_changed": 1}, results)
}

func TestOpenAIStickyIndexDoesNotShortenOtherSessionLifetime(t *testing.T) {
	cache, mr := newDispatchCacheTest(t)
	seedDispatchTest(t, cache, dispatchTestScope("long"), 1)
	short := dispatchTestScope("short")
	_, applied, err := cache.MutateOpenAIStickyBinding(context.Background(), short, *readDispatchTest(t, cache, short), service.OpenAIStickyMutation{Action: "set", AccountID: 1, TTLMillis: 60000, LegacyTTLMillis: 60000, NewRevision: uuid.NewString()})
	require.NoError(t, err)
	require.True(t, applied)
	require.GreaterOrEqual(t, mr.TTL(fmt.Sprintf("%s42:1", openAIStickyIndexPrefix)), time.Hour)
}

func TestOpenAIStickyRejectedOwnerProtocolAndScriptPrevalidation(t *testing.T) {
	for _, kind := range []string{"owner", "identity", "ws", "bad-index", "bad-ttl"} {
		t.Run(kind, func(t *testing.T) {
			cache, mr := newDispatchCacheTest(t)
			scope := dispatchTestScope(kind)
			old := seedDispatchTest(t, cache, scope, 1)
			op := reserveDispatchTest(t, cache, old, 2)
			foreign := scope
			switch kind {
			case "owner":
				foreign.UserID = 43
			case "identity":
				foreign.Fingerprint = "different"
			case "ws":
				foreign.Protocol = "ws"
			case "bad-index":
				require.NoError(t, mr.Set(openAIStickyIndexPrefix+"42:2", "bad"))
			}
			if kind == "owner" || kind == "identity" || kind == "ws" {
				readDispatchTest(t, cache, foreign)
			}
			legacyTTL := 10 * time.Minute
			if kind == "bad-ttl" {
				legacyTTL = 0
			}
			err := cache.RebindOpenAIDispatchSession(context.Background(), op, 0, time.Hour, legacyTTL, uuid.NewString(), true)
			if kind == "bad-index" || kind == "bad-ttl" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, int64(1), readDispatchTest(t, cache, scope).AccountID, "预校验失败不得留下部分绑定")
		})
	}
}

func TestOpenAIDispatchRealRedisAtomicRecovery(t *testing.T) {
	socket := os.Getenv("SUB2API_DISPATCH_TEST_REDIS_SOCKET")
	if socket == "" {
		t.Skip("set isolated local Redis socket to verify native Lua")
	}
	require.True(t, strings.HasPrefix(socket, "/tmp/sub2api-dispatch."), "only an isolated test socket is accepted")
	rdb := redis.NewClient(&redis.Options{Network: "unix", Addr: socket})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(context.Background()).Err())
	cache := &gatewayCache{rdb: rdb}
	scope := dispatchTestScope(uuid.NewString())
	old := seedDispatchTest(t, cache, scope, 1)
	first := reserveDispatchTest(t, cache, old, 2)
	rebindDispatchTest(t, cache, first)
	_, applied := mutateDispatchTest(t, cache, scope, old, "delete", 0)
	require.False(t, applied)
	second := reserveDispatchTest(t, cache, readDispatchTest(t, cache, scope), 1)
	rebindDispatchTest(t, cache, second)
	// 新建 cache 模拟进程恢复，再用原 O1 身份重试；操作事实与绑定必须一起保留。
	restarted := &gatewayCache{rdb: rdb}
	rebindDispatchTest(t, restarted, first)
	require.Equal(t, int64(1), readDispatchTest(t, restarted, scope).AccountID)
	stored, err := restarted.GetOpenAIDispatchOperation(context.Background(), first.Operation.OperationID)
	require.NoError(t, err)
	require.Equal(t, "rebound", stored.Operation.Sessions[0].RebindResult)
	require.Equal(t, "superseded", stored.Operation.Sessions[0].Observation)

	shared := readDispatchTest(t, restarted, scope)
	ops := []*service.OpenAIDispatchOperationRecord{reserveDispatchTest(t, restarted, shared, 2), reserveDispatchTest(t, restarted, shared, 3)}
	var wg sync.WaitGroup
	for _, op := range ops {
		wg.Add(1)
		go func(op *service.OpenAIDispatchOperationRecord) {
			defer wg.Done()
			if err := restarted.RebindOpenAIDispatchSession(context.Background(), op, 0, time.Hour, 10*time.Minute, uuid.NewString(), true); err != nil {
				t.Error(err)
			}
		}(op)
	}
	wg.Wait()
	counts := map[string]int{}
	for _, op := range ops {
		record, err := restarted.GetOpenAIDispatchOperation(context.Background(), op.Operation.OperationID)
		require.NoError(t, err)
		counts[record.Operation.Sessions[0].RebindResult]++
	}
	require.Equal(t, map[string]int{"rebound": 1, "binding_changed": 1}, counts)
}

func TestOpenAIDispatchAdmissionRechecksOwnerAfterCapture(t *testing.T) {
	cache, _ := newDispatchCacheTest(t)
	scope := dispatchTestScope("late-owner-conflict")
	op := reserveDispatchTest(t, cache, seedDispatchTest(t, cache, scope, 1), 2)
	rebindDispatchTest(t, cache, op)
	captured := readDispatchTest(t, cache, scope)
	foreign := scope
	foreign.UserID = 43
	readDispatchTest(t, cache, foreign)
	require.NoError(t, cache.ObserveOpenAIDispatchAdmission(context.Background(), foreign, *captured.Marker, 2, false))
	require.NoError(t, cache.ObserveOpenAIDispatchAdmission(context.Background(), scope, *captured.Marker, 2, false))
	stored, err := cache.GetOpenAIDispatchOperation(context.Background(), op.Operation.OperationID)
	require.NoError(t, err)
	require.Equal(t, "awaiting_observation", stored.Operation.Sessions[0].Observation)
	require.Nil(t, stored.Operation.Sessions[0].AssignedAccountID, "碰撞发生在 capture 和 admission 之间，也不能错误归属")
}
