package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QQ bot credentials and identities are stored separately from exported farm configuration.
type QQBotConfig struct {
	Enabled        bool        `json:"enabled"`
	Mode           string      `json:"mode"`
	AppID          string      `json:"appId"`
	Secret         string      `json:"clientSecret,omitempty"`
	WebhookListen  string      `json:"webhookListen"`
	Cooldown       int         `json:"cooldownSeconds"`
	NotifySuccess  bool        `json:"notifySuccess"`
	NotifyFailure  bool        `json:"notifyFailure"`
	NotifyOffline  bool        `json:"notifyOffline"`
	NotifyRecovery bool        `json:"notifyRecovery"`
	Bindings       []QQBinding `json:"bindings"`
}
type QQBinding struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Kind      string   `json:"kind"`
	Target    string   `json:"target"`
	User      string   `json:"user"`
	Accounts  []string `json:"accounts"`
	Panels    []string `json:"panels"`
	AllowPush bool     `json:"allowPush"`
	Notify    bool     `json:"notify"`
}
type qqPendingBind struct {
	Hash    string
	Expires time.Time
	Binding QQBinding
}
type QQReply struct {
	Kind, Target, User, MessageID string
	At                            time.Time
	Seq                           int
}
type qqMessage struct {
	BindingID, Text, Category string
	Account, Panel            string
	Reply                     *QQReply
	Task                      *PushTask
}
type qqRecord struct {
	Time   int64  `json:"time"`
	Target string `json:"target"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}
type QQBot struct {
	bridge                  *Bridge
	path, seenPath, apiBase string
	client                  *http.Client
	mu                      sync.Mutex
	opMu                    sync.RWMutex
	commandMu               sync.Mutex
	lifecycle               sync.Mutex
	cfg                     QQBotConfig
	state, lastError        string
	lastEvent, lastSent     int64
	records                 []qqRecord
	pending                 map[string]qqPendingBind
	cooldown                map[string]time.Time
	tracked                 map[string]struct {
		Binding string
		Reply   QQReply
	}
	delivered              map[string]time.Time
	inputs                 chan qqEvent
	outputs                chan qqMessage
	cancel                 context.CancelFunc
	done                   chan struct{}
	started                bool
	generation             uint64
	tokenMu                sync.Mutex
	tokenForRedact         atomic.Value
	accessToken, accessKey string
	accessExpiry           time.Time
	seenMu                 sync.Mutex
	seen                   map[string]int64
	sendInterval           time.Duration
}

func newQQBot(b *Bridge, dir string) *QQBot {
	q := &QQBot{bridge: b, apiBase: "https://api.bot.qq.com", client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, state: "disabled", pending: map[string]qqPendingBind{}, cooldown: map[string]time.Time{}, tracked: map[string]struct {
		Binding string
		Reply   QQReply
	}{}, delivered: map[string]time.Time{}, seen: map[string]int64{}, sendInterval: 3100 * time.Millisecond}
	q.cfg = QQBotConfig{Mode: "websocket", WebhookListen: "127.0.0.1:17891", Cooldown: 300, NotifySuccess: true, NotifyFailure: true, NotifyOffline: true, NotifyRecovery: true, Bindings: []QQBinding{}}
	if dir != "" {
		q.path = filepath.Join(dir, "qqbot.json")
		q.seenPath = filepath.Join(dir, "qqbot-events.json")
		if raw, err := os.ReadFile(q.path); err == nil {
			if json.Unmarshal(raw, &q.cfg) != nil {
				q.cfg.Enabled = false
				q.lastError = "机器人配置无法读取，请重新保存"
			}
			tightenPrivateFile(q.path)
		}
		if raw, err := os.ReadFile(q.seenPath); err == nil {
			if json.Unmarshal(raw, &q.seen) != nil {
				q.cfg.Enabled = false
				q.lastError = "事件去重记录损坏，请检查数据文件后重新启用"
			}
		}
	}
	if q.cfg.Bindings == nil {
		q.cfg.Bindings = []QQBinding{}
	}
	if q.seen == nil {
		q.seen = map[string]int64{}
	}
	if err := q.validate(q.cfg); err != nil {
		q.cfg.Enabled = false
		q.lastError = "机器人配置无效，请重新保存"
	}
	return q
}
func randomQQID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
func cloneQQConfig(c QQBotConfig) QQBotConfig {
	raw, _ := json.Marshal(c)
	var out QQBotConfig
	_ = json.Unmarshal(raw, &out)
	return out
}
func (q *QQBot) config() QQBotConfig { q.mu.Lock(); defer q.mu.Unlock(); return cloneQQConfig(q.cfg) }
func (q *QQBot) validate(c QQBotConfig) error {
	if c.Mode != "websocket" && c.Mode != "webhook" && c.Mode != "notify" {
		return errors.New("请选择 WebSocket、Webhook 或仅提醒模式")
	}
	if c.Enabled && (c.AppID == "" || c.Secret == "") {
		return errors.New("启用前请填写 AppID 和 AppSecret")
	}
	if c.AppID != "" {
		if _, err := strconv.ParseUint(c.AppID, 10, 64); err != nil {
			return errors.New("AppID 必须为数字")
		}
	}
	if len(c.Secret) > 256 {
		return errors.New("AppSecret 长度无效")
	}
	if c.Cooldown < 30 || c.Cooldown > 86400 {
		return errors.New("提醒冷却时间应为 30～86400 秒")
	}
	host, port, err := net.SplitHostPort(c.WebhookListen)
	p, _ := strconv.Atoi(port)
	if err != nil || p < 1024 || p > 65535 || (host != "" && net.ParseIP(host) == nil) {
		return errors.New("回调监听地址应为 IP:端口，端口范围 1024～65535")
	}
	if len(c.Bindings) > 50 {
		return errors.New("最多绑定 50 个管理员")
	}
	seen := map[string]bool{}
	for _, b := range c.Bindings {
		if b.ID == "" || seen[b.ID] || !validQQID(b.Target) || !validQQID(b.User) || (b.Kind != "c2c" && b.Kind != "group") || len(b.Label) > 100 {
			return errors.New("绑定资料无效")
		}
		seen[b.ID] = true
		if b.Kind == "c2c" && b.Target != b.User {
			return errors.New("单聊绑定身份不一致")
		}
	}
	return nil
}
func validQQID(v string) bool {
	if v == "" || len(v) > 128 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func (q *QQBot) saveLocked(c QQBotConfig) error {
	if q.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFile(q.path, append(raw, '\n'))
}
func (q *QQBot) Update(c QQBotConfig, clearSecret bool) error {
	q.opMu.Lock()
	err := func() error {
		q.mu.Lock()
		defer q.mu.Unlock()
		if c.Secret == "" && !clearSecret && strings.TrimSpace(c.AppID) == q.cfg.AppID {
			c.Secret = q.cfg.Secret
		}
		c.AppID = strings.TrimSpace(c.AppID)
		c.Secret = strings.TrimSpace(c.Secret)
		c.WebhookListen = strings.TrimSpace(c.WebhookListen)
		c.Bindings = q.cfg.Bindings
		if c.AppID != q.cfg.AppID {
			c.Bindings = []QQBinding{}
		}
		if err := q.validate(c); err != nil {
			return err
		}
		if err := q.saveLocked(c); err != nil {
			return errors.New("机器人设置保存失败")
		}
		q.cfg = cloneQQConfig(c)
		q.pending = map[string]qqPendingBind{}
		q.tracked = map[string]struct {
			Binding string
			Reply   QQReply
		}{}
		// Invalidate old workers before releasing the operation guard.
		q.generation++
		q.inputs = nil
		q.outputs = nil
		return nil
	}()
	q.opMu.Unlock()
	if err != nil {
		return err
	}
	q.restart()
	return nil
}
func (q *QQBot) status(state, err string) {
	q.mu.Lock()
	q.state = state
	q.lastError = err
	q.mu.Unlock()
}
func (q *QQBot) Public() map[string]any {
	q.mu.Lock()
	defer q.mu.Unlock()
	bindings := []map[string]any{}
	for _, b := range q.cfg.Bindings {
		bindings = append(bindings, map[string]any{"id": b.ID, "label": b.Label, "kind": b.Kind, "target": maskQQ(b.Target), "user": maskQQ(b.User), "accounts": b.Accounts, "panels": b.Panels, "allowPush": b.AllowPush, "notify": b.Notify})
	}
	return map[string]any{"enabled": q.cfg.Enabled, "mode": q.cfg.Mode, "appId": q.cfg.AppID, "hasSecret": q.cfg.Secret != "", "webhookListen": q.cfg.WebhookListen, "callbackPath": "/qqbot/callback", "cooldownSeconds": q.cfg.Cooldown, "notifySuccess": q.cfg.NotifySuccess, "notifyFailure": q.cfg.NotifyFailure, "notifyOffline": q.cfg.NotifyOffline, "notifyRecovery": q.cfg.NotifyRecovery, "bindings": bindings, "status": q.state, "lastError": q.lastError, "lastEventAt": q.lastEvent, "lastSentAt": q.lastSent, "records": append([]qqRecord{}, q.records...)}
}
func maskQQ(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return "****" + string(r[len(r)-4:])
}
func (q *QQBot) redactSecrets(s string) string {
	q.mu.Lock()
	secret := q.cfg.Secret
	q.mu.Unlock()
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "[已隐藏]")
	}
	token := ""
	if value := q.tokenForRedact.Load(); value != nil {
		token, _ = value.(string)
	}
	if token != "" {
		s = strings.ReplaceAll(s, token, "[已隐藏]")
	}
	return s
}
func (q *QQBot) text(s string) string {
	s = q.bridge.redact(s)
	s = strings.ReplaceAll(s, "\x00", "")
	r := []rune(s)
	if len(r) > 1200 {
		s = string(r[:1200]) + "…"
	}
	return s
}
func (q *QQBot) Start() { q.lifecycle.Lock(); q.started = true; q.lifecycle.Unlock(); q.restart() }
func (q *QQBot) restart() {
	q.lifecycle.Lock()
	defer q.lifecycle.Unlock()
	if !q.started {
		return
	}
	q.mu.Lock()
	oldDone := q.done
	if q.cancel != nil {
		q.cancel()
	}
	q.mu.Unlock()
	if oldDone != nil {
		select {
		case <-oldDone:
		case <-time.After(12 * time.Second):
			q.status("error", "旧连接未能停止，请重启插件后再试")
			return
		}
	}
	q.mu.Lock()
	c := cloneQQConfig(q.cfg)
	q.generation++
	gen := q.generation
	q.inputs = make(chan qqEvent, 64)
	q.outputs = make(chan qqMessage, 128)
	inputs, outputs := q.inputs, q.outputs
	ctx, cancel := context.WithCancel(context.Background())
	q.cancel = cancel
	q.done = make(chan struct{})
	done := q.done
	q.mu.Unlock()
	go func() {
		defer close(done)
		if !c.Enabled {
			q.statusFor(gen, "disabled", "")
			return
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); q.eventWorker(ctx, gen, inputs) }()
		go func() { defer wg.Done(); q.sendWorker(ctx, gen, c, outputs) }()
		switch c.Mode {
		case "websocket":
			q.runWebSocket(ctx, gen, c)
		case "webhook":
			q.runWebhook(ctx, gen, c)
		default:
			q.statusFor(gen, "notify_only", "")
			<-ctx.Done()
		}
		wg.Wait()
	}()
}
func (q *QQBot) Stop() {
	q.lifecycle.Lock()
	q.started = false
	q.mu.Lock()
	cancel, done := q.cancel, q.done
	q.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	q.lifecycle.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(12 * time.Second):
		}
	}
}
func (q *QQBot) statusFor(gen uint64, state, err string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.generation == gen {
		q.state = state
		q.lastError = err
	}
}
func (q *QQBot) enqueue(m qqMessage) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.cfg.Enabled || q.outputs == nil {
		return errors.New("QQ 机器人未启用")
	}
	select {
	case q.outputs <- m:
		return nil
	default:
		return errors.New("提醒队列已满，请稍后重试")
	}
}
func (q *QQBot) accept(e qqEvent) bool { return q.acceptGen(0, e) }
func (q *QQBot) acceptGen(gen uint64, e qqEvent) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.cfg.Enabled || q.inputs == nil || gen != 0 && gen != q.generation {
		return false
	}
	select {
	case q.inputs <- e:
		q.lastEvent = time.Now().UnixMilli()
		return true
	default:
		return false
	}
}
func (q *QQBot) reserveEvent(id string) (bool, error) {
	if id == "" || len(id) > 300 {
		return false, errors.New("事件缺少有效标识")
	}
	q.seenMu.Lock()
	defer q.seenMu.Unlock()
	now := time.Now().UnixMilli()
	if at, ok := q.seen[id]; ok && now-at < 86400000 {
		return false, nil
	}
	next := map[string]int64{}
	for k, v := range q.seen {
		if now-v < 86400000 {
			next[k] = v
		}
	}
	if len(next) >= 5000 {
		return false, errors.New("事件去重记录已满，请稍后重试")
	}
	next[id] = now
	if q.seenPath != "" {
		raw, _ := json.Marshal(next)
		if err := writePrivateFile(q.seenPath, raw); err != nil {
			return false, errors.New("无法保存事件去重记录，已阻止执行")
		}
	}
	q.seen = next
	return true, nil
}
func (q *QQBot) binding(id string) (QQBinding, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, b := range q.cfg.Bindings {
		if b.ID == id {
			return b, true
		}
	}
	return QQBinding{}, false
}
func containsQQ(in []string, s string) bool {
	for _, v := range in {
		if v == s {
			return true
		}
	}
	return false
}
func (q *QQBot) canSee(b QQBinding, t PushTask) bool {
	return containsQQ(b.Accounts, accountKey(t.SelfID, t.ProtocolID)) && containsQQ(b.Panels, t.PanelURL)
}
func (q *QQBot) scopeValid(b QQBinding) error {
	if len(b.Accounts) > 2000 || len(b.Panels) > 100 {
		return errors.New("绑定权限数量过多")
	}
	panels := map[string]bool{}
	for _, p := range q.bridge.Panels() {
		panels[p.BaseURL] = true
	}
	for _, p := range b.Panels {
		if !panels[p] {
			return errors.New("绑定面板已不存在")
		}
	}
	for _, k := range b.Accounts {
		parts := strings.Split(k, ":")
		if len(parts) != 2 {
			return errors.New("账号标识无效")
		}
		id, e := strconv.ParseInt(parts[0], 10, 64)
		proto, e2 := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || e2 != nil || id <= 0 || proto < 0 {
			return errors.New("账号标识无效")
		}
	}
	return nil
}
func (q *QQBot) NewBinding(b QQBinding) (string, int64, error) {
	if b.Kind != "c2c" && b.Kind != "group" {
		return "", 0, errors.New("请选择单聊或群聊")
	}
	if strings.TrimSpace(b.Label) == "" || len(b.Label) > 100 {
		return "", 0, errors.New("请填写 1～100 字节的绑定名称")
	}
	if err := q.scopeValid(b); err != nil {
		return "", 0, err
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.cfg.Enabled || q.cfg.Mode == "notify" {
		return "", 0, errors.New("请先启用 WebSocket 或 Webhook 接收事件")
	}
	if len(q.cfg.Bindings) >= 50 {
		return "", 0, errors.New("管理员绑定已达上限")
	}
	code := randomQQID()
	if code == "" {
		return "", 0, errors.New("无法生成绑定码")
	}
	sum := sha256.Sum256([]byte(code))
	hash := hex.EncodeToString(sum[:])
	now := time.Now()
	for k, v := range q.pending {
		if now.After(v.Expires) {
			delete(q.pending, k)
		}
	}
	if len(q.pending) >= 10 {
		return "", 0, errors.New("待使用绑定码过多，请稍后再试")
	}
	b.ID = randomQQID()
	if b.ID == "" {
		return "", 0, errors.New("无法生成绑定身份")
	}
	expiry := now.Add(5 * time.Minute)
	q.pending[hash] = qqPendingBind{hash, expiry, b}
	return code, expiry.UnixMilli(), nil
}
func (q *QQBot) ModifyBinding(id string, remove bool, b QQBinding) error {
	q.opMu.Lock()
	defer q.opMu.Unlock()
	if !remove {
		if err := q.scopeValid(b); err != nil {
			return err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := cloneQQConfig(q.cfg)
	found := false
	for i, old := range c.Bindings {
		if old.ID != id {
			continue
		}
		found = true
		if remove {
			c.Bindings = append(c.Bindings[:i], c.Bindings[i+1:]...)
		} else {
			old.Notify = b.Notify
			old.AllowPush = b.AllowPush
			old.Accounts = b.Accounts
			old.Panels = b.Panels
			if strings.TrimSpace(b.Label) != "" {
				old.Label = strings.TrimSpace(b.Label)
			}
			c.Bindings[i] = old
		}
		break
	}
	if !found {
		return errors.New("绑定不存在")
	}
	if err := q.validate(c); err != nil {
		return err
	}
	if err := q.saveLocked(c); err != nil {
		return errors.New("绑定保存失败")
	}
	q.cfg = c
	return nil
}
func (q *QQBot) failRecord(m qqMessage, status, err string) {
	target := ""
	kind := ""
	if b, ok := q.binding(m.BindingID); ok {
		target, kind = b.Label, b.Kind
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.records = append([]qqRecord{{time.Now().UnixMilli(), target, kind, status, err}}, q.records...)
	if len(q.records) > 50 {
		q.records = q.records[:50]
	}
	if status == "sent" {
		q.lastSent = time.Now().UnixMilli()
	}
}
func (q *QQBot) claimCooldown(key string, seconds int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if until := q.cooldown[key]; now.Before(until) {
		return false
	}
	for k, v := range q.cooldown {
		if now.After(v) {
			delete(q.cooldown, k)
		}
	}
	if len(q.cooldown) > 10000 {
		return false
	}
	q.cooldown[key] = now.Add(time.Duration(seconds) * time.Second)
	return true
}
func (q *QQBot) NotifyPanel(title, level, base string) {
	c := q.config()
	if !c.Enabled || level == "success" && !c.NotifyRecovery || level != "success" && !c.NotifyFailure {
		return
	}
	for _, b := range c.Bindings {
		if !b.Notify || !containsQQ(b.Panels, base) {
			continue
		}
		if q.claimCooldown("panel:"+level+":"+b.Kind+":"+b.Target+":"+base, c.Cooldown) {
			q.queue(qqMessage{BindingID: b.ID, Text: title + "\n请在插件管理端查看详情。", Category: "panel", Panel: base})
		}
	}
}
func (q *QQBot) NotifyOffline(self, proto int64) {
	c := q.config()
	if !c.Enabled || !c.NotifyOffline {
		return
	}
	for _, b := range c.Bindings {
		if !b.Notify || !containsQQ(b.Accounts, accountKey(self, proto)) {
			continue
		}
		if q.claimCooldown("offline:"+b.Kind+":"+b.Target+":"+accountKey(self, proto), c.Cooldown) {
			q.queue(qqMessage{BindingID: b.ID, Text: "农场账号 " + maskQQ(strconv.FormatInt(self, 10)) + " 已离线，请检查萌卡登录状态。", Category: "offline", Account: accountKey(self, proto)})
		}
	}
}
func (q *QQBot) TrackTask(id, binding string, reply QQReply) {
	q.commandMu.Lock()
	defer q.commandMu.Unlock()
	q.trackTaskLocked(id, binding, reply)
}
func (q *QQBot) trackTaskLocked(id, binding string, reply QQReply) {
	q.mu.Lock()
	q.tracked[id] = struct {
		Binding string
		Reply   QQReply
	}{binding, reply}
	q.mu.Unlock()
	for _, t := range q.bridge.ops.Tasks() {
		if t.ID == id && !taskActive(t.Status) {
			q.taskFinishedLocked(t)
			return
		}
	}
}
func (q *QQBot) TaskFinished(t PushTask) {
	q.commandMu.Lock()
	defer q.commandMu.Unlock()
	q.taskFinishedLocked(t)
}
func (q *QQBot) taskFinishedLocked(t PushTask) {
	if taskActive(t.Status) {
		return
	}
	c := q.config()
	if !c.Enabled {
		return
	}
	q.mu.Lock()
	track, tracked := q.tracked[t.ID]
	delete(q.tracked, t.ID)
	q.mu.Unlock()
	for _, b := range c.Bindings {
		if !q.canSee(b, t) {
			continue
		}
		direct := tracked && track.Binding == b.ID
		if !direct && (!b.Notify || (t.Status == "succeeded" || t.Status == "skipped") && !c.NotifySuccess || (t.Status != "succeeded" && t.Status != "skipped") && !c.NotifyFailure) {
			continue
		}
		if !direct && !q.claimCooldown("task:"+t.Status+":"+accountKey(t.SelfID, t.ProtocolID)+":"+t.PanelURL+":"+b.Kind+":"+b.Target, c.Cooldown) {
			continue
		}
		key := t.ID + ":" + b.Kind + ":" + b.Target
		q.mu.Lock()
		_, exists := q.delivered[key]
		if !exists {
			if len(q.delivered) >= 5000 {
				oldKey := ""
				oldAt := time.Now()
				for k, at := range q.delivered {
					if at.Before(oldAt) {
						oldKey, oldAt = k, at
					}
				}
				delete(q.delivered, oldKey)
			}
			q.delivered[key] = time.Now()
		}
		for k, at := range q.delivered {
			if time.Since(at) > 24*time.Hour {
				delete(q.delivered, k)
			}
		}
		q.mu.Unlock()
		if exists {
			continue
		}
		state := map[string]string{"succeeded": "推送成功", "failed": "推送失败", "interrupted": "结果待核实", "skipped": "已跳过重复推送", "cancelled": "已取消"}[t.Status]
		if state == "" {
			state = "任务结束"
		}
		text := fmt.Sprintf("%s\n账号：%s\n任务：%s\n详情请在管理端查看。", state, maskQQ(strconv.FormatInt(t.SelfID, 10)), t.ID)
		var reply *QQReply
		if direct {
			r := track.Reply
			r.Seq = 2
			reply = &r
		}
		copy := t
		q.queue(qqMessage{BindingID: b.ID, Text: text, Category: "task", Reply: reply, Task: &copy})
	}
}

func (q *QQBot) queue(m qqMessage) {
	if err := q.enqueue(m); err != nil {
		q.failRecord(m, "dropped", err.Error())
	}
}
