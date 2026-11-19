package main

import (
	"strings"
	"testing"
)

func TestPickFarmCodeRejectsNonStringCredentials(t *testing.T) {
	for _, value := range []any{true, 12345678, map[string]any{"credential": "private-value"}, []any{"private-value"}} {
		if code, err := pickFarmCode(map[string]any{"code": value}); err == nil {
			t.Errorf("必须拒绝 %T 类型的 Code，实际得到 %q", value, code)
		}
	}
	code, err := pickFarmCode(map[string]any{"code": true, "farm_code": "VALID-CODE"})
	if err != nil || code != "VALID-CODE" {
		t.Fatalf("非字符串字段不能阻止兼容字段：%q %v", code, err)
	}
}

func TestPickFarmCodeErrorDoesNotExposeResponse(t *testing.T) {
	const secret = "private-test-credential-do-not-log"
	_, err := pickFarmCode(map[string]any{"error": secret, "token": secret})
	if err == nil {
		t.Fatal("缺少 Code 应报错")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("取码错误不能包含框架原始返回值或敏感凭据")
	}
}

func TestSamePanelAuthDetectsTokenAndModeChanges(t *testing.T) {
	original := Panel{Token: "old-token", AuthMode: "token", Username: "admin", Password: "password"}
	changedToken := original
	changedToken.Token = "new-token"
	changedMode := original
	changedMode.AuthMode = "admin"
	for name, panel := range map[string]Panel{"token": changedToken, "mode": changedMode} {
		t.Run(name, func(t *testing.T) {
			if samePanelAuth([]Panel{original}, []Panel{panel}) {
				t.Fatal("凭据或认证模式变更必须使会话缓存失效")
			}
		})
	}
}

func BenchmarkPickFarmCode(b *testing.B) {
	response := map[string]any{"code": "TEST-FARM-CODE"}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := pickFarmCode(response); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPickFarmCodeSkipsPlaceholderStrings(t *testing.T) {
	for _, value := range []string{"", "  ", "<nil>", "200"} {
		code, err := pickFarmCode(map[string]any{"code": value, "farm_code": "VALID-CODE"})
		if err != nil || code != "VALID-CODE" {
			t.Errorf("无效字段 %q 应跳过：%q %v", value, code, err)
		}
	}
}
