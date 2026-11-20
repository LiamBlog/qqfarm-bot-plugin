package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func qqTest(t *testing.T) *QQBot {
	t.Helper()
	b := opsTestBridge(t)
	q := b.qq
	q.cfg.Enabled = true
	q.cfg.AppID = "123456"
	q.cfg.Secret = "test-bot-secret"
	q.inputs = make(chan qqEvent, 64)
	q.outputs = make(chan qqMessage, 128)
	return q
}
func qqListIPC(t *testing.T, bots []Bot) *IPC {
	t.Helper()
	r, w := io.Pipe()
	outR, outW := io.Pipe()
	c := newIPC(r, outW)
	close(c.ready)
	t.Cleanup(func() { _ = w.Close(); _ = outW.Close(); _ = outR.Close() })
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var v map[string]any
			if json.Unmarshal(sc.Bytes(), &v) != nil {
				continue
			}
			id, _ := v["id"].(string)
			var result any = bots
			if v["action"] == "get_qq_classic_farm_code" {
				result = map[string]any{"code": "MOCK-FARM-SECRET"}
			}
			c.mu.Lock()
			ch := c.pending[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- map[string]any{"type": "action_result", "id": id, "ok": true, "data": result}
			}
		}
	}()
	return c
}
func qqFixtureBinding() QQBinding {
	return QQBinding{ID: "owner", Label: "管理员", Kind: "c2c", Target: "OPENID_OWNER", User: "OPENID_OWNER", Accounts: []string{"10001:0"}, Panels: []string{"http://a.test"}, AllowPush: true, Notify: true}
}
func qqIncomingEvent(id, kind, user, target, content string) qqEvent {
	d := map[string]any{"id": "msg-" + id, "content": content, "timestamp": time.Now().Format(time.RFC3339), "author": map[string]string{"user_openid": user, "member_openid": user}, "group_openid": target}
	raw, _ := json.Marshal(d)
	return qqEvent{ID: id, Type: kind, Data: raw}
}
func TestQQConfigPrivateAndBindingMigration(t *testing.T) {
	q := qqTest(t)
	c := q.config()
	c.Bindings = []QQBinding{qqFixtureBinding()}
	q.cfg = c
	if err := q.Update(c, false); err != nil {
		t.Fatal(err)
	}
	public, _ := json.Marshal(q.Public())
	if bytes.Contains(public, []byte(c.Secret)) || bytes.Contains(public, []byte("OPENID_OWNER")) {
		t.Fatal("public response exposed secret/identity")
	}
	raw, _ := os.ReadFile(q.path)
	if !bytes.Contains(raw, []byte(c.Secret)) {
		t.Fatal("private credentials missing")
	}
	q2 := newQQBot(q.bridge, filepath.Dir(q.path))
	if q2.cfg.Secret != c.Secret || len(q2.cfg.Bindings) != 1 {
		t.Fatal("configuration did not survive restart")
	}
	c.Secret = ""
	c.NotifyOffline = false
	if err := q.Update(c, false); err != nil || q.config().Secret == "" {
		t.Fatal("blank secret should be retained for same app")
	}
	c.AppID = "987654"
	if q.Update(c, false) == nil {
		t.Fatal("new app reused previous app secret")
	}
	c.Secret = "new-bot-secret"
	if err := q.Update(c, false); err != nil {
		t.Fatal(err)
	}
	if len(q.config().Bindings) != 0 {
		t.Fatal("app switch retained identities")
	}
	c = q.config()
	c.Cooldown = 1
	if q.Update(c, false) == nil {
		t.Fatal("invalid cooldown accepted")
	}
	c = q.config()
	c.WebhookListen = "domain.test:443"
	if q.Update(c, false) == nil {
		t.Fatal("invalid listener accepted")
	}
}
func TestQQBindingOneTimeScopedAndPrivate(t *testing.T) {
	q := qqTest(t)
	b := qqFixtureBinding()
	b.ID, b.Target, b.User = "", "", ""
	code, _, err := q.NewBinding(b)
	if err != nil {
		t.Fatal(err)
	}
	event := qqIncomingEvent("bind-1", "C2C_MESSAGE_CREATE", "OPENID_OWNER", "", "/绑定 "+code)
	q.handleEvent(event)
	if len(q.config().Bindings) != 1 {
		t.Fatal("binding failed")
	}
	if len(q.outputs) != 1 {
		t.Fatal("binding acknowledgement missing")
	}
	q.handleEvent(event)
	if len(q.outputs) != 1 {
		t.Fatal("duplicate event was processed")
	}
	q.handleEvent(qqIncomingEvent("bind-2", "C2C_MESSAGE_CREATE", "ATTACKER", "", "/绑定 "+code))
	if len(q.config().Bindings) != 1 {
		t.Fatal("binding code reused")
	}
	q.handleEvent(qqIncomingEvent("untrusted", "C2C_MESSAGE_CREATE", "ATTACKER", "", "/推送 A1 P1"))
	if len(q.bridge.ops.Tasks()) != 0 {
		t.Fatal("untrusted command executed")
	}
	q2 := newQQBot(q.bridge, filepath.Dir(q.path))
	var decoded qqIncoming
	_ = json.Unmarshal(event.Data, &decoded)
	fresh, err := q2.reserveEvent(q2.eventKey(event, decoded))
	if err != nil || fresh {
		t.Fatal("event deduplication lost after restart")
	}
}
func TestQQGroupIdentityAndScope(t *testing.T) {
	q := qqTest(t)
	b := qqFixtureBinding()
	b.Kind, b.Target, b.User = "group", "GROUP_ONE", "OWNER_ONE"
	q.cfg.Bindings = []QQBinding{b}
	q.handleEvent(qqIncomingEvent("wrong-user", "GROUP_AT_MESSAGE_CREATE", "STRANGER", "GROUP_ONE", "/帮助"))
	q.handleEvent(qqIncomingEvent("wrong-group", "GROUP_AT_MESSAGE_CREATE", "OWNER_ONE", "GROUP_TWO", "/帮助"))
	if len(q.outputs) != 0 {
		t.Fatal("group sender/target authorization bypass")
	}
	q.handleEvent(qqIncomingEvent("right", "GROUP_AT_MESSAGE_CREATE", "OWNER_ONE", "GROUP_ONE", "<@!123456> /帮助"))
	if len(q.outputs) != 1 {
		t.Fatal("authorized group command not handled")
	}
	if q.canSee(b, PushTask{SelfID: 10001, ProtocolID: 1, PanelURL: "http://a.test"}) || q.canSee(b, PushTask{SelfID: 10001, ProtocolID: 0, PanelURL: "http://b.test"}) {
		t.Fatal("scope bypass")
	}
	if !q.canSee(b, PushTask{SelfID: 10001, ProtocolID: 0, PanelURL: "http://a.test"}) {
		t.Fatal("protocol zero rejected")
	}
}
func TestQQStableAccountAliasesAndAuthorizedPush(t *testing.T) {
	q := qqTest(t)
	bot := Bot{SelfID: 10001, Nickname: "测试农场", Online: true}
	bot.Protocol.ID = 0
	bot.Protocol.Platform = "android"
	q.bridge.ipc = qqListIPC(t, []Bot{bot})
	b := qqFixtureBinding()
	b.Accounts = []string{"99999:0", "10001:0"}
	q.cfg.Bindings = []QQBinding{b}
	q.bridge.ops.sem <- struct{}{}
	q.bridge.ops.sem <- struct{}{}
	text, _ := q.command(b, []string{"账号"})
	if !strings.Contains(text, "A2") || strings.Contains(text, "10001") {
		t.Fatal("aliases shifted or account not masked:", text)
	}
	_, ids := q.command(b, []string{"推送", "A1", "P1"})
	if len(ids) != 0 {
		t.Fatal("missing account alias retargeted")
	}
	_, ids = q.command(b, []string{"推送", "A2", "P1"})
	if len(ids) != 1 {
		t.Fatal("authorized push not enqueued")
	}
	task := q.bridge.ops.Tasks()[0]
	if task.ProtocolID != 0 || task.Source != "qqbot" || task.SelfID != 10001 {
		t.Fatal("incorrect task identity")
	}
	_ = q.bridge.ops.Cancel(ids[0])
	<-q.bridge.ops.sem
	<-q.bridge.ops.sem
	b.AllowPush = false
	_, ids = q.command(b, []string{"推送", "A2", "P1"})
	if len(ids) != 0 {
		t.Fatal("read-only binding enqueued push")
	}
}
func qqSignedRequest(secret string, body []byte, at int64) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/qqbot/callback", bytes.NewReader(body))
	ts := strconv.FormatInt(at, 10)
	r.Header.Set("X-Signature-Timestamp", ts)
	r.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(qqPrivateKey(secret), append([]byte(ts), body...))))
	return r
}
func TestQQWebhookSignatureChallengeAndReplay(t *testing.T) {
	q := qqTest(t)
	q.cfg.Mode = "webhook"
	body := []byte(`{"op":13,"d":{"plain_token":"challenge","event_ts":"123"}}`)
	w := httptest.NewRecorder()
	q.webhook(w, qqSignedRequest(q.cfg.Secret, body, time.Now().Unix()))
	if w.Code != 200 {
		t.Fatal("signed challenge rejected", w.Code)
	}
	var result map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	sig, _ := hex.DecodeString(result["signature"])
	if result["plain_token"] != "challenge" || !ed25519.Verify(qqPrivateKey(q.cfg.Secret).Public().(ed25519.PublicKey), []byte("123challenge"), sig) {
		t.Fatal("invalid validation signature")
	}
	for name, r := range map[string]*http.Request{"unsigned": httptest.NewRequest("POST", "/qqbot/callback", bytes.NewReader(body)), "wrong key": qqSignedRequest("bad-secret", body, time.Now().Unix()), "stale": qqSignedRequest(q.cfg.Secret, body, time.Now().Add(-10*time.Minute).Unix())} {
		w = httptest.NewRecorder()
		q.webhook(w, r)
		if w.Code != 401 {
			t.Fatal(name, " accepted", w.Code)
		}
	}
	event, _ := json.Marshal(qqIncomingEvent("safe", "C2C_MESSAGE_CREATE", "U", "", "/帮助"))
	w = httptest.NewRecorder()
	q.webhook(w, qqSignedRequest(q.cfg.Secret, event, time.Now().Unix()))
	if w.Code != 200 || len(q.inputs) != 1 {
		t.Fatal("valid event not accepted")
	}
	for len(q.inputs) < cap(q.inputs) {
		q.inputs <- qqEvent{}
	}
	w = httptest.NewRecorder()
	q.webhook(w, qqSignedRequest(q.cfg.Secret, event, time.Now().Unix()))
	if w.Code != 503 {
		t.Fatal("queue overflow was acknowledged")
	}
}
func TestQQTokenCacheAndSendingNeverRetries(t *testing.T) {
	q := qqTest(t)
	var tokens, sends atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			tokens.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"private-access-token","expires_in":"7200"}`)
			return
		}
		sends.Add(1)
		if r.Header.Get("Authorization") != "QQBot private-access-token" {
			t.Error("missing auth")
		}
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"code":40003,"message":"test-bot-secret private-access-token"}`)
	}))
	defer server.Close()
	q.apiBase = server.URL
	c := q.config()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := q.access(context.Background(), c); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if tokens.Load() != 1 {
		t.Fatal("credential stampede", tokens.Load())
	}
	_, err := q.api(context.Background(), c, "POST", "/v2/users/U/messages", map[string]any{"content": "hello"}, true)
	if err == nil || strings.Contains(err.Error(), c.Secret) || strings.Contains(err.Error(), "private-access-token") || sends.Load() != 1 {
		t.Fatal("unsafe failure/retry", err, sends.Load())
	}
	if strings.Contains(q.text("test-bot-secret private-access-token test-secret-abcdef code=MOCK-FARM-SECRET"), "MOCK-FARM-SECRET") {
		t.Fatal("message redaction failed")
	}
}
func TestQQSendWorkerReplyAndRevocation(t *testing.T) {
	q := qqTest(t)
	binding := qqFixtureBinding()
	q.cfg.Bindings = []QQBinding{binding}
	var sent atomic.Int64
	var mu sync.Mutex
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"secret-access","expires_in":7200}`)
			return
		}
		mu.Lock()
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Unlock()
		sent.Add(1)
		_, _ = io.WriteString(w, `{"id":"sent"}`)
	}))
	defer srv.Close()
	q.apiBase = srv.URL
	q.sendInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); q.sendWorker(ctx, 0, q.config(), q.outputs) }()
	reply := QQReply{Kind: "c2c", Target: binding.Target, User: binding.User, MessageID: "MSG_1", At: time.Now(), Seq: 1}
	_ = q.enqueue(qqMessage{BindingID: binding.ID, Text: "hello", Category: "reply", Reply: &reply})
	deadline := time.Now().Add(time.Second)
	for sent.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sent.Load() != 1 {
		t.Fatal("reply not sent")
	}
	mu.Lock()
	if payload["msg_id"] != "MSG_1" || payload["msg_seq"] != float64(1) {
		t.Error("reply context missing", payload)
	}
	mu.Unlock()
	if err := q.ModifyBinding(binding.ID, true, QQBinding{}); err != nil {
		t.Fatal(err)
	}
	_ = q.enqueue(qqMessage{BindingID: binding.ID, Text: "should not send", Category: "test"})
	time.Sleep(25 * time.Millisecond)
	cancel()
	<-done
	if sent.Load() != 1 {
		t.Fatal("revoked recipient received queued message")
	}
}
func TestQQTaskNotificationsAndCooldown(t *testing.T) {
	q := qqTest(t)
	b := qqFixtureBinding()
	q.cfg.Bindings = []QQBinding{b}
	task := PushTask{ID: "task-1", SelfID: 10001, ProtocolID: 0, PanelURL: "http://a.test", Status: "succeeded"}
	q.TaskFinished(task)
	q.TaskFinished(task)
	if len(q.outputs) != 1 {
		t.Fatal("duplicate task notification")
	}
	task.ID = "task-2"
	q.TaskFinished(task)
	if len(q.outputs) != 1 {
		t.Fatal("notification cooldown ignored")
	}
	q.NotifyOffline(10001, 0)
	q.NotifyOffline(10001, 0)
	q.NotifyOffline(10001, 1)
	if len(q.outputs) != 2 {
		t.Fatal("offline scope/cooldown ignored")
	}
	q.NotifyPanel("恢复", "success", "http://b.test")
	if len(q.outputs) != 2 {
		t.Fatal("panel scope bypass")
	}
}
func TestQQAdminAuthorizationAndLazyAsset(t *testing.T) {
	q := qqTest(t)
	a := newAdmin(q.bridge.cfg, q.bridge, q.bridge.dataDir)
	a.token = "managed-secret"
	for _, path := range []string{"/api/qqbot", "/qqbot.js"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		a.handler(w, r)
		if w.Code != 403 {
			t.Fatal("authorization bypass", path, w.Code)
		}
		r = httptest.NewRequest("GET", path, nil)
		r.Header.Set("x-mengka-managed-token", a.token)
		w = httptest.NewRecorder()
		a.handler(w, r)
		if w.Code != 200 || strings.Contains(w.Body.String(), q.cfg.Secret) {
			t.Fatal("authorized read failed or leaked secret", path)
		}
	}
	page := prepareAdminAssets()["/"].plain
	if bytes.Contains(page, []byte("qqConfigForm")) || !bytes.Contains(page, []byte("qqbotDetails")) {
		t.Fatal("optional UI was bundled into critical path")
	}
}
func TestQQWebSocketIdentifyHeartbeatResume(t *testing.T) {
	q := qqTest(t)
	var connections atomic.Int64
	checks := make(chan string, 8)
	upgrader := websocket.Upgrader{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 1000}})
		var auth map[string]any
		_ = conn.ReadJSON(&auth)
		n := connections.Add(1)
		if n == 1 {
			if auth["op"] != float64(2) {
				checks <- "identify opcode"
			}
			d, _ := auth["d"].(map[string]any)
			if d["intents"] != float64(1<<25) || d["token"] != "QQBot WS-TOKEN" {
				checks <- "identify token/intents"
			}
			_ = conn.WriteJSON(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]string{"session_id": "SESSION"}})
			_ = conn.WriteJSON(map[string]any{"op": 0, "s": 2, "id": "E1", "t": "C2C_MESSAGE_CREATE", "d": map[string]string{"id": "M1"}})
			var hb map[string]any
			_ = conn.ReadJSON(&hb)
			if hb["op"] != float64(1) || hb["d"] != float64(2) {
				checks <- "heartbeat sequence"
			}
			_ = conn.WriteJSON(map[string]any{"op": 11})
			_ = conn.WriteJSON(map[string]any{"op": 7})
		} else {
			if auth["op"] != float64(6) {
				checks <- "resume opcode"
			}
			d, _ := auth["d"].(map[string]any)
			if d["session_id"] != "SESSION" || d["seq"] != float64(2) {
				checks <- "resume context"
			}
			_ = conn.WriteJSON(map[string]any{"op": 0, "s": 3, "id": "E2", "t": "C2C_MESSAGE_CREATE", "d": map[string]string{"id": "M2"}})
			_ = conn.WriteJSON(map[string]any{"op": 0, "s": 4, "t": "RESUMED", "d": ""})
			_ = conn.WriteJSON(map[string]any{"op": 9})
		}
	}))
	defer srv.Close()
	// Only this local test connection trusts its test certificate.
	old := websocket.DefaultDialer
	websocket.DefaultDialer = &websocket.Dialer{TLSClientConfig: srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone(), HandshakeTimeout: time.Second}
	defer func() { websocket.DefaultDialer = old }()
	q.accessToken = "WS-TOKEN"
	sum := shaQQ(q.cfg)
	q.accessKey = sum
	q.accessExpiry = time.Now().Add(time.Hour)
	session := qqSession{}
	address := "wss" + strings.TrimPrefix(srv.URL, "https")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connected, err := q.websocketSession(ctx, 0, q.config(), address, &session)
	if !connected || err == nil || session.ID != "SESSION" || session.Sequence == nil || *session.Sequence != 2 {
		t.Fatal("identify/heartbeat failed", connected, err, session)
	}
	if len(q.inputs) != 1 {
		t.Fatal("dispatch event lost")
	}
	_, _ = q.websocketSession(ctx, 0, q.config(), address, &session)
	if len(q.inputs) != 2 {
		t.Fatal("resume replay before RESUMED was lost")
	}
	if session.ID != "" || session.Sequence != nil {
		t.Fatal("invalid session did not reset")
	}
	close(checks)
	for problem := range checks {
		t.Error(problem)
	}
}

func shaQQ(c QQBotConfig) string {
	sum := sha256.Sum256([]byte(c.AppID + ":" + c.Secret))
	return hex.EncodeToString(sum[:])
}
func TestQQWebhookLimitsAndDurableDedupFailure(t *testing.T) {
	q := qqTest(t)
	q.cfg.Mode = "webhook"
	body := bytes.Repeat([]byte("x"), 300<<10)
	w := httptest.NewRecorder()
	q.webhook(w, qqSignedRequest(q.cfg.Secret, body, time.Now().Unix()))
	if w.Code != 400 {
		t.Fatal("oversized event accepted")
	}
	q.seenPath = filepath.Join(t.TempDir(), "missing", "events.json")
	if fresh, err := q.reserveEvent("never-run"); err == nil || fresh {
		t.Fatal("event executed without durable dedup")
	}
	if _, ok := q.seen["never-run"]; ok {
		t.Fatal("failed dedup write mutated memory")
	}
}
func TestQQRecipientRejectAndRemovedScope(t *testing.T) {
	q := qqTest(t)
	b := qqFixtureBinding()
	q.cfg.Bindings = []QQBinding{b}
	raw := json.RawMessage(`{"openid":"OPENID_OWNER","timestamp":1784570599}`)
	q.handleEvent(qqEvent{ID: "rejected", Type: "C2C_MSG_REJECT", Data: raw})
	if q.config().Bindings[0].Notify {
		t.Fatal("recipient reject ignored")
	}
	q.NotifyOffline(10001, 0)
	if len(q.outputs) != 0 {
		t.Fatal("notification sent after opt-out")
	}
	q.handleEvent(qqEvent{ID: "deleted", Type: "FRIEND_DEL", Data: raw})
	if len(q.config().Bindings) != 0 {
		t.Fatal("deleted friend retained command permission")
	}
}
func TestQQReplySequencesAreUniqueAndBounded(t *testing.T) {
	q := qqTest(t)
	b := qqFixtureBinding()
	b.Notify = false
	q.cfg.Bindings = []QQBinding{b}
	var mu sync.Mutex
	seqs := []int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"seq-token","expires_in":7200}`)
			return
		}
		var d struct {
			Seq int `json:"msg_seq"`
		}
		_ = json.NewDecoder(r.Body).Decode(&d)
		mu.Lock()
		seqs = append(seqs, d.Seq)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"ok"}`)
	}))
	defer srv.Close()
	q.apiBase = srv.URL
	q.sendInterval = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.sendWorker(ctx, 0, q.config(), q.outputs); close(done) }()
	r := QQReply{Kind: "c2c", Target: b.Target, User: b.User, MessageID: "one-msg", At: time.Now(), Seq: 1}
	for i := 0; i < 6; i++ {
		copy := r
		q.queue(qqMessage{BindingID: b.ID, Text: "reply", Category: "reply", Reply: &copy})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(q.Public()["records"].([]qqRecord)) >= 6 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(seqs) != 4 {
		t.Fatal("reply count quota exceeded", seqs)
	}
	for i, v := range seqs {
		if v != i+1 {
			t.Fatal("duplicate msg_seq", seqs)
		}
	}
}
func TestQQStaleEventsAndOldGenerationCannotExecute(t *testing.T) {
	q := qqTest(t)
	q.cfg.Bindings = []QQBinding{qqFixtureBinding()}
	e := qqIncomingEvent("stale", "C2C_MESSAGE_CREATE", "OPENID_OWNER", "", "/帮助")
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	d["timestamp"] = time.Now().Add(-10 * time.Minute).Format(time.RFC3339)
	e.Data, _ = json.Marshal(d)
	q.handleEvent(e)
	if len(q.outputs) != 0 {
		t.Fatal("stale command was handled")
	}
	q.generation = 3
	if q.acceptGen(2, qqEvent{}) {
		t.Fatal("stale connection injected event after configuration changed")
	}
}
func TestQQWebhookConfigurationReloadReleasesPort(t *testing.T) {
	q := qqTest(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	q.cfg.Mode = "webhook"
	q.cfg.WebhookListen = address
	q.Start()
	defer q.Stop()
	wait := func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if q.Public()["status"] == "webhook_listening" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("webhook did not listen", q.Public()["lastError"])
	}
	wait()
	c := q.config()
	c.NotifySuccess = false
	if err := q.Update(c, false); err != nil {
		t.Fatal(err)
	}
	wait()
	body := []byte(`{"op":13,"d":{"plain_token":"reload","event_ts":"123"}}`)
	r := qqSignedRequest(c.Secret, body, time.Now().Unix())
	r.URL, _ = url.Parse("http://" + address + "/qqbot/callback")
	r.RequestURI = ""
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal("reloaded listener unavailable", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("reloaded listener rejected signed callback", resp.StatusCode)
	}
}
func TestQQDuplicateMessageWithDifferentDeliveryID(t *testing.T) {
	q := qqTest(t)
	q.cfg.Bindings = []QQBinding{qqFixtureBinding()}
	e := qqIncomingEvent("delivery-1", "C2C_MESSAGE_CREATE", "OPENID_OWNER", "", "/帮助")
	q.handleEvent(e)
	e.ID = "delivery-2"
	q.mu.Lock()
	q.cooldown = map[string]time.Time{}
	q.mu.Unlock()
	q.handleEvent(e)
	if len(q.outputs) != 1 {
		t.Fatal("same message with new delivery ID executed twice")
	}
	e.ID = "quoted"
	var d map[string]any
	_ = json.Unmarshal(e.Data, &d)
	d["id"] = "quoted-message"
	d["message_type"] = 103
	e.Data, _ = json.Marshal(d)
	q.handleEvent(e)
	if len(q.outputs) != 1 {
		t.Fatal("quoted/card command was executed")
	}
}
func TestQQCommandToFarmTaskAndResult(t *testing.T) {
	q := qqTest(t)
	panel := &fake404Panel{token: "mock-panel-token"}
	farm := httptest.NewServer(panel)
	defer farm.Close()
	cfg := q.bridge.cfg.Get()
	cfg.Panels = []Panel{{Label: "测试面板", BaseURL: farm.URL, Token: panel.token, AuthMode: "token"}}
	if err := q.bridge.cfg.Set(cfg); err != nil {
		t.Fatal(err)
	}
	bot := Bot{SelfID: 10001, Nickname: "测试农场", Online: true}
	bot.Protocol.Platform = "android"
	bot.Protocol.ID = 0
	q.bridge.ipc = qqListIPC(t, []Bot{bot})
	binding := qqFixtureBinding()
	binding.Panels = []string{farm.URL}
	q.cfg.Bindings = []QQBinding{binding}
	q.cfg.Mode = "notify"
	q.sendInterval = time.Millisecond
	var mu sync.Mutex
	messages := []map[string]any{}
	qqServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/getAppAccessToken" {
			_, _ = io.WriteString(w, `{"access_token":"integration-token","expires_in":7200}`)
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		messages = append(messages, payload)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"sent"}`)
	}))
	defer qqServer.Close()
	q.apiBase = qqServer.URL
	q.Start()
	defer q.Stop()
	q.handleEvent(qqIncomingEvent("integration-command", "C2C_MESSAGE_CREATE", binding.User, "", "/推送 A1 P1"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(messages)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 2 {
		t.Fatal("missing queued/result feedback", len(messages))
	}
	for i, p := range messages {
		content, _ := p["content"].(string)
		if strings.Contains(content, "MOCK-FARM-SECRET") || strings.Contains(content, panel.token) {
			t.Fatal("message exposed farm credential")
		}
		if p["msg_seq"] != float64(i+1) {
			t.Fatal("incorrect result sequence", messages)
		}
	}
	panel.mu.Lock()
	defer panel.mu.Unlock()
	if len(panel.posts) != 1 || panel.posts[0]["code"] != "MOCK-FARM-SECRET" {
		t.Fatal("farm push did not use existing IPC code flow")
	}
	tasks := q.bridge.ops.Tasks()
	if len(tasks) != 1 || tasks[0].Status != "succeeded" || tasks[0].Source != "qqbot" {
		t.Fatal("task did not complete", tasks)
	}
}
