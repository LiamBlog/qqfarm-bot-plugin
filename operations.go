package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const historyCapacity = 10000
const historyDays = 90

type AccountPolicy struct {
	Group   string   `json:"group"`
	Paused  bool     `json:"paused"`
	Targets []string `json:"targets"`
}
type Routing struct {
	Accounts map[string]AccountPolicy `json:"accounts"`
	Groups   map[string][]string      `json:"groups"`
}
type DailyCount struct {
	Success int `json:"success"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}
type PushTask struct {
	ID          string `json:"id"`
	SelfID      int64  `json:"self_id"`
	ProtocolID  int64  `json:"protocol_id"`
	Nickname    string `json:"nickname"`
	PanelURL    string `json:"panel_url"`
	Panel       string `json:"panel"`
	Source      string `json:"source"`
	Status      string `json:"status"`
	Stage       string `json:"stage"`
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	Error       string `json:"error,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	NextAt      int64  `json:"next_at,omitempty"`
	cancel      chan struct{}
}
type Operations struct {
	mu      sync.Mutex
	saveMu  sync.Mutex
	routing Routing
	daily   map[string]DailyCount
	tasks   []*PushTask
	sem     chan struct{}
	path    string
	bridge  *Bridge
}

func newOperations(b *Bridge) *Operations {
	o := &Operations{bridge: b, sem: make(chan struct{}, 2), routing: Routing{Accounts: map[string]AccountPolicy{}, Groups: map[string][]string{}}, daily: map[string]DailyCount{}}
	if b.dataDir != "" {
		o.path = filepath.Join(b.dataDir, "operations.json")
		raw, err := os.ReadFile(o.path)
		if err == nil {
			var d struct {
				Routing Routing               `json:"routing"`
				Daily   map[string]DailyCount `json:"daily"`
				Tasks   []*PushTask           `json:"tasks"`
			}
			if json.Unmarshal(raw, &d) == nil {
				if d.Routing.Accounts != nil {
					o.routing.Accounts = d.Routing.Accounts
				}
				if d.Routing.Groups != nil {
					o.routing.Groups = d.Routing.Groups
				}
				if d.Daily != nil {
					o.daily = d.Daily
				}
				for _, t := range d.Tasks {
					if t == nil {
						continue
					}
					t.cancel = make(chan struct{})
					if taskActive(t.Status) {
						t.Status = "interrupted"
						t.Stage = "进程重启，请先核实面板状态再重试"
						t.NextAt = 0
					}
					o.tasks = append(o.tasks, t)
				}
			}
		}
	}
	return o
}
func taskActive(s string) bool          { return s == "queued" || s == "running" || s == "retry_wait" }
func accountKey(id, proto int64) string { return fmt.Sprintf("%d:%d", id, proto) }
func cloneRouting(r Routing) Routing {
	b, _ := json.Marshal(r)
	var v Routing
	_ = json.Unmarshal(b, &v)
	return v
}
func (o *Operations) Routing() Routing {
	o.mu.Lock()
	defer o.mu.Unlock()
	return cloneRouting(o.routing)
}
func (o *Operations) save() error {
	if o.path == "" {
		return nil
	}
	o.saveMu.Lock()
	defer o.saveMu.Unlock()
	o.mu.Lock()
	data, err := json.MarshalIndent(struct {
		Routing Routing               `json:"routing"`
		Daily   map[string]DailyCount `json:"daily"`
		Tasks   []*PushTask           `json:"tasks"`
	}{o.routing, o.daily, o.tasks}, "", "  ")
	o.mu.Unlock()
	if err != nil {
		return err
	}
	return writePrivateFile(o.path, append(data, '\n'))
}
func (o *Operations) saveLogged() {
	if err := o.save(); err != nil {
		logf("任务与统计保存失败：%v", err)
	}
}
func (o *Operations) SetRouting(r Routing) error {
	if r.Accounts == nil {
		r.Accounts = map[string]AccountPolicy{}
	}
	if r.Groups == nil {
		r.Groups = map[string][]string{}
	}
	if len(r.Accounts) > 5000 || len(r.Groups) > 100 {
		return errors.New("分组或账号策略数量超限")
	}
	valid := map[string]bool{}
	for _, p := range o.bridge.Panels() {
		valid[p.BaseURL] = true
	}
	check := func(targets []string) error {
		if len(targets) > 8 {
			return errors.New("每条路由最多 8 个面板")
		}
		seen := map[string]bool{}
		for _, u := range targets {
			if !valid[u] || seen[u] {
				return errors.New("目标面板已被移除或重复，请重新选择")
			}
			seen[u] = true
		}
		return nil
	}
	for g, targets := range r.Groups {
		if strings.TrimSpace(g) != g || len(g) == 0 || len([]rune(g)) > 40 {
			return errors.New("分组名称需为 1–40 个字符")
		}
		if err := check(targets); err != nil {
			return err
		}
	}
	for key, p := range r.Accounts {
		var id, proto int64
		if _, err := fmt.Sscanf(key, "%d:%d", &id, &proto); err != nil || id <= 0 || accountKey(id, proto) != key {
			return errors.New("账号标识无效")
		}
		if p.Group != "" {
			if _, ok := r.Groups[p.Group]; !ok {
				return errors.New("账号分组不存在")
			}
		}
		if err := check(p.Targets); err != nil {
			return err
		}
	}
	old := o.Routing()
	o.mu.Lock()
	o.routing = cloneRouting(r)
	o.mu.Unlock()
	if err := o.save(); err != nil {
		o.mu.Lock()
		o.routing = old
		o.mu.Unlock()
		return err
	}
	return nil
}
func (o *Operations) Targets(bot Bot, automatic bool) []string {
	r := o.Routing()
	p := r.Accounts[accountKey(bot.SelfID, bot.ProtocolID)]
	if automatic && p.Paused {
		return nil
	}
	if len(p.Targets) > 0 {
		return p.Targets
	}
	if p.Group != "" {
		if v := r.Groups[p.Group]; len(v) > 0 {
			return v
		}
	}
	ps := o.bridge.Panels()
	if len(ps) == 0 {
		return nil
	}
	return []string{ps[o.bridge.activeIndex(ps)].BaseURL}
}
func (o *Operations) AutoAllowed(bot Bot, base string) bool {
	for _, u := range o.Targets(bot, true) {
		if u == base {
			return true
		}
	}
	return false
}
func (o *Operations) Record(bot Bot, index int, res map[string]any, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failed"
	} else if res["skipped"] == true && res["started"] != true {
		outcome = "skipped"
	}
	day := time.Now().Format("2006-01-02")
	o.mu.Lock()
	d := o.daily[day]
	switch outcome {
	case "success":
		d.Success++
	case "failed":
		d.Failed++
	case "skipped":
		d.Skipped++
	}
	o.daily[day] = d
	cut := time.Now().AddDate(0, 0, -historyDays).Format("2006-01-02")
	for k := range o.daily {
		if k < cut {
			delete(o.daily, k)
		}
	}
	o.mu.Unlock()
	o.saveLogged()
	label, base := "已移除面板", ""
	ps := o.bridge.Panels()
	if index >= 0 && index < len(ps) {
		label, base = ps[index].Label, ps[index].BaseURL
	}
	message := map[string]string{"success": "推送成功", "failed": "推送失败", "skipped": "Code 未变化或已有任务，已跳过"}[outcome]
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	ok := err == nil
	o.bridge.appendActivity(Activity{Time: time.Now().UnixMilli(), Type: "attempt", Message: fmt.Sprintf("%d → %s：%s", bot.SelfID, label, message), Detail: detail, OK: &ok, SelfID: strconv.FormatInt(bot.SelfID, 10), ProtocolID: bot.ProtocolID, Panel: label, PanelURL: base, Outcome: outcome})
}
func (o *Operations) Stats() map[string]any {
	o.mu.Lock()
	defer o.mu.Unlock()
	d := o.daily[time.Now().Format("2006-01-02")]
	return map[string]any{"todayOk": d.Success, "todayFail": d.Failed, "todaySkipped": d.Skipped, "todayAttempts": d.Success + d.Failed + d.Skipped, "day": time.Now().Format("2006-01-02"), "timezone": time.Now().Location().String(), "definition": "按账号×面板×执行次数计数；成功含补启动；Code 未变或执行中归为跳过；清空历史不重置统计"}
}
func (o *Operations) Tasks() []PushTask {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]PushTask, 0, len(o.tasks))
	for i := len(o.tasks) - 1; i >= 0; i-- {
		t := *o.tasks[i]
		t.cancel = nil
		out = append(out, t)
	}
	return out
}
func (o *Operations) Enqueue(bot Bot, base, source string, delay time.Duration) (string, error) {
	label := ""
	for _, p := range o.bridge.Panels() {
		if p.BaseURL == base {
			label = p.Label
		}
	}
	if label == "" {
		return "", errors.New("目标面板已移除，请重新选择")
	}
	o.mu.Lock()
	for _, t := range o.tasks {
		if t.SelfID == bot.SelfID && t.ProtocolID == bot.ProtocolID && t.PanelURL == base && taskActive(t.Status) {
			id := t.ID
			o.mu.Unlock()
			return id, nil
		}
	}
	active := 0
	for _, t := range o.tasks {
		if taskActive(t.Status) {
			active++
		}
	}
	if active >= 500 {
		o.mu.Unlock()
		return "", errors.New("待执行任务已达 500，请等待完成")
	}
	now := time.Now().UnixMilli()
	t := &PushTask{ID: fmt.Sprintf("%d-%d", time.Now().UnixNano(), len(o.tasks)), SelfID: bot.SelfID, ProtocolID: bot.ProtocolID, Nickname: bot.Nickname, PanelURL: base, Panel: label, Source: source, Status: "queued", Stage: "等待执行", MaxAttempts: 4, CreatedAt: now, UpdatedAt: now, cancel: make(chan struct{})}
	o.tasks = append(o.tasks, t)
	if len(o.tasks) > 1000 {
		for i, v := range o.tasks {
			if !taskActive(v.Status) {
				o.tasks = append(o.tasks[:i], o.tasks[i+1:]...)
				break
			}
		}
	}
	o.mu.Unlock()
	o.saveLogged()
	go o.run(t, delay)
	return t.ID, nil
}
func (o *Operations) setTask(t *PushTask, status, stage, reason string, next int64) {
	o.mu.Lock()
	if t.Status == "cancelled" {
		o.mu.Unlock()
		return
	}
	t.Status = status
	t.Stage = stage
	t.Error = o.bridge.redact(reason)
	t.NextAt = next
	t.UpdatedAt = time.Now().UnixMilli()
	o.mu.Unlock()
	o.saveLogged()
}
func (o *Operations) wait(t *PushTask, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	o.mu.Lock()
	reason := t.Error
	o.mu.Unlock()
	o.setTask(t, "retry_wait", "等待重试", reason, time.Now().Add(d).UnixMilli())
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-t.cancel:
		return false
	}
}
func (o *Operations) run(t *PushTask, delay time.Duration) {
	acquired := false
	operationHeld := false
	defer func() {
		if operationHeld {
			o.bridge.cfg.operationMu.RUnlock()
		}
		if acquired {
			<-o.sem
		}
		if r := recover(); r != nil {
			o.setTask(t, "interrupted", "任务异常中断，请先核实面板状态", "执行异常，详见运行日志", 0)
			logf("任务执行异常：%s", o.bridge.redact(fmt.Sprint(r)))
		}
	}()
	waits := []time.Duration{delay, 30 * time.Second, 2 * time.Minute, 5 * time.Minute}
	for attempt, wait := range waits {
		if !o.wait(t, wait) {
			return
		}
		select {
		case o.sem <- struct{}{}:
			acquired = true
		case <-t.cancel:
			return
		}
		o.mu.Lock()
		if t.Status == "cancelled" {
			o.mu.Unlock()
			<-o.sem
			acquired = false
			return
		}
		t.Status = "running"
		t.Stage = "读取 Code 并同步面板"
		t.NextAt = 0
		t.Attempt = attempt + 1
		t.UpdatedAt = time.Now().UnixMilli()
		o.mu.Unlock()
		o.saveLogged()
		bot := Bot{SelfID: t.SelfID, ProtocolID: t.ProtocolID, Nickname: t.Nickname}
		if t.Source == "automatic" && (!o.bridge.cfg.Get().Enabled || !o.AutoAllowed(bot, t.PanelURL)) {
			<-o.sem
			acquired = false
			o.setTask(t, "cancelled", "自动推送已暂停或路由已变更", "", 0)
			return
		}
		// Resolve identity under the same guard used by configuration writes, so a reorder cannot retarget an in-flight request.
		o.bridge.cfg.operationMu.RLock()
		operationHeld = true
		index := -1
		for i, p := range o.bridge.Panels() {
			if p.BaseURL == t.PanelURL {
				index = i
				break
			}
		}
		if index < 0 {
			o.bridge.cfg.operationMu.RUnlock()
			operationHeld = false
			<-o.sem
			acquired = false
			o.setTask(t, "failed", "目标已被移除", "不会改投其他面板", 0)
			return
		}
		res, err := o.bridge.pushAccountTracked(bot, index)
		o.bridge.cfg.operationMu.RUnlock()
		operationHeld = false
		<-o.sem
		acquired = false
		if err == nil {
			status, stage := "succeeded", "同步完成"
			if res["skipped"] == true && res["started"] != true {
				status, stage = "skipped", "无需重复同步，已跳过"
			}
			o.setTask(t, status, stage, "", 0)
			return
		}
		if isUncertainWrite(err) {
			o.setTask(t, "interrupted", "远端结果不确定，请核实后再重试", err.Error(), 0)
			return
		}
		if o.bridge.authBlockedMessage(index) != "" || attempt == len(waits)-1 {
			o.setTask(t, "failed", "请检查凭据或连接", err.Error(), 0)
			return
		}
		o.setTask(t, "queued", "执行失败，准备重试", err.Error(), 0)
	}
}
func (o *Operations) Cancel(id string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, t := range o.tasks {
		if t.ID == id {
			if t.Status != "queued" && t.Status != "retry_wait" {
				return errors.New("只能取消尚未执行的任务；运行中的远端操作不能撤销")
			}
			t.Status = "cancelled"
			t.Stage = "已取消后续执行"
			t.NextAt = 0
			t.UpdatedAt = time.Now().UnixMilli()
			close(t.cancel)
			go o.saveLogged()
			return nil
		}
	}
	return errors.New("任务不存在")
}
func (o *Operations) Retry(id string) (string, error) {
	for _, t := range o.Tasks() {
		if t.ID == id {
			if t.Status != "failed" && t.Status != "interrupted" {
				return "", errors.New("仅失败或中断任务可重试")
			}
			return o.Enqueue(Bot{SelfID: t.SelfID, ProtocolID: t.ProtocolID, Nickname: t.Nickname}, t.PanelURL, "manual", 0)
		}
	}
	return "", errors.New("任务不存在")
}
func (b *Bridge) QueueAutomatic(bot Bot) {
	if b.ops == nil {
		return
	}
	for _, base := range b.ops.Targets(bot, true) {
		if _, err := b.ops.Enqueue(bot, base, "automatic", 0); err != nil {
			logf("自动任务创建失败：%v", err)
		}
	}
}
func (b *Bridge) PushAutomaticAll() {
	bots, err := b.botList()
	if err != nil {
		return
	}
	for _, bot := range bots {
		if bot.Online && bot.isAndroid() {
			b.QueueAutomatic(bot)
		}
	}
}

