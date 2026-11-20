package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// No remote body, URL, or credential is included in errors exposed to logs/UI.
func (q *QQBot) api(ctx context.Context, c QQBotConfig, method, path string, payload any, auth bool) (map[string]json.RawMessage, error) {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, errors.New("请求内容编码失败")
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, q.apiBase+path, body)
	if err != nil {
		return nil, errors.New("QQ API 请求无效")
	}
	req.Header.Set("Content-Type", "application/json")
	if auth {
		token, err := q.access(ctx, c)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "QQBot "+token)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return nil, errors.New("QQ API 连接失败或超时；消息不会自动重发")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, errors.New("QQ API 响应读取失败；请先核实发送结果")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(raw, &result) != nil {
		return nil, fmt.Errorf("QQ API 响应格式无效（HTTP %d）", resp.StatusCode)
	}
	var code int64
	_ = json.Unmarshal(result["code"], &code)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || code != 0 {
		if resp.StatusCode == 401 {
			q.tokenMu.Lock()
			q.accessExpiry = time.Time{}
			q.tokenMu.Unlock()
		}
		return nil, fmt.Errorf("QQ API 拒绝请求（HTTP %d，业务码 %d）；请检查权限、额度及凭证", resp.StatusCode, code)
	}
	return result, nil
}
func (q *QQBot) access(ctx context.Context, c QQBotConfig) (string, error) {
	keySum := sha256.Sum256([]byte(c.AppID + ":" + c.Secret))
	key := hex.EncodeToString(keySum[:])
	q.tokenMu.Lock()
	defer q.tokenMu.Unlock()
	if q.accessKey == key && q.accessToken != "" && time.Now().Before(q.accessExpiry.Add(-60*time.Second)) {
		return q.accessToken, nil
	}
	if c.AppID == "" || c.Secret == "" {
		return "", errors.New("请先保存机器人 AppID 和 AppSecret")
	}
	data, _ := json.Marshal(map[string]string{"appId": c.AppID, "clientSecret": c.Secret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.apiBase+"/app/getAppAccessToken", bytes.NewReader(data))
	if err != nil {
		return "", errors.New("凭证请求无效")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := q.client.Do(req)
	if err != nil {
		return "", errors.New("无法获取机器人凭证，请检查网络")
	}
	defer resp.Body.Close()
	var body struct {
		Token   string          `json:"access_token"`
		Expires json.RawMessage `json:"expires_in"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body) != nil || body.Token == "" {
		return "", errors.New("机器人凭证验证失败，请检查 AppID 和 AppSecret")
	}
	var seconds int
	if json.Unmarshal(body.Expires, &seconds) != nil {
		var s string
		_ = json.Unmarshal(body.Expires, &s)
		seconds, _ = strconv.Atoi(s)
	}
	if seconds < 1 || seconds > 86400 {
		return "", errors.New("机器人凭证有效期无效")
	}
	q.accessToken, q.accessKey, q.accessExpiry = body.Token, key, time.Now().Add(time.Duration(seconds)*time.Second)
	q.tokenForRedact.Store(body.Token)
	return body.Token, nil
}
func (q *QQBot) gateway(ctx context.Context, c QQBotConfig) (string, error) {
	r, err := q.api(ctx, c, http.MethodGet, "/gateway/bot", nil, true)
	if err != nil {
		return "", err
	}
	var raw string
	_ = json.Unmarshal(r["url"], &raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("官方返回的 WebSocket 网关地址无效")
	}
	return raw, nil
}
func (q *QQBot) Probe(ctx context.Context) error {
	c := q.config()
	if _, err := q.access(ctx, c); err != nil {
		return err
	}
	if c.Mode == "websocket" {
		_, err := q.gateway(ctx, c)
		return err
	}
	return nil
}
func qqReplyValid(r *QQReply) bool {
	if r == nil || r.MessageID == "" || r.At.IsZero() {
		return false
	}
	maxAge := 5 * time.Minute
	if r.Kind == "group" {
		maxAge = 59 * time.Minute
	}
	return time.Since(r.At) >= -time.Minute && time.Since(r.At) < maxAge
}
func (q *QQBot) sendWorker(ctx context.Context, gen uint64, c QQBotConfig, ch <-chan qqMessage) {
	var next time.Time
	type replyCount struct {
		Seq int
		At  time.Time
	}
	replies := map[string]replyCount{}
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-ch:
			if time.Now().Before(next) {
				timer := time.NewTimer(time.Until(next))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
			if ctx.Err() != nil {
				return
			}
			q.opMu.RLock()
			current := q.config()
			b, ok := q.binding(m.BindingID)
			q.mu.Lock()
			sameGeneration := q.generation == gen
			q.mu.Unlock()
			if ctx.Err() != nil || !sameGeneration {
				q.opMu.RUnlock()
				return
			}
			if !current.Enabled || !ok || m.Task != nil && !q.canSee(b, *m.Task) || m.Account != "" && !containsQQ(b.Accounts, m.Account) || m.Panel != "" && !containsQQ(b.Panels, m.Panel) {
				q.opMu.RUnlock()
				continue
			}
			if m.Category != "reply" && m.Category != "test" && m.Reply == nil && !b.Notify {
				q.opMu.RUnlock()
				continue
			}
			path := "/v2/users/" + url.PathEscape(b.Target) + "/messages"
			if b.Kind == "group" {
				path = "/v2/groups/" + url.PathEscape(b.Target) + "/messages"
			}
			payload := map[string]any{"msg_type": 0, "content": q.text(m.Text)}
			if m.Reply != nil {
				if m.Reply.User != b.User || m.Reply.Target != b.Target || m.Reply.Kind != b.Kind {
					q.opMu.RUnlock()
					continue
				}
				if qqReplyValid(m.Reply) {
					key := b.Kind + ":" + b.Target + ":" + m.Reply.MessageID
					seq := max(max(1, m.Reply.Seq), replies[key].Seq+1)
					limit := 4
					if b.Kind == "group" {
						limit = 5
					}
					if seq <= limit {
						payload["msg_id"] = m.Reply.MessageID
						payload["msg_seq"] = seq
						replies[key] = replyCount{seq, time.Now()}
					} else if !b.Notify || m.Category == "reply" {
						q.opMu.RUnlock()
						q.failRecord(m, "expired", "该消息的回复次数已用完，请重新查询任务")
						continue
					}
					for k, v := range replies {
						if time.Since(v.At) > time.Hour {
							delete(replies, k)
						}
					}
				} else if m.Category == "reply" {
					q.opMu.RUnlock()
					q.failRecord(m, "expired", "回复窗口已过期，请重新发送指令")
					continue
				} else if !b.Notify {
					q.opMu.RUnlock()
					q.failRecord(m, "expired", "回复窗口已过期，且该绑定未开启主动提醒")
					continue
				}
			}
			_, err := q.api(ctx, c, http.MethodPost, path, payload, true)
			q.opMu.RUnlock()
			next = time.Now().Add(q.sendInterval)
			if err != nil {
				q.failRecord(m, "failed", err.Error())
			} else {
				q.failRecord(m, "sent", "")
			}
		}
	}
}
func (a *Admin) serveQQBot(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/qqbot") {
		return false
	}
	q := a.bridge.qq
	if q == nil {
		http.NotFound(w, r)
		return true
	}
	out := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(err error) { out(400, map[string]any{"error": err.Error()}) }
	if r.URL.Path == "/api/qqbot" && r.Method == http.MethodGet {
		out(200, q.Public())
		return true
	}
	if r.Method != http.MethodPost {
		out(405, map[string]any{"error": "method not allowed"})
		return true
	}
	var body struct {
		Config      QQBotConfig `json:"config"`
		ClearSecret bool        `json:"clearSecret"`
		Binding     QQBinding   `json:"binding"`
		ID          string      `json:"id"`
		Remove      bool        `json:"remove"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&body) != nil {
		fail(errors.New("请求内容无效"))
		return true
	}
	switch r.URL.Path {
	case "/api/qqbot/config":
		if err := q.Update(body.Config, body.ClearSecret); err != nil {
			fail(err)
			return true
		}
		out(200, map[string]any{"ok": true, "qqbot": q.Public()})
	case "/api/qqbot/probe":
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()
		if err := q.Probe(ctx); err != nil {
			fail(err)
			return true
		}
		out(200, map[string]any{"ok": true, "message": "凭证验证通过；实际连接状态请查看状态栏"})
	case "/api/qqbot/bind":
		code, expiry, err := q.NewBinding(body.Binding)
		if err != nil {
			fail(err)
			return true
		}
		out(200, map[string]any{"ok": true, "code": code, "expiresAt": expiry})
	case "/api/qqbot/binding":
		if err := q.ModifyBinding(body.ID, body.Remove, body.Binding); err != nil {
			fail(err)
			return true
		}
		out(200, map[string]any{"ok": true, "qqbot": q.Public()})
	case "/api/qqbot/test":
		if _, ok := q.binding(body.ID); !ok {
			fail(errors.New("请选择已绑定的接收对象"))
			return true
		}
		if !q.claimCooldown("test:"+body.ID, 30) {
			fail(errors.New("请等待 30 秒后再次测试"))
			return true
		}
		if err := q.enqueue(qqMessage{BindingID: body.ID, Text: "农场桥接助手：QQ 官方机器人提醒通道测试。此消息不会触发账号推送。", Category: "test"}); err != nil {
			fail(err)
			return true
		}
		out(200, map[string]any{"ok": true})
	default:
		out(404, map[string]any{"error": "not found"})
	}
	return true
}
