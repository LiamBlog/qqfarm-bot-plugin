package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fake404Panel 模拟 qq-farm-bot-404 的管理接口：用户名密码换 token、账号读写、单独启动。
type fake404Panel struct {
	mu         sync.Mutex
	token      string
	password   string
	mustChange bool
	accounts   []map[string]any
	loginCount int
	attempts   int
	posts      []map[string]any
	starts     []string
}

func (f *fake404Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	writeJSON := func(code int, body any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}
	auth := func() bool {
		if r.Header.Get("x-admin-token") != f.token {
			writeJSON(401, map[string]any{"ok": false, "error": "Unauthorized"})
			return false
		}
		return true
	}
	switch {
	case r.URL.Path == "/api/login":
		f.attempts++
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pass := f.password
		if pass == "" {
			pass = "secret"
		}
		if body.Username != "admin" || body.Password != pass {
			writeJSON(401, map[string]any{"ok": false, "error": "用户名或密码错误"})
			return
		}
		f.loginCount++
		writeJSON(200, map[string]any{"ok": true, "data": map[string]any{
			"token": f.token, "mustChangePassword": f.mustChange,
		}})
	case r.URL.Path == "/api/accounts":
		if !auth() {
			return
		}
		if r.Method == http.MethodGet {
			writeJSON(200, map[string]any{"ok": true, "data": map[string]any{"accounts": f.accounts}})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if name, _ := body["name"].(string); strings.TrimSpace(name) == "" {
			writeJSON(400, map[string]any{"ok": false, "error": "账号备注不能为空"})
			return
		}
		f.posts = append(f.posts, body)
		if id, ok := body["id"].(string); ok {
			for _, acc := range f.accounts {
				if acc["id"] == id {
					acc["code"] = body["code"]
					acc["name"] = body["name"]
					break
				}
			}
		} else {
			uin, _ := body["uin"].(string)
			f.accounts = append(f.accounts, map[string]any{
				"id": fmt.Sprintf("%d", len(f.accounts)+1), "name": body["name"],
				"uin": uin, "qq": uin, "code": body["code"], "platform": "qq", "running": true,
			})
		}
		writeJSON(200, map[string]any{"ok": true, "data": map[string]any{"accounts": f.accounts}})
	case strings.HasPrefix(r.URL.Path, "/api/accounts/") && strings.HasSuffix(r.URL.Path, "/start"):
		if !auth() {
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/start")
		f.starts = append(f.starts, id)
		for _, acc := range f.accounts {
			if acc["id"] == id {
				acc["running"] = true
			}
		}
		writeJSON(200, map[string]any{"ok": true})
	default:
		writeJSON(404, map[string]any{"ok": false, "error": "not found"})
	}
}

// newFakeIPC 让 fetchFarmCode 拿到固定 Code，避免依赖真实框架。
func newFakeIPC(t *testing.T, code string) *IPC {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	t.Cleanup(func() {
		_ = inW.Close()
		_ = outW.Close()
	})
	c := newIPC(inR, outW)
	close(c.ready)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var msg map[string]any
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg["type"] != "action" {
				continue
			}
			id, _ := msg["id"].(string)
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- map[string]any{"type": "action_result", "id": id, "ok": true, "data": map[string]any{"code": code}}
			}
		}
	}()
	return c
}

func newTestBridge(t *testing.T, panel Panel) *Bridge {
	t.Helper()
	cfg := &Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{panel}, ActivePanel: 0}}
	return newBridge(cfg, newFakeIPC(t, "FARM-CODE"), "")
}

