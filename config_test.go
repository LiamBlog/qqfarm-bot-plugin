package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestNormalizePanels(t *testing.T) {
	out := normalizePanels([]Panel{
		{Label: " A ", BaseURL: "http://a/", AccessCode: " 1 "},
		{Label: "", BaseURL: "http://b"},
		{Label: "dup", BaseURL: "http://a"},
		{Label: "empty", BaseURL: "  "},
	})
	if len(out) != 2 {
		t.Fatalf("期望 2 个面板，实际 %d", len(out))
	}
	if out[0].Label != "A" || out[0].BaseURL != "http://a" || out[0].AccessCode != "1" {
		t.Fatalf("首个面板归一化错误：%+v", out[0])
	}
	if out[1].Label != "http://b" {
		t.Fatalf("空 label 应回退为地址：%+v", out[1])
	}
}

func TestPanelTokenMode(t *testing.T) {
	// 未显式指定认证方式：Token 优先，且仍按 404 面板处理。
	out := normalizePanels([]Panel{{
		Label: "404", BaseURL: "http://a", Token: " tok ", Username: "admin", Password: "pw",
	}})
	p := out[0]
	if p.Token != "tok" || p.mode() != "token" || !p.isToken() || !p.isAdmin() || !p.hasAuth() {
		t.Fatalf("Token 面板识别错误：%+v mode=%s", p, p.mode())
	}

	// 显式 Token 模式：清理用户名密码与访问 Code。
	p = normalizePanels([]Panel{{
		BaseURL: "http://b", AuthMode: "TOKEN", Token: "tok", Username: "admin", Password: "pw", AccessCode: "c",
	}})[0]
	if p.AuthMode != "token" || p.Username != "" || p.Password != "" || p.AccessCode != "" || !p.hasAuth() {
		t.Fatalf("显式 Token 模式应清空无关凭据：%+v", p)
	}

	// 显式密码模式：清掉 Token，避免残留 Token 抢占。
	p = normalizePanels([]Panel{{
		BaseURL: "http://c", AuthMode: "admin", Token: "tok", Username: "admin", Password: "pw",
	}})[0]
	if p.Token != "" || p.mode() != "admin" || !p.hasAuth() {
		t.Fatalf("显式密码模式应清空 Token：%+v", p)
	}

	// Token 模式但未填 Token：视为未配置凭据，便于前端提示。
	p = Panel{BaseURL: "http://d", AuthMode: "token"}
	if p.hasAuth() || !p.isAdmin() {
		t.Fatalf("空 Token 不应视为已配置：%+v", p)
	}
}

func TestMigrateLegacyConfig(t *testing.T) {
	t.Setenv("FARM_ACCESS_CODE", "env-code")
	enabled, push := true, false
	farm := migrateFarm(rawFarm{
		Enabled:     &enabled,
		PushOnLogin: &push,
		BaseURL:     "http://legacy/",
	})
	if len(farm.Panels) != 1 || farm.Panels[0].BaseURL != "http://legacy" || farm.Panels[0].AccessCode != "env-code" {
		t.Fatalf("旧版单面板迁移错误：%+v", farm.Panels)
	}
	if !farm.Enabled || farm.PushOnLogin {
		t.Fatalf("开关迁移错误：%+v", farm)
	}
	if farm.AutoRepair || farm.RepairMinutes != 30 {
		t.Fatalf("自动巡检默认值错误：%+v", farm)
	}
}

func TestClamp(t *testing.T) {
	cases := map[int]int{-1: 0, 0: 0, 1: 5, 5: 5, 30: 30, 99999: 1440}
	for in, want := range cases {
		if got := clampRefresh(in); got != want {
			t.Fatalf("clampRefresh(%d)=%d，期望 %d", in, got, want)
		}
	}
	if clampStale(-5) != 0 || clampStale(999) != 168 || clampStale(12) != 12 {
		t.Fatal("clampStale 边界错误")
	}
	if clampRepair(0) != 30 || clampRepair(1) != 5 || clampRepair(9999) != 1440 || clampRepair(15) != 15 {
		t.Fatal("clampRepair 边界错误")
	}
}

func TestPrivateFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 不适用 POSIX 权限")
	}
	dir := t.TempDir()
	LoadConfig(dir)
	configPath := filepath.Join(dir, "config.json")
	st, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config.json 应为 0600：%v", st.Mode().Perm())
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	LoadConfig(dir)
	if st, err = os.Stat(configPath); err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("旧 config.json 应在加载时收紧：%v", st.Mode().Perm())
	}

	if err := saveRemark(dir, 10001, "备注"); err != nil {
		t.Fatal(err)
	}
	if st, err = os.Stat(filepath.Join(dir, "remarks.json")); err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("remarks.json 应为 0600：%v", st.Mode().Perm())
	}

	tokenPath := filepath.Join(dir, "farm-tokens.json")
	if err := os.WriteFile(tokenPath, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{dataDir: dir, tokens: map[int]string{1: "token"}}
	b.saveTokens()
	if st, err = os.Stat(tokenPath); err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("farm-tokens.json 应为 0600：%v", st.Mode().Perm())
	}
}

