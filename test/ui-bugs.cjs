const fs=require('node:fs'),http=require('node:http'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const repo=path.resolve(__dirname,'..'),out=process.env.QQFARM_BUG_OUTPUT||path.join(repo,'../../work/bugs-v383');
fs.mkdirSync(out,{recursive:true});
const now=Date.now(),primary={label:'主面板',baseUrl:'http://primary.example.test',mode:'token',hasToken:true,hasAuth:true},backup={label:'备用面板',baseUrl:'http://backup.example.test',mode:'code',hasCode:true,hasAuth:true};
let state={version:'3.8.3',ipc_ready:true,instance_id:'mock-ui',data_path:'/data',now,config:{enabled:true,pushOnLogin:true,refreshMinutes:0,staleHours:12,autoRepair:true,repairMinutes:30,alertFails:3,webhook:'',panels:[primary,backup],activePanel:0},accounts:Array.from({length:8},(_,i)=>({self_id:10001+i,protocol_id:i===0?0:i,nickname:['春日农场','晚风牧场','小麦计划'][i]||`农场账号${i+1}`,remark:'',online:i<6,platform:'android',level:20+i,friend_count:30,group_count:3,code_age_ms:i===2?null:3600000})),panels:[{...primary,ok:true,ms:36,checkedAt:now,accounts:[{uin:'10001',running:true,hasCode:true}]},{...backup,ok:false,authBlocked:true,error:'凭据失效',accounts:[],checkedAt:now}],routing:{groups:{日常:[primary.baseUrl]},accounts:{'10001:0':{group:'日常',paused:false,targets:[]}}},sources:{accountsAt:now,accountsStale:false},stats:{todayOk:222,todayFail:7,todaySkipped:9,todayAttempts:238,definition:'按执行次数累计'},tasks:[{id:'failed-1',self_id:10001,protocol_id:0,panel:'备用面板',panel_url:backup.baseUrl,source:'manual',status:'failed',stage:'请检查凭据',attempt:1,max_attempts:4,updated_at:now,error:'认证失败'},{id:'waiting-1',self_id:10003,protocol_id:2,panel:'主面板',source:'manual',status:'retry_wait',stage:'等待重试',attempt:1,max_attempts:4,updated_at:now,next_at:now+30000}],history:[],schedule:{},retries:[]};
const initial=structuredClone(state);
function resetState(){
 state=structuredClone(initial);
 const bot=state.accounts[0];
 state.accounts=[bot,{...bot,protocol_id:1},{...bot,self_id:10002,protocol_id:2,nickname:'独立账号'}];
 state.routing={groups:{日常:[primary.baseUrl],备用:[primary.baseUrl]},accounts:{'10001:0':{group:'日常',targets:[],paused:false},'10001:1':{group:'备用',targets:[],paused:false}}};
}
let browser,server,pendingConfig,pendingRouting,holdConfig=false,holdRouting=false;
const checks=[],errors=[];
(async()=>{
 server=http.createServer((req,res)=>{let raw='';req.on('data',c=>raw+=c);req.on('end',()=>{
  const u=new URL(req.url,'http://fixture');
  const send=(body)=>{res.writeHead(200,{'Content-Type':'application/json'});res.end(JSON.stringify(body))};
  const body=raw?JSON.parse(raw):{};
  if(u.pathname==='/api/state')return send(state);
  if(u.pathname==='/api/config'){
   const complete=()=>{Object.assign(state.config,body.farm);send({ok:true,state})};
   if(holdConfig){holdConfig=false;pendingConfig=complete;return}return complete();
  }
  if(u.pathname==='/api/routing'){
   const complete=()=>{state.routing=body.routing;send({ok:true,routing:state.routing})};
   if(holdRouting){holdRouting=false;pendingRouting=complete;return}return complete();
  }
  if(u.pathname==='/api/tasks')return send({tasks:state.tasks});
  if(u.pathname.startsWith('/api/history'))return send({items:[],total:0,offset:0,limit:50});
  const name=u.pathname==='/'?'index.html':u.pathname.slice(1);
  if(!['index.html','enhancements.css','enhancements.js'].includes(name)){res.writeHead(204);res.end();return}
  res.writeHead(200,{'Content-Type':name.endsWith('.js')?'text/javascript':name.endsWith('.css')?'text/css':'text/html;charset=utf-8'});
  res.end(fs.readFileSync(path.join(repo,'web',name)));
 })});
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 browser=await chromium.launch({channel:process.env.QQFARM_BROWSER_CHANNEL||'msedge',headless:true});
 async function check(name,run){
  resetState();const context=await browser.newContext({viewport:{width:1440,height:1000},reducedMotion:'reduce'}),page=await context.newPage();
  page.on('pageerror',e=>errors.push(e.message));page.on('console',m=>{if(m.type()==='warning'&&m.text().includes('render 失败'))errors.push(m.text())});
  try{await page.goto(`http://127.0.0.1:${server.address().port}/`);await page.waitForSelector('#accounts .rowCheck');await run(page);checks.push({name,passed:true});console.log('PASS '+name)}
  catch(e){checks.push({name,passed:false,error:e.message});console.error('FAIL '+name+': '+e.message)}
  finally{pendingConfig?.();pendingConfig=undefined;pendingRouting?.();pendingRouting=undefined;await context.close()}
 }
 await check('settings edits survive a pending save',async p=>{
  await p.locator('#tab-settings').click();await p.locator('#f_refreshMinutes').fill('45');await p.evaluate(()=>{const save=$('btnSave').onclick;$('btnSave').onclick=()=>window.pendingSave=save()});holdConfig=true;
  await Promise.all([p.waitForRequest(r=>r.url().endsWith('/api/config')),p.locator('#btnSave').click()]);
  await p.locator('#f_refreshMinutes').fill('60');pendingConfig();pendingConfig=undefined;
  await p.evaluate(()=>window.pendingSave);
  assert.equal(await p.locator('#f_refreshMinutes').inputValue(),'60');assert.equal(await p.evaluate(()=>settingsDirty),true);
  await p.locator('#btnSave').click();await p.waitForFunction(()=>!settingsDirty);assert.equal(state.config.refreshMinutes,60);
 });
 await check('routing edits survive save and prevent duplicate submission',async p=>{
  await p.locator('#routingDetails summary').click();await p.locator('[data-policy-key="10001:0"] [data-policy-paused]').check();await p.evaluate(()=>{const save=$('saveRouting').onclick;$('saveRouting').onclick=()=>window.pendingSave=save()});holdRouting=true;
  await Promise.all([p.waitForRequest(r=>r.url().endsWith('/api/routing')),p.locator('#saveRouting').click()]);
  const group=p.locator('[data-policy-key="10001:0"] [data-policy-group]');await group.selectOption('备用');
  const disabledDuringSave=await p.locator('#saveRouting').isDisabled();pendingRouting();pendingRouting=undefined;
  await p.evaluate(()=>window.pendingSave);
  assert.equal(await group.inputValue(),'备用');assert.equal(await p.evaluate(()=>routingDirty),true);assert(disabledDuringSave);
  await p.locator('#saveRouting').click();await p.waitForFunction(()=>!routingDirty);assert.equal(state.routing.accounts['10001:0'].group,'备用');
 });
 await check('group filters isolate protocol instances including zero',async p=>{
  await p.locator('#groupFilter').selectOption('日常');assert.equal(await p.locator('#accounts .rowCheck').count(),1);
  await p.locator('#checkAll').check();assert.deepEqual(await p.evaluate(()=>[...selected]),['10001:0']);
 });
 await check('a removed group resets before rendering accounts',async p=>{
  await p.locator('#groupFilter').selectOption('日常');delete state.routing.groups.日常;state.routing.accounts['10001:0'].group='';
  await p.evaluate(()=>load());assert.equal(await p.locator('#groupFilter').inputValue(),'');assert.equal(await p.locator('#accounts .rowCheck').count(),3);
 });
 await check('offline and removed accounts leave bulk selection',async p=>{
  const checkbox=p.locator('#accounts .rowCheck[data-key="10001:0"]');await checkbox.check();state.accounts[0].online=false;await p.evaluate(()=>load());
  assert.equal(await p.evaluate(()=>selected.size),0);assert.equal(await p.locator('#bulkBar').isVisible(),false);
  state.accounts[0].online=true;await p.evaluate(()=>load());await checkbox.check();state.accounts=state.accounts.filter(a=>a.protocol_id!==0);await p.evaluate(()=>load());
  assert.equal(await p.evaluate(()=>selected.size),0);assert.equal(await p.locator('#bulkBar').isVisible(),false);
 });
 const passed=checks.every(c=>c.passed)&&errors.length===0;
 fs.writeFileSync(path.join(out,'results.json'),JSON.stringify({passed,checks,errors},null,2));if(!passed)process.exitCode=1;
})().catch(e=>{console.error(e);process.exitCode=1}).finally(async()=>{await browser?.close();server?.closeAllConnections();await new Promise(resolve=>server?server.close(resolve):resolve())});
