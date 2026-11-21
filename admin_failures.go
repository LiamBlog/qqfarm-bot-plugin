package main

import "strings"

// Expose a concise next step; raw remote errors stay redacted in details.
func classifyPanelFailure(err error, blocked, hasAuth bool) (string, string) {
	if blocked || !hasAuth {
		return "auth", "面板凭据已失效或未填写，请更新凭据后重新检测"
	}
	if err == nil {
		return "", ""
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "unauthorized") || strings.Contains(s, "凭据") || strings.Contains(s, "密码") || strings.Contains(s, "token 失效") {
		return "auth", "认证未通过，请更新凭据后重新检测"
	}
	if strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "超时") {
		return "timeout", "面板响应超时，请检查面板运行状态后重新检测"
	}
	if strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") || strings.Contains(s, "connectex") || strings.Contains(s, "dial tcp") || strings.Contains(s, "network") || strings.Contains(s, "tls") || strings.Contains(s, "连接") {
		return "connection", "无法连接面板，请检查地址、网络与证书后重新检测"
	}
	if strings.Contains(s, "http 5") || strings.Contains(s, "http 429") {
		return "server", "面板服务暂时异常或限流，请稍后重新检测"
	}
	if strings.Contains(s, "json") || strings.Contains(s, "格式") || strings.Contains(s, "http 404") {
		return "response", "面板返回内容不兼容，请核对面板地址与类型"
	}
	return "unknown", "面板检测失败，请查看详情或重新检测"
}
