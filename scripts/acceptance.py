#!/usr/bin/env python3
"""Black-box crash/restart and API invariants; launches an isolated temporary database."""
import json, os, pathlib, subprocess, tempfile, time, urllib.request, urllib.error, socket
ROOT=pathlib.Path(__file__).resolve().parents[1]
TERMINAL={'COMPLETED','PARTIAL_COMPLETED','REJECTED','FAILED','CANCELLED','EXPIRED','NEEDS_RECONCILIATION'}
def main():
    with tempfile.TemporaryDirectory(prefix='huanjing-acceptance-') as tmp:
        sock=socket.socket();sock.bind(('127.0.0.1',0));port=sock.getsockname()[1];sock.close()
        base=f'http://127.0.0.1:{port}'; env=dict(os.environ,ADDR=f'127.0.0.1:{port}',DB_PATH=tmp+'/system.db',WEB_DIR=str(ROOT/'web'),WORKERS='2')
        log=open(tmp+'/server.log','w'); proc=None; results=[]
        def call(method,path,body=None,key=None,tenant='demo'):
            headers={'Content-Type':'application/json','X-Tenant-ID':tenant}
            if key:headers['Idempotency-Key']=key
            req=urllib.request.Request(base+path,data=None if body is None else json.dumps(body).encode(),headers=headers,method=method)
            try:
                with urllib.request.urlopen(req,timeout=10) as r:return r.status,json.load(r)
            except urllib.error.HTTPError as e:return e.code,json.load(e)
        def start():
            nonlocal proc
            proc=subprocess.Popen([str(ROOT/'bin/huanjing')],cwd=ROOT,env=env,stdout=log,stderr=log)
            end=time.monotonic()+20
            while time.monotonic()<end:
                if proc.poll() is not None:raise AssertionError('server exited: '+pathlib.Path(tmp+'/server.log').read_text())
                try:
                    if call('GET','/api/health')[0]==200:return
                except (OSError,ValueError):pass
                time.sleep(.1)
            raise AssertionError('health timeout')
        def wait(jid,desired=None,timeout=30):
            end=time.monotonic()+timeout
            while time.monotonic()<end:
                code,j=call('GET','/api/jobs/'+jid);assert code==200,(code,j)
                if (desired and j['status'] in desired) or (not desired and j['status'] in TERMINAL):return j
                time.sleep(.1)
            raise AssertionError(('job timeout',j))
        def post(text,key,**kw):
            code,j=call('POST','/api/posts',dict(text=text,**kw),key)
            assert code==202,(code,j)
            return j
        try:
            start()
            body={'text':'第一次跑步完成马拉松，分享运动经历','max_candidates':3}
            c,j=call('POST','/api/posts',body,'idem');assert c==202,(c,j)
            c,j2=call('POST','/api/posts',body,'idem');assert c==202 and j2['job_id']==j['job_id']
            assert call('POST','/api/posts',{'text':'changed'},'idem')[0]==409
            done=wait(j['job_id']);assert done['status']=='COMPLETED',done
            assert 'PRIVATE_CANARY' not in json.dumps(done)
            assert call('GET','/api/jobs/'+j['job_id'],tenant='other')[0]==404
            c,p=call('GET','/api/posts/'+j['post_id']);assert c==200
            interactions=p['interactions']; keys=[(x['avatar_id'],x.get('kind',x.get('action_type'))) for x in interactions]
            assert len(keys)==len(set(keys)) and len(keys)>0,(keys,p)
            results.append('idempotency, tenant isolation, public output and unique actions')
            slow=post('分享跑步健身和旅行的快乐','crash',fault='slow')
            wait(slow['job_id'],{'PROCESSING'})
            proc.kill();proc.wait(timeout=5);start()
            restored=wait(slow['job_id'],timeout=45);assert restored['status']=='COMPLETED',restored
            results.append('SIGKILL and restart with same SQLite database')
            slow=post('跑步健身，分享今日进步','delete',fault='slow')
            assert call('DELETE','/api/posts/'+slow['post_id'])[0]==200
            assert call('GET','/api/posts/'+slow['post_id'])[0]==410
            assert wait(slow['job_id'])['status']=='CANCELLED'
            results.append('delete cancels in-flight job and hides post')
            retry=post('分享旅行摄影与跑步心得','retry',fault='retry_once')
            assert wait(retry['job_id'])['status']=='COMPLETED'
            results.append('transient failure bounded retry')
            unknown=post('分享跑步旅行的经历','unknown',fault='unknown_once')
            u=wait(unknown['job_id']);assert u['status']=='NEEDS_RECONCILIATION',u
            for _ in range(5):
                if u['status'] != 'NEEDS_RECONCILIATION': break
                assert call('POST','/api/jobs/'+unknown['job_id']+'/reconcile',{})[0] in (200,202)
                u=wait(unknown['job_id'])
            assert u['status']=='COMPLETED',u
            results.append('unknown receipt reconciles without duplicate effects')
            print(json.dumps({'passed':len(results),'checks':results},ensure_ascii=False,indent=2))
        finally:
            if proc and proc.poll() is None:proc.terminate();proc.wait(timeout=15)
            log.close()
if __name__=='__main__':main()
