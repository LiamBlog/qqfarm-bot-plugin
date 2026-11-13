package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type Admin struct {
	cfg     *Config
	bridge  *Bridge
	dataDir string
	token   string
	origin  string

	cacheMu   sync.Mutex
	botsAt    time.Time
	bots      []Bot
	panelAt   time.Time
	panels    []map[string]any
	pageBytes []byte
}

func newAdmin(cfg *Config, bridge *Bridge, dataDir string) *Admin {
	page, _ := webFS.ReadFile("web/index.html")
	a := &Admin{cfg: cfg, bridge: bridge, dataDir: dataDir, pageBytes: page}
	a.token = readTokenFile()
	a.origin = strings.TrimSpace(getenv("MENGKA_PLUGIN_ADMIN_ORIGIN", ""))
	return a
}

func readTokenFile() string {
	path := strings.TrimSpace(getenv("MENGKA_PLUGIN_ADMIN_TOKEN_FILE", ""))
	if path == "" {
		return ""
	}
	raw, err := readFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func (a *Admin) allowed(r *http.Request) bool {
	if a.token != "" {
		got := r.Header.Get("x-mengka-managed-token")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			return false
		}
	}
	if a.origin != "" {
		if o := r.Header.Get("Origin"); o != "" && o != a.origin {
			return false
		}
	}
	return true
}

func (a *Admin) botsCached() []Bot {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if time.Since(a.botsAt) < 2*time.Second && a.bots != nil {
		return a.bots
	}
	bots, err := a.bridge.botList()
	if err != nil {
		logf("获取账号列表失败：%v", err)
		return a.bots
	}
	a.bots = bots
	a.botsAt = time.Now()
	return bots
}

func (a *Admin) panelsCached() []map[string]any {
	if !a.cfg.Get().Enabled {
		return []map[string]any{}
	}
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	if time.Since(a.panelAt) < 5*time.Second && a.panels != nil {
		return a.panels
	}
	a.panels = a.bridge.PanelStatus()
	a.panelAt = time.Now()
	return a.panels
}

func (a *Admin) invalidatePanels() {
	a.cacheMu.Lock()
	a.panelAt = time.Time{}
	a.cacheMu.Unlock()
}

func (a *Admin) state(historyLimit int) map[string]any {
	farm := a.cfg.Get()
	remarks := loadRemarks(a.dataDir)
	now := time.Now().UnixMilli()

	accounts := []any{}
	for _, bot := range a.botsCached() {
		if !bot.isAndroid() {
			continue
		}
		var codeAge any
		if ts, ok := a.bridge.PushTime(bot.SelfID); ok {
			codeAge = now - ts
		}
		accounts = append(accounts, map[string]any{
			"self_id":          bot.SelfID,
			"protocol_id":      bot.ProtocolID,
			"nickname":         bot.Nickname,
			"remark":           remarks[strconv.FormatInt(bot.SelfID, 10)],
			"online":           bot.Online,
			"platform":         bot.Platform,
			"protocol_version": bot.ProtoVer,
			"level":            bot.Level,
			"friend_count":     bot.FriendCount,
			"group_count":      bot.GroupCount,
			"extra_info":       bot.ExtraInfo,
			"code_age_ms":      codeAge,
		})
	}

	panels := a.panelsCached()

	dayStart := time.Now()
	dayStart = time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), 0, 0, 0, 0, dayStart.Location())
	okCount, failCount := 0, 0
	for _, item := range a.bridge.History(200) {
		if item.Type != "push" || item.OK == nil || item.Time < dayStart.UnixMilli() {
			continue
		}
		if *item.OK {
			okCount++
		} else {
			failCount++
		}
	}

	panelMeta := []map[string]any{}
	for _, p := range farm.Panels {
		panelMeta = append(panelMeta, map[string]any{
			"label": p.Label, "baseUrl": p.BaseURL, "hasCode": p.AccessCode != "",
		})
	}

	history := []Activity{}
	for _, item := range a.bridge.History(historyLimit) {
		history = append(history, item)
	}
	nextRefresh, nextStale, nextRepair := a.bridge.Schedule()

	return map[string]any{
		"version":     version,
		"plugin_id":   pluginID,
		"instance_id": instanceID,
		"ipc_ready":   true,
		"data_path":   a.dataDir,
		"schedule":    map[string]any{"nextRefreshAt": nextRefresh, "nextStaleAt": nextStale, "nextRepairAt": nextRepair},
		"config": map[string]any{
			"enabled":        farm.Enabled,
			"pushOnLogin":    farm.PushOnLogin,
			"refreshMinutes": farm.RefreshMinutes,
			"staleHours":     farm.StaleHours,
			"autoRepair":     farm.AutoRepair,
			"repairMinutes":  farm.RepairMinutes,
			"activePanel":    farm.ActivePanel,
			"panels":         panelMeta,
		},
		"accounts": accounts,
		"panels":   panels,
		"stats":    map[string]any{"todayOk": okCount, "todayFail": failCount},
		"history":  history,
		"now":      now,
	}
}

