package repository

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const openAIStickyMetaPrefix = "openai_sticky_binding_meta:"
const openAIDispatchMarkerPrefix = "dispatch_marker:"
const openAIStickyIndexPrefix = "openai_sticky_user_index:"
const openAIDispatchOperationPrefix = "openai_dispatch_operation:"

func openAIStickyKeys(s service.OpenAIStickyScope) []string {
	suffix := strconv.FormatInt(s.GroupID, 10) + ":" + s.Hash
	legacy := s.LegacyHash
	if legacy == "" {
		legacy = "unused:" + s.Hash
	}
	return []string{buildSessionKey(s.GroupID, "openai:"+s.Hash), buildSessionKey(s.GroupID, "openai:"+legacy), openAIStickyMetaPrefix + suffix, openAIDispatchMarkerPrefix + suffix}
}

// 当前部署是单节点 Redis；动态索引键与 primary/legacy 元数据在同一脚本中处理。
const openAIStickyLuaBase = `
local q = cjson.decode(ARGV[1])
local s = q.scope
local p,l,m,k = KEYS[1],KEYS[2],KEYS[3],KEYS[4]
local function check(key, wanted)
 local t=redis.call('TYPE',key).ok
 if t~='none' and t~=wanted then error('dispatch key type mismatch: '..wanted) end
end
check(p,'string');check(l,'string');check(m,'hash');check(k,'string')
local tm=redis.call('TIME'); local now=tonumber(tm[1])*1000+math.floor(tonumber(tm[2])/1000)
local function effective()
 local a=redis.call('GET',p)
 local ttl=redis.call('PTTL',p)
 if not a and s.fallback and s.legacy_hash~='' then a=redis.call('GET',l);ttl=redis.call('PTTL',l) end
 if a and not tonumber(a) then error('invalid sticky account value') end
 return tonumber(a) or 0, math.max(ttl,0)
end
local account,life=effective()
local rev=redis.call('HGET',m,'revision') or 'ABSENT'
local rawmark=redis.call('GET',k)
local mark=nil
if rawmark then mark=cjson.decode(rawmark) end
local function indexkey(uid,aid) return 'openai_sticky_user_index:'..uid..':'..aid end
local function preindex(aid)
 if s.own and s.supported and s.protocol=='http' and s.user_id>0 and aid>0 then
  local idx=indexkey(s.user_id,aid);check(idx,'zset');check(idx..':truncated','string')
 end
end
local function keepmeta()
 local ttl=math.max(redis.call('PTTL',p),redis.call('PTTL',l),3900000)
 redis.call('PEXPIRE',m,ttl+300000)
end
local function index(aid,ttl)
 if not s.own or not s.supported or s.protocol~='http' or s.user_id<=0 or aid<=0 or ttl<=0 then return end
 if redis.call('HGET',m,'owner_conflict')=='1' or redis.call('HGET',m,'identity_conflict')=='1' or redis.call('HGET',m,'has_ws')=='1' then return end
 local idx=indexkey(s.user_id,aid)
 local member=tostring(s.group_id)..'|'..s.hash..'|'..s.legacy_hash
 redis.call('ZREMRANGEBYSCORE',idx,'-inf',now)
 redis.call('ZADD',idx,now+ttl,member)
 local count=redis.call('ZCARD',idx)
 if count>500 then
  redis.call('ZREMRANGEBYRANK',idx,0,count-501)
  redis.call('SET',idx..':truncated','1')
 end
 local last=redis.call('ZRANGE',idx,-1,-1,'WITHSCORES')
 local expire=ttl+300000
 if #last==2 then expire=math.max(expire,tonumber(last[2])-now+300000) end
 expire=math.max(expire,redis.call('PTTL',idx))
 redis.call('PEXPIRE',idx,expire)
 if redis.call('EXISTS',idx..':truncated')==1 then redis.call('PEXPIRE',idx..':truncated',expire) end
end
local function snapshot(at)
 local a,t=effective()
 return {account_id=a,revision=redis.call('HGET',m,'revision') or 'ABSENT',captured_at_ms=at,ttl_millis=t,
  owner_id=tonumber(redis.call('HGET',m,'owner')) or 0,owner_conflict=redis.call('HGET',m,'owner_conflict')=='1',
  identity_conflict=redis.call('HGET',m,'identity_conflict')=='1',has_ws=redis.call('HGET',m,'has_ws')=='1',
  supported=redis.call('HGET',m,'supported')=='1',marker=mark or cjson.null}
end
`