func TestLoginAdminPanelAndPush(t *testing.T) {
	fake := &fake404Panel{token: "tok-404"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "secret"})

	res, err := b.PushAccount(Bot{SelfID: 10001, Nickname: "测试号"}, 0)
	if err != nil {
		t.Fatalf("推送失败：%v", err)
	}
	if fake.loginCount != 1 {
		t.Fatalf("应只登录一次，实际 %d", fake.loginCount)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("应写入 1 次账号，实际 %d", len(fake.posts))
	}
	post := fake.posts[0]
	if post["name"] != "测试号" || post["uin"] != "10001" || post["code"] != "FARM-CODE" {
		t.Fatalf("账号参数错误：%v", post)
	}
	if _, ok := post["startAfterSave"]; ok {
		t.Fatal("404 面板不应发送 startAfterSave")
	}
	if res["started"] != true || res["updated"] != false {
		t.Fatalf("新增账号结果错误：%v", res)
	}

	// Code 未变化：第二次推送应跳过，不重复写账号
	res, err = b.PushAccount(Bot{SelfID: 10001, Nickname: "测试号"}, 0)
	if err != nil || res["skipped"] != true || len(fake.posts) != 1 {
		t.Fatalf("Code 未变化应跳过：res=%v err=%v posts=%d", res, err, len(fake.posts))
	}
}

func TestPushRestartsStopped404Account(t *testing.T) {
	fake := &fake404Panel{
		token: "tok-404",
		accounts: []map[string]any{{
			"id": "9", "name": "旧号", "uin": "10002", "qq": "10002",
			"code": "OLD", "platform": "qq", "running": false,
		}},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "secret"})

	res, err := b.PushAccount(Bot{SelfID: 10002, Nickname: "旧号"}, 0)
	if err != nil {
		t.Fatalf("推送失败：%v", err)
	}
	if len(fake.starts) != 1 || fake.starts[0] != "9" {
		t.Fatalf("更新已停止账号后应调用启动，实际 %v", fake.starts)
	}
	if res["started"] != true || res["updated"] != true {
		t.Fatalf("更新结果错误：%v", res)
	}
}

func TestLoginFailureCooldown(t *testing.T) {
	fake := &fake404Panel{token: "tok-404"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "wrong"})

	if _, err := b.login(0); err == nil {
		t.Fatal("错误密码应登录失败")
	}
	if _, err := b.login(0); err == nil {
		t.Fatal("冷却期内应继续失败")
	}
	if fake.loginCount != 0 {
		t.Fatalf("错误密码不应计入成功登录，实际 %d", fake.loginCount)
	}
	if fake.attempts != 1 {
		t.Fatalf("冷却期内不应重复登录，实际请求 %d 次", fake.attempts)
	}
}

func TestAuthFailureBlocksAutoRetry(t *testing.T) {
	fake := &fake404Panel{token: "tok-404", password: "secret"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "wrong"})

	for i := 0; i < 2; i++ {
		if _, err := b.PushAccount(Bot{SelfID: 10004, Nickname: "错误密码"}, 0); err == nil {
			t.Fatal("错误密码应推送失败")
		}
	}
	if fake.attempts != 1 {
		t.Fatalf("认证失败后不应自动重复登录，实际 %d 次", fake.attempts)
	}
	if b.authBlockedMessage(0) == "" {
		t.Fatal("认证失败后应标记暂停自动推送")
	}
}

func TestForceLoginRecoversAfterConfigFix(t *testing.T) {
	fake := &fake404Panel{token: "tok-404", password: "secret"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	panel := Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "wrong"}
	cfg := &Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{panel}, ActivePanel: 0}}
	b := newBridge(cfg, newFakeIPC(t, "FARM-CODE"), "")

	if _, err := b.PushAccount(Bot{SelfID: 10005, Nickname: "修复前"}, 0); err == nil {
		t.Fatal("错误密码应推送失败")
	}
	panel.Password = "secret"
	cfg.Set(FarmConfig{Enabled: true, Panels: []Panel{panel}, ActivePanel: 0})
	if _, err := b.farmFetchForce(0, http.MethodGet, "/api/accounts", nil); err != nil {
		t.Fatalf("测试按钮应强制重登并恢复：%v", err)
	}
	if fake.loginCount != 1 || b.authBlockedMessage(0) != "" {
		t.Fatalf("恢复状态错误：login=%d blocked=%q", fake.loginCount, b.authBlockedMessage(0))
	}
}

func TestMustChangePasswordWarning(t *testing.T) {
	fake := &fake404Panel{token: "tok-404", mustChange: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "secret"})

	if _, err := b.login(0); err != nil {
		t.Fatalf("登录失败：%v", err)
	}
	if b.panelWarning(srv.URL) == "" {
		t.Fatal("默认密码应产生面板警告")
	}
}

