/**
 * Go 版插件联调：模拟萌卡NT 框架宿主 + qq-farm-bot 面板，驱动 dist 构建产物。
 * 用法：bash build.sh && node test/ipc-host.mjs
 */
import { spawn } from 'node:child_process'
import { mkdtempSync, writeFileSync, readFileSync, existsSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import readline from 'node:readline'
import http from 'node:http'

const PKG = join(new URL('..', import.meta.url).pathname, 'dist/package-amd64')
const ENTRY = join(PKG, 'bin/qqfarm-bot')
const PLUGIN_ID = 'qqfarm-bot'
// 版本从构建产物清单读取，CI 用开发版本号也能跑
const VERSION = JSON.parse(readFileSync(join(PKG, 'mengka-plugin.json'), 'utf8')).version
const INSTANCE_ID = 'go-test-001'
const FARM_ACCESS_CODE = 'farm-access-code'
const FARM_SESSION_TOKEN = 'farm-session-token-abc'

const results = []
let farmAccounts = [{ id: 'acc-1', name: 'QQ10001', qq: '10001', uin: '10001', code: 'OLD_CODE', platform: 'qq', running: true }]
let failOnce = 0
let postCount = 0
let forcedCode = ''
let hello = null
let actions = 0

const summarize = (label, ok, detail = '') => {
  results.push(ok)
  console.error(`  ${ok ? 'PASS' : 'FAIL'}  ${label}${detail ? ` — ${detail}` : ''}`)
}
const delay = ms => new Promise(r => setTimeout(r, ms))

/* ---------- 模拟 farm-bot 面板 ---------- */
const farm = http.createServer((req, res) => {
  let raw = ''
  req.on('data', c => { raw += c })
  req.on('end', () => {
    let body = {}
    try { body = raw ? JSON.parse(raw) : {} } catch {}
    const send = (status, obj) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(JSON.stringify(obj))
    }
    if (req.url === '/api/login' && req.method === 'POST') {
      if (body.code !== FARM_ACCESS_CODE) return send(401, { ok: false, error: 'Code 不正确' })
      res.setHeader('Set-Cookie', `farm_admin_session=${FARM_SESSION_TOKEN}; Path=/; HttpOnly`)
      return send(200, { ok: true, data: { username: 'admin' } })
    }
    if (req.headers['x-admin-token'] !== FARM_SESSION_TOKEN) return send(401, { ok: false, error: 'Unauthorized' })
    if (req.url === '/api/accounts' && req.method === 'POST' && failOnce > 0) {
      failOnce -= 1
      return send(500, { ok: false, error: 'upstream boom' })
    }
    if (req.url === '/api/accounts' && req.method === 'GET') return send(200, { ok: true, data: { accounts: farmAccounts } })
    if (req.url === '/api/accounts' && req.method === 'POST') {
      postCount += 1
      const idx = farmAccounts.findIndex(a => a.id === body.id)
      if (idx >= 0) Object.assign(farmAccounts[idx], body, { running: true })
      else farmAccounts.push({ id: `acc-${farmAccounts.length + 1}`, ...body, running: true })
      return send(200, { ok: true, data: { accounts: farmAccounts } })
    }
    send(404, { ok: false, error: 'Not Found' })
  })
})
await new Promise(r => farm.listen(0, '127.0.0.1', r))
const farmPort = farm.address().port
const farmURL = `http://127.0.0.1:${farmPort}`

/* ---------- 启动插件 ---------- */
const dataDir = mkdtempSync(join(tmpdir(), 'mengka-go-'))
writeFileSync(join(dataDir, 'config.json'), JSON.stringify({
  // 旧版单面板结构，验证自动迁移
  farm: { enabled: true, baseUrl: farmURL, accessCode: FARM_ACCESS_CODE, pushOnLogin: true, refreshMinutes: 0 },
}, null, 2))
const connectionFile = join(dataDir, 'connection.json')
writeFileSync(connectionFile, JSON.stringify({
  schema: 1, transport: 'native-ipc-v1', framework_api: '3.0.0',
  instance_id: INSTANCE_ID, data_directory: dataDir,
}))

const ADMIN_PORT = 21000 + (process.pid % 20000)
const BASE = `http://127.0.0.1:${ADMIN_PORT}`

const child = spawn(ENTRY, [], {
  cwd: PKG,
  env: {
    ...process.env,
    MENGKA_PLUGIN_CONNECTION_FILE: connectionFile,
    MENGKA_PLUGIN_ADMIN_PORT: String(ADMIN_PORT),
    MENGKA_PLUGIN_ADMIN_HOST: '127.0.0.1',
    FARM_ACCESS_CODE,
  },
  stdio: ['pipe', 'pipe', 'inherit'],
})
child.on('exit', code => console.error(`插件进程退出：code=${code}`))

