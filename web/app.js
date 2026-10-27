'use strict';
const $ = id => document.getElementById(id);
const esc = v => String(v ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const pretty = v => typeof v === 'string' ? v : JSON.stringify(v, null, 2);
const state = {selected:null, posts:[], jobs:[], previews:[], busy:false};
let pendingSubmission=null;try{pendingSubmission=JSON.parse(sessionStorage.getItem('huanjing.pending')||'null');}catch(_){/* Session storage can be unavailable; keep in-memory retry support. */}
const labels = {ACCEPTED:'已接收',PROCESSING:'处理中',COMPLETED:'已完成',PARTIAL_COMPLETED:'部分完成',REJECTED:'审核拒绝',FAILED:'失败',CANCELLED:'已取消',EXPIRED:'已过期',WAITING_DEPENDENCY:'等待依赖恢复',NEEDS_RECONCILIATION:'等待对账',SUCCEEDED:'成功',PENDING:'待处理',RUNNING:'执行中',UNKNOWN:'结果未知',OUTPUT_REJECTED:'评论未通过',DELETED:'已删除'};
const stageNames = {MODERATE:'内容审核',UNDERSTAND:'语义理解',MATCH:'候选召回',DECIDE:'个性化决策',PUBLISH:'逐动作发布',DONE:'完成'};
const badge = s => `<span class="badge ${['COMPLETED','SUCCEEDED'].includes(s)?'good':['FAILED','REJECTED','EXPIRED','OUTPUT_REJECTED'].includes(s)?'bad':['WAITING_DEPENDENCY','NEEDS_RECONCILIATION','PARTIAL_COMPLETED','UNKNOWN'].includes(s)?'warn':''}">${esc(labels[s]||s||'待处理')}</span>`;
async function api(path, options={}) {
  const response = await fetch('/api'+path,{...options,headers:{'X-Tenant-ID':'demo',...(options.body && !(options.body instanceof FormData)?{'Content-Type':'application/json'}:{}),...options.headers}});
  const data = await response.json(); if(!response.ok){const error=new Error(data.error || `HTTP ${response.status}`);error.status=response.status;throw error;} return data;
}
function errorMessage(error){ $('form-message').className='error'; $('form-message').textContent=error.message; }
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
  const openDetails=[...$('inspector').querySelectorAll('details')].map(d=>d.open);const scroll=$('inspector').parentElement.scrollTop;
  const names=['MODERATE','UNDERSTAND','MATCH','DECIDE','PUBLISH']; const stages=j.stages||[];
  $('inspector').innerHTML=`<div class="post-head">${badge(j.status)}<span class="meta">尝试 ${esc(j.attempts)} · epoch ${esc(j.epoch)}</span></div><div class="meta" style="margin-top:8px">${esc(j.id)}</div><div class="stepper">${names.map(n=>`<div title="${stageNames[n]}" class="step ${stages.some(s=>s.name===n&&s.status==='SUCCEEDED')?'done':j.stage===n?'current':''}"></div>`).join('')}</div>${names.map(n=>{const s=stages.find(s=>s.name===n);return `<div class="stage"><span>${stageNames[n]}</span>${badge(s?.status||(j.stage===n?'RUNNING':'PENDING'))}</div>`;}).join('')}${j.error?`<p class="error small">${esc(j.error)}</p>`:''}<div class="tools">${j.status==='NEEDS_RECONCILIATION'?'<button data-command="reconcile">查询模拟回执并对账</button>':''}${['FAILED','EXPIRED'].includes(j.status)?'<button data-command="retry">受控重试</button>':''}<button data-command="delete" class="danger" data-post="${esc(j.post_id)}">删除帖子</button></div><h3>候选与逐动作结果</h3>${(j.candidates||[]).length?(j.candidates||[]).map(c=>`<div class="candidate"><div class="candidate-head"><strong>${esc(c.name||c.avatar_id)}</strong>${badge(c.status)}</div><div class="evidence">Mock 匹配分数 ${esc(Number(c.score||0).toFixed(3))}<br>${esc(pretty(c.evidence))}</div><div>${renderActions(c.actions,c.action_states)}</div>${c.reason?`<div class="small">决策原因：${esc(c.reason)}</div>`:''}${c.comment?`<div class="comment">${esc(c.comment)}</div>`:''}</div>`).join(''):'<div class="empty">尚未产生候选，等待审核与召回。</div>'}<details><summary>阶段证据 · Mock 输出</summary>${stages.map(s=>`<h3>${esc(stageNames[s.name]||s.name)}</h3><pre>${esc(pretty(s.output))}</pre>`).join('')}</details><details><summary>外部调用账本（${(j.operations||[]).length}）</summary><pre>${esc(pretty(j.operations||[]))}</pre></details><details><summary>事件与恢复记录</summary><pre>${esc(pretty(j.events||[]))}</pre></details><div class="meta" style="margin-top:14px">截止：${esc(date(j.deadline))}<br>更新：${esc(date(j.updated_at))}</div>`;
  $('inspector').querySelectorAll('details').forEach((d,i)=>{d.open=openDetails[i]||false;});$('inspector').parentElement.scrollTop=scroll;
}
function renderActions(actions, actionStates={}){
  if(!actions || (Array.isArray(actions)&&!actions.length))return '<span class="action">IGNORE · 忽略</span>';
  if(!Array.isArray(actions))return `<span class="action">${esc(pretty(actions))}</span>`;
  return actions.map(a=>typeof a==='string'?`<span class="action">${a==='LIKE'?'♡ 点赞':a==='COMMENT'?'✎ 评论':esc(a)} · ${esc(labels[actionStates?.[a]]||actionStates?.[a]||'待处理')}</span>`:`<span class="action">${esc(a.kind||a.action_type||a.type||a.action)} · ${esc(labels[a.status]||a.status)}</span>`).join('');
}
async function refresh(){
  if(state.busy)return; state.busy=true;
  try{
    const [posts,jobs,stats]=await Promise.all([api('/posts'),api('/jobs'),api('/stats')]);state.posts=posts.posts||[];state.jobs=jobs.jobs||[];
    $('connection').textContent='● 已连接';
    $('stats').innerHTML=`<div class="stat"><strong>${esc(stats.posts)}</strong><span>已接收帖子</span></div><div class="stat"><strong>${esc(stats.interactions)}</strong><span>唯一互动动作</span></div><div class="stat"><strong>${esc(stats.queue_depth)}</strong><span>等待 / 执行中</span></div><div class="meta" style="grid-column:1/-1">Mock 预算：已用 ${esc(stats.budget?.spent)} / 预留 ${esc(stats.budget?.reserved)} / 上限 ${esc(stats.budget?.limit)}</div>`;
    renderPosts(); if(state.selected){const job=await api('/jobs/'+encodeURIComponent(state.selected));renderJob(job);}
  }catch(e){$('connection').textContent='○ 连接异常';$('connection').title=e.message;}finally{state.busy=false;}
}
async function loadAvatars(){try{const data=await api('/avatars');$('avatars').innerHTML=(data.avatars||[]).map(a=>`<div class="avatar"><div class="avatar-icon">${esc(a.name?.slice(0,1)||'AI')}</div><div class="avatar-info"><strong>${esc(a.name)}</strong><p>${esc(a.persona)}</p><p>${esc(Array.isArray(a.interests)?a.interests.join(' · '):a.interests)}</p></div><button data-avatar="${esc(a.id)}" data-active="${a.active?'false':'true'}">${a.active?'停用':'启用'}</button></div>`).join('');}catch(e){$('avatars').textContent=e.message;}}
$('compose').addEventListener('submit',async event=>{event.preventDefault();if(pendingSubmission)return;$('publish').disabled=true;$('form-message').className='muted';$('form-message').textContent='正在上传并持久化任务…';try{
 const attachment_ids=[];for(const file of $('files').files){const form=new FormData();form.append('file',file);form.append('caption',$('caption').value);const a=await api('/uploads',{method:'POST',body:form});attachment_ids.push(a.id);}
 savePending({key:crypto.randomUUID(),body:{text:$('post-text').value,attachment_ids,max_candidates:Number($('candidate-count').value),fault:$('fault').value,deadline_seconds:120}});await submitPending();
 }catch(e){if(e.status>=400&&e.status<500)savePending(null);errorMessage(e);}finally{$('publish').disabled=Boolean(pendingSubmission);}});
