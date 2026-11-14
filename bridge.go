package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const sessionCookie = "farm_admin_session"

type Bot struct {
	SelfID      int64  `json:"self_id"`
	Nickname    string `json:"nickname"`
	Online      bool   `json:"online"`
	Platform    string `json:"-"`
	ProtocolID  int64  `json:"-"`
	ProtoVer    string `json:"-"`
	Level       *int64 `json:"level"`
	FriendCount *int64 `json:"friend_count"`
	GroupCount  *int64 `json:"group_count"`
	ExtraInfo   string `json:"extra_info"`

	Protocol struct {
		ID       int64  `json:"id"`
		Platform string `json:"platform"`
		Version  string `json:"version"`
	} `json:"protocol"`
}

func (b *Bot) normalize() {
	b.Platform = b.Protocol.Platform
	b.ProtocolID = b.Protocol.ID
	b.ProtoVer = b.Protocol.Version
}

func (b Bot) isAndroid() bool {
	return strings.Contains(strings.ToLower(b.Platform), "android")
}

type Activity struct {
	Time    int64  `json:"time"`
	Type    string `json:"type"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	OK      *bool  `json:"ok,omitempty"`
}

type Bridge struct {
	cfg     *Config
	ipc     *IPC
	dataDir string
	client  *http.Client

	mu           sync.Mutex
	stateMu      sync.Mutex
	tokens       map[int]string
	activity     []Activity
	pushTimes    map[string]int64
	lastAutoPush map[string]int64
	panelState   map[string]bool

	loginMu     sync.Mutex
	loginFail   map[int]loginFailure
	authBlocked map[int]string

	panelWarn map[string]string

	timerMu   sync.Mutex
	stopChans []chan struct{}

	nextMu        sync.Mutex
	nextRefreshAt int64
	nextStaleAt   int64
	nextRepairAt  int64
	repairing     atomic.Bool

	pushingMu sync.Mutex
	pushing   map[string]bool
	retrying  map[string]bool
}

func newBridge(cfg *Config, ipc *IPC, dataDir string) *Bridge {
	jar, _ := cookiejar.New(nil)
	b := &Bridge{
		cfg:          cfg,
		ipc:          ipc,
		dataDir:      dataDir,
		client:       &http.Client{Timeout: 8 * time.Second, Jar: jar},
		tokens:       map[int]string{},
		pushTimes:    map[string]int64{},
		lastAutoPush: map[string]int64{},
		panelState:   map[string]bool{},
		loginFail:    map[int]loginFailure{},
		authBlocked:  map[int]string{},
		panelWarn:    map[string]string{},
		pushing:      map[string]bool{},
		retrying:     map[string]bool{},
	}
	b.loadState()
	return b
}

/* ---------- 持久化 ---------- */

func (b *Bridge) loadState() {
	if b.dataDir == "" {
		return
	}
	tokenPath := filepath.Join(b.dataDir, "farm-tokens.json")
	if raw, err := os.ReadFile(tokenPath); err == nil {
		tightenPrivateFile(tokenPath)
		var d struct {
			Tokens map[string]string `json:"tokens"`
		}
		if json.Unmarshal(raw, &d) == nil {
			for k, v := range d.Tokens {
				if i, err := strconv.Atoi(k); err == nil {
					b.tokens[i] = v
				}
			}
		}
	}
	activityPath := filepath.Join(b.dataDir, "activity.json")
	if raw, err := os.ReadFile(activityPath); err == nil {
		tightenPrivateFile(activityPath)
		var d struct {
			Activity  []Activity       `json:"activity"`
			PushTimes map[string]int64 `json:"pushTimes"`
		}
		if json.Unmarshal(raw, &d) == nil {
			if len(d.Activity) > 120 {
				d.Activity = d.Activity[:120]
			}
			b.activity = d.Activity
			if d.PushTimes != nil {
				b.pushTimes = d.PushTimes
			}
		}
	}
}

func (b *Bridge) saveTokens() {
	if b.dataDir == "" {
		return
	}
	b.mu.Lock()
	tokens := map[string]string{}
	for k, v := range b.tokens {
		tokens[strconv.Itoa(k)] = v
	}
	b.mu.Unlock()
	data, _ := json.MarshalIndent(map[string]any{"tokens": tokens}, "", "  ")
	_ = writePrivateFile(filepath.Join(b.dataDir, "farm-tokens.json"), append(data, '\n'))
}

func (b *Bridge) saveActivity() {
	if b.dataDir == "" {
		return
	}
	b.mu.Lock()
	payload := struct {
		Activity  []Activity       `json:"activity"`
		PushTimes map[string]int64 `json:"pushTimes"`
	}{append([]Activity(nil), b.activity...), b.pushTimes}
	b.mu.Unlock()
	data, _ := json.MarshalIndent(payload, "", "  ")
	_ = writePrivateFile(filepath.Join(b.dataDir, "activity.json"), append(data, '\n'))
}

func (b *Bridge) clearTokens() {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	b.mu.Lock()
	b.tokens = map[int]string{}
	b.mu.Unlock()
	b.loginFail = map[int]loginFailure{}
	b.authBlocked = map[int]string{}
	if b.dataDir != "" {
		_ = os.Remove(filepath.Join(b.dataDir, "farm-tokens.json"))
	}
}

func (b *Bridge) LogActivity(kind, message, detail string, ok *bool) {
	b.mu.Lock()
	item := Activity{Time: time.Now().UnixMilli(), Type: kind, Message: message, Detail: detail, OK: ok}
	b.activity = append([]Activity{item}, b.activity...)
	if len(b.activity) > 120 {
		b.activity = b.activity[:120]
	}
	b.mu.Unlock()
	b.saveActivity()
}

func (b *Bridge) ClearActivity() {
	b.mu.Lock()
	b.activity = nil
	b.mu.Unlock()
	b.saveActivity()
	logf("动态流已清空")
}

func (b *Bridge) History(limit int) []Activity {
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit > len(b.activity) {
		limit = len(b.activity)
	}
	return append([]Activity(nil), b.activity[:limit]...)
}

func (b *Bridge) PushTime(selfID int64) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ts, ok := b.pushTimes[strconv.FormatInt(selfID, 10)]
	return ts, ok
}

/* ---------- 面板访问 ---------- */

func (b *Bridge) Panels() []Panel {
	return normalizePanels(b.cfg.Get().Panels)
}

func (b *Bridge) activeIndex(panels []Panel) int {
	i := b.cfg.Get().ActivePanel
	if i < 0 {
		i = 0
	}
	if i >= len(panels) {
		i = len(panels) - 1
	}
	if i < 0 {
		i = 0
	}
	return i
}

const loginRetryCooldown = 60 * time.Second

type loginFailure struct {
	until time.Time
	msg   string
}

// authError 表示面板明确拒绝了登录凭据；继续自动重试可能触发面板锁定。
type authError struct{ msg string }

func (e authError) Error() string { return e.msg }

func isAuthError(err error) bool {
	var target authError
	return errors.As(err, &target)
}

func (b *Bridge) setPanelWarning(baseURL, msg string) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if msg == "" {
		delete(b.panelWarn, baseURL)
		return
	}
	b.panelWarn[baseURL] = msg
}

func (b *Bridge) panelWarning(baseURL string) string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.panelWarn[baseURL]
}

func (b *Bridge) authBlockedMessage(index int) string {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()
	return b.authBlocked[index]
}

// login 串行登录：并发推送只登录一次；失败后 60 秒内复用错误，避免连续失败触发 404 面板 5 次锁定。
func (b *Bridge) login(index int) (string, error) {
	b.loginMu.Lock()
	defer b.loginMu.Unlock()

	b.mu.Lock()
	if token := b.tokens[index]; token != "" {
		b.mu.Unlock()
		return token, nil
	}
	b.mu.Unlock()
	if msg := b.authBlocked[index]; msg != "" {
		return "", errors.New(msg + "（自动推送已暂停，修复后点「测试」恢复）")
	}
	if fail, ok := b.loginFail[index]; ok && time.Now().Before(fail.until) {
		return "", errors.New(fail.msg)
	}

	panels := b.Panels()
	if index < 0 || index >= len(panels) {
		return "", errors.New("面板不存在")
	}
	token, err := b.loginPanel(panels[index])
	if err != nil {
		b.loginFail[index] = loginFailure{until: time.Now().Add(loginRetryCooldown), msg: err.Error()}
		if isAuthError(err) {
			b.authBlocked[index] = err.Error()
		}
		return "", err
	}
	delete(b.loginFail, index)
	delete(b.authBlocked, index)
	b.mu.Lock()
	b.tokens[index] = token
	b.mu.Unlock()
	b.saveTokens()
	return token, nil
}

func (b *Bridge) loginPanel(panel Panel) (string, error) {
	if panel.isAdmin() {
		return b.loginAdmin(panel)
	}
	return b.loginCode(panel)
}

// loginCode 旧版 qq-farm-bot：访问 Code 换 farm_admin_session Cookie。
func (b *Bridge) loginCode(panel Panel) (string, error) {
	if panel.AccessCode == "" {
		return "", fmt.Errorf("面板「%s」未配置访问 Code", panel.Label)
	}
	payload, _ := json.Marshal(map[string]string{"code": panel.AccessCode})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, panel.BaseURL+"/api/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("面板「%s」请求失败：%v", panel.Label, err)
	}
	defer res.Body.Close()
	var body struct {
		OK            bool   `json:"ok"`
		Error         string `json:"error"`
		SetupRequired bool   `json:"setupRequired"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode == http.StatusConflict || body.SetupRequired {
		return "", authError{fmt.Sprintf("面板「%s」尚未设置访问 Code，请先在该面板完成首次设置", panel.Label)}
	}
	token := ""
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			token = c.Value
			break
		}
	}
	if res.StatusCode/100 != 2 || !body.OK || token == "" {
		reason := body.Error
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		msg := fmt.Sprintf("面板「%s」登录失败：%s", panel.Label, reason)
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != http.StatusRequestTimeout {
			return "", authError{msg}
		}
		return "", errors.New(msg)
	}
	b.setPanelWarning(panel.BaseURL, "")
	return token, nil
}