const send = frame => child.stdin.write(`${JSON.stringify(frame)}\n`)
readline.createInterface({ input: child.stdout }).on('line', line => {
  let msg
  try { msg = JSON.parse(line) } catch { return }
  if (msg.type === 'hello') {
    hello = msg
    send({ type: 'ready', transport: 'native-ipc-v1', api_version: '1', framework_api: '3.0.0', plugin_id: PLUGIN_ID, instance_id: INSTANCE_ID })
    return
  }
  if (msg.type === 'action') {
    actions += 1
    if (msg.action === 'get_bot_list') {
      return send({
        type: 'action_result', id: msg.id, ok: true, data: [
          { self_id: 10001, nickname: 'MockBot', online: true, level: 42, friend_count: 10, group_count: 3, protocol: { id: 0, platform: 'android', version: '8.9.63' } },
          { self_id: 10002, nickname: 'MockPC', online: true, protocol: { id: 1, platform: 'windows' } },
        ],
      })
    }
    if (msg.action === 'get_qq_classic_farm_code') {
      return send({ type: 'action_result', id: msg.id, ok: true, data: { code: forcedCode || `FARM_CODE_${msg.params.self_id}` } })
    }
    return send({ type: 'action_result', id: msg.id, ok: false, error: `未实现的 action：${msg.action}` })
  }
})

await delay(1200)
summarize('hello 握手字段正确', hello?.plugin_id === PLUGIN_ID && hello?.version === VERSION && hello?.api_version === '1',
  hello ? `v${hello.version}` : '未收到 hello')

const health = await fetch(`${BASE}/api/managed/health`).then(r => r.json()).catch(() => null)
summarize('框架闸门 /api/managed/health', health?.active === true, health ? JSON.stringify(health) : '无响应')

const state = await fetch(`${BASE}/api/state`).then(r => r.json()).catch(() => null)
summarize('state 仅返回 Android 账号', Array.isArray(state?.accounts) && state.accounts.length === 1 && state.accounts[0].platform === 'android',
  state ? `accounts=${state.accounts.length}` : '无响应')
summarize('旧版单面板配置自动迁移', state?.config?.panels?.length === 1 && state.config.panels[0].baseUrl === farmURL, '')

await delay(1500)
summarize('启动推送已写入面板', farmAccounts.some(a => String(a.qq) === '10001' && a.code !== 'OLD_CODE'), farmAccounts.find(a => a.qq === '10001')?.code?.slice(0, 24) || '')

send({ type: 'event', data: { post_type: 'bot_online', self_id: 10001, protocol_id: 0 } })
await delay(1500)
const afterOnline = await fetch(`${BASE}/api/state`).then(r => r.json()).catch(() => null)
summarize('bot_online 触发推送并记录动态', afterOnline?.history?.some(h => h.type === 'push' && String(h.message).includes('10001')), '')

const pushAll = await fetch(`${BASE}/api/push_all`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).then(r => r.json()).catch(() => null)
summarize('管理端推送全部', pushAll?.ok === true && Array.isArray(pushAll.results) && pushAll.results.length === 1, `results=${pushAll?.results?.length}`)
summarize('推送结果统计', pushAll?.state?.stats?.todayOk >= 1, JSON.stringify(pushAll?.state?.stats))

const tp = await fetch(`${BASE}/api/test_panel`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"panel":0}' }).then(r => r.json()).catch(() => null)
summarize('面板连通性测试（可达）', tp?.ok === true && tp.result.accounts >= 1, tp?.ok ? `${tp.result.ms}ms` : tp?.error)

const remark = await fetch(`${BASE}/api/remarks`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"self_id":10001,"remark":"主号"}' }).then(r => r.json()).catch(() => null)
summarize('备注保存并落盘', remark?.ok === true && JSON.parse(readFileSync(join(dataDir, 'remarks.json'), 'utf8'))['10001'] === '主号', '')

const beforeSkip = postCount
const skipPush = await fetch(`${BASE}/api/push`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"self_id":10001,"protocol_id":0}' }).then(r => r.json()).catch(() => null)
summarize('Code 未变化时跳过重复推送', skipPush?.ok === true && skipPush.result?.skipped === true && postCount === beforeSkip, `POST ${beforeSkip} → ${postCount}`)

const clamp = await fetch(`${BASE}/api/config`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"farm":{"refreshMinutes":1}}' }).then(r => r.json()).catch(() => null)
summarize('定时刷新下限修正为 5 分钟', clamp?.state?.config?.refreshMinutes === 5, `refreshMinutes=${clamp?.state?.config?.refreshMinutes}`)

