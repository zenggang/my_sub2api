from __future__ import print_function
import json, os, subprocess, sys, uuid

os.umask(0o077)
port=int(sys.argv[1]); model=sys.argv[2]; key_id=int(sys.argv[3])
expected_model=sys.argv[4] if len(sys.argv)>4 else model
key=subprocess.check_output(['sudo','-u','postgres','/opt/postgresql/bin/psql','-h','/var/run/postgresql','-d','sub2api','-X','-At','-c','select key from api_keys where id=%d and deleted_at is null' % key_id]).strip().decode('utf-8')
if not key: raise RuntimeError('smoke key not found')
body=json.load(open('/data/sub2api-patched/tools/smoke-namespace.json'))
body['model']=model
body['input'].append({'type':'additional_tools','role':'developer','tools':body.pop('tools')})
def esc(s):
    return s.replace('\\','\\\\').replace('"','\\"').replace('\n','\\n').replace('\r','\\r')
for turn in range(2):
    rid=str(uuid.uuid4())
    headers={'Authorization':'Bearer '+key,'Content-Type':'application/json','Accept':'text/event-stream','X-OpenAI-Internal-Codex-Responses-Lite':'true','x-client-request-id':rid,'User-Agent':'codex_cli_rs/0.153.4'}
    cfg='\n'.join('header = "'+esc(k+': '+v)+'"' for k,v in headers.items())+'\ndata = "'+esc(json.dumps(body))+'"\n'
    p=subprocess.Popen(['curl','-sS','--noproxy','*','--max-time','90','--config','-','http://127.0.0.1:%d/v1/responses'%port],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
    out,err=p.communicate(cfg.encode('utf-8'))
    events=[json.loads(line[6:]) for line in out.decode('utf-8').splitlines() if line.startswith('data: {')]
    terms=[e for e in events if e.get('type') in ['response.completed','response.failed','error']]
    items=[e['item'] for e in events if e.get('type')=='response.output_item.done']
    if not terms:
        print(json.dumps({'turn':turn,'client_request_id':rid,'error':'missing_terminal','exit':p.returncode}));sys.exit(1)
    terminal=terms[-1]
    if not items: items=terminal.get('response',{}).get('output',[])
    print(json.dumps({'turn':turn,'client_request_id':rid,'terminal':terminal.get('type'),'model':terminal.get('response',{}).get('model'),'item_types':[item.get('type') for item in items],'tool_namespaces':[item.get('namespace') for item in items if item.get('type')=='function_call']}))
    sys.stdout.flush()
    if terminal.get('type')!='response.completed':sys.exit(2)
    # A configured channel alias can change the upstream model; still require the exact target.
    if terminal.get('response',{}).get('model')!=expected_model: raise RuntimeError('downstream model name changed')
    if turn==0:
        calls=[item for item in items if item.get('type')=='function_call']
        if not calls or calls[0].get('namespace')!='compat_probe': raise RuntimeError('missing namespace tool call')
        body['input'].extend(items)
        for item in calls:
            body['input'].append({'type':'function_call_output','call_id':item['call_id'],'output':'hello'})
        body['instructions']='The echo tool has returned. Reply with its text and do not call any tool again.'
    else:
        texts=[c.get('text','') for item in items for c in item.get('content',[]) if c.get('type')=='output_text']
        if 'hello' not in ' '.join(texts).lower(): raise RuntimeError('missing final answer')