// loginAdmin qq-farm-bot-404：管理员用户名 + 密码换 x-admin-token（令牌在响应体内）。
func (b *Bridge) loginAdmin(panel Panel) (string, error) {
	if panel.Password == "" {
		return "", fmt.Errorf("面板「%s」未配置管理员密码", panel.Label)
	}
	payload, _ := json.Marshal(map[string]string{"username": panel.Username, "password": panel.Password})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, panel.BaseURL+"/api/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	res, err := b.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("面板「%s」请求失败：%v", panel.Label, err)
	}
	defer res.Body.Close()
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Data  struct {
			Token              string `json:"token"`
			MustChangePassword bool   `json:"mustChangePassword"`
		} `json:"data"`
	}
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode == http.StatusLocked {
		return "", authError{fmt.Sprintf("面板「%s」管理员登录已锁定，请稍后重试或直接在该面板修改密码", panel.Label)}
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return "", authError{fmt.Sprintf("面板「%s」登录请求过于频繁，请稍后重试", panel.Label)}
	}
	if res.StatusCode/100 != 2 || !body.OK || body.Data.Token == "" {
		reason := body.Error
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		msg := fmt.Sprintf("面板「%s」登录失败：%s", panel.Label, reason)
		if res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != http.StatusRequestTimeout {
			return "", authError{msg}
		}
		if body.Error != "" {
			return "", authError{msg}
		}
		return "", errors.New(msg)
	}
	if body.Data.MustChangePassword {
		b.setPanelWarning(panel.BaseURL, "面板仍在使用默认管理员密码，请尽快修改")
		logf("面板「%s」仍在使用默认管理员密码，建议尽快在面板内修改", panel.Label)
	} else {
		b.setPanelWarning(panel.BaseURL, "")
	}
	return body.Data.Token, nil
}