var secretPattern = regexp.MustCompile(`(?i)(token|password|access[_-]?code|authorization|code)(["\s:=]+)[^\s,;"}]+`)

func (b *Bridge) redact(s string) string {
	for _, p := range b.Panels() {
		for _, secret := range []string{p.Token, p.Password, p.AccessCode} {
			if secret != "" {
				s = strings.ReplaceAll(s, secret, "[已隐藏]")
			}
		}
	}
	s = secretPattern.ReplaceAllString(s, "$1$2[已隐藏]")
	if len(s) > 4000 {
		s = s[:4000]
	}
	return s
}
func (b *Bridge) appendActivity(item Activity) {
	item.Message = b.redact(item.Message)
	item.Detail = b.redact(item.Detail)
	b.mu.Lock()
	b.activity = append([]Activity{item}, b.activity...)
	cut := time.Now().AddDate(0, 0, -historyDays).UnixMilli()
	if len(b.activity) > historyCapacity {
		b.activity = b.activity[:historyCapacity]
	}
	for len(b.activity) > 0 && b.activity[len(b.activity)-1].Time < cut {
		b.activity = b.activity[:len(b.activity)-1]
	}
	b.mu.Unlock()
	b.saveActivity()
}
func (b *Bridge) QueryHistory(q url.Values, export bool) map[string]any {
	items := b.History(historyCapacity)
	out := []Activity{}
	from, errFrom := time.ParseInLocation("2006-01-02", q.Get("from"), time.Local)
	to, errTo := time.ParseInLocation("2006-01-02", q.Get("to"), time.Local)
	for _, v := range items {
		if q.Get("account") != "" && !strings.Contains(v.SelfID, q.Get("account")) {
			continue
		}
		if q.Get("panel") != "" && v.PanelURL != q.Get("panel") {
			continue
		}
		if q.Get("outcome") != "" && v.Outcome != q.Get("outcome") {
			continue
		}
		if errFrom == nil && v.Time < from.UnixMilli() {
			continue
		}
		if errTo == nil && v.Time >= to.AddDate(0, 0, 1).UnixMilli() {
			continue
		}
		if export {
			v.Message = ""
			v.Detail = ""
			if len(v.SelfID) > 4 {
				v.SelfID = v.SelfID[:2] + "***" + v.SelfID[len(v.SelfID)-2:]
			}
			if v.PanelURL != "" {
				v.PanelURL = "[面板地址已隐藏]"
			}
			v.Panel = "面板"
		}
		out = append(out, v)
	}
	total := len(out)
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	if export {
		offset = 0
		limit = historyCapacity
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return map[string]any{"items": out[offset:end], "total": total, "offset": offset, "limit": limit, "retentionDays": historyDays, "capacity": historyCapacity}
}

type uncertainWriteError struct{ message string }

func (e uncertainWriteError) Error() string { return e.message }
func isUncertainWrite(err error) bool       { var target uncertainWriteError; return errors.As(err, &target) }
func targetPushKey(base string, bot Bot) string {
	return base + ":" + accountKey(bot.SelfID, bot.ProtocolID)
}

// Successful manual/legacy calls supersede only waiting retries; running remote calls are never cancelled.
func (o *Operations) completeWaiting(bot Bot, index int) {
	ps := o.bridge.Panels()
	if index < 0 || index >= len(ps) {
		return
	}
	changed := false
	o.mu.Lock()
	for _, t := range o.tasks {
		if t.SelfID == bot.SelfID && t.ProtocolID == bot.ProtocolID && t.PanelURL == ps[index].BaseURL && (t.Status == "queued" || t.Status == "retry_wait") {
			t.Status = "cancelled"
			t.Stage = "已由其他操作完成，取消后续重试"
			t.NextAt = 0
			t.UpdatedAt = time.Now().UnixMilli()
			close(t.cancel)
			changed = true
		}
	}
	o.mu.Unlock()
	if changed {
		o.saveLogged()
	}
}
