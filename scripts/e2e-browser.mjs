// Run with: python3 scripts/run-browser-e2e.py --base-url http://127.0.0.1:8084
// Requires the ego-browser runtime (taskSpace), not plain node or Playwright.
const fs = await import('node:fs/promises');
const path = await import('node:path');
const root = globalThis.HJ_E2E_CONFIG.root;
const base = globalThis.HJ_E2E_CONFIG.base;
const reportPath = path.join(root, 'reports/e2e-browser.json');
await fs.mkdir(path.dirname(reportPath), {recursive:true});
const task = globalThis.HJ_E2E_CONFIG.space ? await taskSpace(globalThis.HJ_E2E_CONFIG.space) : await taskSpace('HuanJing 真实浏览器端到端验收');
const page = task.page('p1');
const report = {schema_version:'browser-e2e.v1',started_at:new Date().toISOString(),base_url:base,space_id:task.spaceId,
  build:globalThis.HJ_E2E_CONFIG.build,
  scope:'Real browser controls against a separate SQLite-backed LOCAL_MOCK server. Assertions inspect DOM and read-only API state. Model quality and production Temporal/Qdrant are outside this test.',
  cases:[],screenshots:[],visual_review:{status:'PENDING',note:'Screenshots require separate human/model visual inspection; automatic assertions do not establish visual quality.'}};
const save = async()=>fs.writeFile(reportPath,JSON.stringify(report,null,2)+'\n');
const assert = (condition,message)=>{if(!condition)throw new Error(message);};
async function test(id,name,steps,fn){
  const start=Date.now();let result;
  try{result={id,name,steps,status:'PASS',assertions:await fn()};}
  catch(e){result={id,name,steps,status:'FAIL',error:String(e.stack||e)};}
  result.duration_ms=Date.now()-start;report.cases.push(result);await save();console.log(`${result.status} ${id}: ${name}`);
}
async function installProbe(){
  await page.evaluate(()=>{
    if(window.__hjt)return;
    const original=window.fetch.bind(window);
    window.__hjt={original,creates:[],errors:[],dropNext:false};
    window.addEventListener('error',event=>window.__hjt.errors.push(String(event.message)));
    window.addEventListener('unhandledrejection',event=>window.__hjt.errors.push(String(event.reason)));
    window.fetch=async(input,init={})=>{
      const response=await original(input,init);
      if(new URL(typeof input==='string'?input:input.url,location.href).pathname==='/api/posts'&&(init.method||'GET').toUpperCase()==='POST'){
        let body;try{body=await response.clone().json();}catch(_){body=null;}
        window.__hjt.creates.push({key:new Headers(init.headers).get('Idempotency-Key'),input:JSON.parse(init.body),http:response.status,result:body});
        if(window.__hjt.dropNext){window.__hjt.dropNext=false;throw new TypeError('E2E injected response loss after server commit');}
      }
      return response;
    };
  });
}
async function read(pathname){return page.evaluate(async p=>{const r=await fetch(p,{headers:{'X-Tenant-ID':'demo'}});return {http:r.status,body:await r.json()};},pathname);}
async function publish(text,{fault='',file=null,caption='',candidates=1}={}){
  if(file)await page.setInputFiles('#files',[path.join(root,'fixtures',file)]);
  else if(await page.evaluate(()=>document.querySelector('#files').files.length>0))await page.click('#clear-files');
  assert(await page.evaluate(expected=>document.querySelector('#files').files.length===expected,file?1:0),'Attachment input did not reach expected count');
  await page.fill('#post-text',text);
  await page.fill('#caption',caption);
  await page.fill('#candidate-count',String(candidates));
  await page.selectOption('#fault',fault);
  const before=await page.evaluate(()=>window.__hjt.creates.length);
  await page.click('#publish');
  await page.waitForFunction(n=>window.__hjt.creates.length>n, before,{timeout:15000});
  const mutation=await page.evaluate(()=>window.__hjt.creates.at(-1));
  assert(mutation.http===202,`Create returned ${mutation.http}: ${JSON.stringify(mutation.result)}`);
  return mutation.result;
}
async function waitJob(jobId,statuses=['COMPLETED'],timeout=30000){
  await page.waitForFunction(async({jobId,statuses})=>{
    const r=await fetch('/api/jobs/'+jobId,{headers:{'X-Tenant-ID':'demo'}});const j=await r.json();
    if(statuses.includes(j.status)){window.__hjt.lastJob=j;return true;}return false;
  },{jobId,statuses},{timeout});
  const job=await page.evaluate(()=>window.__hjt.lastJob);
  await page.waitForFunction(id=>document.querySelector('#inspector')?.innerText.includes(id),jobId,{timeout:10000});
  return job;
}
async function inspect(created){
  const post=(await read('/api/posts/'+created.post_id)).body;
  const job=(await read('/api/jobs/'+created.job_id)).body;
  const identities=(post.interactions||[]).map(i=>`${i.avatar_id}/${i.kind}`);
  assert(new Set(identities).size===identities.length,'Duplicate business action');
  return {post_id:created.post_id,job_id:created.job_id,status:job.status,actions:(post.interactions||[]).map(i=>i.kind),candidate_count:(job.candidates||[]).length,unique_actions:true,job,post};
}
function concise(value){const {job,post,...rest}=value;return rest;}
await page.goto(base);
await page.waitForFunction(()=>document.querySelector('#connection')?.textContent.includes('已连接'));
await installProbe();
console.log({spaceId:task.spaceId,page:page.label});
console.log(await page.snapshot());