func (b *Bridge) farmFetch(index int, method, path string, payload any) (map[string]any, error) {
	return b.farmFetchInternal(index, method, path, payload, false, false)
}

// farmFetchForce 清除登录失败缓存与旧 token，用于「测试」时强制重新登录。
func (b *Bridge) farmFetchForce(index int, method, path string, payload any) (map[string]any, error) {
	b.loginMu.Lock()
	delete(b.loginFail, index)
	delete(b.authBlocked, index)
	b.mu.Lock()
	delete(b.tokens, index)
	b.mu.Unlock()
	b.loginMu.Unlock()
	return b.farmFetch(index, method, path, payload)
}

func (b *Bridge) farmFetchInternal(index int, method, path string, payload any, retried, netRetried bool) (map[string]any, error) {
	panels := b.Panels()
	if index < 0 || index >= len(panels) {
		return nil, errors.New("面板不存在")
	}
	panel := panels[index]

	b.mu.Lock()
	token := b.tokens[index]
	b.mu.Unlock()
	if token == "" {
		var err error
		if token, err = b.login(index); err != nil {
			return nil, err
		}
	}

	var reader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, panel.BaseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-admin-token", token)

	res, err := b.client.Do(req)
	if err != nil {
		if method == http.MethodPost && !netRetried {
			time.Sleep(600 * time.Millisecond)
			return b.farmFetchInternal(index, method, path, payload, retried, true)
		}
		return nil, fmt.Errorf("面板「%s」请求失败：%v", panel.Label, err)
	}
	defer res.Body.Close()

	if res.StatusCode >= 500 && method == http.MethodPost && !netRetried {
		time.Sleep(800 * time.Millisecond)
		return b.farmFetchInternal(index, method, path, payload, retried, true)
	}
	if res.StatusCode == http.StatusUnauthorized {
		if !retried {
			b.mu.Lock()
			delete(b.tokens, index)
			b.mu.Unlock()
			b.saveTokens()
			return b.farmFetchInternal(index, method, path, payload, true, netRetried)
		}
		return nil, fmt.Errorf("面板「%s」访问 Code 不正确，请在「面板」页更新后重试", panel.Label)
	}

	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)
	if res.StatusCode/100 != 2 || (body != nil && body["ok"] == false) {
		reason, _ := body["error"].(string)
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, errors.New(reason)
	}
	return body, nil
}

func (b *Bridge) fetchFarmCode(bot Bot) (string, error) {
	data, err := b.ipc.call("get_qq_classic_farm_code", map[string]any{
		"self_id":     bot.SelfID,
		"protocol_id": bot.ProtocolID,
	})
	if err != nil {
		return "", err
	}
	m, _ := data.(map[string]any)
	return pickFarmCode(m)
}

// pickFarmCode 兼容官方返回表未固化的字段命名，排除疑似业务状态码的短数字。
func pickFarmCode(m map[string]any) (string, error) {
	keys := []string{"code", "farm_code", "farmCode", "login_code", "loginCode", "auth_code", "authCode"}
	short := regexp.MustCompile(`^\d{1,3}$`)
	for _, k := range keys {
		raw := strings.TrimSpace(fmt.Sprintf("%v", m[k]))
		if raw == "" || raw == "<nil>" || short.MatchString(raw) {
			continue
		}
		return raw, nil
	}
	encoded, _ := json.Marshal(m)
	if len(encoded) > 200 {
		encoded = encoded[:200]
	}
	return "", fmt.Errorf("框架未返回农场 Code，返回内容：%s", string(encoded))
}

/* ---------- 推送 ---------- */

