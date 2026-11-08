package main

import (
	"net/http"
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