var openAIStickyReadScript = redis.NewScript(openAIStickyLuaBase + `
preindex(account)
if rev=='ABSENT' then redis.call('HSET',m,'revision',q.new_revision) end
if s.own and s.user_id>0 then
 local owner=tonumber(redis.call('HGET',m,'owner')) or 0
 if owner==0 then redis.call('HSET',m,'owner',s.user_id) elseif owner~=s.user_id then redis.call('HSET',m,'owner_conflict','1') end
 if s.fingerprint~='' then
  local fp=redis.call('HGET',m,'fingerprint')
  if fp and fp~=s.fingerprint then redis.call('HSET',m,'identity_conflict','1') else redis.call('HSET',m,'fingerprint',s.fingerprint) end
 end
 if s.protocol=='ws' then redis.call('HSET',m,'has_ws','1') end
 if s.supported and s.protocol=='http' then redis.call('HSET',m,'supported','1') end
 redis.call('HSET',m,'last_seen_ms',now)
end
keepmeta();index(account,life)
return cjson.encode(snapshot(now))
`)

var openAIStickyMutationScript = redis.NewScript(openAIStickyLuaBase + `
local e=q.expected;local v=q.mutation
if account~=e.account_id or rev~=e.revision or now-e.captured_at_ms>3600000 or now<e.captured_at_ms then
 return cjson.encode({applied=false,snapshot=snapshot(e.captured_at_ms)})
end
preindex(account);preindex(v.account_id)
if v.action~='set' and v.action~='refresh' and v.action~='delete' then error('invalid sticky mutation') end
if v.action~='delete' and (v.ttl_millis<=0 or (s.legacy_hash~='' and v.legacy_ttl_millis<=0)) then error('invalid binding TTL') end
if v.action=='set' then
 redis.call('SET',p,v.account_id,'PX',v.ttl_millis)
 if s.dual_write and s.legacy_hash~='' then redis.call('SET',l,v.account_id,'PX',v.legacy_ttl_millis) end
 if account~=v.account_id then redis.call('HSET',m,'revision',v.new_revision) end
elseif v.action=='refresh' then
 if account==0 then return cjson.encode({applied=false,snapshot=snapshot(e.captured_at_ms)}) end
 if tonumber(redis.call('GET',p))==account then redis.call('PEXPIRE',p,v.ttl_millis) end
 if s.legacy_hash~='' and tonumber(redis.call('GET',l))==account then redis.call('PEXPIRE',l,v.legacy_ttl_millis) end
elseif v.action=='delete' then
 redis.call('DEL',p)
 if s.legacy_hash~='' then redis.call('DEL',l) end
 redis.call('HSET',m,'revision',v.new_revision)
else error('invalid sticky mutation') end
keepmeta()
local a,t=effective();index(a,t)
return cjson.encode({applied=true,snapshot=snapshot(e.captured_at_ms)})
`)

func (c *gatewayCache) ReadOpenAIStickySnapshot(ctx context.Context, scope service.OpenAIStickyScope, revision string) (*service.OpenAIStickySnapshot, error) {
	b, err := json.Marshal(map[string]any{"scope": scope, "new_revision": revision})
	if err != nil {
		return nil, err
	}
	raw, err := openAIStickyReadScript.Run(ctx, c.rdb, openAIStickyKeys(scope), b).Text()
	if err != nil {
		return nil, err
	}
	var out service.OpenAIStickySnapshot
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	out.Scope = scope
	return &out, nil
}