func (a *Admin) handler(w http.ResponseWriter, r *http.Request) { a.serve(w, r, true) }

// uiHandler 直连管理页：仅本机监听，不下发跨域头，避免任意网页读取本地端口。
func (a *Admin) uiHandler(w http.ResponseWriter, r *http.Request) { a.serve(w, r, false) }

func (a *Admin) serve(w http.ResponseWriter, r *http.Request, allowCORS bool) {
	cors := map[string]string{}
	if allowCORS {
		cors = map[string]string{
			"Access-Control-Allow-Origin":  "*",
			"Access-Control-Allow-Methods": "GET,POST,OPTIONS",
			"Access-Control-Allow-Headers": "Content-Type, x-mengka-managed-token",
		}
		if a.origin != "" {
			cors["Access-Control-Allow-Origin"] = a.origin
		}
	}
	jsonOut := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		for k, v := range cors {
			w.Header().Set(k, v)
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	readBody := func() map[string]any {
		var body map[string]any
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body)
		if body == nil {
			body = map[string]any{}
		}
		return body
	}

	if r.Method == http.MethodOptions {
		for k, v := range cors {
			w.Header().Set(k, v)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	switch r.URL.Path {
	case "/api/managed/health":
		jsonOut(200, map[string]any{"ready": true, "connected": true, "configured": true, "active": true})
		return
	case "/api/managed/control":
		jsonOut(200, map[string]any{"active": true})
		return
	}

	if !a.allowed(r) {
		jsonOut(403, map[string]any{"error": "forbidden"})
		return
	}

	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(a.pageBytes)
		return
	}

	switch {
	case r.URL.Path == "/api/state" && r.Method == http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("h"))
		if limit <= 0 {
			limit = 60
		}
		if limit > 200 {
			limit = 200
		}
		jsonOut(200, a.state(limit))

	case r.URL.Path == "/api/push" && r.Method == http.MethodPost:
		body := readBody()
		selfID := int64(toFloat(body["self_id"]))
		if selfID <= 0 {
			jsonOut(400, map[string]any{"error": "self_id 无效"})
			return
		}
		index := a.cfg.Get().ActivePanel
		if v, ok := body["panel"]; ok {
			index = int(toFloat(v))
		}
		bot := Bot{SelfID: selfID, ProtocolID: int64(toFloat(body["protocol_id"])), Nickname: stringField(body, "nickname")}
		result, err := a.bridge.PushAccount(bot, index)
		if err != nil {
			jsonOut(400, map[string]any{"error": err.Error()})
			return
		}
		a.invalidatePanels()
		jsonOut(200, map[string]any{"ok": true, "result": result, "state": a.state(60)})

	case r.URL.Path == "/api/push_all" && r.Method == http.MethodPost:
		body := readBody()
		index := a.cfg.Get().ActivePanel
		if v, ok := body["panel"]; ok {
			index = int(toFloat(v))
		}
		results := a.bridge.PushAllOnline(index)
		a.invalidatePanels()
		jsonOut(200, map[string]any{"ok": true, "results": results, "state": a.state(60)})

	case r.URL.Path == "/api/broadcast" && r.Method == http.MethodPost:
		body := readBody()
		selfID := int64(toFloat(body["self_id"]))
		if selfID <= 0 {
			jsonOut(400, map[string]any{"error": "self_id 无效"})
			return
		}
		bot := Bot{SelfID: selfID, ProtocolID: int64(toFloat(body["protocol_id"])), Nickname: stringField(body, "nickname")}
		results, err := a.bridge.Broadcast(bot)
		a.invalidatePanels()
		if err != nil {
			jsonOut(400, map[string]any{"error": err.Error(), "results": results})
			return
		}
		jsonOut(200, map[string]any{"ok": true, "results": results, "state": a.state(60)})

	case r.URL.Path == "/api/test_panel" && r.Method == http.MethodPost:
		body := readBody()
		index := a.cfg.Get().ActivePanel
		if v, ok := body["panel"]; ok {
			index = int(toFloat(v))
		}
		result, err := a.bridge.TestPanel(index)
		if err != nil {
			jsonOut(200, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		jsonOut(200, map[string]any{"ok": true, "result": result})

	case r.URL.Path == "/api/remarks" && r.Method == http.MethodPost:
		body := readBody()
		selfID := int64(toFloat(body["self_id"]))
		if selfID <= 0 {
			jsonOut(400, map[string]any{"error": "self_id 无效"})
			return
		}
		if err := saveRemark(a.dataDir, selfID, stringField(body, "remark")); err != nil {
			jsonOut(400, map[string]any{"error": err.Error()})
			return
		}
		jsonOut(200, map[string]any{"ok": true, "state": a.state(60)})

	case r.URL.Path == "/api/repair_now" && r.Method == http.MethodPost:
		results := a.bridge.RepairMissing()
		a.invalidatePanels()
		jsonOut(200, map[string]any{"ok": true, "results": results, "state": a.state(60)})

	case r.URL.Path == "/api/history_clear" && r.Method == http.MethodPost:
		a.bridge.ClearActivity()
		jsonOut(200, map[string]any{"ok": true, "state": a.state(60)})

	case r.URL.Path == "/api/logs" && r.Method == http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 200
		}
		if limit > logBufferSize {
			limit = logBufferSize
		}
		jsonOut(200, map[string]any{"ok": true, "lines": RecentLogs(limit)})

	case r.URL.Path == "/api/config/export" && r.Method == http.MethodGet:
		farm := a.cfg.Get()
		panels := []map[string]any{}
		for _, p := range farm.Panels {
			// 不导出访问 Code
			panels = append(panels, map[string]any{"label": p.Label, "baseUrl": p.BaseURL})
		}
		payload := map[string]any{
			"version": version, "exportedAt": time.Now().UnixMilli(),
			"farm": map[string]any{
				"enabled": farm.Enabled, "pushOnLogin": farm.PushOnLogin,
				"refreshMinutes": farm.RefreshMinutes, "staleHours": farm.StaleHours,
				"autoRepair": farm.AutoRepair, "repairMinutes": farm.RepairMinutes,
				"activePanel": farm.ActivePanel, "panels": panels,
			},
		}
		data, _ := json.MarshalIndent(payload, "", "  ")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=qqfarm-bot-config.json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		_, _ = w.Write(append(data, '\n'))

	case r.URL.Path == "/api/config/import" && r.Method == http.MethodPost:
		body := readBody()
		farm, ok := body["farm"].(map[string]any)
		if !ok {
			jsonOut(400, map[string]any{"error": "缺少 farm 配置"})
			return
		}
		next := a.cfg.Get()
		if v, ok := farm["enabled"].(bool); ok {
			next.Enabled = v
		}
		if v, ok := farm["pushOnLogin"].(bool); ok {
			next.PushOnLogin = v
		}
		if _, ok := farm["refreshMinutes"]; ok {
			next.RefreshMinutes = clampRefresh(int(toFloat(farm["refreshMinutes"])))
		}
		if _, ok := farm["staleHours"]; ok {
			next.StaleHours = clampStale(int(toFloat(farm["staleHours"])))
		}
		if v, ok := farm["autoRepair"].(bool); ok {
			next.AutoRepair = v
		}
		if _, ok := farm["repairMinutes"]; ok {
			next.RepairMinutes = clampRepair(int(toFloat(farm["repairMinutes"])))
		}
		list, _ := farm["panels"].([]any)
		panels := []Panel{}
		for i, item := range list {
			p, _ := item.(map[string]any)
			label := strings.TrimSpace(stringField(p, "label"))
			if label == "" {
				label = fmt.Sprintf("FARM%d", i+1)
			}
			panels = append(panels, Panel{
				Label:      label,
				BaseURL:    strings.TrimRight(strings.TrimSpace(stringField(p, "baseUrl")), "/"),
				AccessCode: strings.TrimSpace(stringField(p, "accessCode")),
			})
		}
		migrated := normalizePanels(panels)
		if len(migrated) == 0 || len(migrated) > 8 {
			jsonOut(400, map[string]any{"error": "面板数量需在 1–8 之间"})
			return
		}
		// 导入未带 Code 时，按地址沿用现有 Code
		for i, p := range migrated {
			if p.AccessCode != "" {
				continue
			}
			for _, old := range next.Panels {
				if old.BaseURL == p.BaseURL {
					migrated[i].AccessCode = old.AccessCode
					break
				}
			}
		}
		next.Panels = migrated
		next.ActivePanel = clampIndex(int(toFloat(farm["activePanel"])), len(migrated))
		a.cfg.Set(next)
		a.bridge.clearTokens()
		a.bridge.StartTimers()
		a.invalidatePanels()
		logf("配置已通过导入更新：%d 个面板", len(migrated))
		jsonOut(200, map[string]any{"ok": true, "state": a.state(60)})

	case r.URL.Path == "/api/config" && r.Method == http.MethodPost:
		body := readBody()
		farm, ok := body["farm"].(map[string]any)
		if !ok {
			jsonOut(400, map[string]any{"error": "缺少 farm 配置"})
			return
		}
		next := a.cfg.Get()
		if v, ok := farm["enabled"].(bool); ok {
			next.Enabled = v
		}
		if v, ok := farm["pushOnLogin"].(bool); ok {
			next.PushOnLogin = v
		}
		if _, ok := farm["refreshMinutes"]; ok {
			next.RefreshMinutes = clampRefresh(int(toFloat(farm["refreshMinutes"])))
		}
		if _, ok := farm["staleHours"]; ok {
			next.StaleHours = clampStale(int(toFloat(farm["staleHours"])))
		}
		if v, ok := farm["autoRepair"].(bool); ok {
			next.AutoRepair = v
		}
		if _, ok := farm["repairMinutes"]; ok {
			next.RepairMinutes = clampRepair(int(toFloat(farm["repairMinutes"])))
		}
		_, hasPanels := farm["panels"]
		_, hasBaseURL := farm["baseUrl"]
		if hasPanels || hasBaseURL {
			candidate := rawFarm{
				Enabled:        boolPtr(next.Enabled),
				PushOnLogin:    boolPtr(next.PushOnLogin),
				RefreshMinutes: intPtr(next.RefreshMinutes),
				StaleHours:     intPtr(next.StaleHours),
			}
			if hasPanels {
				list, _ := farm["panels"].([]any)
				panels := []Panel{}
				for i, item := range list {
					p, _ := item.(map[string]any)
					label := strings.TrimSpace(stringField(p, "label"))
					if label == "" {
						label = fmt.Sprintf("FARM%d", i+1)
					}
					panels = append(panels, Panel{
						Label:      label,
						BaseURL:    strings.TrimRight(strings.TrimSpace(stringField(p, "baseUrl")), "/"),
						AccessCode: strings.TrimSpace(stringField(p, "accessCode")),
					})
				}
				if len(panels) > 8 {
					panels = panels[:8]
				}
				candidate.Panels = panels
			} else {
				candidate.BaseURL = strings.TrimRight(strings.TrimSpace(stringField(farm, "baseUrl")), "/")
				candidate.AccessCode = strings.TrimSpace(stringField(farm, "accessCode"))
			}
			migrated := migrateFarm(candidate)
			if len(migrated.Panels) == 0 {
				jsonOut(400, map[string]any{"error": "至少保留 1 个有效面板"})
				return
			}
			// 编辑时留空的访问 Code 保留原值
			for i, p := range migrated.Panels {
				if p.AccessCode != "" {
					continue
				}
				for _, old := range next.Panels {
					if old.BaseURL == p.BaseURL {
						migrated.Panels[i].AccessCode = old.AccessCode
						break
					}
				}
			}
			next.Panels = migrated.Panels
			a.bridge.clearTokens()
			if _, ok := farm["activePanel"]; ok {
				next.ActivePanel = clampIndex(int(toFloat(farm["activePanel"])), len(next.Panels))
			} else {
				next.ActivePanel = clampIndex(next.ActivePanel, len(next.Panels))
			}
		} else if _, ok := farm["activePanel"]; ok {
			next.ActivePanel = clampIndex(int(toFloat(farm["activePanel"])), len(next.Panels))
		}
		a.cfg.Set(next)
		a.bridge.StartTimers()
		a.invalidatePanels()
		logf("配置已通过管理端更新")
		jsonOut(200, map[string]any{"ok": true, "state": a.state(60)})

	default:
		jsonOut(404, map[string]any{"error": "not found"})
	}
}

func (a *Admin) start() {
	port := strings.TrimSpace(getenv("MENGKA_PLUGIN_ADMIN_PORT", ""))
	host := getenv("MENGKA_PLUGIN_ADMIN_HOST", "127.0.0.1")
	if port == "" || port == "0" {
		logf("未注入管理端端口，跳过管理端启动")
	} else {
		srv := &http.Server{
			Addr:              net.JoinHostPort(host, port),
			Handler:           http.HandlerFunc(a.handler),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logf("框架管理端已监听 %s", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logf("管理端启动失败：%v", err)
			}
		}()
	}
	// 直连管理页端口（框架可选注入）：默认只绑本机，需外部访问时显式设置
	// MENGKA_PLUGIN_UI_INSECURE=1 且自担风险（该端口不校验令牌）。
	uiPort := strings.TrimSpace(getenv("MENGKA_PLUGIN_UI_PORT", ""))
	if uiPort != "" && uiPort != "0" && uiPort != port {
		uiHost := getenv("MENGKA_PLUGIN_UI_HOST", "127.0.0.1")
		if getenv("MENGKA_PLUGIN_UI_INSECURE", "") == "1" {
			uiHost = "0.0.0.0"
			logf("警告：直连管理页绑定 0.0.0.0 且不校验令牌，请确认端口未暴露到公网")
		}
		srv := &http.Server{
			Addr:              net.JoinHostPort(uiHost, uiPort),
			Handler:           http.HandlerFunc(a.uiHandler),
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logf("直连管理页已监听 %s", srv.Addr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logf("直连管理页启动失败：%v", err)
			}
		}()
	}
}

func clampIndex(i, length int) int {
	if length <= 0 {
		return 0
	}
	if i < 0 {
		return 0
	}
	if i >= length {
		return length - 1
	}
	return i
}

func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }
func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
