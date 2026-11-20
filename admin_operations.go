package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// serveOperations is called only after the existing managed-token/origin gate.
func (a *Admin) serveOperations(w http.ResponseWriter, r *http.Request) bool {
	known := map[string]bool{"/api/tasks": true, "/api/tasks/cancel": true, "/api/tasks/retry": true, "/api/routing": true, "/api/history": true, "/api/history/export": true, "/api/setup/probe": true}
	if !known[r.URL.Path] {
		return false
	}
	out := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(err error) { out(400, map[string]any{"error": a.bridge.redact(err.Error())}) }
	if r.Method == http.MethodGet && (r.URL.Path == "/api/history" || r.URL.Path == "/api/history/export") {
		export := r.URL.Path == "/api/history/export"
		if export {
			w.Header().Set("Content-Disposition", "attachment; filename=qqfarm-history-redacted.json")
		}
		out(200, a.bridge.QueryHistory(r.URL.Query(), export))
		return true
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/tasks" {
		out(200, map[string]any{"tasks": a.bridge.ops.Tasks()})
		return true
	}
	if r.Method != http.MethodPost {
		out(405, map[string]any{"error": "method not allowed"})
		return true
	}
	var body struct {
		IDs       []string `json:"ids"`
		PanelURLs []string `json:"panel_urls"`
		Group     string   `json:"group"`
		All       bool     `json:"all"`
		ID        string   `json:"id"`
		Routing   *Routing `json:"routing"`
		Panel     Panel    `json:"panel"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		fail(fmt.Errorf("请求内容无效"))
		return true
	}
	switch r.URL.Path {
	case "/api/tasks":
		bots, err := a.bridge.botList()
		if err != nil {
			fail(err)
			return true
		}
		wanted := map[string]bool{}
		for _, id := range body.IDs {
			wanted[id] = true
		}
		if len(wanted) == 0 && !body.All && body.Group == "" {
			fail(fmt.Errorf("请选择账号或分组"))
			return true
		}
		ps := a.bridge.Panels()
		valid := map[string]bool{}
		for _, p := range ps {
			valid[p.BaseURL] = true
		}
		for _, base := range body.PanelURLs {
			if !valid[base] {
				fail(fmt.Errorf("目标面板已移除"))
				return true
			}
		}
		routing := a.bridge.ops.Routing()
		if body.Group != "" {
			if _, ok := routing.Groups[body.Group]; !ok {
				fail(fmt.Errorf("分组不存在"))
				return true
			}
		}
		selected := []Bot{}
		for _, bot := range bots {
			if !bot.Online || !bot.isAndroid() {
				continue
			}
			key := accountKey(bot.SelfID, bot.ProtocolID)
			if body.Group != "" && routing.Accounts[key].Group != body.Group {
				continue
			}
			if !body.All && body.Group == "" && !wanted[key] {
				continue
			}
			selected = append(selected, bot)
		}
		if len(selected) == 0 {
			fail(fmt.Errorf("没有可推送的在线 Android 账号"))
			return true
		}
		ids := []string{}
		for _, bot := range selected {
			targets := body.PanelURLs
			if len(targets) == 0 {
				targets = a.bridge.ops.Targets(bot, false)
			}
			for _, base := range targets {
				id, err := a.bridge.ops.Enqueue(bot, base, "manual", 0)
				if err != nil {
					fail(fmt.Errorf("已排队 %d 项；%w", len(ids), err))
					return true
				}
				ids = append(ids, id)
			}
		}
		out(200, map[string]any{"ok": true, "ids": ids, "tasks": a.bridge.ops.Tasks()})
	case "/api/tasks/cancel":
		if err := a.bridge.ops.Cancel(body.ID); err != nil {
			fail(err)
		} else {
			out(200, map[string]any{"ok": true})
		}
	case "/api/tasks/retry":
		id, err := a.bridge.ops.Retry(body.ID)
		if err != nil {
			fail(err)
		} else {
			out(200, map[string]any{"ok": true, "id": id})
		}
	case "/api/routing":
		if body.Routing == nil {
			fail(fmt.Errorf("缺少路由策略"))
		} else if err := a.bridge.ops.SetRouting(*body.Routing); err != nil {
			fail(err)
		} else {
			out(200, map[string]any{"ok": true, "routing": a.bridge.ops.Routing()})
		}
	case "/api/setup/probe":
		p := body.Panel
		normalized := normalizePanels([]Panel{p})
		if len(normalized) != 1 {
			fail(fmt.Errorf("请填写面板地址"))
			return true
		}
		p = normalized[0]
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			fail(fmt.Errorf("请输入不含凭据或查询参数的 HTTP/HTTPS 面板地址"))
			return true
		}
		for _, old := range a.bridge.Panels() {
			if old.BaseURL == p.BaseURL && old.mode() == p.mode() {
				switch p.mode() {
				case "token":
					if p.Token == "" {
						p.Token = old.Token
					}
				case "admin":
					if p.Password == "" && p.Username == old.Username {
						p.Password = old.Password
					}
				case "code":
					if p.AccessCode == "" {
						p.AccessCode = old.AccessCode
					}
				}
			}
		}
		if !p.hasAuth() {
			fail(fmt.Errorf("请填写所选认证方式的凭据"))
			return true
		}
		temp := newBridge(&Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{p}}}, a.bridge.ipc, "")
		result, err := temp.TestPanel(0)
		if err != nil {
			fail(err)
		} else {
			out(200, map[string]any{"ok": true, "result": result, "checked_at": time.Now().UnixMilli()})
		}
	default:
		out(405, map[string]any{"error": "method not allowed"})
	}
	return true
}

func clonePanelValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, v := range x {
			out[k] = clonePanelValue(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = clonePanelValue(v)
		}
		return out
	case []map[string]any:
		return clonePanelStatus(x)
	default:
		return v
	}
}
func clonePanelStatus(in []map[string]any) []map[string]any {
	out := make([]map[string]any, len(in))
	for i, p := range in {
		out[i] = clonePanelValue(p).(map[string]any)
	}
	return out
}
