const fs=require('node:fs'),http=require('node:http'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require('playwright');
const repo=path.resolve(__dirname,'..'),out=process.env.QQFARM_PERF_OUTPUT||path.join(repo,'../../work/perf-v384');
const native=process.env.QQFARM_UI_NATIVE_HTML;if(!native||!fs.existsSync(native))throw Error('Capture the compiled admin HTML through ipc-host.mjs first');
const baseline=process.env.QQFARM_PERF_BASELINE==='1';fs.mkdirSync(out,{recursive:true});
const now=Date.now(),primary={label:'主面板',baseUrl:'http://primary.example.test',mode:'token',hasToken:true,hasAuth:true},backup={label:'备用面板',baseUrl:'http://backup.example.test',mode:'code',hasCode:true,hasAuth:true};
const state={version:'3.8.4',ipc_ready:true,instance_id:'mock-ui',data_path:'/data',now,config:{enabled:true,pushOnLogin:true,refreshMinutes:0,staleHours:12,autoRepair:true,repairMinutes:30,alertFails:3,webhook:'',panels:[primary,backup],activePanel:0},accounts:Array.from({length:8},(_,i)=>({self_id:10001+i,protocol_id:i===0?0:i,nickname:['春日农场','晚风牧场','小麦计划'][i]||`农场账号${i+1}`,remark:'',online:i<6,platform:'android',level:20+i,friend_count:30,group_count:3,code_age_ms:i===2?null:3600000})),panels:[{...primary,ok:true,ms:36,checkedAt:now,accounts:[{uin:'10001',running:true,hasCode:true}]},{...backup,ok:false,authBlocked:true,error:'凭据失效',accounts:[],checkedAt:now}],routing:{groups:{日常:[primary.baseUrl]},accounts:{'10001:0':{group:'日常',paused:false,targets:[]}}},sources:{accountsAt:now,accountsStale:false},stats:{todayOk:222,todayFail:7,todaySkipped:9,todayAttempts:238,definition:'按执行次数累计'},tasks:[{id:'failed-1',self_id:10001,protocol_id:0,panel:'备用面板',panel_url:backup.baseUrl,source:'manual',status:'failed',stage:'请检查凭据',attempt:1,max_attempts:4,updated_at:now,error:'认证失败'},{id:'waiting-1',self_id:10003,protocol_id:2,panel:'主面板',source:'manual',status:'retry_wait',stage:'等待重试',attempt:1,max_attempts:4,updated_at:now,next_at:now+30000}],history:[],schedule:{},retries:[]};
const count=Number(process.env.QQFARM_PERF_ACCOUNTS)||200,delay=200;
state.accounts=Array.from({length:count},(_,i)=>({...state.accounts[0],self_id:10001+i,protocol_id:i%4,nickname:`农场账号${i+1}`,online:true}));
let server,browser;const requests=[],errors=[];
(async()=>{
 server=http.createServer((req,res)=>{const u=new URL(req.url,'http://fixture');requests.push(u.pathname);setTimeout(()=>{
  const send=(type,body)=>{res.writeHead(200,{'Content-Type':type});res.end(body)};
  if(u.pathname==='/api/state')return send('application/json',JSON.stringify(state));
  if(u.pathname.startsWith('/api/history'))return send('application/json',JSON.stringify({items:[],total:0,offset:0,limit:50}));
  if(u.pathname==='/')return send('text/html;charset=utf-8',fs.readFileSync(native));
  if(['/enhancements.css','/enhancements.js'].includes(u.pathname))return send(u.pathname.endsWith('.js')?'text/javascript':'text/css',fs.readFileSync(path.join(process.env.QQFARM_PERF_WEB_ROOT||path.join(repo,'web'),u.pathname.slice(1))));
  res.writeHead(204);res.end();
 },delay)});
 await new Promise(r=>server.listen(0,'127.0.0.1',r));browser=await chromium.launch({channel:process.env.QQFARM_BROWSER_CHANNEL||'msedge',headless:true});
 const context=await browser.newContext({viewport:{width:1440,height:1000},reducedMotion:'reduce'}),p=await context.newPage();
 p.on('pageerror',e=>errors.push(e.message));p.on('console',m=>{if(m.type()==='warning'&&m.text().includes('render 失败'))errors.push(m.text())});
 await p.goto(`http://127.0.0.1:${server.address().port}/`,{waitUntil:'domcontentloaded'});
 await p.waitForFunction(n=>typeof STATE!=='undefined'&&STATE?.accounts.length===n&&document.querySelectorAll('#accounts .rowCheck').length===n,count);
 const metrics=await p.evaluate(()=>{
  const ready=performance.now(),renders=[],firstRow=document.querySelector('#accounts tr');for(let i=0;i<7;i++){const t=performance.now();render(STATE);renders.push(performance.now()-t)}
  renders.sort((a,b)=>a-b);
  return{accountReadyMs:Math.round(ready),firstContentfulPaintMs:performance.getEntriesByName('first-contentful-paint')[0]?.startTime||null,refreshRenderMedianMs:Number(renders[3].toFixed(2)),initialPolicyRows:document.querySelectorAll('.policy-row').length,initialTaskRows:document.querySelectorAll('.task-item').length,hiddenAccountCards:document.querySelectorAll('#accountCards .acc-card').length,accountRowsReused:firstRow===document.querySelector('#accounts tr'),domNodes:document.querySelectorAll('*').length};
 });
 const initialRequests=requests.filter(x=>['/','/enhancements.css','/enhancements.js','/api/state'].includes(x));
 if(!baseline){assert.equal(initialRequests.length,2,'Initial page must use one HTML request and one shared state request');assert.equal(metrics.initialPolicyRows,0,'Closed routing editor should not build account forms');assert.equal(metrics.hiddenAccountCards,0,'Desktop should not build hidden mobile cards');assert(metrics.accountRowsReused,'Unchanged refresh should preserve account DOM')}
 await p.locator('#routingDetails summary').click();await p.waitForFunction(n=>document.querySelectorAll('.policy-row').length===n,count);
 if(!baseline){
  await p.locator('#accounts .rowCheck').first().check();
  const key=await p.locator('#accounts .rowCheck').first().getAttribute('data-key');
  await p.setViewportSize({width:390,height:844});await p.waitForFunction(()=>accountMedia.matches&&document.querySelector('#accountCards .rowCheck'));
  assert(await p.locator('#accountCards .rowCheck[data-key="'+key+'"]').isChecked());
  await p.setViewportSize({width:1440,height:1000});await p.waitForFunction(()=>!accountMedia.matches&&document.querySelector('#accounts .rowCheck'));
  assert(await p.locator('#accounts .rowCheck[data-key="'+key+'"]').isChecked());
 }
 assert.deepEqual(errors,[]);
 const result={passed:true,mode:baseline?'baseline':'optimized',accounts:count,simulatedRequestDelayMs:delay,initialRequests,...metrics,routingOnOpen:true,errors};
 fs.writeFileSync(path.join(out,'results.json'),JSON.stringify(result,null,2));console.log(JSON.stringify(result,null,2));
 await p.screenshot({path:path.join(out,'desktop.png'),fullPage:false});await context.close();
})().catch(e=>{console.error(e);process.exitCode=1}).finally(async()=>{await browser?.close();server?.closeAllConnections();await new Promise(r=>server?server.close(r):r())});
