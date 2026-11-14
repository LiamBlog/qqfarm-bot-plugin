package main

import (
	"os"
	"path/filepath"
	"runtime"
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
