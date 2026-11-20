/* 3.8 control flows: keep the original view and native IPC transport. */
let settingsDirty=false, routingDirty=false, routingDraft=null, lastStateSuccess=0, historyOffset=0, historyRequest=0, lastHistoryRefresh=0;
let settingsRevision=0,routingRevision=0,settingsSaving=false,routingSaving=false,lastRoutingSignature='';
const taskLabels={queued:'排队中',running:'执行中',retry_wait:'等待重试',succeeded:'成功',skipped:'已跳过',failed:'失败',cancelled:'已取消',interrupted:'已中断'};
const keyOf=a=>`${a.self_id}:${a.protocol_id}`;
const dateText=t=>t?new Date(t).toLocaleString('zh-CN',{hour12:false}):'尚无';
const api=async(path,body)=>{const options={signal:AbortSignal.timeout(20000)};if(body!==undefined)Object.assign(options,{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});const res=await fetch(path,options);const j=await res.json();if(!res.ok||j.error)throw Error(j.error||`HTTP ${res.status}`);return j};
const download=(data,name)=>{const u=URL.createObjectURL(new Blob([JSON.stringify(data,null,2)],{type:'application/json;charset=utf-8'}));const a=document.createElement('a');a.href=u;a.download=name;a.click();setTimeout(()=>URL.revokeObjectURL(u),3000)};
function addUI(){
 $('tabs').insertAdjacentHTML('afterend','<div id="freshness" class="freshness" role="status" aria-live="polite"><span id="freshnessText">正在读取数据…</span><button class="mini-btn" id="freshRetry" type="button">立即刷新</button></div>');
 const controls=document.querySelector('.account-controls');controls.insertAdjacentHTML('beforeend','<label class="details-switch"><input id="advancedAccounts" type="checkbox">高级列</label><label class="sr-only" for="groupFilter">筛选分组</label><select id="groupFilter" class="sort-select"><option value="">全部分组</option></select>');
 // Move existing controls so IDs, saved preferences and handlers stay intact.
 const filters=document.createElement('div');filters.id='accountFilters';filters.className='account-filters';filters.setAttribute('role','group');filters.setAttribute('aria-label','更多账号筛选');filters.dataset.open='false';
 const sortLabel=controls.querySelector('label[for="accSort"]');sortLabel.className='filter-label';sortLabel.textContent='排序';
 const groupLabel=controls.querySelector('label[for="groupFilter"]');groupLabel.className='filter-label';groupLabel.textContent='分组';
 filters.append(sortLabel,$('accSort'),groupLabel,$('groupFilter'),controls.querySelector('.details-switch'));
 controls.insertAdjacentHTML('beforeend','<button type="button" class="mini-btn filter-toggle" id="accountFilterToggle" aria-expanded="false" aria-controls="accountFilters"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"><path d="M4 7h16M7 12h10M10 17h4"/></svg><span id="accountFilterLabel">筛选</span></button>');
 controls.append(filters);
 document.querySelector('.account-toolbar').insertAdjacentHTML('afterend','<div class="target-toolbar"><label for="pushTarget">推送目标</label><select id="pushTarget"><option value="route">按账号 / 分组路由</option></select><span id="targetHint" class="faint"></span><button id="btnQueueAll" class="btn primary" type="button">推送在线账号</button></div>');
 $('panel-accounts').insertAdjacentHTML('beforeend',`<details class="card routing-card" id="routingDetails"><summary>账号分组与自动推送 <span class="faint">设置分组、路由和单账号暂停</span></summary><div class="routing-inner"><p class="faint">目标优先级：本次手动指定 → 账号单独目标 → 分组目标 → 默认面板。暂停仅影响自动任务，手动推送仍可执行。</p><div class="route-new"><label for="newGroupName">新分组</label><input id="newGroupName" maxlength="40" placeholder="例如：日常账号"><button id="addGroup" class="mini-btn" type="button">添加分组</button></div><div id="groupRoutes"></div><div id="accountPolicies" class="policy-scroll"></div><div class="route-save"><span id="routingStatus" role="status"></span><button id="saveRouting" class="btn primary" type="button" disabled>保存分组与路由</button></div></div></details>`);
 const activity=$('panel-activity');activity.querySelector('.card').id='legacyHistory';activity.insertAdjacentHTML('afterbegin',`<div class="card" id="taskCenter"><div class="card-head"><h2>任务中心 <span class="badge" id="taskCount">0</span></h2><button class="mini-btn" id="retryFailed" type="button">仅重试失败项</button></div><div class="task-toolbar"><label for="taskFilter">显示</label><select id="taskFilter"><option value="active">待执行 / 执行中</option><option value="failed">失败 / 中断</option><option value="all">全部任务</option></select><span id="taskSummary" class="faint"></span></div><p class="faint hint">可取消排队或等待重试的任务。已开始的远端操作不能撤销。进程重启后的未完成任务标记为中断，核实面板后再手动重试。</p><div id="taskList" class="task-list"></div></div><div class="card" id="historyCenter"><div class="card-head"><h2>历史记录</h2><div class="head-right"><button class="mini-btn" id="exportHistory" type="button">脱敏导出</button><button class="mini-btn danger" id="clearHistory" type="button">清空历史</button></div></div><form id="historyQuery" class="history-query"><label>账号<input id="histAccount" inputmode="numeric" placeholder="QQ 号"></label><label>面板<select id="histPanel"><option value="">全部面板</option></select></label><label>结果<select id="histOutcome"><option value="">全部结果与事件</option><option value="success">成功</option><option value="failed">失败</option><option value="skipped">跳过</option></select></label><label>开始日期<input type="date" id="histFrom"></label><label>结束日期<input type="date" id="histTo"></label><button class="btn" type="submit">查询</button></form><div id="historyStatus" class="faint hint" role="status">保留 90 天、最多 10,000 条，先达到的上限生效。旧版本已丢弃的记录无法恢复。</div><div id="historyResults" class="history-results"></div><div class="history-pager"><button id="histPrev" class="mini-btn" type="button">上一页</button><span id="histPage"></span><button id="histNext" class="mini-btn" type="button">下一页</button></div></div>`);
 document.querySelector('#panel-panels .card-head .head-right').insertAdjacentHTML('afterbegin','<button class="mini-btn" id="setupStart" type="button">接入向导</button>');
 document.body.insertAdjacentHTML('beforeend',`<dialog id="setupWizard" class="setup-wizard" aria-labelledby="wizardTitle"><form id="wizardForm"><div class="wizard-heading"><h2 id="wizardTitle">面板接入向导</h2><button type="button" id="wizardClose" class="mini-btn" aria-label="关闭接入向导">关闭</button></div><ol class="wizard-steps"><li>面板</li><li>凭据</li><li>检测</li><li>确认保存</li></ol><section data-step="0"><label>面板名称<input id="wLabel" maxlength="80" autocomplete="off" placeholder="我的农场"></label><label>面板地址<input id="wURL" type="url" placeholder="https://farm.example.com" autocomplete="off"></label><label>认证方式<select id="wMode"><option value="token">qq-farm-bot-404 · 我的 Token</option><option value="admin">qq-farm-bot-404 · 管理员密码</option><option value="code">旧版 qq-farm-bot · 访问 Code</option></select></label></section><section data-step="1"><label id="wUserRow">管理员用户名<input id="wUser" autocomplete="off"></label><label id="wSecretLabel"><span id="wSecretTitle">我的 Token</span><input id="wSecret" type="password" autocomplete="new-password"></label><p class="faint">只发送到你填写的面板进行认证；保存后不在网页状态中返回凭据。</p></section><section data-step="2"><p>检测连接并读取面板账号，确认凭据可用后继续。</p><button type="button" class="btn" id="wizardProbe">检测并读取账号</button><p id="wizardProbeResult" role="status"></p></section><section data-step="3"><div id="wizardReview"></div><p class="faint">保存配置不会自动推送账号，可返回账号页确认目标后再推送。</p></section><p class="wizard-error" id="wizardError" role="alert"></p><div class="wizard-footer"><button class="btn" type="button" id="wizardBack">上一步</button><button class="btn primary" type="submit" id="wizardNext">下一步</button></div></form></dialog>`);
}
function markFreshness(ok,s,error=''){
 const box=$('freshness'),text=$('freshnessText');if(ok){lastStateSuccess=Date.now();box.classList.toggle('stale',!!s?.sources?.accountsStale);text.textContent=s?.sources?.accountsStale?`账号数据未更新：${s.sources.accountsError||'正在等待框架'}；展示最近缓存。最近成功读取账号 ${dateText(s.sources.accountsAt)}`:`数据已刷新 ${dateText(lastStateSuccess)}`;}else{box.classList.add('stale');text.textContent=`刷新失败：${error}。当前显示缓存；最近成功刷新 ${dateText(lastStateSuccess)}，15 秒后重试。`;set('farmState',e=>e.textContent='状态未更新');}
}
function setSelectOptions(id,html,force=false){const el=$(id);if(!el||(!force&&document.activeElement===el))return;const val=el.value;if(el.dataset.options===html)return;el.innerHTML=html;el.dataset.options=html;if([...el.options].some(o=>o.value===val))el.value=val;}
function panelOptions(s){return(s.config?.panels||[]).map(p=>`<option value="${esc(p.baseUrl)}">${esc(p.label)}</option>`).join('')}
function syncAccountInputs(s){
 const groups=s.routing?.groups||{},current=$('groupFilter').value;
 setSelectOptions('groupFilter','<option value="">全部分组</option>'+Object.keys(groups).sort().map(g=>`<option>${esc(g)}</option>`).join(''),!!current&&!Object.hasOwn(groups,current));
 // Preserve selections across filters, but never retain unavailable instances.
 const valid=new Set((s.accounts||[]).filter(a=>a.online&&(a.platform||'').toLowerCase().includes('android')).map(keyOf));
 for(const id of selected)if(!valid.has(id))selected.delete(id);
}
function renderEnhancements(s){
 const panels=s.config?.panels||[];
 $('btnQueueAll').textContent=$('groupFilter').value?'推送此分组':'推送在线账号';
 setSelectOptions('pushTarget','<option value="route">按账号 / 分组路由</option><option value="all">推送到全部面板</option>'+panelOptions(s));setSelectOptions('histPanel','<option value="">全部面板</option>'+panelOptions(s));
 $('targetHint').textContent=`默认面板：${panels[s.config?.activePanel||0]?.label||'尚未配置'}`;
 document.querySelector('.stat[data-stat="activity"]').title=s.stats?.definition||'查看今日推送记录';syncFilterSummary();
 document.querySelectorAll('#accounts .btnPush,#accountCards .btnPush').forEach(b=>{b.title='按上方推送目标创建任务'});
 if(document.body.dataset.tab==='activity')renderTasks(s);else renderTaskSummary(s);if(!routingDirty)renderRouting(s);
 if(document.body.dataset.tab==='activity'&&Date.now()-lastHistoryRefresh>10000)refreshHistory();
 syncDisabledSettings();
}
function onTabChanged(name){if(!STATE)return;if(name==='panels'){renderPanels(STATE);renderDiagnosis(STATE)}if(name==='activity'){renderTasks(STATE);refreshHistory()}}
function targetsFor(a,override){const ps=STATE.config.panels||[];if(override==='all')return ps.map(p=>p.baseUrl);if(override&&override!=='route')return[override];const r=STATE.routing||{},p=r.accounts?.[keyOf(a)]||{};return p.targets?.length?p.targets:r.groups?.[p.group]?.length?r.groups[p.group]:[ps[STATE.config.activePanel||0]?.baseUrl].filter(Boolean)}
async function queuePush(ids,override=$('pushTarget')?.value||'route'){
 const accounts=(STATE?.accounts||[]).filter(a=>a.online&&ids.includes(keyOf(a)));if(!accounts.length)return toast('没有可推送的在线账号',false);
 const names=new Map((STATE.config.panels||[]).map(p=>[p.baseUrl,p.label]));const routes=accounts.map(a=>({a,targets:targetsFor(a,override)}));if(routes.some(r=>!r.targets.length))return toast('请先添加目标面板',false);
 const n=routes.reduce((n,r)=>n+r.targets.length,0);const preview=routes.slice(0,8).map(({a,targets})=>`${a.nickname||a.self_id} → ${targets.map(t=>names.get(t)||'已移除面板').join('、')}`).join('\n');
 if(!confirm(`确认推送 ${accounts.length} 个账号实例、共 ${n} 项任务？\n${preview}${routes.length>8?'\n…其余账号按所选路由执行':''}\nCode 更新或补启动可能重启农场会话。已发出的远端操作无法撤销。`))return;
 const urls=override==='route'?[]:override==='all'?[...names.keys()]:[override];try{const j=await api('api/tasks',{ids:accounts.map(keyOf),panel_urls:urls});toast(`已提交 ${j.ids.length} 项任务，可在任务中心查看`);if(STATE){STATE.tasks=j.tasks;renderTasks(STATE)};await load()}catch(e){toast(e.message,false)}
}
function renderTaskSummary(s){
 const tasks=s.tasks||[],active=tasks.filter(t=>['queued','running','retry_wait'].includes(t.status)),failed=tasks.filter(t=>['failed','interrupted'].includes(t.status));$('taskCount').textContent=active.length;$('tabCntActivity').textContent=active.length?`${active.length} 待处理`:tasks.length;$('taskSummary').textContent=`${active.length} 进行中 · ${failed.length} 失败或中断 · ${tasks.length} 项（最多保留 1,000 项）`;$('retryFailed').disabled=!failed.length;

 $('retryHint').innerHTML=active.length?`<button class="mini-btn" id="openTasks">${active.length} 项任务正在处理 · 打开任务中心</button>`:'';set('openTasks',b=>b.onclick=()=>showTab('activity'));
 return{tasks,active,failed};
}
function renderTasks(s){
 const {tasks,active,failed}=renderTaskSummary(s);
 const f=$('taskFilter').value,list=tasks.filter(t=>f==='all'||(f==='active'?['queued','running','retry_wait']:['failed','interrupted']).includes(t.status));
 const html=list.length?list.slice(0,200).map(t=>`<article class="task-item"><div class="task-top"><strong>${esc(t.nickname||String(t.self_id))} <span class="faint">${esc(String(t.self_id))} #${t.protocol_id}</span></strong><span class="pill ${t.status==='failed'||t.status==='interrupted'?'r':t.status==='succeeded'?'g':'n'}">${taskLabels[t.status]||esc(t.status)}</span></div><div>→ ${esc(t.panel)} <span class="faint">${t.source==='automatic'?'自动':'手动'} · 尝试 ${t.attempt}/${t.max_attempts}</span></div><div class="task-stage"><progress max="2" value="${t.status==='running'?1:['succeeded','skipped'].includes(t.status)?2:0}" aria-label="${esc(t.stage)}"></progress><span>${esc(t.stage)}</span></div>${t.next_at?`<div class="faint">重试时间 ${dateText(t.next_at)}</div>`:''}${t.error?`<div class="task-error">${esc(t.error)}</div>`:''}<div class="task-actions"><time class="faint">${dateText(t.updated_at)}</time>${['queued','retry_wait'].includes(t.status)?`<button class="mini-btn" data-cancel-task="${esc(t.id)}">取消待执行</button>`:''}${['failed','interrupted'].includes(t.status)?`<button class="mini-btn" data-retry-task="${esc(t.id)}">重试此项</button>`:''}</div></article>`).join(''):'<div class="empty">当前筛选没有任务</div>';
 setHTML('taskList',html);
}
function routingMark(){routingRevision++;routingDirty=true;$('routingStatus').textContent='有未保存的分组或路由更改';$('saveRouting').disabled=routingSaving}
function selectedValues(el){return[...el.selectedOptions].map(o=>o.value)}
function renderRouting(s,force=false){
 if(routingDirty&&!force)return;
 if(!force&&!$('routingDetails').open)return;
 const signature=JSON.stringify([s.routing||{},(s.accounts||[]).map(a=>[a.self_id,a.protocol_id,a.nickname]),(s.config?.panels||[]).map(p=>[p.baseUrl,p.label])]);
 if(!force&&signature===lastRoutingSignature){$('routingStatus').textContent='';$('saveRouting').disabled=true;return}
 lastRoutingSignature=signature;
 routingDraft=JSON.parse(JSON.stringify(s.routing||{accounts:{},groups:{}}));routingDraft.accounts||={};routingDraft.groups||={};const opts=panelOptions(s);
 $('groupRoutes').innerHTML=Object.entries(routingDraft.groups).map(([g,targets])=>`<div class="group-route"><strong>${esc(g)}</strong><label>分组目标<select multiple size="2" data-group-target="${esc(g)}">${opts}</select></label><button class="mini-btn danger" data-delete-group="${esc(g)}">删除分组</button></div>`).join('')||'<p class="faint">暂无分组，未指定路由时使用默认面板。</p>';
 $('groupRoutes').querySelectorAll('[data-group-target]').forEach(el=>{for(const o of el.options)o.selected=routingDraft.groups[el.dataset.groupTarget].includes(o.value)});
 const groups=Object.keys(routingDraft.groups).sort();$('accountPolicies').innerHTML=(s.accounts||[]).map(a=>{const key=keyOf(a),p=routingDraft.accounts[key]||{};return`<div class="policy-row" data-policy-key="${key}"><div><strong>${esc(a.nickname||String(a.self_id))}</strong><div class="faint">${a.self_id} · 协议 ${a.protocol_id}</div></div><label>所属分组<select data-policy-group><option value="">未分组</option>${groups.map(g=>`<option ${p.group===g?'selected':''}>${esc(g)}</option>`).join('')}</select></label><label>单独目标（可多选）<select multiple size="2" data-policy-targets>${opts}</select><small>不选则继承分组 / 默认面板</small></label><label class="pause-label"><input type="checkbox" data-policy-paused ${p.paused?'checked':''}>暂停自动推送</label></div>`}).join('')||'<p class="faint">等待框架中的 Android 账号。</p>';
 $('accountPolicies').querySelectorAll('[data-policy-key]').forEach(row=>{const p=routingDraft.accounts[row.dataset.policyKey]||{};for(const opt of row.querySelector('[data-policy-targets]').options)opt.selected=(p.targets||[]).includes(opt.value)});$('routingStatus').textContent='';$('saveRouting').disabled=true;
}
function historyParams(){const q=new URLSearchParams({limit:'50',offset:String(historyOffset)});for(const [id,key]of [['histAccount','account'],['histPanel','panel'],['histOutcome','outcome'],['histFrom','from'],['histTo','to']])if($(id).value)q.set(key,$(id).value);return q}
async function refreshHistory(){const seq=++historyRequest;lastHistoryRefresh=Date.now();try{const j=await api('api/history?'+historyParams());if(seq!==historyRequest)return;$('histPage').textContent=`${j.total?j.offset+1:0}–${Math.min(j.offset+j.items.length,j.total)} / ${j.total}`;$('histPrev').disabled=historyOffset===0;$('histNext').disabled=historyOffset+j.items.length>=j.total;$('historyStatus').textContent='保留 90 天、最多 10,000 条；清空历史不影响今日累计。';$('historyResults').innerHTML=j.items.length?j.items.map(h=>`<article class="history-item"><time class="faint">${dateText(h.time)}</time><div><span class="pill ${h.outcome==='failed'?'r':h.outcome==='success'?'g':'n'}">${{success:'成功',failed:'失败',skipped:'跳过'}[h.outcome]||'事件'}</span> ${esc(h.message||'事件记录')}</div>${h.detail?`<div class="faint">${esc(h.detail)}</div>`:''}</article>`).join(''):'<div class="empty">此条件下没有记录</div>'}catch(e){if(seq===historyRequest)$('historyStatus').textContent='历史加载失败：'+e.message+'，保留上次查询结果'}}
function syncDisabledSettings(){const enabled=$('f_enabled').checked;$('f_pushOnLogin').disabled=!enabled;$('f_refreshMinutes').disabled=!enabled;$('f_autoRepair').disabled=!enabled;$('f_staleHours').disabled=!enabled;$('f_repairMinutes').disabled=!enabled||!$('f_autoRepair').checked}

