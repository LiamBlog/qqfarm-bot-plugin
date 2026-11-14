package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminAllowed(t *testing.T) {
	a := &Admin{token: "secret", origin: "https://mk.example"}
	req := func(token, origin string) *http.Request {
		r, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/state", nil)
		if token != "" {
			r.Header.Set("x-mengka-managed-token", token)
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return r
	}
	if !a.allowed(req("secret", "https://mk.example")) {
		t.Fatal("正确令牌与来源应放行")
	}
	if a.allowed(req("wrong", "https://mk.example")) {
		t.Fatal("错误令牌应拒绝")
	}
	if a.allowed(req("secret", "https://evil.example")) {
		t.Fatal("非法来源应拒绝")
	}

	open := &Admin{}
	if !open.allowed(req("", "")) {
		t.Fatal("未配置令牌时应放行（仅本地端口）")
	}
}

func TestClampIndex(t *testing.T) {
	if clampIndex(-1, 3) != 0 || clampIndex(5, 3) != 2 || clampIndex(1, 0) != 0 || clampIndex(1, 3) != 1 {
		t.Fatal("clampIndex 边界错误")
	}
}

func TestConfigKeepsPanelPasswordWhenBlank(t *testing.T) {
	t.Setenv("MENGKA_PLUGIN_ADMIN_TOKEN_FILE", "")
	t.Setenv("MENGKA_PLUGIN_ADMIN_ORIGIN", "")
	dir := t.TempDir()
	cfg := LoadConfig(dir)
	cfg.Set(FarmConfig{Enabled: false, Panels: []Panel{{
		Label: "404", BaseURL: "http://panel", Username: "admin", Password: "secret",
	}}})
	bridge := newBridge(cfg, newFakeIPC(t, "X"), dir)
	a := newAdmin(cfg, bridge, dir)

	body := `{"farm":{"enabled":false,"panels":[{"label":"404","baseUrl":"http://panel","username":"admin"}]}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	a.handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存配置失败：%d %s", rec.Code, rec.Body.String())
	}
	p := cfg.Get().Panels[0]
	if p.Username != "admin" || p.Password != "secret" {
		t.Fatalf("留空密码应保留原值：%+v", p)
	}
	if p.mode() != "admin" || !p.hasAuth() {
		t.Fatalf("404 面板模式错误：%+v", p)
	}

	// 用户名变化且密码留空：不应沿用旧密码
	body = `{"farm":{"enabled":false,"panels":[{"label":"404","baseUrl":"http://panel","username":"root"}]}}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	a.handler(rec, req)
	p = cfg.Get().Panels[0]
	if p.Username != "root" || p.Password != "" {
		t.Fatalf("更换用户名后不应保留旧密码：%+v", p)
	}
}