func (b *Bridge) PushAccount(bot Bot, index int) (map[string]any, error) {
	panels := b.Panels()
	if index < 0 || index >= len(panels) {
		return nil, errors.New("目标面板不存在")
	}
	panel := panels[index]
	// 同一账号同一面板同时只允许一个推送在途：巡检/定时/手动叠加时直接跳过
	pushKey := fmt.Sprintf("%d:%d", index, bot.SelfID)
	b.pushingMu.Lock()
	if b.pushing[pushKey] {
		b.pushingMu.Unlock()
		return map[string]any{"uin": strconv.FormatInt(bot.SelfID, 10), "skipped": true, "busy": true, "panel": panel.Label}, nil
	}
	b.pushing[pushKey] = true
	b.pushingMu.Unlock()
	defer func() {
		b.pushingMu.Lock()
		delete(b.pushing, pushKey)
		b.pushingMu.Unlock()
	}()

	code, err := b.fetchFarmCode(bot)
	if err != nil {
		return nil, err
	}
	list, err := b.farmFetch(index, http.MethodGet, "/api/accounts", nil)
	if err != nil {
		return nil, err
	}
	uin := strconv.FormatInt(bot.SelfID, 10)
	name := bot.Nickname
	if name == "" {
		name = "QQ" + uin
	}
	var existingID any
	existingCode := ""
	existingRunning := false
	existingName := ""
	nameConflict := false
	if data, ok := list["data"].(map[string]any); ok {
		if arr, ok := data["accounts"].([]any); ok {
			var byName map[string]any
			for _, item := range arr {
				acc, _ := item.(map[string]any)
				qq := fmt.Sprintf("%v", acc["qq"])
				u := fmt.Sprintf("%v", acc["uin"])
				accName := fmt.Sprintf("%v", acc["name"])
				if qq == uin || u == uin {
					existingID = acc["id"]
					existingCode = fmt.Sprintf("%v", acc["code"])
					existingRunning, _ = acc["running"].(bool)
					if accName != "<nil>" {
						existingName = strings.TrimSpace(accName)
					}
					break
				}
				// 404 面板以备注名识别账号：同名但已有其他 QQ 归属时不能复用，否则会覆盖对方
				if panel.isAdmin() && accName == name {
					if (u == "" || u == "<nil>") && (qq == "" || qq == "<nil>") {
						byName = acc
					} else {
						nameConflict = true
					}
				}
			}
			if existingID == nil && byName != nil {
				existingID = byName["id"]
				existingCode = fmt.Sprintf("%v", byName["code"])
				existingRunning, _ = byName["running"].(bool)
				if n := fmt.Sprintf("%v", byName["name"]); n != "<nil>" {
					existingName = strings.TrimSpace(n)
				}
			}
		}
	}
	if panel.isAdmin() {
		switch {
		case existingName != "":
			// 保留面板内已有备注，避免定时推送覆盖用户改名
			name = existingName
		case existingID == nil && nameConflict:
			name = fmt.Sprintf("%s（%s）", name, uin)
		}
	}
	// Code 未变化时不重复 POST：避免无谓重启面板会话
	if existingID != nil && strings.TrimSpace(existingCode) == strings.TrimSpace(code) && existingCode != "" && existingCode != "<nil>" {
		// 404 面板更新已停止账号时不会自动启动，这里直接补启动
		if panel.isAdmin() && !existingRunning {
			if err := b.startAccount(index, existingID); err != nil {
				return nil, err
			}
			b.mu.Lock()
			b.pushTimes[uin] = time.Now().UnixMilli()
			b.mu.Unlock()
			b.saveActivity()
			ok := true
			nick := ""
			if bot.Nickname != "" {
				nick = "（" + bot.Nickname + "）"
			}
			b.LogActivity("push", fmt.Sprintf("%s%s → %s：Code 未变化，已启动账号", uin, nick, panel.Label), "", &ok)
			return map[string]any{"uin": uin, "updated": false, "skipped": true, "started": true, "panel": panel.Label}, nil
		}
		b.mu.Lock()
		b.pushTimes[uin] = time.Now().UnixMilli()
		b.mu.Unlock()
		b.saveActivity()
		ok := true
		nick := ""
		if bot.Nickname != "" {
			nick = "（" + bot.Nickname + "）"
		}
		b.LogActivity("push", fmt.Sprintf("%s%s → %s：Code 未变化，已跳过", uin, nick, panel.Label), "", &ok)
		return map[string]any{"uin": uin, "updated": false, "skipped": true, "panel": panel.Label}, nil
	}
	payload := buildAccountPayload(panel, code, uin, name, existingID)
	if _, err := b.farmFetch(index, http.MethodPost, "/api/accounts", payload); err != nil {
		return nil, err
	}
	updated := existingID != nil
	started := !updated
	startNote := ""
	if panel.isAdmin() && updated && !existingRunning {
		if err := b.startAccount(index, existingID); err != nil {
			startNote = "，启动失败：" + err.Error()
		} else {
			started = true
		}
	}
	b.mu.Lock()
	b.pushTimes[uin] = time.Now().UnixMilli()
	b.mu.Unlock()
	b.saveActivity()
	ok := true
	action := "Code 已更新"
	if !updated {
		action = "新增账号并启动"
	} else if started {
		action = "Code 已更新并启动"
	}
	action += startNote
	nick := ""
	if bot.Nickname != "" {
		nick = "（" + bot.Nickname + "）"
	}
	b.LogActivity("push", fmt.Sprintf("%s%s → %s：%s", uin, nick, panel.Label, action), "", &ok)
	return map[string]any{"uin": uin, "updated": updated, "started": started, "panel": panel.Label}, nil
}

