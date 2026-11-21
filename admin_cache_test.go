package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func cacheTestAdmin(t *testing.T, url string) *Admin {
	t.Helper()
	cfg := &Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{{Label: "测试面板", BaseURL: url, Token: "test-private-token", AuthMode: "token"}}}}
	b := newBridge(cfg, newFakeIPC(t, "CODE"), t.TempDir())
	a := newAdmin(cfg, b, b.dataDir)
	a.bots = []Bot{{SelfID: 10001, Online: true, Platform: "android"}}
	a.botsAt = time.Now()
	return a
}
func waitCache(t *testing.T, a *Admin, source string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a.sourceStatus()[source] == false {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cache worker did not finish", source)
}
func TestAdminSlowPanelDoesNotBlockStateOrOtherReaders(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-gate
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer srv.Close()
	defer release()
	a := cacheTestAdmin(t, srv.URL)
	start := time.Now()
	state := a.state(2)
	elapsed := time.Since(start)
	if elapsed > 400*time.Millisecond {
		t.Fatal("slow panel blocked first state", elapsed)
	}
	t.Logf("cold state with blocked panel: %d ms", elapsed.Milliseconds())
	if len(state["accounts"].([]any)) != 1 {
		t.Fatal("accounts missing while panel is pending")
	}
	panels := state["panels"].([]map[string]any)
	if len(panels) != 1 || panels[0]["checking"] != true {
		t.Fatal("unknown panel should be checking, not failed")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := time.Now()
			a.state(2)
			if time.Since(st) > 400*time.Millisecond {
				t.Error("concurrent reader blocked on cache lock")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("duplicate remote probes", calls.Load())
	}
	release()
	waitCache(t, a, "panelsRefreshing")
	if a.panelsCached()[0]["ok"] != true {
		t.Fatal("completed panel result not published")
	}
}
func TestAdminPanelInvalidationDiscardsInFlightResult(t *testing.T) {
	gate := make(chan struct{})
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-gate
		}
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer srv.Close()
	a := cacheTestAdmin(t, srv.URL)
	a.panelsCached()
	a.invalidatePanels()
	close(gate)
	waitCache(t, a, "panelsRefreshing")
	a.cacheMu.Lock()
	at := a.panelAt
	a.cacheMu.Unlock()
	if !at.IsZero() {
		t.Fatal("invalidated probe overwrote cache")
	}
	a.panelsCached()
	waitCache(t, a, "panelsRefreshing")
	if calls.Load() != 2 || a.panelsCached()[0]["ok"] != true {
		t.Fatal("new probe missing after invalidation")
	}
}
func TestAdminPanelCacheDoesNotReturnRemovedIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer srv.Close()
	a := cacheTestAdmin(t, srv.URL)
	a.panelsCached()
	waitCache(t, a, "panelsRefreshing")
	f := a.cfg.Get()
	f.Panels = []Panel{{Label: "新面板", BaseURL: srv.URL + "/new", Token: "new-private-token", AuthMode: "token"}}
	if err := a.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	for _, p := range a.panelsCached() {
		if p["baseUrl"] != f.Panels[0].BaseURL || p["label"] != "新面板" {
			t.Fatal("old panel identity leaked into new configuration")
		}
	}
	waitCache(t, a, "panelsRefreshing")
}
func TestAdminWarmAccountCacheCoalescesSlowIPC(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inW.Close()
	defer outW.Close()
	defer outR.Close()
	ipc := newIPC(inR, outW)
	close(ipc.ready)
	gate := make(chan struct{})
	var calls atomic.Int64
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			_ = json.Unmarshal(sc.Bytes(), &m)
			calls.Add(1)
			<-gate
			id, _ := m["id"].(string)
			ipc.mu.Lock()
			ch := ipc.pending[id]
			ipc.mu.Unlock()
			if ch != nil {
				ch <- map[string]any{"ok": true, "data": []any{map[string]any{"self_id": 10001, "online": false, "protocol": map[string]any{"id": 0, "platform": "android"}}}}
			}
		}
	}()
	a := cacheTestAdmin(t, "http://unused.test")
	a.bridge.ipc = ipc
	a.botsAt = time.Now().Add(-3 * time.Second)
	for i := 0; i < 6; i++ {
		st := time.Now()
		if len(a.botsCached()) != 1 || time.Since(st) > 300*time.Millisecond {
			t.Fatal("warm accounts blocked instead of showing snapshot")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("IPC refresh was not coalesced")
	}
	close(gate)
	waitCache(t, a, "accountsRefreshing")
	if a.botsCached()[0].Online {
		t.Fatal("refreshed accounts not published")
	}
}
func TestPanelCloneIsolatedWithoutJSONRoundTrip(t *testing.T) {
	in := []map[string]any{{"checkedAt": int64(9007199254740993), "accounts": []any{map[string]any{"uin": "10001"}}}}
	out := clonePanelStatus(in)
	if out[0]["checkedAt"] != int64(9007199254740993) {
		t.Fatal("clone lost integer precision")
	}
	out[0]["accounts"].([]any)[0].(map[string]any)["uin"] = "changed"
	if in[0]["accounts"].([]any)[0].(map[string]any)["uin"] != "10001" {
		t.Fatal("cache snapshot aliases response")
	}
}
func TestRemovedOfficialBotEndpointsAndAssets(t *testing.T) {
	a := cacheTestAdmin(t, "http://unused.test")
	a.token = "secret"
	for _, path := range []string{"/api/qqbot", "/api/qqbot/config", "/qqbot.js"} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("x-mengka-managed-token", a.token)
		w := httptest.NewRecorder()
		a.handler(w, r)
		if w.Code != 404 {
			t.Fatal("removed surface is still accessible", path, w.Code)
		}
	}
	body := string(prepareAdminAssets()["/"].plain)
	if strings.Contains(body, "qqbotDetails") || strings.Contains(body, "QQBotSettings") {
		t.Fatal("removed UI remains in native bundle")
	}
}
func TestAdminFastPanelPublishesBeforeSlowPeer(t *testing.T) {
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	defer release()
	var fastCalls, slowCalls atomic.Int64
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[{"qq":"10001","running":true}]}}`)
	}))
	defer fast.Close()
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		<-gate
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer slow.Close()
	defer release()
	a := cacheTestAdmin(t, fast.URL)
	f := a.cfg.Get()
	f.Panels = append(f.Panels, Panel{Label: "慢面板", BaseURL: slow.URL, Token: "slow-secret", AuthMode: "token"})
	if err := a.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	p := a.panelsCached()
	deadline := time.Now().Add(time.Second)
	for p[0]["ok"] != true && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
		p = a.panelsCached()
	}
	if p[0]["ok"] != true || p[0]["refreshing"] != false || p[1]["checking"] != true || a.sourceStatus()["panelsRefreshing"] != true {
		t.Fatal("fast panel waited for slow peer", p)
	}
	t.Logf("fast panel available while slow peer is blocked: %d ms", time.Since(start).Milliseconds())
	if fastCalls.Load() != 1 || slowCalls.Load() != 1 {
		t.Fatal("probe duplication")
	}
	release()
	waitCache(t, a, "panelsRefreshing")
	if a.panelsCached()[1]["ok"] != true {
		t.Fatal("slow result not published")
	}
}
func TestAdminCompactStateOmitsRetainedDetails(t *testing.T) {
	a := cacheTestAdmin(t, "http://unused.test")
	f := a.cfg.Get()
	f.Enabled = false
	if err := a.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	a.bridge.ops.mu.Lock()
	for i := 0; i < 1000; i++ {
		a.bridge.ops.tasks = append(a.bridge.ops.tasks, &PushTask{ID: fmt.Sprint(i), Status: "failed", Stage: strings.Repeat("历史任务内容", 20), Error: strings.Repeat("旧错误详情", 30)})
	}
	a.bridge.ops.mu.Unlock()
	full := a.state(2)
	compact := a.stateView(2, "accounts")
	rawFull, _ := json.Marshal(full)
	rawCompact, _ := json.Marshal(compact)
	if _, ok := compact["tasks"]; ok {
		t.Fatal("compact state contains full tasks")
	}
	if _, ok := compact["history"]; ok {
		t.Fatal("compact state contains history")
	}
	if _, ok := compact["retries"]; ok {
		t.Fatal("compact state contains retry list")
	}
	if compact["task_summary"].(map[string]any)["failed"] != 1000 {
		t.Fatal("task summary lost failures")
	}
	if len(rawCompact) > len(rawFull)/10 {
		t.Fatal("compact response did not shrink", len(rawFull), len(rawCompact))
	}
	t.Logf("1000-task state bytes: full=%d compact=%d", len(rawFull), len(rawCompact))
	r := httptest.NewRequest("GET", "/api/state?view=invalid", nil)
	w := httptest.NewRecorder()
	a.handler(w, r)
	if w.Code != 400 {
		t.Fatal("unknown view accepted")
	}
	r = httptest.NewRequest("GET", "/api/tasks", nil)
	w = httptest.NewRecorder()
	a.handler(w, r)
	var body struct {
		Tasks []PushTask `json:"tasks"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Tasks) != 1000 {
		t.Fatal("explicit task endpoint lost retained tasks")
	}
}
func TestPanelFailureCategories(t *testing.T) {
	for _, v := range []struct {
		message, kind string
		blocked, auth bool
	}{{"HTTP 401", "auth", false, true}, {"password denied", "auth", true, true}, {"context deadline exceeded", "timeout", false, true}, {"dial tcp: connection refused", "connection", false, true}, {"HTTP 503", "server", false, true}, {"HTTP 404", "response", false, true}, {"json invalid", "response", false, true}, {"unknown", "unknown", false, true}, {"", "auth", false, false}} {
		kind, hint := classifyPanelFailure(errors.New(v.message), v.blocked, v.auth)
		if kind != v.kind || hint == "" {
			t.Errorf("%s: %s %s", v.message, kind, hint)
		}
	}
}
func TestTaskSummaryRevisionChangesForSameCountAndTimestamp(t *testing.T) {
	b := opsTestBridge(t)
	task := &PushTask{ID: "same-time", Status: "queued", UpdatedAt: 123}
	b.ops.tasks = []*PushTask{task}
	before := b.ops.Summary()
	b.ops.mu.Lock()
	task.Stage = "执行进度已更新"
	b.ops.mu.Unlock()
	b.ops.saveLogged()
	after := b.ops.Summary()
	if before["active"] != after["active"] || before["updatedAt"] != after["updatedAt"] || before["revision"] == after["revision"] {
		t.Fatal("same-count progress changes cannot invalidate task details")
	}
}