func (c *gatewayCache) MutateOpenAIStickyBinding(ctx context.Context, scope service.OpenAIStickyScope, expected service.OpenAIStickySnapshot, mutation service.OpenAIStickyMutation) (*service.OpenAIStickySnapshot, bool, error) {
	b, err := json.Marshal(map[string]any{"scope": scope, "expected": expected, "mutation": mutation})
	if err != nil {
		return nil, false, err
	}
	raw, err := openAIStickyMutationScript.Run(ctx, c.rdb, openAIStickyKeys(scope), b).Text()
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Applied  bool                         `json:"applied"`
		Snapshot service.OpenAIStickySnapshot `json:"snapshot"`
	}
	if err = json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, false, err
	}
	out.Snapshot.Scope = scope
	return &out.Snapshot, out.Applied, nil
}

func (c *gatewayCache) ListOpenAIStickyUserBindings(ctx context.Context, uid, aid int64) ([]service.OpenAIStickyScope, bool, error) {
	idx := openAIStickyIndexPrefix + strconv.FormatInt(uid, 10) + ":" + strconv.FormatInt(aid, 10)
	now, err := c.rdb.Time(ctx).Result()
	if err != nil {
		return nil, false, err
	}
	if err = c.rdb.ZRemRangeByScore(ctx, idx, "-inf", strconv.FormatInt(now.UnixMilli(), 10)).Err(); err != nil {
		return nil, false, err
	}
	pipe := c.rdb.Pipeline()
	members := pipe.ZRange(ctx, idx, 0, 500)
	truncated := pipe.Exists(ctx, idx+":truncated")
	if _, err = pipe.Exec(ctx); err != nil {
		return nil, false, err
	}
	out := make([]service.OpenAIStickyScope, 0, len(members.Val()))
	for _, member := range members.Val() {
		parts := strings.SplitN(member, "|", 3)
		if len(parts) != 3 {
			continue
		}
		gid, e := strconv.ParseInt(parts[0], 10, 64)
		if e != nil {
			continue
		}
		out = append(out, service.OpenAIStickyScope{GroupID: gid, Hash: parts[1], LegacyHash: parts[2], UserID: uid, Own: false})
	}
	return out, truncated.Val() > 0, nil
}

func openAIDispatchPreviewKey(id string) string   { return "openai_dispatch_preview:" + id }
func openAIDispatchOperationKey(id string) string { return openAIDispatchOperationPrefix + id }
func openAIDispatchIdempotencyKey(owner, key string) string {
	return "openai_dispatch_idempotency:" + owner + ":" + key
}