// buildAccountPayload 按面板类型组装账号写入参数：
// 旧版依赖 startAfterSave，404 更新已停止账号时需单独调用 start。
func buildAccountPayload(panel Panel, code, uin, name string, existingID any) map[string]any {
	payload := map[string]any{
		"code": code, "uin": uin, "qq": uin, "name": name, "platform": "qq",
	}
	if !panel.isAdmin() {
		payload["loginType"] = "manual"
		payload["startAfterSave"] = true
	}
	if existingID != nil {
		payload["id"] = existingID
	}
	return payload
}

func (b *Bridge) startAccount(index int, id any) error {
	accountID := accountIDString(id)
	if accountID == "" {
		return errors.New("面板账号 ID 缺失")
	}
	_, err := b.farmFetch(index, http.MethodPost, "/api/accounts/"+url.PathEscape(accountID)+"/start", nil)
	return err
}

func accountIDString(v any) string {
	switch id := v.(type) {
	case string:
		return strings.TrimSpace(id)
	case float64:
		return strconv.FormatInt(int64(id), 10)
	case json.Number:
		return id.String()
	}
	return strings.TrimSpace(fmt.Sprintf("%v", v))
}

func (b *Bridge) PushAllOnline(index int) []map[string]any {
	panels := b.Panels()
	if len(panels) == 0 {
		logf("尚未配置面板，跳过推送（请在管理端「面板」页添加）")
		return nil
	}
	bots, err := b.botList()
	if err != nil {
		logf("获取账号列表失败：%v", err)
		return nil
	}
	targets := make([]Bot, 0, len(bots))
	skipped := 0
	for _, bot := range bots {
		if !bot.Online {
			continue
		}
		if bot.isAndroid() {
			targets = append(targets, bot)
		} else {
			skipped++
		}
	}
	if skipped > 0 {
		logf("推送全部：跳过 %d 个非 Android 协议账号（仅 Android 支持农场 Code）", skipped)
	}
	if len(targets) == 0 {
		return []map[string]any{}
	}

	results := make([]map[string]any, len(targets))
	sem := make(chan struct{}, 2)
	var wg sync.WaitGroup
	for i, bot := range targets {
		wg.Add(1)
		go func(i int, bot Bot) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := b.PushAccount(bot, index)
			if err != nil {
				fail := false
				b.LogActivity("push", fmt.Sprintf("%d 推送失败：%v", bot.SelfID, err), "", &fail)
				results[i] = map[string]any{"self_id": bot.SelfID, "protocol_id": bot.ProtocolID, "ok": false, "error": err.Error()}
				// 失败后 30 秒自动重试一次（不阻塞本次响应）
				go b.retryPushLater(bot, index)
				return
			}
			out := map[string]any{"self_id": bot.SelfID, "protocol_id": bot.ProtocolID, "ok": true}
			for k, v := range res {
				out[k] = v
			}
			results[i] = out
		}(i, bot)
	}
	wg.Wait()
	return results
}

// Broadcast 把同一账号推送到全部面板，返回逐面板结果。
func (b *Bridge) Broadcast(bot Bot) ([]map[string]any, error) {
	panels := b.Panels()
	if len(panels) == 0 {
		return nil, errors.New("尚未配置面板")
	}
	results := make([]map[string]any, 0, len(panels))
	okN, failN := 0, 0
	for i, p := range panels {
		res, err := b.PushAccount(bot, i)
		if err != nil {
			failN++
			results = append(results, map[string]any{"panel": p.Label, "ok": false, "error": err.Error()})
			continue
		}
		okN++
		results = append(results, map[string]any{"panel": p.Label, "ok": true, "skipped": res["skipped"] == true})
	}
	good := failN == 0
	b.LogActivity("push", fmt.Sprintf("%d 广播 %d 个面板：成功 %d / 失败 %d", bot.SelfID, len(panels), okN, failN), "", &good)
	if okN == 0 {
		return results, errors.New("所有面板推送均失败")
	}
	return results, nil
}

// retryPushLater 失败后按 30s → 2min → 5min 退避重试三次；同一账号同一面板只保留一条重试链。
func (b *Bridge) retryPushLater(bot Bot, index int) {
	key := fmt.Sprintf("%d:%d", index, bot.SelfID)
	b.pushingMu.Lock()
	if b.retrying[key] {
		b.pushingMu.Unlock()
		return
	}
	b.retrying[key] = true
	b.pushingMu.Unlock()
	defer func() {
		b.pushingMu.Lock()
		delete(b.retrying, key)
		b.pushingMu.Unlock()
	}()

	for attempt, wait := range []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute} {
		if b.authBlockedMessage(index) != "" {
			return
		}
		time.Sleep(wait)
		if _, err := b.PushAccount(bot, index); err == nil {
			if attempt > 0 {
				ok := true
				b.LogActivity("push", fmt.Sprintf("%d 第 %d 次重试成功", bot.SelfID, attempt+1), "", &ok)
			}
			return
		}
	}
	fail := false
	b.LogActivity("push", fmt.Sprintf("%d 自动重试 3 次仍失败，请检查面板", bot.SelfID), "", &fail)
}