await test('B01','初始页面与 Mock 边界',['打开根页面','等待已连接','检查公开画像与声明'],async()=>{
  await page.waitForSelector('#avatars [data-avatar]');
  const result=await page.evaluate(()=>({title:document.title,notice:document.querySelector('.notice').innerText,avatars:document.querySelectorAll('#avatars [data-avatar]').length,privateCanaryVisible:document.body.innerText.includes('PRIVATE_CANARY_')}));
  assert(result.title.includes('HuanJing')&&result.notice.includes('Mock'),'Mock disclosure absent');assert(result.avatars===5,'Seed personas absent');assert(!result.privateCanaryVisible,'Private canary leaked');return result;
});
for(const [id,name,text,expected] of [
 ['B02','点赞并评论','E2E 徒步旅行摄影经验，希望交流装备。',['COMMENT','LIKE']],
 ['B03','仅点赞','E2E 徒步旅行摄影经验。只点赞。',['LIKE']],
 ['B04','仅评论','E2E 徒步旅行摄影经验。只评论。',['COMMENT']],
 ['B05','忽略','E2E 徒步旅行摄影经验。不需要互动。',[]]
]){
 await test(id,name,['填写帖子与候选上限1','点击发布','等待终态并检查动作集合'],async()=>{
   const created=await publish(text);await waitJob(created.job_id);const result=await inspect(created);
   assert(result.candidate_count===1,'Expected one matched avatar');assert(JSON.stringify(result.actions.sort())===JSON.stringify(expected),'Unexpected action set');
   await page.waitForFunction(expected=>{const text=document.querySelector('#inspector').innerText;return expected.length?expected.every(a=>text.includes(a==='LIKE'?'点赞 · 成功':'评论 · 成功')):text.includes('IGNORE · 忽略');},expected,{timeout:10000});return concise(result);
 });
}
for(const [id,name,file,kind] of [['B06','PNG 上传与解码','running.png','img'],['B07','WAV 上传与解码','training.wav','audio'],['B08','MP4 上传与解码','outdoor.mp4','video']]){
 await test(id,name,['通过文件输入选择真实fixture','填写明确Mock描述并发布','打开附件检查浏览器解码'],async()=>{
   const created=await publish(`E2E ${name} 跑步训练分享`,{file,caption:'Mock 跑步训练语义；实际素材为测试图或正弦音，无真实感知识别。'});await waitJob(created.job_id);const result=await inspect(created);
   assert(result.post.attachments.length===1,'Attachment missing');
   await page.click(`#feed .post[data-job="${created.job_id}"] details.attachment summary`);
   await page.evaluate(({id,kind})=>{const e=document.querySelector(`#feed .post[data-job="${id}"] ${kind}`);if(kind!=='img')e.load();},{id:created.job_id,kind});
   await page.waitForFunction(({id,kind})=>{const e=document.querySelector(`#feed .post[data-job="${id}"] ${kind}`);return kind==='img'?e?.naturalWidth>0:e?.readyState>=1;},{id:created.job_id,kind},{timeout:10000});
   const decoded=await page.evaluate(({id,kind})=>{const e=document.querySelector(`#feed .post[data-job="${id}"] ${kind}`);return {width:e.naturalWidth||e.videoWidth||null,duration:Number.isFinite(e.duration)?e.duration:null};},{id:created.job_id,kind});
   return {...concise(result),mime:result.post.attachments[0].mime,browser_decoded:decoded};
 });
}
await test('B09','瞬时依赖失败自动恢复',['选择retry_once','发布并等待完成','检查失败调用和成功替代调用'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 自动恢复',{fault:'retry_once'});await waitJob(created.job_id);const result=await inspect(created);
 const ops=result.job.operations.filter(o=>o.name==='understand');assert(ops.some(o=>o.status==='FAILED')&&ops.some(o=>o.status==='SUCCEEDED'),'Retry evidence absent');return {...concise(result),understand_operations:ops};
});
await test('B10','持续失败与受控重试',['选择always_fail','等待失败','点击受控重试','依赖仍故障时再次明确失败'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 持续依赖失败',{fault:'always_fail'});const first=await waitJob(created.job_id,['FAILED']);
 await page.waitForSelector('[data-command="retry"]');await page.click('[data-command="retry"]');
 await page.waitForFunction(async({id,epoch})=>{const j=await(await fetch('/api/jobs/'+id)).json();return j.epoch>epoch&&j.status==='FAILED';},{id:created.job_id,epoch:first.epoch},{timeout:30000});
 const result=await inspect(created);assert(result.actions.length===0,'Failed stage published actions');return {...concise(result),old_epoch:first.epoch,new_epoch:result.job.epoch,dependency_still_failed:true};
});
await test('B11','UNKNOWN 回执对账',['选择unknown_once','等待对账状态','点击查询模拟回执并对账','检查只产生一个评论'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 结果未知',{fault:'unknown_once'});await waitJob(created.job_id,['NEEDS_RECONCILIATION']);
 await page.waitForSelector('[data-command="reconcile"]');await page.click('[data-command="reconcile"]');await waitJob(created.job_id);
 const result=await inspect(created);assert(result.actions.filter(a=>a==='COMMENT').length===1,'Reconciliation duplicated or lost comment');return concise(result);
});
await test('B12','评论拒绝不丢失已计划点赞',['选择comment_reject','等待部分完成','核验点赞保留且评论无发布'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 评论审核',{fault:'comment_reject'});await waitJob(created.job_id,['PARTIAL_COMPLETED']);const result=await inspect(created);
 assert(JSON.stringify(result.actions)===JSON.stringify(['LIKE']),'Comment rejection must preserve only the planned like');return concise(result);
});
await test('B13','删除已完成帖子',['发布并等待完成','点击删除并确认','确认读取410且列表无可见互动'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 删除测试');await waitJob(created.job_id);
 const receipt=await page.click('[data-command="delete"]');await page.acceptDialog();
 await page.waitForFunction(async id=>(await fetch('/api/posts/'+id)).status===410,created.post_id,{timeout:10000});
 const posts=(await read('/api/posts')).body.posts;const post=posts.find(p=>p.id===created.post_id);
 assert(!post||(post.deleted&&(post.interactions||[]).length===0),'Deleted interactions remained visible');return {post_id:created.post_id,get_status:410,visible_interactions:0};
});
await test('B14','停用分身后不得召回发布',['停用山野旅行家','发布相关帖子','确认候选与互动排除travel','重新启用'],async()=>{
 await page.click('[data-avatar="travel"]');await page.waitForFunction(()=>document.querySelector('[data-avatar="travel"]')?.textContent==='启用');
 const created=await publish('E2E 徒步旅行摄影 停用验证',{candidates:4});await waitJob(created.job_id);const result=await inspect(created);
 assert(!result.job.candidates.some(c=>c.avatar_id==='travel'),'Inactive avatar recalled');assert(!result.post.interactions.some(i=>i.avatar_id==='travel'),'Inactive avatar published');
 await page.click('[data-avatar="travel"]');await page.waitForFunction(()=>document.querySelector('[data-avatar="travel"]')?.textContent==='停用');return {...concise(result),revoked_avatar:'travel',restored:true};
});
await test('B15','页面刷新恢复在途任务',['选择slow发布','刷新页面','从帖子列表重新选中同一任务','等待原任务完成'],async()=>{
 const created=await publish('E2E 徒步旅行摄影 页面刷新恢复',{fault:'slow'});await page.reload();await page.waitForFunction(()=>document.querySelector('#connection')?.textContent.includes('已连接'));await installProbe();
 await page.click(`#feed .post[data-job="${created.job_id}"]`);await waitJob(created.job_id,['COMPLETED'],30000);return concise(await inspect(created));
});
await test('B16','响应丢失后刷新与幂等确认',['在浏览器fetch层丢弃已提交POST的响应','核验sessionStorage保留键和body','刷新后点击重试确认','确认同一帖子和任务'],async()=>{
 await page.evaluate(()=>{window.__hjt.dropNext=true;});
 const created=await publish('E2E 徒步旅行摄影 提交响应丢失');
 await page.waitForFunction(()=>!document.querySelector('#retry-submit').hidden);
 const saved=await page.evaluate(()=>JSON.parse(sessionStorage.getItem('huanjing.pending')));assert(saved?.key&&saved?.body,'Pending submission was not persisted');
 await page.reload();await page.waitForFunction(()=>!document.querySelector('#retry-submit').hidden);await installProbe();await page.click('#retry-submit');
 await page.waitForFunction(()=>window.__hjt.creates.length===1);const replay=await page.evaluate(()=>window.__hjt.creates[0]);
 assert(replay.key===saved.key&&JSON.stringify(replay.input)===JSON.stringify(saved.body),'Retry changed key or body');assert(replay.result.post_id===created.post_id&&replay.result.job_id===created.job_id,'Retry created another post/job');
 await waitJob(created.job_id);assert(await page.evaluate(()=>sessionStorage.getItem('huanjing.pending')===null),'Pending identity not cleared');
 return {...concise(await inspect(created)),same_request_key:true,same_payload:true,same_ids:true,pending_cleared:true};
});
await test('B17','快速重复点击只提交一次',['填写纯文本帖子','对按钮连续触发两次原生click','核验仅一次创建请求'],async()=>{
 if(await page.evaluate(()=>document.querySelector('#files').files.length>0))await page.click('#clear-files');await page.fill('#post-text','E2E 徒步旅行摄影 双击发布');await page.selectOption('#fault','');
 const before=await page.evaluate(()=>window.__hjt.creates.length);
 await page.evaluate(()=>{const b=document.querySelector('#publish');b.click();b.click();});
 await page.waitForFunction(n=>window.__hjt.creates.length>n,before);const creates=await page.evaluate(n=>window.__hjt.creates.slice(n),before);
 assert(creates.length===1,'Double-click created more than one request');await waitJob(creates[0].result.job_id);return {...concise(await inspect(creates[0].result)),create_request_count:1};
});
await test('B18','帖子 HTML 安全转义',['提交含img/onerror的文本','等待渲染','核验作为文本展示且未执行'],async()=>{
 const text='E2E 徒步旅行摄影 <img id="e2e-injected" src="x" onerror="window.__e2eXSS=true">';const created=await publish(text);await waitJob(created.job_id);
 const result=await page.evaluate(()=>({injectedElement:!!document.querySelector('#e2e-injected'),executed:!!window.__e2eXSS,privateCanaryVisible:document.body.innerText.includes('PRIVATE_CANARY_')}));
 assert(!result.injectedElement&&!result.executed&&!result.privateCanaryVisible,'Unsafe content or private canary rendered');return {...concise(await inspect(created)),...result};
});
await test('B19','媒体缺少Mock证据时停止放行',['上传PNG但留空模拟描述','发布','等待审核拒绝且无互动'],async()=>{
 const created=await publish('E2E 跑步训练 未提供媒体描述',{file:'running.png',caption:''});await waitJob(created.job_id,['REJECTED']);const result=await inspect(created);assert(result.actions.length===0,'Unperceived media generated an interaction');return concise(result);
});
await test('B20','浏览器运行错误检查',['完成所有交互后检查error与unhandledrejection记录'],async()=>{
 const errors=await page.evaluate(()=>window.__hjt.errors);assert(errors.length===0,'Browser runtime errors: '+JSON.stringify(errors));return {runtime_errors:errors,note:'Probe reinstalled after intentional refreshes; earlier scenario assertions capture their own failures.'};
});
await test('B22','纯马拉松只召回运动分身',['候选上限设置4','发布纯跑步马拉松帖子','检查所有候选与互动仅fitness'],async()=>{
 const created=await publish('第一次参加半程马拉松跑步，最后五公里很吃力，想请教科学配速和恢复。',{candidates:4});await waitJob(created.job_id);const result=await inspect(created);
 const ids=result.job.candidates.map(c=>c.avatar_id);assert(JSON.stringify(ids)===JSON.stringify(['fitness']),'Irrelevant personas recalled: '+JSON.stringify(ids));assert(result.post.interactions.every(i=>i.avatar_id==='fitness'),'Irrelevant persona published');return {...concise(result),candidate_ids:ids};
});
// Leave a successful example selected for the desktop screenshot.
const created=await publish('E2E 最终演示：徒步旅行摄影，想交流装备和拍摄经验。');await waitJob(created.job_id);
await page.evaluate(()=>window.scrollTo(0,0));
const desktop=path.join(root,'reports/e2e-browser-desktop.png');await page.screenshot({path:desktop});report.screenshots.push('reports/e2e-browser-desktop.png');
await page.cdp('Emulation.setDeviceMetricsOverride',{width:390,height:844,deviceScaleFactor:1,mobile:true});
await page.evaluate(()=>window.scrollTo(0,0));
await test('B21','窄屏布局无横向溢出',['390×844移动视口','检查文档宽度','保存截图供人工视觉审查'],async()=>{
 const size=await page.evaluate(()=>({viewport:window.innerWidth,document:document.documentElement.scrollWidth}));assert(size.document<=size.viewport+1,'Horizontal overflow');return size;
});
await page.screenshot({path:path.join(root,'reports/e2e-browser-mobile.png')});report.screenshots.push('reports/e2e-browser-mobile.png');
await page.cdp('Emulation.clearDeviceMetricsOverride');
report.finished_at=new Date().toISOString();report.passed=report.cases.filter(c=>c.status==='PASS').length;report.failed=report.cases.filter(c=>c.status==='FAIL').length;
await save();
if(report.failed===0)await task.finish({keep:[]});
console.log(JSON.stringify({report:reportPath,passed:report.passed,failed:report.failed}));