func (c *gatewayCache) SaveOpenAIDispatchPreview(ctx context.Context, p *service.OpenAIDispatchPreviewRecord) error {
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	return c.rdb.Set(ctx, openAIDispatchPreviewKey(p.Preview.PreviewID), raw, service.OpenAIDispatchPreviewTTL).Err()
}
func (c *gatewayCache) GetOpenAIDispatchPreview(ctx context.Context, id string) (*service.OpenAIDispatchPreviewRecord, error) {
	raw, e := c.rdb.Get(ctx, openAIDispatchPreviewKey(id)).Bytes()
	if e != nil {
		return nil, e
	}
	var p service.OpenAIDispatchPreviewRecord
	e = json.Unmarshal(raw, &p)
	return &p, e
}
func (c *gatewayCache) GetOpenAIDispatchOperation(ctx context.Context, id string) (*service.OpenAIDispatchOperationRecord, error) {
	raw, e := c.rdb.Get(ctx, openAIDispatchOperationKey(id)).Bytes()
	if e != nil {
		return nil, e
	}
	var p service.OpenAIDispatchOperationRecord
	e = json.Unmarshal(raw, &p)
	return &p, e
}
func (c *gatewayCache) FindOpenAIDispatchOperation(ctx context.Context, owner, key string) (*service.OpenAIDispatchOperationRecord, error) {
	id, e := c.rdb.Get(ctx, openAIDispatchIdempotencyKey(owner, key)).Result()
	if e == redis.Nil {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return c.GetOpenAIDispatchOperation(ctx, id)
}

var reserveOpenAIDispatchScript = redis.NewScript(`
local id=redis.call('GET',KEYS[1])
if id then return redis.call('GET','openai_dispatch_operation:'..id) or redis.error_reply('dispatch result missing') end
local a=redis.call('TYPE',KEYS[2]).ok
if a~='none' then return redis.error_reply('dispatch operation collision') end
redis.call('SET',KEYS[2],ARGV[2],'PX',ARGV[3])
redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[3])
return ARGV[2]
`)

func (c *gatewayCache) ReserveOpenAIDispatchOperation(ctx context.Context, key string, record *service.OpenAIDispatchOperationRecord) (*service.OpenAIDispatchOperationRecord, error) {
	raw, e := json.Marshal(record)
	if e != nil {
		return nil, e
	}
	result, e := reserveOpenAIDispatchScript.Run(ctx, c.rdb, []string{openAIDispatchIdempotencyKey(record.OwnerHash, key), openAIDispatchOperationKey(record.Operation.OperationID)}, record.Operation.OperationID, raw, service.OpenAIDispatchOperationTTL.Milliseconds()).Text()
	if e != nil {
		return nil, e
	}
	var out service.OpenAIDispatchOperationRecord
	e = json.Unmarshal([]byte(result), &out)
	return &out, e
}

var rebindOpenAIDispatchScript = redis.NewScript(openAIStickyLuaBase + `
local opkey=KEYS[5];check(opkey,'string')
local raw=redis.call('GET',opkey)
if not raw then return redis.error_reply('dispatch operation missing') end
local op=cjson.decode(raw);local n=q.index;local item=op.operation.sessions[n]
if not item then return redis.error_reply('dispatch session missing') end
if item.rebind_result~='not_processed' and item.rebind_result~='result_unknown' then return item.rebind_result end
if q.ttl_ms<=0 or (s.dual_write and s.legacy_hash~='' and q.legacy_ttl_ms<=0) then return redis.error_reply('invalid binding TTL') end
local oldkey=nil;local old=nil;local oldref=nil
if mark and mark.operation_id~=op.operation.operation_id then
 oldref=mark.session_ref;oldkey='openai_dispatch_operation:'..mark.operation_id;check(oldkey,'string')
 local oldraw=redis.call('GET',oldkey)
 if not oldraw then return redis.error_reply('prior dispatch result missing') end
 old=cjson.decode(oldraw)
 local found=false
 for _,v in ipairs(old.operation.sessions) do
  if v.session_ref==oldref and v.rebind_result=='rebound' then found=true end
 end
 if not found then return redis.error_reply('prior dispatch session missing') end
end
preindex(q.target)
local reason=nil
if now>q.execution_deadline_ms then reason='execution_expired'
elseif not q.enabled then return 'not_processed'
elseif account~=q.expected.account_id or rev~=q.expected.revision then reason='binding_changed'
elseif tonumber(redis.call('HGET',m,'owner'))~=op.operation.user_id then reason='owner_unknown'
elseif redis.call('HGET',m,'owner_conflict')=='1' or redis.call('HGET',m,'identity_conflict')=='1' then reason='owner_conflict'
elseif redis.call('HGET',m,'has_ws')=='1' then reason='protocol_unsupported'
elseif redis.call('HGET',m,'supported')~='1' then reason='identity_unsupported'
elseif not q.target_eligible then reason='target_ineligible' end
if reason then item.rebind_result=reason;item.reason=reason;item.observation='not_observed'
else
 redis.call('SET',p,q.target,'PX',q.ttl_ms)
 if s.legacy_hash~='' then
  if s.dual_write then redis.call('SET',l,q.target,'PX',q.legacy_ttl_ms) else redis.call('DEL',l) end
 end
 redis.call('HSET',m,'revision',q.new_revision)
 mark={operation_id=op.operation.operation_id,session_ref=item.session_ref,rebind_revision=q.new_revision,deadline_ms=q.observation_deadline_ms}
 redis.call('SET',k,cjson.encode(mark),'PX',math.max(q.observation_deadline_ms-now,1))
 if old then
  for _,v in ipairs(old.operation.sessions) do
   if v.session_ref==oldref and v.observation=='awaiting_observation' then v.observation='superseded' end
  end
  redis.call('SET',oldkey,cjson.encode(old),'KEEPTTL')
 end
 keepmeta();index(q.target,q.ttl_ms)
 item.rebind_result='rebound';item.rebind_revision=q.new_revision;item.rebound_at=q.rebound_at
 item.observation='awaiting_observation';item.current_binding=q.target
end
redis.call('SET',opkey,cjson.encode(op),'KEEPTTL')
return item.rebind_result
`)

func (c *gatewayCache) RebindOpenAIDispatchSession(ctx context.Context, record *service.OpenAIDispatchOperationRecord, i int, ttl, legacyTTL time.Duration, revision string, eligible bool) error {
	expected := record.Bindings[i]
	scope := expected.Scope
	scope.UserID = record.Operation.UserID
	scope.Own = true
	scope.Supported = true
	scope.Protocol = "http"
	// 旧操作的引用由脚本读阶段捕获，不能从请求携带的旧 marker 决定要替代哪条历史。
	q := map[string]any{"scope": scope, "expected": expected, "index": i + 1, "target": record.Operation.TargetAccountID, "target_eligible": eligible, "enabled": true, "ttl_ms": ttl.Milliseconds(), "legacy_ttl_ms": legacyTTL.Milliseconds(), "new_revision": revision, "rebound_at": time.Now().UTC().Format(time.RFC3339Nano), "execution_deadline_ms": record.Operation.ExecutionDeadline.UnixMilli(), "observation_deadline_ms": record.Operation.ObservationDeadline.UnixMilli()}
	raw, e := json.Marshal(q)
	if e != nil {
		return e
	}
	keys := append(openAIStickyKeys(scope), openAIDispatchOperationKey(record.Operation.OperationID))
	_, e = rebindOpenAIDispatchScript.Run(ctx, c.rdb, keys, raw).Result()
	return e
}

var observeOpenAIDispatchScript = redis.NewScript(`
local markraw=redis.call('GET',KEYS[1]);local raw=redis.call('GET',KEYS[2])
if not markraw or not raw then return 0 end
local marker=cjson.decode(markraw);local expected=cjson.decode(ARGV[1])
local tm=redis.call('TIME');local now=tonumber(tm[1])*1000+math.floor(tonumber(tm[2])/1000)
if marker.operation_id~=expected.operation_id or marker.rebind_revision~=expected.rebind_revision or marker.session_ref~=expected.session_ref or now>marker.deadline_ms then return 0 end
local op=cjson.decode(raw)
for _,item in ipairs(op.operation.sessions) do
 if item.session_ref==expected.session_ref and item.rebind_result=='rebound' and item.observation=='awaiting_observation' then
  item.assigned_account_id=tonumber(ARGV[2]);item.assigned_at=ARGV[4];item.queued=ARGV[3]=='1'
  item.observation='assigned_other';if tonumber(ARGV[2])==op.operation.target_account_id then item.observation='assigned_target' end
  redis.call('SET',KEYS[2],cjson.encode(op),'KEEPTTL');return 1
 end
end
return 0
`)

func (c *gatewayCache) ObserveOpenAIDispatchAdmission(ctx context.Context, scope service.OpenAIStickyScope, marker service.OpenAIDispatchMarker, aid int64, queued bool) error {
	b, e := json.Marshal(marker)
	if e != nil {
		return e
	}
	flag := "0"
	if queued {
		flag = "1"
	}
	_, e = observeOpenAIDispatchScript.Run(ctx, c.rdb, []string{openAIStickyKeys(scope)[3], openAIDispatchOperationKey(marker.OperationID)}, b, aid, flag, time.Now().UTC().Format(time.RFC3339Nano)).Result()
	return e
}

var _ service.OpenAIUserDispatchCache = (*gatewayCache)(nil)
