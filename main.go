package main

import (
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var instanceID string

// getenv 读取环境变量，缺省返回默认值。
func getenv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		println(version)
		return
	}
	conn, dataDir := loadConnection()
	if conn != nil && conn.InstanceID != "" {
		instanceID = conn.InstanceID
	}
	logf("%s v%s 启动，数据目录 %s", pluginName, version, orDash(dataDir))

	ipc := newIPC(os.Stdin, os.Stdout)

	// hello 必须是最先发出的帧；其余初始化全部放到 ready 之后
	if err := ipc.hello(); err != nil {
		logf("hello 发送失败：%v", err)
		os.Exit(1)
	}

	var handleEvent func(map[string]any)
	done := make(chan struct{})
	go func() {
		err := ipc.run(func(event map[string]any) {
			if handleEvent != nil {
				handleEvent(event)
			}
		})
		if err != nil {
			logf("IPC 读取结束：%v", err)
		}
		close(done)
	}()

	select {
	case <-ipc.ready:
	case <-time.After(10 * time.Second):
		logf("IPC 握手超时（未收到 ready）")
		os.Exit(1)
	}
	if ipc.instID != "" {
		instanceID = ipc.instID
	}
	logf("插件已就绪，实例 %s，数据目录 %s", orDash(instanceID), orDash(dataDir))

	cfg := LoadConfig(dataDir)
	bridge := newBridge(cfg, ipc, dataDir)

	handleEvent = func(event map[string]any) {
		switch event["post_type"] {
		case "bot_online":
			bridge.OnBotOnline(event)
		case "bot_offline":
			bridge.OnBotOffline(event)
		}
	}

	admin := newAdmin(cfg, bridge, dataDir)
	admin.start()

	bridge.StartTimers()
	bridge.StartConfigWatcher()

	// 启动推送：与旧版行为一致，仅在启用且开启「上线自动推送」时执行一次
	farm := cfg.Get()
	if farm.Enabled && farm.PushOnLogin {
		go func() {
			results := bridge.PushAllOnline(bridge.activeIndex(bridge.Panels()))
			if len(results) > 0 {
				okCount := 0
				for _, r := range results {
					if r["ok"] == true {
						okCount++
					}
				}
				logf("farm-bot 启动推送完成：%d/%d", okCount, len(results))
			}
		}()
	}

	// 启动 15 秒后做一次 Code 保鲜检查（仅 staleHours>0 时生效）
	time.AfterFunc(15*time.Second, func() {
		bridge.RefreshStaleCodes()
	})

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
		logf("收到退出信号，正在停止")
	case <-done:
		logf("IPC 连接已关闭")
	}
	bridge.StopTimers()
}

func orDash(s string) string {
	if s == "" {
		return "未提供"
	}
	return s
}