func TestReloadIfChanged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	initial := `{"farm":{"enabled":true,"refreshMinutes":30,"panels":[{"label":"A","baseUrl":"http://a","accessCode":"x"}]}}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := LoadConfig(dir)
	if cfg.Get().RefreshMinutes != 30 {
		t.Fatalf("初始配置错误：%+v", cfg.Get())
	}
	if changed, _ := cfg.ReloadIfChanged(); changed {
		t.Fatal("未修改时不应触发热加载")
	}

	next := `{"farm":{"enabled":false,"refreshMinutes":1,"panels":[{"label":"B","baseUrl":"http://b"}]}}`
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if changed, err := cfg.ReloadIfChanged(); err != nil || !changed {
		t.Fatalf("应触发热加载：changed=%v err=%v", changed, err)
	}
	farm := cfg.Get()
	if farm.Enabled || farm.RefreshMinutes != 5 || len(farm.Panels) != 1 || farm.Panels[0].BaseURL != "http://b" {
		t.Fatalf("热加载结果错误：%+v", farm)
	}
}

func TestSaveRemarkConcurrentUpdatesArePreserved(t *testing.T) {
	dir := t.TempDir()
	const count = 64
	start := make(chan struct{})
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- saveRemark(dir, int64(10000+i), fmt.Sprintf("remark-%d", i))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发保存备注失败：%v", err)
		}
	}
	got := loadRemarks(dir)
	if len(got) != count {
		t.Fatalf("并发更新有丢失：得到 %d 条备注，期望 %d 条", len(got), count)
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%d", 10000+i)
		want := fmt.Sprintf("remark-%d", i)
		if got[id] != want {
			t.Errorf("账号 %s 的备注 = %q，期望 %q", id, got[id], want)
		}
	}
}

func TestV314FarmDefaultsAndNormalization(t *testing.T) {
	farm := migrateFarm(rawFarm{Panels: []Panel{{BaseURL: "http://a"}, {BaseURL: "http://b", Priority: 0}}})
	if farm.PanelStrategy != "broadcast" || farm.MaxConcurrent != 2 || farm.CircuitFails != 5 || farm.CircuitCooldownMinutes != 5 {
		t.Fatalf("v3.14 默认值错误：%+v", farm)
	}
	if farm.Panels[0].Priority != 1 || farm.Panels[1].Priority != 2 {
		t.Fatalf("面板默认优先级错误：%+v", farm.Panels)
	}
	farm.PanelStrategy = "unknown"
	farm.MaxConcurrent = 99
	farm.CircuitFails = -1
	farm.CircuitCooldownMinutes = 99999
	farm = normalizeFarm(farm)
	if farm.PanelStrategy != "broadcast" || farm.MaxConcurrent != 8 || farm.CircuitFails != 5 || farm.CircuitCooldownMinutes != 1440 {
		t.Fatalf("v3.14 边界归一化错误：%+v", farm)
	}
}

func TestConfigSetCreatesRotatingBackups(t *testing.T) {
	dir := t.TempDir()
	cfg := LoadConfig(dir)
	first := cfg.Get()
	first.RefreshMinutes = 10
	if err := cfg.Set(first); err != nil {
		t.Fatal(err)
	}
	second := cfg.Get()
	second.RefreshMinutes = 20
	if err := cfg.Set(second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json.bak")); err != nil {
		t.Fatalf("缺少当前配置备份：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json.bak.1")); err != nil {
		t.Fatalf("缺少轮换配置备份：%v", err)
	}
	got, err := readConfigBackup(cfg.Path, "config.json.bak")
	if err != nil || got.RefreshMinutes != 10 {
		t.Fatalf("备份内容错误：%+v %v", got, err)
	}
	if len(listConfigBackups(cfg.Path)) < 2 {
		t.Fatal("配置备份列表不完整")
	}
}

func TestPanelPausePersistsAndLegacyDefaultsEnabled(t *testing.T) {
	dir := t.TempDir()
	cfg := LoadConfig(dir)
	farm := cfg.Get()
	farm.Panels = []Panel{
		{Label: "暂停面板", BaseURL: "http://paused.test", AccessCode: "a", Paused: true},
		{Label: "启用面板", BaseURL: "http://active.test", AccessCode: "b"},
	}
	if err := cfg.Set(farm); err != nil {
		t.Fatal(err)
	}
	reloaded := LoadConfig(dir).Get()
	if len(reloaded.Panels) != 2 || !reloaded.Panels[0].Paused || reloaded.Panels[1].Paused {
		t.Fatalf("面板暂停状态未正确持久化：%+v", reloaded.Panels)
	}
	legacy := migrateFarm(rawFarm{Panels: []Panel{{Label: "旧面板", BaseURL: "http://legacy.test", AccessCode: "c"}}})
	if len(legacy.Panels) != 1 || legacy.Panels[0].Paused {
		t.Fatalf("旧配置面板应默认启用：%+v", legacy.Panels)
	}
}