// Diagnostics preserve the panel identity for each repair item.
renderDiagnosis=function(s){const list=$('diagList'),issues=[];for(const [i,p]of(s.panels||[]).entries()){
 if(p.checking)continue;
 if(!p.ok){issues.push({label:`${p.label}：${p.authBlocked||p.hasAuth===false?'认证失败，请更新凭据':p.error||'连接失败'}`,act:p.authBlocked||p.hasAuth===false?'edit':'test',panel:i});continue}
 for(const a of s.accounts||[]){if(!a.online)continue;const policy=s.routing?.accounts?.[keyOf(a)]||{};const targets=targetsFor(a,'route');if(!targets.includes(p.baseUrl))continue;const hit=panelAccount(p,a.self_id);const stale=a.code_age_ms!=null&&a.code_age_ms>(s.config.staleHours||48)*3600000;if(!hit||hit.running===false||stale)issues.push({label:`${a.nickname||a.self_id} → ${p.label}：${!hit?'尚未接入':hit.running===false?'账号已停止':'Code 待更新'}${policy.paused?'（自动推送已暂停）':''}`,key:keyOf(a),base:p.baseUrl})}
 };$('diagCount').textContent=issues.length?`${issues.length} 项`:'';$('diagCount').style.display=issues.length?'':'none';$('btnDiagAll').style.display=issues.some(i=>i.key)?'':'none';list.innerHTML=issues.length?issues.map((i,n)=>`<div class="diagnosis-item"><span>${esc(i.label)}</span><button class="mini-btn" data-diagnosis="${n}">${i.act==='edit'?'更新凭据':i.act==='test'?'重新检测':'修复此项'}</button></div>`).join(''):'<div class="faint hint">当前路由没有待处理问题。</div>';
 list.querySelectorAll('[data-diagnosis]').forEach(b=>b.onclick=()=>{const i=issues[Number(b.dataset.diagnosis)];if(i.act)panelAction(i.act,i.panel);else queuePush([i.key],i.base)});$('btnDiagAll').onclick=async()=>{const repairs=issues.filter(i=>i.key);if(!repairs.length)return;if(!confirm(`按当前路由修复 ${repairs.length} 项账号与面板对应关系？可能重启农场会话。`))return;try{for(const i of repairs)await api('api/tasks',{ids:[i.key],panel_urls:[i.base]});toast('修复任务已排队');load()}catch(e){toast(e.message,false)}};
};