func TestBuildAccountPayloadByMode(t *testing.T) {
	legacy := buildAccountPayload(Panel{AccessCode: "code"}, "C", "10001", "昵称", "7")
	if legacy["startAfterSave"] != true || legacy["loginType"] != "manual" || legacy["id"] != "7" {
		t.Fatalf("旧版参数错误：%v", legacy)
	}
	admin := buildAccountPayload(Panel{Username: "admin", Password: "p"}, "C", "10001", "昵称", nil)
	if _, ok := admin["startAfterSave"]; ok {
		t.Fatalf("404 参数不应含 startAfterSave：%v", admin)
	}
	if _, ok := admin["loginType"]; ok {
		t.Fatalf("404 参数不应含 loginType：%v", admin)
	}
	if admin["name"] != "昵称" {
		t.Fatalf("备注名错误：%v", admin)
	}
}

func TestPushKeepsExistingRemark(t *testing.T) {
	fake := &fake404Panel{
		token: "tok-404",
		accounts: []map[string]any{{
			"id": "3", "name": "用户自定义备注", "uin": "10003", "qq": "10003",
			"code": "OLD", "platform": "qq", "running": true,
		}},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "secret"})

	if _, err := b.PushAccount(Bot{SelfID: 10003, Nickname: "框架昵称"}, 0); err != nil {
		t.Fatalf("推送失败：%v", err)
	}
	if got := fake.posts[0]["name"]; got != "用户自定义备注" {
		t.Fatalf("应保留面板内备注，实际 %v", got)
	}
}

func TestPushAvoidsDuplicateNicknameOverwrite(t *testing.T) {
	fake := &fake404Panel{
		token: "tok-404",
		accounts: []map[string]any{{
			"id": "1", "name": "同名", "uin": "20001", "qq": "20001",
			"code": "OTHER", "platform": "qq", "running": true,
		}},
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	b := newTestBridge(t, Panel{Label: "404", BaseURL: srv.URL, Username: "admin", Password: "secret"})

	if _, err := b.PushAccount(Bot{SelfID: 20002, Nickname: "同名"}, 0); err != nil {
		t.Fatalf("推送失败：%v", err)
	}
	if len(fake.posts) != 1 {
		t.Fatalf("应新增 1 次，实际 %d", len(fake.posts))
	}
	post := fake.posts[0]
	if _, hasID := post["id"]; hasID {
		t.Fatalf("同名不同 QQ 不应覆盖已有账号：%v", post)
	}
	if post["name"] != "同名（20002）" {
		t.Fatalf("同名冲突时应生成唯一备注，实际 %v", post["name"])
	}
	if len(fake.accounts) != 2 || fake.accounts[0]["uin"] != "20001" {
		t.Fatalf("原账号不应被覆盖：%v", fake.accounts)
	}
}

func TestSamePanelAuth(t *testing.T) {
	a := []Panel{{AccessCode: "a", Username: "u", Password: "p"}}
	if !samePanelAuth(a, a) {
		t.Fatal("凭据未变应返回 true")
	}
	if samePanelAuth(a, []Panel{{AccessCode: "a", Username: "u", Password: "p2"}}) {
		t.Fatal("密码变化应返回 false")
	}
	if samePanelAuth(a, []Panel{{AccessCode: "a", Username: "u2", Password: "p"}}) {
		t.Fatal("用户名变化应返回 false")
	}
	if samePanelAuth(a, []Panel{{AccessCode: "b", Username: "u", Password: "p"}}) {
		t.Fatal("访问 Code 变化应返回 false")
	}
}

func TestPanelModeHelpers(t *testing.T) {
	legacy := Panel{AccessCode: "a"}
	if legacy.isAdmin() || legacy.mode() != "code" || !legacy.hasAuth() {
		t.Fatalf("旧版面板判定错误：%+v", legacy)
	}
	admin := Panel{Username: "admin", Password: "p"}
	if !admin.isAdmin() || admin.mode() != "admin" || !admin.hasAuth() {
		t.Fatalf("404 面板判定错误：%+v", admin)
	}
	if (Panel{Username: "admin"}).hasAuth() {
		t.Fatal("缺密码时不应视为已配置")
	}
}
