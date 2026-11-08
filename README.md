# 农场桥接助手（Go 单二进制版）

萌卡NT 托管 IPC 插件 `qqfarm-bot`，用 Go 重写核心，安装包从 40.6 MB 降到 2.8 MB，不再需要随包分发 Node 运行时。

## 版本记录

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
| `test/ipc-host.mjs` | 模拟框架宿主 + 面板的联调脚本（24 项） |
| `build.sh` | 双架构编译 + 组装清单 + 打包 |
| `site/` | 落地页（纯静态） |
| `docs/images/` | 宣传图 |
| `CHANGELOG.md` / `RELEASE-NOTES-3.x.md` | 版本记录 |

## 构建

```bash
bash build.sh 3.5.0 all
# → dist/qqfarm-bot-ipc-plugin-linux-amd64-3.5.0.tar.gz（2.8 MB）
# → dist/qqfarm-bot-ipc-plugin-linux-arm64-3.5.0.tar.gz（2.6 MB）
# 也可以只构建单架构：bash build.sh 3.5.0 amd64
```

构建要求：Go 1.22+，`CGO_ENABLED=0`（脚本已设置），产物为静态链接 ELF，不依赖容器 glibc。

## 联调

```bash
node test/ipc-host.mjs
```

模拟框架宿主 + qq-farm-bot 面板，覆盖 17 项：hello/ready、管理端闸门、仅 Android 过滤、旧配置迁移、启动推送、`bot_online` 推送、推送全部、连通测试（可达/不可达）、备注落盘、多面板、去重、5xx 重试、历史条数、页面、清空动态。

## 目录

| 文件 | 说明 |
| --- | --- |
| `main.go` | 启动流程：hello 优先 → ready → 配置/桥接/管理端/定时任务 |
| `ipc.go` | native-ipc-v1 帧收发、action 调用与超时 |
| `config.go` | 配置读写、旧版单面板迁移、备注 |
| `bridge.go` | 面板登录、账号 upsert、推送并发、Code 保鲜、动态记录 |
| `admin.go` | 内置管理端 HTTP 接口 + 内嵌页面 |
| `web/index.html` | 管理端页面（`go:embed` 打进二进制） |
| `build.sh` | 编译 + 组装清单 + 打包 |

## 与 Node 版的行为对齐

- 数据文件完全兼容：`config.json`、`remarks.json`、`activity.json`、`farm-tokens.json` 同格式，可原地升级
- 接口一致：`/api/state`、`/api/push`、`/api/push_all`、`/api/test_panel`、`/api/config`、`/api/remarks`、`/api/history_clear`、`/api/managed/health`
- 同样的重试策略：网络异常与 5xx 写请求各重试一次；401 清 token 重登一次
- 同样的推送策略：仅 Android；「推送全部」2 路并发；60 秒自动推送节流；Code 保鲜每 10 分钟检查

## 安装

框架管理端 → 插件 → 导入 `dist/qqfarm-bot-ipc-plugin-linux-amd64-3.0.0.tar.gz`。插件 ID 未变，导入后会替换原实例，数据目录 `/data` 保留。

## 卸载/回滚

需要回到 Node 版时，导入 2.6.x 的安装包即可（同样插件 ID，数据兼容）。