let wizardStep=0,wizardChecked=null;
function wizardCandidate(){const mode=$('wMode').value,p={label:$('wLabel').value.trim(),baseUrl:$('wURL').value.trim().replace(/\/+$/,''),authMode:mode};if(mode==='token')p.token=$('wSecret').value.trim();else if(mode==='admin'){p.username=$('wUser').value.trim();p.password=$('wSecret').value.trim()}else p.accessCode=$('wSecret').value.trim();return p}
function wizardShow(){for(const sec of $('setupWizard').querySelectorAll('[data-step]'))sec.hidden=Number(sec.dataset.step)!==wizardStep;[...$('setupWizard').querySelectorAll('.wizard-steps li')].forEach((li,i)=>li.classList.toggle('current',i===wizardStep));$('wizardBack').disabled=wizardStep===0;$('wizardNext').textContent=wizardStep===3?'保存面板':'下一步';$('wizardError').textContent='';$('wUserRow').hidden=$('wMode').value!=='admin';$('wSecretTitle').textContent={token:'我的 Token',admin:'管理员密码',code:'访问 Code'}[$('wMode').value]}
function wizardOpen(){wizardStep=0;wizardChecked=null;$('wizardForm').reset();$('wizardProbeResult').textContent='';wizardShow();$('setupWizard').showModal();$('wLabel').focus()}
function wizardClose(){$('setupWizard').close();$('wSecret').value='';wizardChecked=null}
function syncFilterSummary(){
 const count=Number($('accSort').value!=='online')+Number(!!$('groupFilter').value)+Number($('advancedAccounts').checked);
 $('accountFilterLabel').textContent=count?'筛选 · '+count:'筛选';
 $('accountFilterToggle').classList.toggle('has-filters',count>0);
}
function installHandlers(){
 $('routingDetails').addEventListener('toggle',()=>{if($('routingDetails').open&&STATE)renderRouting(STATE)});
 const toggle=$('accountFilterToggle'),filters=$('accountFilters');
 const openFilters=open=>{filters.dataset.open=String(open);toggle.setAttribute('aria-expanded',String(open))};
 toggle.onclick=()=>openFilters(toggle.getAttribute('aria-expanded')!=='true');
 filters.addEventListener('keydown',e=>{if(e.key==='Escape'&&matchMedia('(max-width:640px)').matches){openFilters(false);toggle.focus()}});
 filters.addEventListener('change',syncFilterSummary);
 $('panel-accounts').addEventListener('click',e=>{if(!e.target.closest('[data-reset-account-filters]'))return;$('search').value='';$('groupFilter').value='';document.querySelector('.acc-filter [data-filter="all"]').click();$('search').focus()});
 $('freshRetry').onclick=()=>load();$('advancedAccounts').checked=localStorage.getItem('qqfarm-advanced-columns')==='1';document.body.classList.toggle('advanced-accounts',$('advancedAccounts').checked);$('advancedAccounts').onchange=e=>{document.body.classList.toggle('advanced-accounts',e.target.checked);localStorage.setItem('qqfarm-advanced-columns',e.target.checked?'1':'0')};$('groupFilter').onchange=()=>STATE&&render(STATE);
 $('btnQueueAll').onclick=()=>{const group=$('groupFilter').value;return queuePush((STATE?.accounts||[]).filter(a=>a.online&&(!group||STATE.routing?.accounts?.[keyOf(a)]?.group===group)).map(keyOf))};$('btnPushAll').onclick=$('btnQueueAll').onclick;
 pushOne=b=>queuePush([`${b.dataset.self}:${b.dataset.proto}`]);$('bulkPush').onclick=()=>queuePush([...selected]);$('bulkBroadcast').onclick=()=>queuePush([...selected],'all');
 const previousPanelAction=panelAction;panelAction=async(act,i)=>{if(act==='push')return queuePush((STATE?.accounts||[]).filter(a=>a.online).map(keyOf),STATE.config.panels[i].baseUrl);try{return await previousPanelAction(act,i)}catch(e){toast(e.message,false)}};
 // Capture drawer push buttons before legacy synchronous handlers.
 document.addEventListener('click',e=>{const button=e.target.closest('[data-dwpush],#dwBroadcast');if(!button)return;e.preventDefault();e.stopImmediatePropagation();const self=String($('drawer').dataset.self||'');const g=collectGroups(STATE).find(g=>String(g.self_id)===self);const a=g?.instances.find(a=>a.online);if(!a)return toast('账号不在线',false);queuePush([keyOf(a)],button.id==='dwBroadcast'?'all':STATE.config.panels[Number(button.dataset.dwpush)]?.baseUrl)},true);
 $('taskFilter').onchange=()=>STATE&&renderTasks(STATE);$('taskList').onclick=async e=>{const c=e.target.closest('[data-cancel-task]'),r=e.target.closest('[data-retry-task]');if(!c&&!r)return;const b=c||r;b.disabled=true;try{await api(c?'api/tasks/cancel':'api/tasks/retry',{id:c?c.dataset.cancelTask:r.dataset.retryTask});await load()}catch(e){toast(e.message,false)}finally{b.disabled=false}};
 $('retryFailed').onclick=async()=>{const failed=(STATE?.tasks||[]).filter(t=>['failed','interrupted'].includes(t.status));if(!failed.length||!confirm(`仅重试 ${failed.length} 项失败 / 中断任务？已成功项不会重放。中断任务请先核实面板状态。`))return;try{for(const t of failed)await api('api/tasks/retry',{id:t.id});toast('失败任务已重新排队');load()}catch(e){toast(e.message,false)}};
 $('cfg').addEventListener('input',()=>{settingsRevision++;settingsDirty=true;$('saveRow').classList.add('dirty-on');syncDisabledSettings()});$('cfg').addEventListener('change',()=>{settingsRevision++;settingsDirty=true;$('saveRow').classList.add('dirty-on');syncDisabledSettings()});
 $('btnSave').onclick=async()=>{
  if(settingsSaving)return;settingsSaving=true;
  const b=$('btnSave'),revision=settingsRevision;b.disabled=true;
  try{
   const j=await api('api/config',{farm:{enabled:$('f_enabled').checked,pushOnLogin:$('f_pushOnLogin').checked,refreshMinutes:Number($('f_refreshMinutes').value)||0,staleHours:Number($('f_staleHours').value)||0,autoRepair:$('f_autoRepair').checked,repairMinutes:Number($('f_repairMinutes').value)||30,alertFails:Number($('f_alertFails').value)||3,webhook:$('f_webhook').value.trim()}});
   if(revision===settingsRevision){settingsDirty=false;$('saveRow').classList.remove('dirty-on')}
   if(j.state)render(j.state);
   toast(settingsDirty?'本次设置已保存，保存期间的新修改尚未保存':'设置已保存，立即生效');
  }catch(e){toast('保存失败：'+e.message,false)}finally{settingsSaving=false;b.disabled=false}
 };
 window.addEventListener('beforeunload',e=>{if(settingsDirty||routingDirty){e.preventDefault();e.returnValue=''}});
 $('addGroup').onclick=()=>{const name=$('newGroupName').value.trim();if(!name||[...name].length>40)return toast('分组名称需为 1–40 个字符',false);if(Object.hasOwn(routingDraft.groups,name))return toast('分组已存在',false);routingDraft.groups[name]=[];const draft=routingDraft;renderRouting({...STATE,routing:draft},true);routingMark();$('newGroupName').value=''};
 $('groupRoutes').onchange=e=>{const el=e.target.closest('[data-group-target]');if(el){routingDraft.groups[el.dataset.groupTarget]=selectedValues(el);routingMark()}};
 $('groupRoutes').onclick=e=>{const b=e.target.closest('[data-delete-group]');if(!b)return;const g=b.dataset.deleteGroup;if(!confirm(`删除分组「${g}」？其中账号改为未分组。`))return;delete routingDraft.groups[g];for(const p of Object.values(routingDraft.accounts))if(p.group===g)p.group='';const draft=routingDraft;renderRouting({...STATE,routing:draft},true);routingMark()};
 $('accountPolicies').onchange=e=>{const row=e.target.closest('[data-policy-key]');if(!row)return;routingDraft.accounts[row.dataset.policyKey]={group:row.querySelector('[data-policy-group]').value,targets:selectedValues(row.querySelector('[data-policy-targets]')),paused:row.querySelector('[data-policy-paused]').checked};routingMark()};
 $('saveRouting').onclick=async()=>{
  if(routingSaving)return;routingSaving=true;
  const b=$('saveRouting'),revision=routingRevision;b.disabled=true;
  try{
   const j=await api('api/routing',{routing:routingDraft});
   if(revision===routingRevision)routingDirty=false;
   STATE.routing=j.routing;render(STATE);
   toast(routingDirty?'本次路由已保存，保存期间的新修改尚未保存':'分组与路由已保存');
  }catch(e){toast(e.message,false)}finally{routingSaving=false;b.disabled=!routingDirty}
 };
 $('historyQuery').onsubmit=e=>{e.preventDefault();historyOffset=0;refreshHistory()};$('histPrev').onclick=()=>{historyOffset=Math.max(0,historyOffset-50);refreshHistory()};$('histNext').onclick=()=>{historyOffset+=50;refreshHistory()};$('exportHistory').onclick=async()=>{try{download(await api('api/history/export?'+historyParams()),'qqfarm-history-redacted.json');toast('已导出当前筛选下的脱敏历史')}catch(e){toast(e.message,false)}};$('clearHistory').onclick=async()=>{if(!confirm('清空全部历史记录？今日累计统计将保留。'))return;try{await api('api/history_clear',{});historyOffset=0;refreshHistory();load()}catch(e){toast(e.message,false)}};
 $('setupStart').onclick=wizardOpen;$('btnAddPanel').onclick=wizardOpen;$('btnGoPanels').onclick=()=>{showTab('panels');wizardOpen()};$('wizardClose').onclick=wizardClose;$('setupWizard').addEventListener('cancel',()=>{$('wSecret').value='';wizardChecked=null});$('wizardBack').onclick=()=>{wizardStep=Math.max(0,wizardStep-1);wizardShow()};
 $('wizardForm').addEventListener('input',()=>{wizardChecked=null;$('wizardProbeResult').textContent='';});$('wMode').onchange=wizardShow;
 $('wizardProbe').onclick=async()=>{const b=$('wizardProbe');b.disabled=true;$('wizardProbeResult').textContent='正在检测…';try{const j=await api('api/setup/probe',{panel:wizardCandidate()});wizardChecked=j.result;$('wizardProbeResult').textContent=`连接正常 · ${j.result.ms} ms · 读取到 ${j.result.accounts} 个账号（${j.result.running} 个运行中）`}catch(e){wizardChecked=null;$('wizardProbeResult').textContent='检测失败：'+e.message}finally{b.disabled=false}};
 $('wizardForm').onsubmit=async e=>{e.preventDefault();const b=$('wizardNext');try{if(wizardStep===0){const p=wizardCandidate(),u=new URL(p.baseUrl);if(!['http:','https:'].includes(u.protocol)||u.username||u.password||u.search||u.hash)throw Error('请输入有效的 HTTP/HTTPS 面板地址');if((STATE?.config?.panels||[]).some(p=>p.baseUrl===u.href.replace(/\/$/,'')))throw Error('该面板已存在，请在面板卡片中编辑')}if(wizardStep===1){if(!$('wSecret').value.trim())throw Error('请填写凭据');if($('wMode').value==='admin'&&!$('wUser').value.trim())throw Error('请填写管理员用户名')}if(wizardStep===2){if(!wizardChecked)throw Error('请先检测连接并读取账号');const p=wizardCandidate();$('wizardReview').innerHTML=`<p><strong>${esc(p.label||'新面板')}</strong></p><p>${esc(p.baseUrl)}</p><p>已读取 ${wizardChecked.accounts} 个账号，其中 ${wizardChecked.running} 个运行中。</p>`}if(wizardStep<3){wizardStep++;wizardShow();return}b.disabled=true;const ps=[...(STATE.config.panels||[]),wizardCandidate()];if(ps.length>8)throw Error('最多支持 8 个面板');await savePanels(ps,STATE.config.activePanel||0);wizardClose();toast('面板已保存，可在账号页确认目标后推送');load()}catch(err){$('wizardError').textContent=err.message}finally{b.disabled=false}};
}
addUI();installHandlers();document.body.dataset.tab=localStorage.getItem(TAB_KEY)||'accounts';skeleton();load();

