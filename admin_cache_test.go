package main

import (
	"bufio"
	"encoding/json"
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
