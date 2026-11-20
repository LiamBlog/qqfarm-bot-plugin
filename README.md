# 农场桥接助手（Go 单二进制版）

[![build](https://github.com/LiamBlog/qqfarm-bot-plugin/actions/workflows/build.yml/badge.svg)](https://github.com/LiamBlog/qqfarm-bot-plugin/actions/workflows/build.yml)

萌卡NT 托管 IPC 插件 `qqfarm-bot`，用 Go 重写核心，安装包从 40.6 MB 降到 2.8 MB，不再需要随包分发 Node 运行时。

## 版本记录

### 3.7.27
- 品牌标识统一：顶栏、侧栏、问候卡的 logo/头像收敛到 `--brand-grad` 单一变量，此前顶栏与侧栏渐变不一致
- 三处品牌标记补内高光描边，与主按钮、卡片既有质感语言对齐

### 3.7.26
- 并发健壮性：`PushAllOnline`、`PanelStatus`、Webhook 通知与启动推送的 goroutine 全部加 `recover`，单账号 panic 不再导致进程退出，异常记账到该账号结果
- 内嵌页面读取失败改为启动即 panic（此前忽略 error，运行期会输出 0 字节空白页）
- 配置导出 `json.MarshalIndent` 失败返回 500（此前会写出空内容）；Webhook 序列化失败记日志

### 3.7.25
- 清理 `focus` 态级联：删除 5 条互相覆盖的 `border-color` 规则，并移除全局覆盖上的 `!important`，改由级联自然生效
- 全站仅保留 `prefers-reduced-motion` 相关 3 处 `!important`

### 3.7.24
- 样式层去重：CSS 由 724 条规则实例合并为 541 条唯一规则（端到端逐属性比对 0 差异），页面文件减少 17%
- 合并采用分段保序策略，54 处级联敏感边界处切分，保证渲染结果与旧版一致
- 代码审计：`go vet` / `go test` / IPC 联调全通过，清理死代码与失效引用，`.gitignore` 补编译残留

### 3.7.23
- 视觉美化：统计卡改低饱和色阶 + 顶部高光 + 对角细环，深色主题单独配色
- 桌面端顶栏面包屑升级为页面标题 + 副标题，侧栏补底部状态概览与选中指示条
- 统一卡片圆角、内边距、边框、阴影与全站动效曲线，优化表格层级与空状态
- 修复 Webhook 输入框 `type=url` 未命中共享样式导致的默认边框与宽度不足

### 3.7.21
- 问候卡只保留今日维度（今日推送 / 今日动态 / 接入面板），不再与统计卡重复
- 卡片视图断点提升到 ≤1023px，平板不再横滚宽表；补卡片视图全选，批量勾选改为单一数据源
- 层级重排：抽屉 60/61、面板表单 70/71、Toast 90，修正表单被抽屉遮挡
- 账号与动态筛选补 `aria-pressed`

### 3.7.20
- 动态流去掉 300px 内层滚动，消除卡片内外双滚动条
- 移动端吸底保存条抬到固定标签栏之上，并补安全区留白
- 清理未使用样式，内联样式由 37 处收敛为 18 处

### 3.7.18
- 修复顶栏管理员胶囊宽高被基础头像规则锁死导致文字溢出

### 3.7.16
- 删除与侧栏重复的页面标签条，导航由三层收敛为两层（侧栏 tab + 面包屑）

### 3.7.15
- 桌面端顶栏改为与内容列同宽的悬浮工具条，管理员改标准圆形头像，窄桌面自动收缩

### 3.7.13
- 布局重构为侧栏 + 面包屑顶栏工作台，新增问候卡与渐变统计卡
- 面板卡片桌面两列、设置页分组两列
- 新增面板连续推送失败告警与 Webhook 通知、失败重试队列展示

### 3.7.8
- 手机端底部 Tab 导航、状态聚合与待处理筛选
- 表头排序与冻结、统计卡筛选、Shift 区间多选
- 面板表单改居中模态、延迟 sparkline、动态相对时间、设置吸底保存提示、排序偏好记忆

### 3.7.7
- 面板认证新增 Token 模式（`x-admin-token` 直接访问，免账号密码）
- 认证方式可选自动 / 404 密码 / 404 Token / 旧版访问码，表单按选择显示字段
- Token 失效时明确提示并暂停自动推送；导入导出兼容 `authMode` / `hasToken`，凭据不随导出泄露

### 3.7.6
- 面板卡片重排：编辑/删除由折叠改为操作栏直出，面板名与模式标签合并标题行
- 面板账号胶囊横向滚动，Code 截断 8 位

### 3.7.5
- 管理端配色升级为「莓果暮光」：重构浅色/深色主题令牌，统一状态色与焦点环，提升辅助文字与主按钮对比度

### 3.7.4
- 适配 qq-farm-bot-404：支持管理员用户名/密码登录、账号写入与停止恢复，同名 QQ 不再互相覆盖
- 认证失败暂停自动重试以避免触发面板锁定；数据文件权限收紧至 `0600`；诊断包移除账号 Code

### 3.7.0
- 广播推送：账号可一键推送到全部已配置面板（账号详情 / 批量操作均可），逐面板返回结果

### 3.6.2
- 构建支持 Windows：`bash build.sh 3.6.2 windows` 产出 `windows-amd64` 包（bin/qqfarm-bot.exe）
- CI 矩阵扩展到三平台，Release 同时附带 linux-amd64 / linux-arm64 / windows-amd64

### 3.6.1
- 修复设置页所有开关（启用推送 / 上线自动推送 / 自动巡检）点击无响应：隐藏的 checkbox 改为透明覆盖，点击轨道即可切换

### 3.6.0
- 推送在途去重、失败三次退避重试、运行日志导出、账号排序切换

### 3.5.0
- 自动巡检补推（默认关闭，间隔 5–1440 分钟）：面板账号停止/缺失自动恢复；面板页支持「立即巡检」
- 双架构构建：`bash build.sh 3.5.0 all` 产出 amd64 / arm64 安装包

### 3.4.0
- 定时任务可视化（下次刷新 / 下次保鲜时间）、配置导入导出（不含 Code）、账号批量推送、快捷键 `/` 与 `r`

### 3.3.0
- 失败自动重试一次、`config.json` 热加载、Go 单元测试（配置/提取/鉴权）

### 3.2.0
- 直连管理页默认仅本机且不下发跨域头；Code 未变化跳过重复推送；定时刷新下限 5 分钟

### 3.1.0
- 修复 `panelState` 并发写竞态（`-race` 构建跑通 17 项联调无告警）、定时任务 stopChans 竞态
- 「推送全部」无目标时返回 `[]` 而非 `null`
- 支持 `MENGKA_PLUGIN_UI_PORT` 直连管理页监听
- 页面：统计卡合并为概览条、中屏（≤820px）切换卡片视图、抽屉内支持单面板重推与备注编辑

### 3.0.0
- 首个 Go 单二进制版本，安装包 2.8 MB，功能与 Node 版对齐

## 目录

| 文件 | 说明 |
| --- | --- |
| `main.go` | 启动流程：hello 优先 → ready → 配置/桥接/管理端/定时任务 |
| `ipc.go` | native-ipc-v1 帧收发、action 调用与超时 |
| `config.go` | 配置读写、旧版单面板迁移、备注、热加载 |
| `bridge.go` | 面板登录、账号 upsert、推送并发、Code 保鲜、自动巡检、动态记录 |
| `admin.go` | 内置管理端 HTTP 接口 + 内嵌页面 |
| `web/index.html` | 管理端页面（`go:embed` 打进二进制） |
| `test/ipc-host.mjs` | 模拟框架宿主 + 面板的联调脚本（32 项） |
| `build.sh` | 三平台编译 + 组装清单 + TAR.GZ 打包 |
| `site/` | 落地页（纯静态） |
| `docs/images/` | 宣传图 |
| `CHANGELOG.md` / `RELEASE-NOTES-3.x.md` | 版本记录 |

## 构建

```bash
bash build.sh                 # 默认版本 3.10.0，一次生成三个平台的 TAR.GZ 插件包
bash build.sh 3.10.0 all       # Linux amd64 + Linux arm64 + Windows amd64
bash build.sh 3.10.0 linux     # 仅 Linux 双架构
bash build.sh 3.10.0 windows   # 仅 Windows amd64（bin/qqfarm-bot.exe）
# 单平台：bash build.sh 3.10.0 amd64
```

构建要求：Go 1.25+、Bash，以及 `tar`、`sha256sum` 等常用命令。`CGO_ENABLED=0`（脚本已设置）；Linux 产物为静态链接 ELF，不依赖容器 glibc；Windows 产物为 PE 可执行文件。

默认在 `dist/` 生成以下三个可导入插件包，每个包另附同名 `.tar.gz.sha256` 校验文件：

| 平台 | 插件包（以 3.10.0 为例） |
| --- | --- |
| Linux amd64 | `qqfarm-bot-ipc-plugin-linux-amd64-3.10.0.tar.gz` |
| Linux arm64 | `qqfarm-bot-ipc-plugin-linux-arm64-3.10.0.tar.gz` |
| Windows amd64 | `qqfarm-bot-ipc-plugin-windows-amd64-3.10.0.tar.gz` |

三个包均将 `mengka-plugin.json` 放在压缩包根目录，入口位于 `bin/`；Linux 入口保留执行权限。Windows 包要求萌卡 NT 3.0.7 或更高版本。GitHub Release 同样直接提供这三个 `.tar.gz` 插件包及校验文件。

## 联调

```bash
node test/ipc-host.mjs
```

模拟框架宿主 + qq-farm-bot 面板，覆盖 32 项：hello/ready、管理端闸门、仅 Android 过滤、旧配置迁移、启动推送、`bot_online` 推送、推送全部、连通测试（可达/不可达）、备注落盘、多面板、去重、5xx 重试、历史条数、页面、清空动态。

## 与 Node 版的行为对齐

- 数据文件完全兼容：`config.json`、`remarks.json`、`activity.json`、`farm-tokens.json` 同格式，可原地升级
- 接口一致：`/api/state`、`/api/push`、`/api/push_all`、`/api/test_panel`、`/api/config`、`/api/remarks`、`/api/history_clear`、`/api/managed/health`
- 同样的重试策略：网络异常与 5xx 写请求各重试一次；401 清 token 重登一次
- 同样的推送策略：仅 Android；「推送全部」2 路并发；60 秒自动推送节流；Code 保鲜每 10 分钟检查

## 安装

框架管理端 → 插件 → 导入 `dist/qqfarm-bot-ipc-plugin-linux-amd64-3.10.0.tar.gz`。插件 ID 未变，导入后会替换原实例，数据目录 `/data` 保留。

## 卸载/回滚

需要回到 Node 版时，导入 2.6.x 的安装包即可（同样插件 ID，数据兼容）。

## 3.8 操作说明

- **账号管理**：在搜索区选择分组；在推送按钮旁明确选择“按账号 / 分组路由”、单一面板或全部面板。确认框会列出账号实例数、对应目标及可能重启会话的提示。
- **账号分组与自动推送**：展开账号列表下方的设置，分配分组、多个面板及自动暂停开关，然后保存。多目标使用原生多选框（桌面按 Ctrl/⌘ 多选）。清除单独目标会继承分组，再继承默认面板。移除面板后不会把已排队任务改投到别处；请同步清理失效路由。
- **面板管理**：使用接入向导新增面板；“更新凭据”打开对应面板，空白敏感字段保持原凭据。检测只读取账号，不执行账号推送。
- **动态**：上部任务中心可筛选进行中、失败/中断或全部任务，下部历史按条件查询与脱敏导出。取消只影响尚未开始的执行；远端已经开始的操作不能撤销。
- **设置**：未保存的设置和路由跨页面切换保留；定时刷新失败时保留上次数据并在全局提示。

持久化文件位于框架提供的数据目录：原有 `config.json`、`activity.json`、`farm-tokens.json` 和 `remarks.json` 保留；新增 `operations.json` 保存路由、每日累计和任务快照。每日统计采用运行环境时区，状态接口返回当天日期与时区。升级前超过旧 120 条上限而丢弃的历史无法恢复；旧记录缺少结构化账号/面板字段时不会出现在相应精确筛选中。

所有页面脚本和样式随 Go 二进制内嵌，不依赖外部 CDN。新接口继续经过既有的框架管理令牌与 Origin 校验。脱敏历史导出隐藏账号中间位、面板名/地址和自由文本详情；普通页面仍按管理员权限显示账号信息。

### 本地回归

```bash
go vet ./...
go test ./...
# 有 C 编译器的环境运行：go test -race ./...
bash build.sh 3.10.0 amd64
node test/ipc-host.mjs
# UI 测试依赖仅用于开发，Linux/CI 使用 Chromium
npm install --no-save playwright@1.62.1
npx playwright install --with-deps chromium
QQFARM_BROWSER_CHANNEL=chromium node test/ui-regression.cjs
```

UI 测试使用本地合成数据，IPC 联调使用模拟框架与面板，不连接真实 QQ 账号。


## 管理端响应优化（3.10.0）

移除 QQ 官方机器人接入及其依赖、设置和接口。慢面板检测移到后台，页面优先返回账号和缓存；同一数据源的并发读取合并为一次。检测中单独标识，隐藏页面暂停自动刷新，面板和诊断区域切换后再构建。