func (b *Bridge) TestPanel(index int) (map[string]any, error) {
	panels := b.Panels()
	if index < 0 || index >= len(panels) {
		return nil, errors.New("目标面板不存在")
	}
	started := time.Now()
	list, err := b.farmFetchForce(index, http.MethodGet, "/api/accounts", nil)
	if err != nil {
		return nil, err
	}
	accounts, running := 0, 0
	if data, ok := list["data"].(map[string]any); ok {
		if arr, ok := data["accounts"].([]any); ok {
			accounts = len(arr)
			for _, item := range arr {
				acc, _ := item.(map[string]any)
				if r, ok := acc["running"].(bool); ok && r {
					running++
				}
			}
		}
	}
	return map[string]any{
		"panel": panels[index].Label, "ok": true, "accounts": accounts, "running": running,
		"ms": time.Since(started).Milliseconds(), "warning": b.panelWarning(panels[index].BaseURL),
	}, nil
}

func (b *Bridge) PanelStatus() []map[string]any {
	panels := b.Panels()
	out := make([]map[string]any, len(panels))
	var wg sync.WaitGroup
	for i, panel := range panels {
		wg.Add(1)
		go func(i int, panel Panel) {
			defer wg.Done()
			item := map[string]any{
				"label": panel.Label, "baseUrl": panel.BaseURL,
				"hasCode": panel.AccessCode != "", "mode": panel.mode(),
				"username": panel.Username, "hasPassword": panel.Password != "",
				"hasAuth": panel.hasAuth(), "ok": false, "error": "", "ms": 0,
				"warning": b.panelWarning(panel.BaseURL), "accounts": []any{},
			}
			started := time.Now()
			list, err := b.farmFetch(i, http.MethodGet, "/api/accounts", nil)
			if err != nil {
				item["error"] = err.Error()
			} else {
				item["ms"] = time.Since(started).Milliseconds()
				item["ok"] = true
				accounts := []any{}
				if data, ok := list["data"].(map[string]any); ok {
					if arr, ok := data["accounts"].([]any); ok {
						for _, raw := range arr {
							acc, _ := raw.(map[string]any)
							code := ""
							if c, ok := acc["code"].(string); ok && c != "" {
								code = c
								if len(code) > 10 {
									code = code[:10] + "…"
								}
							}
							uin := fmt.Sprintf("%v", acc["qq"])
							if uin == "<nil>" || uin == "" {
								uin = fmt.Sprintf("%v", acc["uin"])
							}
							var running any
							if r, ok := acc["running"].(bool); ok {
								running = r
							}
							accounts = append(accounts, map[string]any{
								"id": acc["id"], "uin": uin, "name": acc["name"],
								"code": code, "platform": acc["platform"], "running": running,
							})
						}
					}
				}
				item["accounts"] = accounts
			}
			if msg := b.authBlockedMessage(i); msg != "" {
				item["authBlocked"] = true
				item["authError"] = msg
			}
			b.stateMu.Lock()
			previous, seen := b.panelState[panel.BaseURL]
			nowOK, _ := item["ok"].(bool)
			if seen && previous != nowOK {
				msg := fmt.Sprintf("面板「%s」已恢复连接", panel.Label)
				if !nowOK {
					reason := fmt.Sprintf("%v", item["error"])
					if len(reason) > 100 {
						reason = reason[:100]
					}
					msg = fmt.Sprintf("面板「%s」连接异常：%s", panel.Label, reason)
				}
				b.LogActivity("panel", msg, "", nil)
			}
			b.panelState[panel.BaseURL] = nowOK
			b.stateMu.Unlock()
			out[i] = item
		}(i, panel)
	}
	wg.Wait()
	return out
}

/* ---------- 事件与定时任务 ---------- */

func (b *Bridge) botList() ([]Bot, error) {
	data, err := b.ipc.call("get_bot_list", map[string]any{})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var bots []Bot
	if err := json.Unmarshal(raw, &bots); err != nil {
		return nil, err
	}
	for i := range bots {
		bots[i].normalize()
	}
	return bots, nil
}

func (b *Bridge) OnBotOnline(event map[string]any) {
	selfID := int64(toFloat(event["self_id"]))
	protoID := int64(toFloat(event["protocol_id"]))
	bots, err := b.botList()
	if err != nil {
		logf("上线事件处理失败（QQ %d）：%v", selfID, err)
		return
	}
	var me *Bot
	for i := range bots {
		if bots[i].SelfID == selfID && bots[i].ProtocolID == protoID {
			me = &bots[i]
			break
		}
	}
	if me == nil {
		logf("QQ %d（协议 %d）未在账号列表找到，忽略上线事件", selfID, protoID)
		return
	}
	if !me.isAndroid() {
		logf("QQ %d（%s 协议）不支持农场 Code，忽略上线事件", selfID, me.Platform)
		return
	}
	b.LogActivity("online", fmt.Sprintf("QQ %d（协议 %d）上线", selfID, protoID), "", nil)

	farm := b.cfg.Get()
	if !farm.Enabled || !farm.PushOnLogin {
		return
	}
	panels := b.Panels()
	if len(panels) == 0 {
		logf("尚未配置面板，跳过自动推送")
		return
	}
	if !me.Online {
		logf("QQ %d 已离线，跳过推送", selfID)
		return
	}
	key := fmt.Sprintf("%d:%d", selfID, protoID)
	now := time.Now().UnixMilli()
	b.mu.Lock()
	last := b.lastAutoPush[key]
	if now-last < 60_000 {
		b.mu.Unlock()
		logf("QQ %d（协议 %d）60 秒内已自动推送过，跳过", selfID, protoID)
		return
	}
	b.lastAutoPush[key] = now
	b.mu.Unlock()

	if _, err := b.PushAccount(Bot{SelfID: selfID, ProtocolID: protoID}, b.activeIndex(panels)); err != nil {
		logf("farm-bot 推送失败（QQ %d）：%v", selfID, err)
		return
	}
	logf("farm-bot 推送完成：QQ %d", selfID)
}

