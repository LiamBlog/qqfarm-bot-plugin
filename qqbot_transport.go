package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

func qqPrivateKey(secret string) ed25519.PrivateKey {
	if secret == "" {
		return nil
	}
	seed := []byte(secret)
	for len(seed) < ed25519.SeedSize {
		seed = append(seed, seed...)
	}
	return ed25519.NewKeyFromSeed(seed[:ed25519.SeedSize])
}
func (q *QQBot) webhook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path != "/qqbot/callback" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	q.opMu.RLock()
	defer q.opMu.RUnlock()
	c := q.config()
	if !c.Enabled || c.Mode != "webhook" || c.Secret == "" {
		http.Error(w, "unavailable", 503)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	ts := r.Header.Get("X-Signature-Timestamp")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	now := time.Now().Unix()
	if err != nil || seconds < now-300 || seconds > now+60 {
		http.Error(w, "invalid timestamp", 401)
		return
	}
	sig, err := hex.DecodeString(r.Header.Get("X-Signature-Ed25519"))
	key := qqPrivateKey(c.Secret)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key.Public().(ed25519.PublicKey), append([]byte(ts), raw...), sig) {
		http.Error(w, "invalid signature", 401)
		return
	}
	var e qqEvent
	if json.Unmarshal(raw, &e) != nil {
		http.Error(w, "invalid event", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if e.Op == 13 {
		var d struct {
			Plain     string `json:"plain_token"`
			Timestamp string `json:"event_ts"`
		}
		if json.Unmarshal(e.Data, &d) != nil || d.Plain == "" || d.Timestamp == "" {
			http.Error(w, "invalid validation", 400)
			return
		}
		signature := hex.EncodeToString(ed25519.Sign(key, []byte(d.Timestamp+d.Plain)))
		_ = json.NewEncoder(w).Encode(map[string]string{"plain_token": d.Plain, "signature": signature})
		return
	}
	if e.Op != 0 {
		http.Error(w, "unsupported opcode", 400)
		return
	}
	if !q.accept(e) {
		http.Error(w, "event queue unavailable", 503)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]int{"op": 12})
}
func (q *QQBot) runWebhook(ctx context.Context, gen uint64, c QQBotConfig) {
	listener, err := net.Listen("tcp", c.WebhookListen)
	if err != nil {
		q.statusFor(gen, "error", "回调监听失败，请检查端口占用和地址")
		<-ctx.Done()
		return
	}
	srv := &http.Server{Handler: http.HandlerFunc(q.webhook), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 6 * time.Second, WriteTimeout: 6 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			q.statusFor(gen, "error", "回调监听异常结束")
		}
	}()
	q.statusFor(gen, "webhook_listening", "")
	select {
	case <-ctx.Done():
	case <-ended:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}

type qqSession struct {
	ID       string
	Sequence *int64
}

func (q *QQBot) runWebSocket(ctx context.Context, gen uint64, c QQBotConfig) {
	session := qqSession{}
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		q.statusFor(gen, "connecting", "")
		gateway, err := q.gateway(ctx, c)
		connected := false
		if err == nil {
			connected, err = q.websocketSession(ctx, gen, c, gateway, &session)
		}
		if ctx.Err() != nil {
			return
		}
		if connected {
			backoff = 2 * time.Second
		}
		message := "WebSocket 已断开，正在重连；若应用不支持该链路，请改用 Webhook"
		if err != nil && gateway == "" {
			message = err.Error()
		}
		q.statusFor(gen, "reconnecting", message)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(60*time.Second, backoff*2)
	}
}
func (q *QQBot) websocketSession(ctx context.Context, gen uint64, c QQBotConfig, address string, session *qqSession) (bool, error) {
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	conn, _, err := dialer.DialContext(ctx, address, nil)
	if err != nil {
		return false, errors.New("WebSocket 连接失败")
	}
	defer conn.Close()
	conn.SetReadLimit(256 << 10)
	cancelDone := make(chan struct{})
	defer close(cancelDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-cancelDone:
		}
	}()
	_ = conn.SetReadDeadline(time.Now().Add(12 * time.Second))
	var hello qqEvent
	if conn.ReadJSON(&hello) != nil || hello.Op != 10 {
		return false, errors.New("WebSocket 握手无效")
	}
	var hd struct {
		Interval int64 `json:"heartbeat_interval"`
	}
	if json.Unmarshal(hello.Data, &hd) != nil || hd.Interval < 1000 || hd.Interval > 300000 {
		return false, errors.New("WebSocket 心跳配置无效")
	}
	interval := time.Duration(hd.Interval) * time.Millisecond
	write := func(op int, data any) error {
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return conn.WriteJSON(map[string]any{"op": op, "d": data})
	}
	token, err := q.access(ctx, c)
	if err != nil {
		return false, err
	}
	resuming := session.ID != "" && session.Sequence != nil
	if resuming {
		err = write(6, map[string]any{"token": "QQBot " + token, "session_id": session.ID, "seq": *session.Sequence})
	} else {
		err = write(2, map[string]any{"token": "QQBot " + token, "intents": 1 << 25, "shard": []int{0, 1}, "properties": map[string]string{"$os": runtime.GOOS, "$browser": "qqfarm-bot", "$device": "qqfarm-bot"}})
	}
	if err != nil {
		return false, err
	}
	frames := make(chan qqEvent, 1)
	readErr := make(chan error, 1)
	readStop := make(chan struct{})
	defer close(readStop)
	go func() {
		for {
			_ = conn.SetReadDeadline(time.Now().Add(2*interval + 5*time.Second))
			var e qqEvent
			if err := conn.ReadJSON(&e); err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
			select {
			case frames <- e:
			case <-ctx.Done():
				return
			case <-readStop:
				return
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	awaiting := false
	connected := false
	for {
		select {
		case <-ctx.Done():
			return connected, ctx.Err()
		case err := <-readErr:
			return connected, err
		case <-ticker.C:
			if awaiting {
				return connected, errors.New("WebSocket 心跳确认超时")
			}
			if err := write(1, session.Sequence); err != nil {
				return connected, err
			}
			awaiting = true
		case e := <-frames:
			switch e.Op {
			case 0:
				if e.Type == "READY" {
					var ready struct {
						SessionID string `json:"session_id"`
					}
					if json.Unmarshal(e.Data, &ready) != nil || ready.SessionID == "" {
						return connected, errors.New("WebSocket 会话无效")
					}
					session.ID = ready.SessionID
					connected = true
					q.statusFor(gen, "connected", "")
				} else if e.Type == "RESUMED" {
					connected = true
					q.statusFor(gen, "connected", "")
				} else {
					if !connected && !resuming {
						return connected, errors.New("WebSocket 未完成鉴权")
					}
					if !q.acceptGen(gen, e) {
						return connected, errors.New("事件队列已满")
					}
				}
				if e.Sequence != nil {
					seq := *e.Sequence
					session.Sequence = &seq
				}
			case 1:
				if err := write(1, session.Sequence); err != nil {
					return connected, err
				}
			case 7:
				return connected, errors.New("官方要求重新连接")
			case 9:
				session.ID = ""
				session.Sequence = nil
				q.tokenMu.Lock()
				q.accessExpiry = time.Time{}
				q.tokenMu.Unlock()
				return connected, errors.New("会话失效")
			case 11:
				awaiting = false
			}
		}
	}
}
