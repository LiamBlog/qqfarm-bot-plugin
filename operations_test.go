package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func opsTestBridge(t *testing.T) *Bridge {
	t.Helper()
	return newBridge(&Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{{Label: "A", BaseURL: "http://a.test", Token: "test-secret-abcdef", AuthMode: "token"}, {Label: "B", BaseURL: "http://b.test", Token: "other-secret-abcdef", AuthMode: "token"}}}}, newIPC(strings.NewReader(""), io.Discard), t.TempDir())
}
func awaitTask(t *testing.T, o *Operations, id, status string) PushTask {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range o.Tasks() {
			if v.ID == id && v.Status == status {
				return v
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s never reached %s: %+v", id, status, o.Tasks())
	return PushTask{}
}
func TestOperationsDailyCountersSurviveHistoryClearAndRestart(t *testing.T) {
	b := opsTestBridge(t)
	bot := Bot{SelfID: 10001, ProtocolID: 0}
	b.ops.Record(bot, 0, nil, nil)
	b.ops.Record(bot, 0, nil, errors.New("rejected"))
	b.ops.Record(bot, 0, map[string]any{"skipped": true}, nil)
	b.ops.Record(bot, 0, map[string]any{"skipped": true, "started": true}, nil)
	b.ClearActivity()
	for i := 0; i < 250; i++ {
		b.LogActivity("online", "event", "", nil)
	}
	d := b.ops.Stats()
	if d["todayOk"] != 2 || d["todayFail"] != 1 || d["todaySkipped"] != 1 {
		t.Fatalf("wrong totals: %v", d)
	}
	if len(b.History(1000)) != 250 {
		t.Fatal("history still truncated to 120")
	}
	b2 := newBridge(b.cfg, b.ipc, b.dataDir)
	if b2.ops.Stats()["todayOk"] != 2 {
		t.Fatal("daily counters lost on restart")
	}
}
func TestOperationsRoutingPriorityPauseAndProtocolZero(t *testing.T) {
	b := opsTestBridge(t)
	bot := Bot{SelfID: 10001, ProtocolID: 0}
	r := Routing{Groups: map[string][]string{"daily": {"http://b.test"}}, Accounts: map[string]AccountPolicy{"10001:0": {Group: "daily"}}}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	if got := b.ops.Targets(bot, true); len(got) != 1 || got[0] != "http://b.test" {
		t.Fatal(got)
	}
	p := r.Accounts["10001:0"]
	p.Targets = []string{"http://a.test", "http://b.test"}
	p.Paused = true
	r.Accounts["10001:0"] = p
	_ = b.ops.SetRouting(r)
	if len(b.ops.Targets(bot, true)) != 0 || len(b.ops.Targets(bot, false)) != 2 {
		t.Fatal("pause must affect automatic paths only")
	}
	p.Targets = []string{"http://removed.test"}
	r.Accounts["10001:0"] = p
	if b.ops.SetRouting(r) == nil {
		t.Fatal("removed target accepted")
	}
	if !b.ops.Routing().Accounts["10001:0"].Paused {
		t.Fatal("rejected routing changed saved data")
	}
}
func TestOperationsCancelWaitingTaskAndDeduplicate(t *testing.T) {
	b := opsTestBridge(t)
	id, err := b.ops.Enqueue(Bot{SelfID: 10001}, "http://a.test", "manual", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	awaitTask(t, b.ops, id, "retry_wait")
	id2, _ := b.ops.Enqueue(Bot{SelfID: 10001}, "http://a.test", "manual", time.Hour)
	if id != id2 {
		t.Fatal("duplicate task accepted")
	}
	if err = b.ops.Cancel(id); err != nil {
		t.Fatal(err)
	}
	awaitTask(t, b.ops, id, "cancelled")
	time.Sleep(15 * time.Millisecond)
	if b.ops.Tasks()[0].Status != "cancelled" {
		t.Fatal("cancellation overwritten by retry timer")
	}
	if _, err = b.ops.Retry(id); err == nil {
		t.Fatal("cancelled task should not be included in failed-only retry")
	}
}
func TestOperationsRemovedTargetNeverFallsBack(t *testing.T) {
	b := opsTestBridge(t)
	b.ops.sem <- struct{}{}
	b.ops.sem <- struct{}{}
	id, err := b.ops.Enqueue(Bot{SelfID: 10001}, "http://a.test", "manual", 0)
	if err != nil {
		t.Fatal(err)
	}
	farm := b.cfg.Get()
	farm.Panels = farm.Panels[1:]
	if err = b.cfg.Set(farm); err != nil {
		t.Fatal(err)
	}
	<-b.ops.sem
	<-b.ops.sem
	v := awaitTask(t, b.ops, id, "failed")
	if !strings.Contains(v.Stage, "移除") || v.PanelURL != "http://a.test" {
		t.Fatalf("task retargeted: %+v", v)
	}
}
func TestOperationsRestartDoesNotReplayRunningTask(t *testing.T) {
	b := opsTestBridge(t)
	raw := `{"tasks":[{"id":"old","status":"running","self_id":10001,"panel_url":"http://a.test"}]}`
	if err := os.WriteFile(filepath.Join(b.dataDir, "operations.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	o := newOperations(b)
	v := o.Tasks()
	if len(v) != 1 || v[0].Status != "interrupted" {
		t.Fatalf("unsafe restart: %+v", v)
	}
}
func TestHistoryFiltersRetentionAndRedactedExport(t *testing.T) {
	b := opsTestBridge(t)
	now := time.Now()
	b.appendActivity(Activity{Time: now.AddDate(0, 0, -91).UnixMilli(), Message: "expired"})
	b.appendActivity(Activity{Time: now.UnixMilli(), Type: "attempt", SelfID: "12345678", ProtocolID: 0, Panel: "私人面板", PanelURL: "http://a.test", Outcome: "failed", Message: "token=test-secret-abcdef", Detail: "password=secret-text"})
	if len(b.History(100)) != 1 {
		t.Fatal("expired history retained")
	}
	q := url.Values{"account": {"1234"}, "panel": {"http://a.test"}, "outcome": {"failed"}, "from": {now.Format("2006-01-02")}, "to": {now.Format("2006-01-02")}}
	result := b.QueryHistory(q, false)
	if result["total"] != 1 {
		t.Fatal(result)
	}
	data, _ := json.Marshal(b.QueryHistory(q, true))
	for _, s := range []string{"12345678", "a.test", "secret-text", "私人面板", "test-secret"} {
		if strings.Contains(string(data), s) {
			t.Fatalf("export leaked %q: %s", s, data)
		}
	}
	data, _ = json.Marshal(b.History(100))
	if strings.Contains(string(data), "test-secret") || strings.Contains(string(data), "secret-text") {
		t.Fatal("history itself retained credentials")
	}
}
func TestOperationsConcurrentStatsAndSnapshots(t *testing.T) {
	b := opsTestBridge(t)
	b.dataDir = ""
	b.ops.path = ""
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.ops.Record(Bot{SelfID: 10001}, 0, nil, nil)
			_ = b.ops.Tasks()
			_ = b.ops.Routing()
		}()
	}
	wg.Wait()
	if b.ops.Stats()["todayOk"] != 30 {
		t.Fatal("lost counters")
	}
}
func TestNewAdminEndpointsRequireManagedAuthAndRejectInvalidInput(t *testing.T) {
	b := opsTestBridge(t)
	a := newAdmin(b.cfg, b, b.dataDir)
	a.token = "managed-test-token"
	for _, path := range []string{"/enhancements.js", "/api/tasks", "/api/history/export", "/api/routing", "/api/setup/probe"} {
		w := httptest.NewRecorder()
		a.handler(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 403 {
			t.Errorf("unprotected %s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/api/routing", strings.NewReader(`{"routing":{"groups":{"x":["http://removed.test"]}}}`))
	r.Header.Set("x-mengka-managed-token", a.token)
	w := httptest.NewRecorder()
	a.handler(w, r)
	if w.Code != 400 {
		t.Fatalf("invalid routing accepted: %d", w.Code)
	}
	r = httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(`{invalid`))
	r.Header.Set("x-mengka-managed-token", a.token)
	w = httptest.NewRecorder()
	a.handler(w, r)
	if w.Code != 400 {
		t.Fatal("invalid JSON accepted")
	}
}
func TestConfigFailedSaveKeepsPreviousBaseline(t *testing.T) {
	b := opsTestBridge(t)
	b.cfg.Path = filepath.Join(b.dataDir, "missing", "config.json")
	next := b.cfg.Get()
	next.RefreshMinutes = 45
	if b.cfg.Set(next) == nil {
		t.Fatal("failed save reported success")
	}
	if b.cfg.Get().RefreshMinutes == 45 {
		t.Fatal("failed save changed server baseline")
	}
}

type farmCodeWriter struct{ host *io.PipeWriter }

func (w farmCodeWriter) Write(p []byte) (int, error) {
	var frame map[string]any
	if err := json.Unmarshal(p, &frame); err != nil {
		return 0, err
	}
	reply := map[string]any{"type": "action_result", "id": frame["id"], "ok": true, "data": map[string]any{"code": "SAFE-MOCK-FARM-CODE"}}
	raw, _ := json.Marshal(reply)
	go func() { _, _ = w.host.Write(append(raw, '\n')) }()
	return len(p), nil
}
func liveTestIPC(t *testing.T) *IPC {
	t.Helper()
	in, host := io.Pipe()
	c := newIPC(in, farmCodeWriter{host})
	close(c.ready)
	go func() { _ = c.run(nil) }()
	t.Cleanup(func() { host.Close(); in.Close() })
	return c
}
func TestTaskResolvesStableURLAfterPanelReorder(t *testing.T) {
	var aPosts, bPosts atomic.Int32
	server := func(posts *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				posts.Add(1)
			}
			_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
		}))
	}
	a := server(&aPosts)
	defer a.Close()
	other := server(&bPosts)
	defer other.Close()
	b := newBridge(&Config{Farm: FarmConfig{Enabled: true, Panels: []Panel{{Label: "A", BaseURL: a.URL, Token: "a", AuthMode: "token"}, {Label: "B", BaseURL: other.URL, Token: "b", AuthMode: "token"}}}}, liveTestIPC(t), t.TempDir())
	b.ops.sem <- struct{}{}
	b.ops.sem <- struct{}{}
	id, err := b.ops.Enqueue(Bot{SelfID: 10001, ProtocolID: 0}, a.URL, "manual", 0)
	if err != nil {
		t.Fatal(err)
	}
	f := b.cfg.Get()
	f.Panels[0], f.Panels[1] = f.Panels[1], f.Panels[0]
	_ = b.cfg.Set(f)
	<-b.ops.sem
	<-b.ops.sem
	awaitTask(t, b.ops, id, "succeeded")
	if aPosts.Load() != 1 || bPosts.Load() != 0 {
		t.Fatalf("wrong target A=%d B=%d", aPosts.Load(), bPosts.Load())
	}
}
func TestRunningTaskCannotBeCancelled(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			select {
			case <-entered:
			default:
				close(entered)
			}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer s.Close()
	b := opsTestBridge(t)
	b.ipc = liveTestIPC(t)
	f := b.cfg.Get()
	f.Panels[0].BaseURL = s.URL
	_ = b.cfg.Set(f)
	id, err := b.ops.Enqueue(Bot{SelfID: 10001}, s.URL, "manual", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("task did not start")
	}
	if b.ops.Cancel(id) == nil {
		close(release)
		t.Fatal("running task cancellation accepted")
	}
	close(release)
	awaitTask(t, b.ops, id, "succeeded")
}
func TestAutomaticTaskHonorsPauseBeforeExecution(t *testing.T) {
	b := opsTestBridge(t)
	b.ops.sem <- struct{}{}
	b.ops.sem <- struct{}{}
	id, _ := b.ops.Enqueue(Bot{SelfID: 10001}, "http://a.test", "automatic", 0)
	r := b.ops.Routing()
	r.Accounts["10001:0"] = AccountPolicy{Paused: true}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	<-b.ops.sem
	<-b.ops.sem
	awaitTask(t, b.ops, id, "cancelled")
	if b.ops.Stats()["todayAttempts"] != 0 {
		t.Fatal("paused automatic task executed")
	}
}
func TestAutomaticTaskHonorsPanelPauseBeforeExecution(t *testing.T) {
	b := opsTestBridge(t)
	b.ops.sem <- struct{}{}
	b.ops.sem <- struct{}{}
	id, err := b.ops.Enqueue(Bot{SelfID: 10001, ProtocolID: 0}, "http://a.test", "automatic", 0)
	if err != nil {
		t.Fatal(err)
	}
	farm := b.cfg.Get()
	farm.Panels[0].Paused = true
	if err := b.cfg.Set(farm); err != nil {
		t.Fatal(err)
	}
	<-b.ops.sem
	<-b.ops.sem
	awaitTask(t, b.ops, id, "cancelled")
	if b.ops.Stats()["todayAttempts"] != 0 {
		t.Fatal("panel-paused automatic task executed")
	}
}
func TestUncertainWriteStopsWithoutAutomaticReplay(t *testing.T) {
	var posts atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"data":{"accounts":[]}}`)
	}))
	defer s.Close()
	b := opsTestBridge(t)
	b.ipc = liveTestIPC(t)
	f := b.cfg.Get()
	f.Panels[0].BaseURL = s.URL
	_ = b.cfg.Set(f)
	id, _ := b.ops.Enqueue(Bot{SelfID: 10001}, s.URL, "manual", 0)
	v := awaitTask(t, b.ops, id, "interrupted")
	if posts.Load() != 1 || v.NextAt != 0 {
		t.Fatalf("uncertain operation replayed: %d %+v", posts.Load(), v)
	}
}

func TestSuccessfulPushSupersedesOnlyWaitingRetries(t *testing.T) {
	b := opsTestBridge(t)
	id, _ := b.ops.Enqueue(Bot{SelfID: 10001}, "http://a.test", "manual", time.Hour)
	awaitTask(t, b.ops, id, "retry_wait")
	b.ops.completeWaiting(Bot{SelfID: 10001}, 0)
	v := awaitTask(t, b.ops, id, "cancelled")
	if !strings.Contains(v.Stage, "其他操作") {
		t.Fatal(v.Stage)
	}
}

func TestAutomaticPanelStrategySkipsOpenPrimary(t *testing.T) {
	b := opsTestBridge(t)
	f := b.cfg.Get()
	f.PanelStrategy = "primary_backup"
	f.Panels = []Panel{{Label: "主面板", BaseURL: "http://primary.test", Priority: 1, Token: "p", AuthMode: "token"}, {Label: "备用面板", BaseURL: "http://backup.test", Priority: 2, Token: "b", AuthMode: "token"}}
	if err := b.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	r := b.ops.Routing()
	r.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://primary.test", "http://backup.test"}}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	b.circuitMu.Lock()
	if b.circuits == nil {
		b.circuits = map[string]panelCircuit{}
	}
	b.circuits["http://primary.test"] = panelCircuit{Failures: 5, OpenedUntil: time.Now().Add(time.Hour).UnixMilli()}
	b.circuitMu.Unlock()
	got := b.ops.Targets(Bot{SelfID: 10001, ProtocolID: 0}, true)
	if len(got) != 1 || got[0] != "http://backup.test" {
		t.Fatalf("应选择备用面板，实际=%v", got)
	}
	r.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://primary.test"}}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	if got := b.ops.Targets(Bot{SelfID: 10001, ProtocolID: 0}, true); len(got) != 0 {
		t.Fatalf("唯一目标熔断时应跳过，实际=%v", got)
	}
}

func TestBroadcastStrategyKeepsAllTargets(t *testing.T) {
	b := opsTestBridge(t)
	f := b.cfg.Get()
	f.PanelStrategy = "broadcast"
	f.Panels = []Panel{{Label: "A", BaseURL: "http://a.test", Priority: 1, Token: "a", AuthMode: "token"}, {Label: "B", BaseURL: "http://b.test", Priority: 2, Token: "b", AuthMode: "token"}}
	if err := b.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	r := b.ops.Routing()
	r.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://a.test", "http://b.test"}}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	got := b.ops.Targets(Bot{SelfID: 10001, ProtocolID: 0}, true)
	if len(got) != 2 || got[0] != "http://a.test" || got[1] != "http://b.test" {
		t.Fatalf("广播策略不应筛选目标：%v", got)
	}
}

func TestFirstAvailableKeepsRoutingOrder(t *testing.T) {
	b := opsTestBridge(t)
	f := b.cfg.Get()
	f.PanelStrategy = "first_available"
	f.Panels = []Panel{{Label: "低优先级", BaseURL: "http://low.test", Priority: 9, Token: "l", AuthMode: "token"}, {Label: "高优先级", BaseURL: "http://high.test", Priority: 1, Token: "h", AuthMode: "token"}}
	if err := b.cfg.Set(f); err != nil {
		t.Fatal(err)
	}
	r := b.ops.Routing()
	r.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://low.test", "http://high.test"}}
	if err := b.ops.SetRouting(r); err != nil {
		t.Fatal(err)
	}
	got := b.ops.Targets(Bot{SelfID: 10001, ProtocolID: 0}, true)
	if len(got) != 1 || got[0] != "http://low.test" {
		t.Fatalf("首个可用应保留路由顺序，实际=%v", got)
	}
}

func TestPanelPauseFiltersAutomaticPushOnly(t *testing.T) {
	for _, strategy := range []string{"broadcast", "primary_backup", "first_available"} {
		t.Run(strategy, func(t *testing.T) {
			b := opsTestBridge(t)
			farm := b.cfg.Get()
			farm.PanelStrategy = strategy
			farm.Panels = []Panel{
				{Label: "A", BaseURL: "http://a.test", Priority: 1, Token: "a", AuthMode: "token", Paused: true},
				{Label: "B", BaseURL: "http://b.test", Priority: 2, Token: "b", AuthMode: "token"},
			}
			if err := b.cfg.Set(farm); err != nil {
				t.Fatal(err)
			}
			routing := b.ops.Routing()
			routing.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://a.test", "http://b.test"}}
			if err := b.ops.SetRouting(routing); err != nil {
				t.Fatal(err)
			}
			bot := Bot{SelfID: 10001, ProtocolID: 0}
			if got := b.ops.Targets(bot, true); len(got) != 1 || got[0] != "http://b.test" {
				t.Fatalf("自动推送应跳过暂停面板，实际=%v", got)
			}
			if got := b.ops.Targets(bot, false); len(got) != 2 || got[0] != "http://a.test" || got[1] != "http://b.test" {
				t.Fatalf("手动目标不应受暂停状态限制，实际=%v", got)
			}
			farm = b.cfg.Get()
			farm.Panels[1].Paused = true
			if err := b.cfg.Set(farm); err != nil {
				t.Fatal(err)
			}
			if got := b.ops.Targets(bot, true); len(got) != 0 {
				t.Fatalf("全部暂停时不应创建自动推送目标，实际=%v", got)
			}
		})
	}
}

func TestBroadcastSkipsCircuitOpenPanelButKeepsManualTarget(t *testing.T) {
	b := opsTestBridge(t)
	farm := b.cfg.Get()
	farm.PanelStrategy = "broadcast"
	farm.Panels = []Panel{
		{Label: "A", BaseURL: "http://a.test", Priority: 1, Token: "a", AuthMode: "token"},
		{Label: "B", BaseURL: "http://b.test", Priority: 2, Token: "b", AuthMode: "token"},
	}
	if err := b.cfg.Set(farm); err != nil {
		t.Fatal(err)
	}
	routing := b.ops.Routing()
	routing.Accounts["10001:0"] = AccountPolicy{Targets: []string{"http://a.test", "http://b.test"}}
	if err := b.ops.SetRouting(routing); err != nil {
		t.Fatal(err)
	}
	b.circuitMu.Lock()
	if b.circuits == nil {
		b.circuits = map[string]panelCircuit{}
	}
	b.circuits["http://a.test"] = panelCircuit{Failures: 5, OpenedUntil: time.Now().Add(time.Hour).UnixMilli()}
	b.circuitMu.Unlock()
	bot := Bot{SelfID: 10001, ProtocolID: 0}
	if got := b.ops.Targets(bot, true); len(got) != 1 || got[0] != "http://b.test" {
		t.Fatalf("广播自动推送应跳过已熔断目标，实际=%v", got)
	}
	if got := b.ops.Targets(bot, false); len(got) != 2 || got[0] != "http://a.test" || got[1] != "http://b.test" {
		t.Fatalf("手动目标不应受熔断策略限制，实际=%v", got)
	}
}