const cfg2 = await fetch(`${BASE}/api/config`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ farm: { panels: [
  { label: 'FARM1', baseUrl: farmURL, accessCode: FARM_ACCESS_CODE },
  { label: 'FARM2', baseUrl: 'http://127.0.0.1:59999', accessCode: 'x' },
], activePanel: 0 } }) }).then(r => r.json()).catch(() => null)
summarize('多面板配置落盘', cfg2?.state?.config?.panels?.length === 2 && cfg2.state.panels[0]?.ok === true && cfg2.state.panels[1]?.ok === false, '')

const tpBad = await fetch(`${BASE}/api/test_panel`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"panel":1}' }).then(r => r.json()).catch(() => null)
summarize('不可达面板返回明确错误', tpBad?.ok === false && Boolean(tpBad.error), tpBad?.error)

const dup = await fetch(`${BASE}/api/config`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ farm: { panels: [
  { label: 'A', baseUrl: `${farmURL}/`, accessCode: FARM_ACCESS_CODE },
  { label: 'B', baseUrl: farmURL, accessCode: 'x' },
] } }) }).then(r => r.json()).catch(() => null)
summarize('重复面板地址自动去重', dup?.state?.config?.panels?.length === 1, `panels=${dup?.state?.config?.panels?.length}`)

forcedCode = `FORCE_5XX_${Date.now()}`
failOnce = 1
const retry = await fetch(`${BASE}/api/push`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"self_id":10001,"protocol_id":0}' }).then(r => r.json()).catch(() => null)
summarize('面板 5xx 写请求自动重试', retry?.ok === true && failOnce === 0, retry?.ok ? '重试后成功' : retry?.error)
forcedCode = ''

const repairCfg = await fetch(`${BASE}/api/config`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{"farm":{"autoRepair":true,"repairMinutes":1}}' }).then(r => r.json()).catch(() => null)
summarize('自动巡检配置生效（下限 5 分钟）', repairCfg?.state?.config?.autoRepair === true && repairCfg?.state?.config?.repairMinutes === 5 && repairCfg?.state?.schedule?.nextRepairAt > 0, `repairMinutes=${repairCfg?.state?.config?.repairMinutes}`)

farmAccounts[0].running = false
const repaired = await fetch(`${BASE}/api/repair_now`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).then(r => r.json()).catch(() => null)
summarize('自动巡检：补推已停止的账号', repaired?.ok === true && repaired.results?.some(r => r.ok) && farmAccounts[0].running === true, `results=${repaired?.results?.length}`)

farmAccounts = farmAccounts.filter(a => String(a.qq) !== '10001')
const restored = await fetch(`${BASE}/api/repair_now`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' }).then(r => r.json()).catch(() => null)
summarize('自动巡检：补回被删除的已推送账号', restored?.ok === true && farmAccounts.some(a => String(a.qq) === '10001'), `results=${restored?.results?.length}`)

const hist = await fetch(`${BASE}/api/state?h=2`).then(r => r.json()).catch(() => null)
summarize('历史条数限制', Array.isArray(hist?.history) && hist.history.length <= 2, `history=${hist?.history?.length}`)

const exported = await fetch(`${BASE}/api/config/export`).then(r => r.json()).catch(() => null)
summarize('配置导出不含访问 Code', Array.isArray(exported?.farm?.panels) && exported.farm.panels.length >= 1 && !('accessCode' in exported.farm.panels[0]), '')

const imported = await fetch(`${BASE}/api/config/import`, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ farm: { enabled: true, refreshMinutes: 30, panels: [{ label: 'IMP', baseUrl: farmURL }], activePanel: 0 } }) }).then(r => r.json()).catch(() => null)
summarize('配置导入并按地址保留 Code', imported?.ok === true && imported.state.config.panels.length === 1 && imported.state.config.panels[0].hasCode === true, `panels=${imported?.state?.config?.panels?.length}`)

const page = await fetch(`${BASE}/`).then(r => r.text()).catch(() => '')
summarize('管理端页面可访问', page.includes('<html') && page.includes('drawerMask'), `${page.length} 字节`)

const cleared = await fetch(`${BASE}/api/history_clear`, { method: 'POST' }).then(r => r.json()).catch(() => null)
summarize('清空动态流', cleared?.ok === true && cleared.state.history.length === 0, '')

child.kill('SIGTERM')
farm.close()
await delay(300)

const pass = results.every(Boolean)
console.error(`\n测试结束：${pass ? 'ALL PASS' : 'HAS FAILURES'}（${results.filter(Boolean).length}/${results.length}），IPC 请求 ${actions} 次`)
process.exit(pass ? 0 : 1)
