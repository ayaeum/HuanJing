'use strict';
const $ = id => document.getElementById(id);
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pretty = v => typeof v === 'string' ? v : JSON.stringify(v, null, 2);
const state = {selected:null, posts:[], jobs:[], previews:[], busy:false, health:null};
let pendingSubmission=null;try{pendingSubmission=JSON.parse(sessionStorage.getItem('huanjing.pending')||'null');}catch(_){/* Session storage can be unavailable; keep in-memory retry support. */}
const labels = {ACCEPTED:'已接收',PROCESSING:'处理中',COMPLETED:'已完成',PARTIAL_COMPLETED:'部分完成',REJECTED:'审核拒绝',FAILED:'失败',CANCELLED:'已取消',SUPERSEDED:'已被新版本替代',EXPIRED:'已过期',WAITING_DEPENDENCY:'等待依赖恢复',NEEDS_RECONCILIATION:'等待处理未知结果',SUCCEEDED:'成功',PENDING:'待处理',RUNNING:'执行中',UNKNOWN:'结果未知',ABANDONED:'已放弃并计入额度',OUTPUT_REJECTED:'评论未通过',DELETED:'已删除'};
const stageNames = {MODERATE:'内容审核',UNDERSTAND:'语义理解',MATCH:'候选召回',DECIDE:'个性化决策',PUBLISH:'逐动作发布',DONE:'完成'};
const badge = s => `<span class="badge ${['COMPLETED','SUCCEEDED'].includes(s)?'good':['FAILED','REJECTED','EXPIRED','OUTPUT_REJECTED'].includes(s)?'bad':['WAITING_DEPENDENCY','NEEDS_RECONCILIATION','PARTIAL_COMPLETED','UNKNOWN'].includes(s)?'warn':''}">${esc(labels[s]||s||'待处理')}</span>`;
async function api(path, options={}) {
  const response = await fetch('/api'+path,{...options,headers:{'X-Tenant-ID':'demo',...(options.body && !(options.body instanceof FormData)?{'Content-Type':'application/json'}:{}),...options.headers}});
  const data = await response.json(); if(!response.ok){const error=new Error(data.error || `HTTP ${response.status}`);error.status=response.status;throw error;} return data;
}
const operationalText = value => String(value??'').replace(/https?:\/\/[^\s"'<>]+/gi,'[服务地址已隐藏]');
function errorMessage(error){ $('form-message').className='error'; $('form-message').textContent=operationalText(error.message); }
function renderHealth(health){
  state.health=health;
  const simulationNames={moderation:'内容审核','raw media perception':'原始媒体感知','action decisions':'动作决策','comment generation':'评论生成'};
  const remaining=String(health.remaining_simulations||'moderation, raw media perception, action decisions, comment generation').split(',').map(x=>simulationNames[x.trim()]||'其他测试环节').join('、');
  const real=health.semantics==='real_model'||health.mode==='REAL_SEMANTICS';
  const fixture=health.semantics==='test_fixture'||['TEST_FIXTURE','LOCAL_MOCK'].includes(health.mode);
  const fault=$('fault');
  for(const option of fault.options){
    if(['retry_once','always_fail'].includes(option.value)){
      option.disabled=!fixture;
      option.textContent=(option.value==='retry_once'?'模拟理解失败一次':'模拟理解持续失败')+(fixture?'':' · 仅离线测试');
    }
  }
  if(fault.selectedOptions[0]?.disabled)fault.value='';
  $('mode-label').textContent=real?'REAL SEMANTICS · 真实文本理解与匹配':fixture?'TEST FIXTURE · 确定性测试模式':'运行模式未确认';
  const text=real?`文本理解与公开画像语义匹配调用真实模型；${remaining}仍为模拟。匹配使用模型相关性评分，不是已实现的向量数据库检索。`:fixture?'文本理解与语义匹配当前为确定性测试实现（Mock），不调用真实语义模型；审核、原始媒体感知、动作决策和评论生成也为模拟。':'暂未确认服务能力，不据此推断模型是否真实调用。';
  $('mode-notice').innerHTML=`<strong>能力边界</strong> ${esc(text)} 附件描述由手动填写，不能视为真实 OCR / ASR / 视频分析。历史帖子按自身阶段记录标注来源。调用额度不是实际金额。`;
}
function outputOf(stage){if(!stage)return null;if(typeof stage.output==='string'){try{return JSON.parse(stage.output);}catch(_){return null;}}return stage.output||null;}
function semanticSource(stages,name){
  const output=outputOf(stages.find(s=>s.name===name));if(!output)return {kind:'pending',label:'尚无阶段结果'};
  if(output.engine==='real_model')return {kind:'real',label:'真实模型'};
  if(['test_fixture','deterministic_mock','mock'].includes(output.engine)||/mock|hash/i.test(`${output.model||''} ${output.embedding||''} ${output.strategy||''}`))return {kind:'fixture',label:'Mock / 测试实现'};
  if(name==='MATCH'&&semanticSource(stages,'UNDERSTAND').kind==='fixture')return {kind:'fixture',label:'历史 Mock / 测试实现'};
  return {kind:'unknown',label:'来源未标注'};
}
function stageSourceLabel(stages,name){
  if(['UNDERSTAND','MATCH'].includes(name))return semanticSource(stages,name).label;
  return {MODERATE:'模拟审核',DECIDE:'模拟决策',PUBLISH:'数据库事务提交'}[name]||'阶段记录';
}
function aiLedger(operations){
  if(!operations.length)return '<div class="small">该帖暂无真实模型调用账本。以阶段记录判断历史来源。</div>';
  const reported=operations.filter(o=>o.usage_reported===true||o.usage_reported===1);
  const input=reported.reduce((n,o)=>n+Number(o.input_tokens||0),0);const output=reported.reduce((n,o)=>n+Number(o.output_tokens||0),0);
  return `<div class="small">已报告 Token：输入 ${esc(input)} / 输出 ${esc(output)}；${operations.length-reported.length} 次调用未报告用量。Token 不等于实际费用。</div>`+operations.map(o=>{
    const hasUsage=o.usage_reported===true||o.usage_reported===1;
    return `<div class="candidate"><div class="candidate-head"><strong>${esc(o.name)}</strong>${badge(o.status)}</div><div class="meta">模型：${esc(operationalText(o.model))} · 尝试 ${esc(o.attempt)}</div><div class="small">${hasUsage?`输入 ${esc(o.input_tokens)} / 输出 ${esc(o.output_tokens)} Token`:'供应商未返回用量；不能按 0 Token 计费'}</div><div class="meta">回执：${esc(o.response_id||'未收到')}<br>操作：${esc(o.operation_key)}</div>${o.error?`<p class="error small">${esc(operationalText(o.error))}</p>`:''}</div>`;
  }).join('');
}
function savePending(value){pendingSubmission=value;try{if(value)sessionStorage.setItem('huanjing.pending',JSON.stringify(value));else sessionStorage.removeItem('huanjing.pending');}catch(_){}$('retry-submit').hidden=!value;$('publish').disabled=Boolean(value);}
async function submitPending(){
  const created=await api('/posts',{method:'POST',headers:{'Idempotency-Key':pendingSubmission.key},body:JSON.stringify(pendingSubmission.body)});
  savePending(null);state.selected=created.job_id;$('form-message').className='muted';$('form-message').textContent='已持久化。任务会继续执行，刷新页面也能恢复查看。';await refresh();
}
function date(ms){return ms ? new Date(ms).toLocaleString('zh-CN',{hour12:false}) : '';}
function renderAttachment(a){
  const kind=(a.mime||'').split('/')[0];const url='/api/uploads/'+encodeURIComponent(a.id);
  const preview=kind==='image'?`<img class="preview" src="${url}" alt="${esc(a.name)}" loading="lazy">`:['audio','video'].includes(kind)?`<${kind} class="preview" src="${url}" controls preload="none"></${kind}>`:'';
  return `<details class="attachment"><summary>▧ ${esc(a.name||a.id)} · ${esc(a.mime||'媒体')}</summary>${preview}<div class="small">Mock 描述：${esc(a.caption||'未填写')}</div></details>`;
}
function renderPosts(){
  const signature=JSON.stringify([state.posts,state.jobs,state.selected]);if(signature===state.feedSignature)return;state.feedSignature=signature;
  $('feed').innerHTML=state.posts.length?state.posts.map(p=>{
    const j=state.jobs.find(j=>j.id===p.job_id); const interactions=p.interactions||[];
    return `<article class="post ${state.selected===p.job_id?'selected':''}" data-job="${esc(p.job_id)}"><div class="post-head"><span class="meta">你 · ${esc(date(p.created_at))}</span>${badge(p.deleted?'DELETED':j?.status||p.status)}</div><div class="post-text">${esc(p.text)}</div><div>${(p.attachments||[]).map(renderAttachment).join('')}</div><div class="meta">${esc(p.id)} · 版本 ${esc(p.version)}</div>${interactions.map(i=>`<div class="interaction"><strong>${esc(i.avatar_name||i.name||i.avatar_id)}</strong> ${i.kind==='LIKE'||i.action_type==='LIKE'||i.type==='LIKE'||i.action==='LIKE'?'♡ 点赞':'✎ 评论'}${i.content||i.comment?`<p>${esc(i.content||i.comment)}</p>`:''}</div>`).join('')}<div class="meta" style="margin-top:12px">${interactions.length?`${interactions.length} 个动作已持久化`:'点击查看召回、决策与执行进度'} →</div></article>`;
  }).join(''):'<div class="panel empty">还没有帖子。发布第一条想法，观察分身如何互动。</div>';
}
function renderJob(j){
  const signature=JSON.stringify(j);if(signature===state.jobSignature)return;state.jobSignature=signature;
  const openDetails=new Map([...$('inspector').querySelectorAll('details[data-section]')].map(d=>[d.dataset.section,d.open]));const scroll=$('inspector').parentElement.scrollTop;
  const names=['MODERATE','UNDERSTAND','MATCH','DECIDE','PUBLISH'];const stages=j.stages||[];const aiOperations=j.ai_operations||[];
  const understanding=semanticSource(stages,'UNDERSTAND');const matching=semanticSource(stages,'MATCH');
  const matchOutput=outputOf(stages.find(s=>s.name==='MATCH'));
  const scoreLabel=matching.kind==='real'?'真实模型相关性评分':matching.kind==='fixture'?'Mock / 测试匹配分数':'来源未确认的匹配分数';
  const unresolvedAI=aiOperations.filter(o=>o.status==='UNKNOWN'||(o.status==='RUNNING'&&['CANCELLED','SUPERSEDED','EXPIRED','FAILED'].includes(j.status)));
  const mockUnknown=(j.operations||[]).some(o=>o.status==='UNKNOWN');
  const tools=[];
  if(mockUnknown)tools.push(`<button data-command="reconcile" data-job="${esc(j.id)}">查询模拟回执并对账</button>`);
  for(const operation of unresolvedAI)tools.push(`<button data-command="resolve-ai" data-job="${esc(j.id)}" data-operation="${esc(operation.operation_key)}" class="danger">放弃未知请求并计入额度 · ${esc(operation.name)}</button>`);
  if(['FAILED','EXPIRED'].includes(j.status)&&!unresolvedAI.length)tools.push(`<button data-command="retry" data-job="${esc(j.id)}">${aiOperations.length?'重试任务（可能新增模型费用）':'受控重试'}</button>`);
  tools.push(`<button data-command="delete" class="danger" data-post="${esc(j.post_id)}">删除帖子</button>`);
  $('inspector').innerHTML=`
    <div class="post-head">${badge(j.status)}<span class="meta">调度 ${esc(j.attempts)} 次 · epoch ${esc(j.epoch)}</span></div>
    <div class="meta" style="margin-top:8px">${esc(j.id)}</div>
    <div class="small" style="margin-top:8px">本帖文本理解：${esc(understanding.label)}<br>本帖语义匹配：${esc(matching.label)}</div>
    <div class="stepper">${names.map(n=>`<div title="${stageNames[n]}" class="step ${stages.some(s=>s.name===n&&s.status==='SUCCEEDED')?'done':j.stage===n?'current':''}"></div>`).join('')}</div>
    ${names.map(n=>{const stage=stages.find(s=>s.name===n);return `<div class="stage"><span>${stageNames[n]}<br><span class="small">${esc(stageSourceLabel(stages,n))}</span></span>${badge(stage?.status||(j.stage===n?'RUNNING':'PENDING'))}</div>`;}).join('')}
    ${j.error?`<p class="error small">${esc(operationalText(j.error))}</p>`:''}
    ${unresolvedAI.length?'<p class="small">真实模型请求的远端结果和费用尚未确认。下面的“放弃”操作只处置本地账本，不查询远端回执，也不退款。</p>':''}
    <div class="tools">${tools.join('')}</div>
    <h3>候选与逐动作结果</h3>
    <div class="small">动作决策与评论生成仍为模拟。${matching.kind==='real'?'模型评分用于排序，不代表互动概率。':''}${matching.kind==='real'&&matchOutput?.threshold!==undefined?` 本帖阈值：${esc(matchOutput.threshold)}。`:''}</div>
    ${(j.candidates||[]).length?(j.candidates||[]).map(c=>`<div class="candidate"><div class="candidate-head"><strong>${esc(c.name||c.avatar_id)}</strong>${badge(c.status)}</div><div class="evidence">${esc(scoreLabel)} ${esc(Number(c.score||0).toFixed(3))}<br>${esc(pretty(c.evidence))}</div><div>${renderActions(c.actions,c.action_states)}</div>${c.reason?`<div class="small">模拟决策原因：${esc(c.reason)}</div>`:''}${c.comment?`<div class="comment"><span class="small">模拟生成评论</span><br>${esc(c.comment)}</div>`:''}</div>`).join(''):'<div class="empty">尚无候选结果；请结合上方阶段状态判断等待、失败或正常零候选。</div>'}
    <details data-section="stages"><summary>阶段证据 · 按阶段标注来源</summary>${stages.map(stage=>`<h3>${esc(stageNames[stage.name]||stage.name)} · ${esc(stageSourceLabel(stages,stage.name))}</h3><pre>${esc(pretty(stage.output))}</pre>`).join('')}</details>
    <details data-section="ai-ledger"><summary>真实模型调用 / Token 账本（${aiOperations.length}）</summary>${aiLedger(aiOperations)}</details>
    <details data-section="mock-ledger"><summary>模拟调用账本（${(j.operations||[]).length}）</summary><div class="small">模拟请求额度，不代表供应商 Token 或实际金额。</div><pre>${esc(operationalText(pretty(j.operations||[])))}</pre></details>
    <details data-section="events"><summary>事件与恢复记录</summary><pre>${esc(pretty(j.events||[]))}</pre></details>
    <div class="meta" style="margin-top:14px">截止：${esc(date(j.deadline))}<br>更新：${esc(date(j.updated_at))}</div>`;
  $('inspector').querySelectorAll('details[data-section]').forEach(d=>{d.open=openDetails.get(d.dataset.section)||false;});$('inspector').parentElement.scrollTop=scroll;
}
function renderActions(actions, actionStates={}){
  if(!actions || (Array.isArray(actions)&&!actions.length))return '<span class="action">IGNORE · 忽略</span>';
  if(!Array.isArray(actions))return `<span class="action">${esc(pretty(actions))}</span>`;
  return actions.map(a=>typeof a==='string'?`<span class="action">${a==='LIKE'?'♡ 点赞':a==='COMMENT'?'✎ 评论':esc(a)} · ${esc(labels[actionStates?.[a]]||actionStates?.[a]||'待处理')}</span>`:`<span class="action">${esc(a.kind||a.action_type||a.type||a.action)} · ${esc(labels[a.status]||a.status)}</span>`).join('');
}
async function refresh(){
  if(state.busy)return; state.busy=true;
  try{
    const [posts,jobs,stats,health]=await Promise.all([api('/posts'),api('/jobs'),api('/stats'),api('/health')]);state.posts=posts.posts||[];state.jobs=jobs.jobs||[];renderHealth(health);
    $('connection').textContent='● 已连接';
    $('stats').innerHTML=`<div class="stat"><strong>${esc(stats.posts)}</strong><span>已接收帖子</span></div><div class="stat"><strong>${esc(stats.interactions)}</strong><span>唯一互动动作</span></div><div class="stat"><strong>${esc(stats.queue_depth)}</strong><span>等待 / 执行中</span></div><div class="meta" style="grid-column:1/-1">调用额度：已用 ${esc(stats.budget?.spent)} / 预留 ${esc(stats.budget?.reserved)} / 上限 ${esc(stats.budget?.limit)}（非实际金额）</div>`;
    renderPosts(); if(state.selected){const job=await api('/jobs/'+encodeURIComponent(state.selected));renderJob(job);}
  }catch(e){$('connection').textContent='○ 连接异常';$('connection').title=operationalText(e.message);}finally{state.busy=false;}
}
async function loadAvatars(){try{const data=await api('/avatars');$('avatars').innerHTML=(data.avatars||[]).map(a=>`<div class="avatar"><div class="avatar-icon">${esc(a.name?.slice(0,1)||'AI')}</div><div class="avatar-info"><strong>${esc(a.name)}</strong><p>${esc(a.persona)}</p><p>${esc(Array.isArray(a.interests)?a.interests.join(' · '):a.interests)}</p></div><button data-avatar="${esc(a.id)}" data-active="${a.active?'false':'true'}">${a.active?'停用':'启用'}</button></div>`).join('');}catch(e){$('avatars').textContent=e.message;}}
$('compose').addEventListener('submit',async event=>{event.preventDefault();if(pendingSubmission)return;$('publish').disabled=true;$('form-message').className='muted';$('form-message').textContent='正在上传并持久化任务…';try{
 const attachment_ids=[];for(const file of $('files').files){const form=new FormData();form.append('file',file);form.append('caption',$('caption').value);const a=await api('/uploads',{method:'POST',body:form});attachment_ids.push(a.id);}
 savePending({key:crypto.randomUUID(),body:{text:$('post-text').value,attachment_ids,max_candidates:Number($('candidate-count').value),fault:$('fault').value,deadline_seconds:120}});await submitPending();
 }catch(e){if(e.status>=400&&e.status<500)savePending(null);errorMessage(e);}finally{$('publish').disabled=Boolean(pendingSubmission);}});
$('retry-submit').addEventListener('click',async()=>{$('retry-submit').disabled=true;try{await submitPending();}catch(e){if(e.status>=400&&e.status<500)savePending(null);errorMessage(e);}finally{$('retry-submit').disabled=false;}});
$('feed').addEventListener('click',async event=>{if(event.target.closest('details, audio, video'))return;const p=event.target.closest('[data-job]');if(p){state.selected=p.dataset.job;renderPosts();try{renderJob(await api('/jobs/'+encodeURIComponent(state.selected)));}catch(e){errorMessage(e);}}});
$('inspector').addEventListener('click',async event=>{
  const button=event.target.closest('[data-command]');if(!button)return;button.disabled=true;
  try{
    if(button.dataset.command==='delete'){
      if(!confirm('删除该帖子并停止其未完成互动？'))return;
      await api('/posts/'+encodeURIComponent(button.dataset.post),{method:'DELETE'});
    }else if(button.dataset.command==='resolve-ai'){
      const message='放弃该未知模型请求，并在本地消耗 1 次调用额度。\n\n这不是远端退款或回执对账：供应商可能已经执行并收费，真实结果与费用仍未知。任务会标记失败；之后只有你显式重试，才会发起可能再次收费的新请求。\n\n确认放弃并计入额度？';
      if(!confirm(message))return;
      await api('/jobs/'+encodeURIComponent(button.dataset.job)+'/resolve-ai',{method:'POST',body:JSON.stringify({operation_key:button.dataset.operation,resolution:'abandon_and_charge'})});
      $('form-message').className='muted';$('form-message').textContent='已放弃该未知请求并计入调用额度。此操作不确认远端费用，也未自动重试。';
    }else{
      await api('/jobs/'+encodeURIComponent(button.dataset.job||state.selected)+'/'+button.dataset.command,{method:'POST',body:'{}'});
    }
    await refresh();
  }catch(error){errorMessage(error);}finally{button.disabled=false;}
});
$('avatars').addEventListener('click',async event=>{const b=event.target.closest('[data-avatar]');if(!b)return;try{await api('/avatars/'+encodeURIComponent(b.dataset.avatar),{method:'PATCH',body:JSON.stringify({active:b.dataset.active==='true'})});await loadAvatars();}catch(e){errorMessage(e);}});
$('files').addEventListener('change',()=>{$('clear-files').hidden=$('files').files.length===0;state.previews.forEach(URL.revokeObjectURL);state.previews=[];$('file-preview').replaceChildren();for(const file of $('files').files){const note=document.createElement('div');note.className='small';note.textContent=`${file.name} · ${(file.size/1024).toFixed(1)} KB`;$('file-preview').append(note);const kind=file.type.split('/')[0];if(['image','audio','video'].includes(kind)){const media=document.createElement(kind==='image'?'img':kind);const url=URL.createObjectURL(file);state.previews.push(url);media.src=url;media.className='preview';if(kind!=='image')media.controls=true;else media.alt=file.name;$('file-preview').append(media);}}});
$('clear-files').addEventListener('click',()=>{$('files').value='';$('files').dispatchEvent(new Event('change',{bubbles:true}));});
const examples={commentOnly:'周末徒步旅行，想交流摄影经验。只评论。',likeOnly:'周末徒步旅行，分享摄影作品。只点赞。',ignore:'周末徒步旅行摄影随记，不需要互动。',fitness:'第一次参加半程马拉松跑步，最后五公里很吃力，想请教如何科学安排配速和恢复。',travel:'计划周末去登山露营，想找适合新手的路线，也想拍摄山顶日出。',cooking:'周末第一次做酸面包，面团发酵不太理想，有什么烘焙技巧？',technology:'如何设计一个高并发、支持幂等和重启恢复的分布式系统？',art:'最近在练习街头摄影，想通过光影讲述城市里的小故事。'};
document.querySelectorAll('[data-example]').forEach(b=>b.addEventListener('click',()=>{$('post-text').value=examples[b.dataset.example];}));
$('refresh').addEventListener('click',refresh);if(pendingSubmission){savePending(pendingSubmission);$('form-message').textContent='上次提交结果尚未确认。请用相同幂等键重试确认，再发布新帖。';}loadAvatars();refresh();setInterval(refresh,1500);