$('retry-submit').addEventListener('click',async()=>{$('retry-submit').disabled=true;try{await submitPending();}catch(e){if(e.status>=400&&e.status<500)savePending(null);errorMessage(e);}finally{$('retry-submit').disabled=false;}});
$('feed').addEventListener('click',async event=>{if(event.target.closest('details, audio, video'))return;const p=event.target.closest('[data-job]');if(p){state.selected=p.dataset.job;renderPosts();try{renderJob(await api('/jobs/'+encodeURIComponent(state.selected)));}catch(e){errorMessage(e);}}});
$('inspector').addEventListener('click',async event=>{const b=event.target.closest('[data-command]');if(!b)return;b.disabled=true;try{if(b.dataset.command==='delete'){if(!confirm('删除该帖子并停止其未完成互动？'))return;await api('/posts/'+encodeURIComponent(b.dataset.post),{method:'DELETE'});}else await api('/jobs/'+encodeURIComponent(state.selected)+'/'+b.dataset.command,{method:'POST',body:'{}'});await refresh();}catch(e){errorMessage(e);}finally{b.disabled=false;}});
$('avatars').addEventListener('click',async event=>{const b=event.target.closest('[data-avatar]');if(!b)return;try{await api('/avatars/'+encodeURIComponent(b.dataset.avatar),{method:'PATCH',body:JSON.stringify({active:b.dataset.active==='true'})});await loadAvatars();}catch(e){errorMessage(e);}});
$('files').addEventListener('change',()=>{state.previews.forEach(URL.revokeObjectURL);state.previews=[];$('file-preview').replaceChildren();for(const file of $('files').files){const note=document.createElement('div');note.className='small';note.textContent=`${file.name} · ${(file.size/1024).toFixed(1)} KB`;$('file-preview').append(note);const kind=file.type.split('/')[0];if(['image','audio','video'].includes(kind)){const media=document.createElement(kind==='image'?'img':kind);const url=URL.createObjectURL(file);state.previews.push(url);media.src=url;media.className='preview';if(kind!=='image')media.controls=true;else media.alt=file.name;$('file-preview').append(media);}}});
const examples={commentOnly:'周末徒步旅行，想交流摄影经验。只评论。',likeOnly:'周末徒步旅行，分享摄影作品。只点赞。',ignore:'周末徒步旅行摄影随记，不需要互动。',fitness:'第一次参加半程马拉松跑步，最后五公里很吃力，想请教如何科学安排配速和恢复。',travel:'计划周末去登山露营，想找适合新手的路线，也想拍摄山顶日出。',cooking:'周末第一次做酸面包，面团发酵不太理想，有什么烘焙技巧？',technology:'如何设计一个高并发、支持幂等和重启恢复的分布式系统？',art:'最近在练习街头摄影，想通过光影讲述城市里的小故事。'};
document.querySelectorAll('[data-example]').forEach(b=>b.addEventListener('click',()=>{$('post-text').value=examples[b.dataset.example];}));
$('refresh').addEventListener('click',refresh);if(pendingSubmission){savePending(pendingSubmission);$('form-message').textContent='上次提交结果尚未确认。请用相同幂等键重试确认，再发布新帖。';}loadAvatars();refresh();setInterval(refresh,1500);