func (b *Bridge) OnBotOffline(event map[string]any) {
	selfID := int64(toFloat(event["self_id"]))
	protoID := int64(toFloat(event["protocol_id"]))
	if bots, err := b.botList(); err == nil {
		for _, bot := range bots {
			if bot.SelfID == selfID && bot.ProtocolID == protoID && !bot.isAndroid() {
				logf("QQ %d（%s 协议）离线，不记录动态", selfID, bot.Platform)
				return
			}
		}
	}
	msg := fmt.Sprintf("QQ %d 离线", selfID)
	if text, ok := event["err_msg"].(string); ok && text != "" {
		msg += "：" + text
	}
	b.LogActivity("offline", msg, "", nil)
}

func (b *Bridge) RefreshStaleCodes() []map[string]any {
	farm := b.cfg.Get()
	if !farm.Enabled || farm.StaleHours <= 0 || len(b.Panels()) == 0 {
		return nil
	}
	bots, err := b.botList()
	if err != nil {
		return nil
	}
	results := []map[string]any{}
	for _, bot := range bots {
		if !bot.Online || !bot.isAndroid() {
			continue
		}
		ts, ok := b.PushTime(bot.SelfID)
		if !ok || time.Now().UnixMilli()-ts <= int64(farm.StaleHours)*3600_000 {
			continue
		}
		if _, err := b.PushAccount(bot, b.activeIndex(b.Panels())); err != nil {
			logf("Code 保鲜推送 %d 失败：%v", bot.SelfID, err)
			results = append(results, map[string]any{"self_id": bot.SelfID, "ok": false, "error": err.Error()})
		} else {
			results = append(results, map[string]any{"self_id": bot.SelfID, "ok": true})
		}
	}
	if len(results) > 0 {
		okCount := 0
		for _, r := range results {
			if r["ok"] == true {
				okCount++
			}
		}
		logf("Code 保鲜：重推 %d/%d 个过期账号", okCount, len(results))
	}
	return results
}

// Schedule 返回下次自动刷新 / 保鲜检查 / 自动巡检的时间戳（毫秒，0 表示未启用）。
func (b *Bridge) Schedule() (int64, int64, int64) {
	b.nextMu.Lock()
	defer b.nextMu.Unlock()
	return b.nextRefreshAt, b.nextStaleAt, b.nextRepairAt
}

func (b *Bridge) setSchedule(refreshAt, staleAt, repairAt int64) {
	b.nextMu.Lock()
	b.nextRefreshAt = refreshAt
	b.nextStaleAt = staleAt
	b.nextRepairAt = repairAt
	b.nextMu.Unlock()
}

// StartTimers 依据当前配置重建定时任务（定时刷新 + Code 保鲜 + 自动巡检）。
func (b *Bridge) StartTimers() {
	b.StopTimers()
	farm := b.cfg.Get()
	b.setSchedule(0, 0, 0)
	if !farm.Enabled {
		return
	}
	if farm.RefreshMinutes > 0 {
		stop := make(chan struct{})
		b.timerMu.Lock()
		b.stopChans = append(b.stopChans, stop)
		b.timerMu.Unlock()
		interval := time.Duration(farm.RefreshMinutes) * time.Minute
		b.nextMu.Lock()
		b.nextRefreshAt = time.Now().Add(interval).UnixMilli()
		b.nextMu.Unlock()
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					b.nextMu.Lock()
					b.nextRefreshAt = time.Now().Add(interval).UnixMilli()
					b.nextMu.Unlock()
					if !b.cfg.Get().Enabled {
						return
					}
					b.PushAllOnline(b.activeIndex(b.Panels()))
				case <-stop:
					return
				}
			}
		}()
		logf("farm-bot Code 定时刷新已开启：每 %d 分钟", farm.RefreshMinutes)
	}
	if farm.StaleHours > 0 {
		stop := make(chan struct{})
		b.timerMu.Lock()
		b.stopChans = append(b.stopChans, stop)
		b.timerMu.Unlock()
		b.nextMu.Lock()
		b.nextStaleAt = time.Now().Add(10 * time.Minute).UnixMilli()
		b.nextMu.Unlock()
		go func() {
			ticker := time.NewTicker(10 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					b.nextMu.Lock()
					b.nextStaleAt = time.Now().Add(10 * time.Minute).UnixMilli()
					b.nextMu.Unlock()
					cur := b.cfg.Get()
					if !cur.Enabled || cur.StaleHours <= 0 {
						return
					}
					b.RefreshStaleCodes()
				case <-stop:
					return
				}
			}
		}()
		logf("farm-bot Code 保鲜已开启：Code 超过 %d 小时自动重推", farm.StaleHours)
	}
	if farm.AutoRepair && farm.RepairMinutes > 0 {
		stop := make(chan struct{})
		b.timerMu.Lock()
		b.stopChans = append(b.stopChans, stop)
		b.timerMu.Unlock()
		interval := time.Duration(farm.RepairMinutes) * time.Minute
		b.nextMu.Lock()
		b.nextRepairAt = time.Now().Add(interval).UnixMilli()
		b.nextMu.Unlock()
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					b.nextMu.Lock()
					b.nextRepairAt = time.Now().Add(interval).UnixMilli()
					b.nextMu.Unlock()
					cur := b.cfg.Get()
					if !cur.Enabled || !cur.AutoRepair {
						return
					}
					b.RepairMissing()
				case <-stop:
					return
				}
			}
		}()
		logf("farm-bot 自动巡检已开启：每 %d 分钟检查面板账号状态", farm.RepairMinutes)
	}
}

