package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type qqEvent struct {
	ID       string          `json:"id"`
	Op       int             `json:"op"`
	Type     string          `json:"t"`
	Sequence *int64          `json:"s,omitempty"`
	Data     json.RawMessage `json:"d"`
}
type qqIncoming struct {
	ID          string          `json:"id"`
	Content     string          `json:"content"`
	Timestamp   json.RawMessage `json:"timestamp"`
	Group       string          `json:"group_openid"`
	OpenID      string          `json:"openid"`
	MessageType int             `json:"message_type"`
	Scene       struct {
		Ext []string `json:"ext"`
	} `json:"message_scene"`
	Author struct {
		User   string `json:"user_openid"`
		Member string `json:"member_openid"`
	} `json:"author"`
}

var qqLeadingMention = regexp.MustCompile(`^\s*<@!?[^<>\s]+>\s*`)

func qqCommand(s string) []string {
	s = qqLeadingMention.ReplaceAllString(s, "")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "/")
	parts := strings.Fields(s)
	if len(parts) > 0 && parts[0] == "农场" {
		parts = parts[1:]
	}
	return parts
}
func qqMessageTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n > 1000000000000 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	return time.Time{}
}
func (q *QQBot) eventWorker(ctx context.Context, gen uint64, ch <-chan qqEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			if ctx.Err() != nil {
				return
			}
			q.opMu.RLock()
			q.mu.Lock()
			current := q.generation == gen && q.cfg.Enabled
			q.mu.Unlock()
			if current && ctx.Err() == nil {
				q.handleEvent(e)
			}
			q.opMu.RUnlock()
		}
	}
}
func (q *QQBot) handleEvent(e qqEvent) {
	if e.Type != "C2C_MESSAGE_CREATE" && e.Type != "GROUP_AT_MESSAGE_CREATE" && e.Type != "C2C_MSG_REJECT" && e.Type != "GROUP_MSG_REJECT" && e.Type != "FRIEND_DEL" && e.Type != "GROUP_DEL_ROBOT" {
		return
	}
	var d qqIncoming
	if json.Unmarshal(e.Data, &d) != nil {
		return
	}
	key := q.eventKey(e, d)
	fresh, err := q.reserveEvent(key)
	if err != nil {
		q.status("error", err.Error())
		return
	}
	if !fresh {
		return
	}
	if e.Type != "C2C_MESSAGE_CREATE" && e.Type != "GROUP_AT_MESSAGE_CREATE" {
		q.rejectTarget(e.Type, d)
		return
	}
	reply := QQReply{Kind: "c2c", Target: d.Author.User, User: d.Author.User, MessageID: d.ID, At: qqMessageTime(d.Timestamp), Seq: 1}
	if e.Type == "GROUP_AT_MESSAGE_CREATE" {
		reply.Kind, reply.Target, reply.User = "group", d.Group, d.Author.Member
	}
	if !validQQID(reply.Target) || !validQQID(reply.User) || reply.MessageID == "" || reply.At.IsZero() || time.Since(reply.At) > 5*time.Minute || time.Until(reply.At) > time.Minute {
		return
	}
	if d.MessageType != 0 {
		return
	}
	parts := qqCommand(d.Content)
	if len(parts) == 0 {
		return
	}
	if parts[0] == "绑定" && len(parts) == 2 {
		b, err := q.consumeBinding(parts[1], reply)
		if err == nil {
			q.queue(qqMessage{BindingID: b.ID, Text: "绑定成功。发送 /帮助 查看指令；账号和面板权限由插件管理端控制。", Category: "reply", Reply: &reply})
		}
		return
	}
	var b QQBinding
	found := false
	for _, v := range q.config().Bindings {
		if v.Kind == reply.Kind && v.Target == reply.Target && v.User == reply.User {
			b = v
			found = true
			break
		}
	}
	if !found {
		return
	}
	if !q.claimCooldown("command:"+b.ID, 2) {
		return
	}
	q.commandMu.Lock()
	defer q.commandMu.Unlock()
	text, ids := q.command(b, parts)
	if text != "" {
		q.queue(qqMessage{BindingID: b.ID, Text: text, Category: "reply", Reply: &reply})
	}
	for _, id := range ids {
		q.trackTaskLocked(id, b.ID, reply)
	}
}
func (q *QQBot) consumeBinding(code string, r QQReply) (QQBinding, error) {
	sum := sha256.Sum256([]byte(strings.ToLower(code)))
	hash := hex.EncodeToString(sum[:])
	q.mu.Lock()
	defer q.mu.Unlock()
	p, ok := q.pending[hash]
	if !ok || time.Now().After(p.Expires) || p.Binding.Kind != r.Kind {
		return QQBinding{}, errors.New("绑定码无效或已过期")
	}
	c := cloneQQConfig(q.cfg)
	if !c.Enabled {
		return QQBinding{}, errors.New("机器人已关闭")
	}
	b := p.Binding
	b.Target, b.User = r.Target, r.User
	for _, old := range c.Bindings {
		if old.Kind == b.Kind && old.Target == b.Target && old.User == b.User {
			return QQBinding{}, errors.New("该管理员已绑定，请在管理端修改权限")
		}
	}
	if len(c.Bindings) >= 50 {
		return QQBinding{}, errors.New("绑定已满")
	}
	c.Bindings = append(c.Bindings, b)
	if err := q.validate(c); err != nil {
		return QQBinding{}, err
	}
	if err := q.saveLocked(c); err != nil {
		return QQBinding{}, errors.New("绑定保存失败")
	}
	q.cfg = c
	delete(q.pending, hash)
	return b, nil
}
func (q *QQBot) rejectTarget(kind string, d qqIncoming) {
	target := d.OpenID
	isGroup := strings.HasPrefix(kind, "GROUP_")
	if isGroup {
		target = d.Group
	}
	if !validQQID(target) {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	c := cloneQQConfig(q.cfg)
	out := []QQBinding{}
	changed := false
	for _, b := range c.Bindings {
		if b.Target == target && ((b.Kind == "group") == isGroup) {
			changed = true
			if kind == "FRIEND_DEL" || kind == "GROUP_DEL_ROBOT" {
				continue
			}
			b.Notify = false
		}
		out = append(out, b)
	}
	if changed {
		c.Bindings = out
		if err := q.saveLocked(c); err == nil {
			q.cfg = c
		} else {
			q.cfg.Enabled = false
			q.state = "error"
			q.lastError = "接收方撤销权限保存失败，机器人已停止业务操作"
		}
	}
}
func (q *QQBot) allowedBots(b QQBinding) ([]Bot, error) {
	bots, err := q.bridge.botList()
	if err != nil {
		return nil, errors.New("萌卡账号列表暂时不可用，请稍后查询")
	}
	out := []Bot{}
	for _, key := range b.Accounts {
		for _, bot := range bots {
			if accountKey(bot.SelfID, bot.ProtocolID) == key {
				out = append(out, bot)
				break
			}
		}
	}
	return out, nil
}
func (q *QQBot) command(b QQBinding, parts []string) (string, []string) {
	help := "农场指令：\n/状态\n/账号\n/推送 A1 P1（省略面板则按已授权路由）\n/任务 任务编号\n/重试 任务编号 确认\n/通知 开 或 /通知 关\n账号和面板序号请用 /账号 查看。"
	switch parts[0] {
	case "帮助", "help":
		return help, nil
	case "状态", "账号":
		bots, err := q.allowedBots(b)
		if err != nil {
			return err.Error(), nil
		}
		online := 0
		for _, bot := range bots {
			if bot.Online {
				online++
			}
		}
		lines := []string{fmt.Sprintf("授权账号 %d 个，在线 %d 个", len(bots), online)}
		if parts[0] == "账号" {
			for _, bot := range bots {
				i := 0
				for idx, key := range b.Accounts {
					if key == accountKey(bot.SelfID, bot.ProtocolID) {
						i = idx
						break
					}
				}
				state := "离线"
				if bot.Online {
					state = "在线"
				}
				lines = append(lines, fmt.Sprintf("A%d · %s（%s）· %s", i+1, bot.Nickname, maskQQ(strconv.FormatInt(bot.SelfID, 10)), state))
			}
			for i, base := range b.Panels {
				for _, p := range q.bridge.Panels() {
					if p.BaseURL == base {
						lines = append(lines, fmt.Sprintf("P%d · %s", i+1, p.Label))
					}
				}
			}
		}
		return strings.Join(lines, "\n"), nil
	case "推送":
		if !b.AllowPush {
			return "当前绑定未获得推送权限，请联系管理端授权。", nil
		}
		if !q.bridge.cfg.Get().Enabled {
			return "管理端已关闭推送，请先确认推送设置。", nil
		}
		if len(parts) < 2 || len(parts) > 3 {
			return "用法：/推送 A1 P1；先用 /账号 查看授权序号。", nil
		}
		bots, err := q.allowedBots(b)
		if err != nil {
			return err.Error(), nil
		}
		index, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(parts[1]), "A"))
		if err != nil || index < 1 || index > len(b.Accounts) {
			return "账号序号无效，请先发送 /账号。", nil
		}
		var bot Bot
		for _, candidate := range bots {
			if accountKey(candidate.SelfID, candidate.ProtocolID) == b.Accounts[index-1] {
				bot = candidate
				break
			}
		}
		if bot.SelfID == 0 {
			return "该授权账号当前不在萌卡列表中，请检查管理端。", nil
		}
		if !bot.Online || !bot.isAndroid() {
			return "该账号不是可推送的在线 Android 账号。", nil
		}
		targets := q.bridge.ops.Targets(bot, false)
		if len(parts) == 3 {
			i, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(parts[2]), "P"))
			if err != nil || i < 1 || i > len(b.Panels) {
				return "面板序号无效，请先发送 /账号。", nil
			}
			targets = []string{b.Panels[i-1]}
		}
		if len(targets) == 0 {
			return "该账号没有推送路由，请在管理端配置。", nil
		}
		for _, base := range targets {
			if !containsQQ(b.Panels, base) {
				return "路由包含未授权面板，已阻止本次推送；请指定已授权的 P 序号。", nil
			}
		}
		ids := []string{}
		for _, base := range targets {
			id, err := q.bridge.ops.Enqueue(bot, base, "qqbot", 0)
			if err != nil {
				return fmt.Sprintf("已排队 %d 项，后续排队失败，请在管理端查看。", len(ids)), ids
			}
			ids = append(ids, id)
		}
		return "已排队，任务编号：\n" + strings.Join(ids, "\n"), ids
	case "任务", "重试":
		if len(parts) < 2 {
			return "请提供任务编号。", nil
		}
		var task *PushTask
		for _, t := range q.bridge.ops.Tasks() {
			if t.ID == parts[1] && q.canSee(b, t) {
				copy := t
				task = &copy
				break
			}
		}
		if task == nil {
			return "任务不存在或未获授权。", nil
		}
		if parts[0] == "任务" {
			return fmt.Sprintf("任务：%s\n状态：%s\n进度：%s\n尝试：%d/%d", task.ID, task.Status, task.Stage, task.Attempt, task.MaxAttempts), nil
		}
		if !b.AllowPush {
			return "当前绑定未获得推送权限。", nil
		}
		if !q.bridge.cfg.Get().Enabled {
			return "管理端已关闭推送。", nil
		}
		if len(parts) != 3 || parts[2] != "确认" {
			return "请先核实面板状态；确定需要再次推送时发送 /重试 " + task.ID + " 确认", nil
		}
		if task.Status != "failed" && task.Status != "interrupted" {
			return "仅失败或中断任务可重试。", nil
		}
		bots, err := q.allowedBots(b)
		if err != nil {
			return err.Error(), nil
		}
		for _, bot := range bots {
			if bot.SelfID == task.SelfID && bot.ProtocolID == task.ProtocolID && bot.Online && bot.isAndroid() {
				id, err := q.bridge.ops.Enqueue(bot, task.PanelURL, "qqbot", 0)
				if err != nil {
					return "任务排队失败，请到管理端检查。", nil
				}
				return "已排队，任务编号：" + id, []string{id}
			}
		}
		return "该账号当前不可推送。", nil
	case "通知":
		if len(parts) != 2 || (parts[1] != "开" && parts[1] != "关") {
			return "用法：/通知 开 或 /通知 关", nil
		}
		q.mu.Lock()
		c := cloneQQConfig(q.cfg)
		for i, v := range c.Bindings {
			if v.ID == b.ID {
				c.Bindings[i].Notify = parts[1] == "开"
			}
		}
		err := q.saveLocked(c)
		if err == nil {
			q.cfg = c
		}
		q.mu.Unlock()
		if err != nil {
			return "通知设置保存失败。", nil
		}
		return "当前绑定通知已" + map[bool]string{true: "开启", false: "关闭"}[parts[1] == "开"], nil
	default:
		return help, nil
	}
}

// Delivery event IDs may change on redelivery; command identity comes from the message and message index.
func (q *QQBot) eventKey(e qqEvent, d qqIncoming) string {
	identity := e.ID
	if e.Type == "C2C_MESSAGE_CREATE" || e.Type == "GROUP_AT_MESSAGE_CREATE" {
		idx := ""
		for _, ext := range d.Scene.Ext {
			if strings.HasPrefix(ext, "msg_idx=") {
				idx = strings.TrimPrefix(ext, "msg_idx=")
				break
			}
		}
		identity = d.ID + ":" + d.Author.User + ":" + d.Author.Member + ":" + d.Group + ":" + idx
	} else if identity == "" {
		identity = string(e.Data)
	}
	c := q.config()
	sum := sha256.Sum256([]byte(c.AppID + ":" + e.Type + ":" + identity))
	return hex.EncodeToString(sum[:])
}