// RepairMissing 自动巡检：面板内已停止的账号补推恢复；
// 已推送过但被删除的账号只补回当前面板（避免把账号自动加进用户未接入的面板）。
func (b *Bridge) RepairMissing() []map[string]any {
	farm := b.cfg.Get()
	panels := b.Panels()
	results := []map[string]any{}
	if !farm.Enabled || len(panels) == 0 {
		return results
	}
	if !b.repairing.CompareAndSwap(false, true) {
		return results
	}
	defer b.repairing.Store(false)

	bots, err := b.botList()
	if err != nil {
		logf("自动巡检获取账号列表失败：%v", err)
		return results
	}
	online := map[int64]Bot{}
	for _, bot := range bots {
		if bot.Online && bot.isAndroid() {
			online[bot.SelfID] = bot
		}
	}
	if len(online) == 0 {
		return results
	}
	activeIdx := b.activeIndex(panels)

	for i, panel := range panels {
		list, err := b.farmFetch(i, http.MethodGet, "/api/accounts", nil)
		if err != nil {
			logf("自动巡检读取面板「%s」失败：%v", panel.Label, err)
			continue
		}
		present := map[string]map[string]any{}
		if data, ok := list["data"].(map[string]any); ok {
			if arr, ok := data["accounts"].([]any); ok {
				for _, item := range arr {
					acc, _ := item.(map[string]any)
					uin := fmt.Sprintf("%v", acc["qq"])
					if uin == "<nil>" || uin == "" {
						uin = fmt.Sprintf("%v", acc["uin"])
					}
					present[uin] = acc
				}
			}
		}
		for selfID, bot := range online {
			uin := strconv.FormatInt(selfID, 10)
			acc, exists := present[uin]
			reason := ""
			if exists {
				if running, isBool := acc["running"].(bool); isBool && !running {
					reason = "面板账号已停止"
				}
			} else if i == activeIdx {
				if _, pushed := b.PushTime(selfID); pushed {
					reason = "面板账号缺失"
				}
			}
			if reason == "" {
				continue
			}
			if _, err := b.PushAccount(bot, i); err != nil {
				fail := false
				b.LogActivity("push", fmt.Sprintf("自动巡检：%s（%s → %s）补推失败：%v", uin, reason, panel.Label, err), "", &fail)
				results = append(results, map[string]any{"uin": uin, "panel": panel.Label, "reason": reason, "ok": false, "error": err.Error()})
			} else {
				results = append(results, map[string]any{"uin": uin, "panel": panel.Label, "reason": reason, "ok": true})
			}
		}
	}
	if len(results) > 0 {
		okCount := 0
		for _, r := range results {
			if r["ok"] == true {
				okCount++
			}
		}
		logf("自动巡检：补推 %d/%d 个账号", okCount, len(results))
	}
	return results
}

// StartConfigWatcher 每 30 秒检查 config.json 是否被外部修改，变更后热加载并重建定时任务。
func (b *Bridge) StartConfigWatcher() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			before := b.Panels()
			changed, err := b.cfg.ReloadIfChanged()
			if err != nil || !changed {
				continue
			}
			after := b.Panels()
			if !samePanelURLs(before, after) || !samePanelAuth(before, after) {
				b.clearTokens()
			}
			b.StartTimers()
			logf("检测到 config.json 外部变更，已热加载并重建定时任务")
		}
	}()
}

func samePanelURLs(a, b []Panel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].BaseURL != b[i].BaseURL {
			return false
		}
	}
	return true
}

// samePanelAuth 判断面板登录凭据是否未变；变更后需重新登录。
func samePanelAuth(a, b []Panel) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].AccessCode != b[i].AccessCode || a[i].Username != b[i].Username || a[i].Password != b[i].Password {
			return false
		}
	}
	return true
}

func (b *Bridge) StopTimers() {
	b.timerMu.Lock()
	defer b.timerMu.Unlock()
	for _, ch := range b.stopChans {
		close(ch)
	}
	b.stopChans = nil
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}
