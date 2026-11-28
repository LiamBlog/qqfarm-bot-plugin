package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Panel 一个 farm 面板配置。
// 旧版 qq-farm-bot 使用 AccessCode 登录；qq-farm-bot-404 使用 Username + Password 或面板「我的 Token」。
type Panel struct {
	Label string `json:"label"`
	// Priority 越小越优先，用于主备和首个可用面板策略。
	Priority int `json:"priority,omitempty"`
	// Paused 暂停该面板的自动推送；手动检测与手动推送仍可用。
	Paused     bool   `json:"paused,omitempty"`
	BaseURL    string `json:"baseUrl"`
	AccessCode string `json:"accessCode"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	Token      string `json:"token,omitempty"`
	// AuthMode 显式指定认证方式：token / admin / code；留空则按已填凭据自动判断。
	AuthMode string `json:"authMode,omitempty"`
}

// mode 返回实际认证方式。Token 优先于用户名密码，避免残留字段误判。
func (p Panel) mode() string {
	switch p.AuthMode {
	case "token", "admin", "code":
		return p.AuthMode
	}
	if p.Token != "" {
		return "token"
	}
	if p.Username != "" {
		return "admin"
	}
	return "code"
}

// isToken 判断是否使用 404 面板「我的 Token」直连。
func (p Panel) isToken() bool {
	return p.mode() == "token"
}

// isAdmin 判断面板是否为 qq-farm-bot-404（密码或 Token 认证）。
func (p Panel) isAdmin() bool {
	return p.mode() != "code"
}

func (p Panel) hasAuth() bool {
	switch p.mode() {
	case "token":
		return p.Token != ""
	case "admin":
		return p.Password != ""
	default:
		return p.AccessCode != ""
	}
}

// FarmConfig 推送设置（字段名与旧版 config.json 保持一致，便于原地升级）。
type FarmConfig struct {
	Enabled        bool    `json:"enabled"`
	PushOnLogin    bool    `json:"pushOnLogin"`
	RefreshMinutes int     `json:"refreshMinutes"`
	StaleHours     int     `json:"staleHours"`
	AutoRepair     bool    `json:"autoRepair"`
	RepairMinutes  int     `json:"repairMinutes"`
	Panels         []Panel `json:"panels"`
	ActivePanel    int     `json:"activePanel"`
	// Webhook 失败/恢复通知地址（可选，兼容飞书等自定义机器人）。
	Webhook string `json:"webhook,omitempty"`
	// AlertFails 同一面板连续推送失败多少次后告警。
	AlertFails int `json:"alertFails"`
	// PanelStrategy 自动任务的面板选择：broadcast / primary_backup / first_available。
	PanelStrategy string `json:"panelStrategy"`
	// MaxConcurrent 全局推送并发数，范围 1–8。
	MaxConcurrent int `json:"maxConcurrent"`
	// CircuitFails 连续失败多少次后暂时熔断面板。
	CircuitFails int `json:"circuitFails"`
	// CircuitCooldownMinutes 熔断后等待多久再允许自动探测。
	CircuitCooldownMinutes int `json:"circuitCooldownMinutes"`
}

type Config struct {
	Farm        FarmConfig
	Path        string
	modTime     time.Time
	mu          sync.RWMutex
	operationMu sync.RWMutex
}

// 兼容旧版单面板字段（baseUrl / accessCode）
type rawFarm struct {
	Enabled                *bool   `json:"enabled"`
	PushOnLogin            *bool   `json:"pushOnLogin"`
	RefreshMinutes         *int    `json:"refreshMinutes"`
	StaleHours             *int    `json:"staleHours"`
	AutoRepair             *bool   `json:"autoRepair"`
	RepairMinutes          *int    `json:"repairMinutes"`
	Panels                 []Panel `json:"panels"`
	ActivePanel            *int    `json:"activePanel"`
	Webhook                *string `json:"webhook"`
	AlertFails             *int    `json:"alertFails"`
	PanelStrategy          string  `json:"panelStrategy"`
	MaxConcurrent          int     `json:"maxConcurrent"`
	CircuitFails           int     `json:"circuitFails"`
	CircuitCooldownMinutes int     `json:"circuitCooldownMinutes"`
	BaseURL                string  `json:"baseUrl"`
	AccessCode             string  `json:"accessCode"`
}

func normalizePanels(in []Panel) []Panel {
	seen := map[string]bool{}
	out := make([]Panel, 0, len(in))
	for _, p := range in {
		p.Label = strings.TrimSpace(p.Label)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.AccessCode = strings.TrimSpace(p.AccessCode)
		p.Username = strings.TrimSpace(p.Username)
		p.Password = strings.TrimSpace(p.Password)
		p.Token = strings.TrimSpace(p.Token)
		p.AuthMode = strings.ToLower(strings.TrimSpace(p.AuthMode))
		if p.AuthMode != "token" && p.AuthMode != "admin" && p.AuthMode != "code" {
			p.AuthMode = ""
		}
		// 显式认证方式下清掉无关凭据，避免切换后旧字段继续生效。
		switch p.AuthMode {
		case "token":
			p.Username, p.Password, p.AccessCode = "", "", ""
		case "admin":
			p.Token, p.AccessCode = "", ""
		case "code":
			p.Token, p.Username, p.Password = "", "", ""
		}
		if p.BaseURL == "" || seen[p.BaseURL] {
			continue
		}
		if p.Label == "" {
			p.Label = p.BaseURL
		}
		if p.Priority <= 0 {
			p.Priority = len(out) + 1
		}
		if p.Priority > 99 {
			p.Priority = 99
		}
		seen[p.BaseURL] = true
		out = append(out, p)
	}
	return out
}

func migrateFarm(raw rawFarm) FarmConfig {
	farm := FarmConfig{
		Enabled:                true,
		PushOnLogin:            true,
		RepairMinutes:          30,
		AlertFails:             3,
		PanelStrategy:          "broadcast",
		MaxConcurrent:          2,
		CircuitFails:           5,
		CircuitCooldownMinutes: 5,
		Panels:                 normalizePanels(raw.Panels),
	}
	if raw.Enabled != nil {
		farm.Enabled = *raw.Enabled
	}
	if raw.PushOnLogin != nil {
		farm.PushOnLogin = *raw.PushOnLogin
	}
	if raw.RefreshMinutes != nil && *raw.RefreshMinutes > 0 {
		farm.RefreshMinutes = clampRefresh(*raw.RefreshMinutes)
	}
	if raw.StaleHours != nil && *raw.StaleHours > 0 {
		farm.StaleHours = clampStale(*raw.StaleHours)
	}
	if raw.AutoRepair != nil {
		farm.AutoRepair = *raw.AutoRepair
	}
	if raw.RepairMinutes != nil {
		farm.RepairMinutes = clampRepair(*raw.RepairMinutes)
	}
	if raw.Webhook != nil {
		farm.Webhook = strings.TrimSpace(*raw.Webhook)
	}
	if raw.AlertFails != nil {
		farm.AlertFails = clampAlert(*raw.AlertFails)
	}
	farm.PanelStrategy = normalizePanelStrategy(raw.PanelStrategy)
	farm.MaxConcurrent = clampConcurrent(raw.MaxConcurrent)
	farm.CircuitFails = clampCircuitFails(raw.CircuitFails)
	farm.CircuitCooldownMinutes = clampCircuitCooldown(raw.CircuitCooldownMinutes)
	if len(farm.Panels) == 0 && strings.TrimSpace(raw.BaseURL) != "" {
		code := strings.TrimSpace(raw.AccessCode)
		if code == "" {
			code = strings.TrimSpace(os.Getenv("FARM_ACCESS_CODE"))
		}
		farm.Panels = normalizePanels([]Panel{{Label: "FARM1", BaseURL: raw.BaseURL, AccessCode: code}})
	}
	if raw.ActivePanel != nil {
		farm.ActivePanel = *raw.ActivePanel
	}
	if farm.ActivePanel < 0 || farm.ActivePanel >= len(farm.Panels) {
		farm.ActivePanel = 0
	}
	return farm
}

func defaultFarm() FarmConfig {
	return FarmConfig{Enabled: true, PushOnLogin: true, RepairMinutes: 30, AlertFails: 3, PanelStrategy: "broadcast", MaxConcurrent: 2, CircuitFails: 5, CircuitCooldownMinutes: 5, Panels: []Panel{}}
}

// clampRefresh 定时刷新下限 5 分钟：低于该值会不断重启面板会话，容易触发风控。
func clampRefresh(v int) int {
	if v <= 0 {
		return 0
	}
	if v < 5 {
		return 5
	}
	if v > 1440 {
		return 1440
	}
	return v
}

func clampStale(v int) int {
	if v < 0 {
		return 0
	}
	if v > 168 {
		return 168
	}
	return v
}

// clampRepair 自动巡检间隔：5–1440 分钟，0 视为默认 30。
func clampRepair(v int) int {
	if v <= 0 {
		return 30
	}
	if v < 5 {
		return 5
	}
	if v > 1440 {
		return 1440
	}
	return v
}

// clampAlert 连续失败告警阈值：1–10 次，0 视为默认 3。
func clampAlert(v int) int {
	if v <= 0 {
		return 3
	}
	if v > 10 {
		return 10
	}
	return v
}

func normalizePanelStrategy(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "primary_backup", "first_available", "broadcast":
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return "broadcast"
	}
}

func clampConcurrent(v int) int {
	if v <= 0 {
		return 2
	}
	if v > 8 {
		return 8
	}
	return v
}

func clampCircuitFails(v int) int {
	if v <= 0 {
		return 5
	}
	if v > 20 {
		return 20
	}
	return v
}

func clampCircuitCooldown(v int) int {
	if v <= 0 {
		return 5
	}
	if v > 1440 {
		return 1440
	}
	return v
}

func normalizeFarm(farm FarmConfig) FarmConfig {
	farm.PanelStrategy = normalizePanelStrategy(farm.PanelStrategy)
	farm.MaxConcurrent = clampConcurrent(farm.MaxConcurrent)
	farm.CircuitFails = clampCircuitFails(farm.CircuitFails)
	farm.CircuitCooldownMinutes = clampCircuitCooldown(farm.CircuitCooldownMinutes)
	farm.AlertFails = clampAlert(farm.AlertFails)
	farm.RepairMinutes = clampRepair(farm.RepairMinutes)
	farm.RefreshMinutes = clampRefresh(farm.RefreshMinutes)
	farm.StaleHours = clampStale(farm.StaleHours)
	farm.Panels = normalizePanels(farm.Panels)
	farm.ActivePanel = clampIndex(farm.ActivePanel, len(farm.Panels))
	return farm
}

// LoadConfig 读取数据目录 config.json；不存在时写入默认配置。
func LoadConfig(dataDir string) *Config {
	cfg := &Config{Path: ""}
	if dataDir != "" {
		cfg.Path = filepath.Join(dataDir, "config.json")
	}
	if cfg.Path == "" {
		cfg.Farm = defaultFarm()
		return cfg
	}
	raw, err := os.ReadFile(cfg.Path)
	if err != nil {
		cfg.Farm = defaultFarm()
		cfg.saveLocked()
		logf("已生成默认配置：%s（在管理端配置后生效）", cfg.Path)
		return cfg
	}
	tightenPrivateFile(cfg.Path)
	var top struct {
		Farm rawFarm `json:"farm"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		logf("配置解析失败，使用默认值：%v", err)
		cfg.Farm = defaultFarm()
		return cfg
	}
	cfg.Farm = migrateFarm(top.Farm)
	if st, err := os.Stat(cfg.Path); err == nil {
		cfg.modTime = st.ModTime()
	}
	return cfg
}

func (c *Config) Get() FarmConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	farm := c.Farm
	farm.Panels = append([]Panel(nil), c.Farm.Panels...)
	return farm
}

func (c *Config) Set(farm FarmConfig) error {
	farm = normalizeFarm(farm)
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if c.Path != "" {
		data, err := json.MarshalIndent(map[string]any{"farm": farm}, "", "  ")
		if err != nil {
			return err
		}
		if err = backupConfig(c.Path); err != nil {
			return err
		}
		if err = writePrivateFile(c.Path, append(data, '\n')); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.Farm = farm
	if c.Path != "" {
		if st, err := os.Stat(c.Path); err == nil {
			c.modTime = st.ModTime()
		}
	}
	c.mu.Unlock()
	return nil
}

func (c *Config) saveLocked() {
	if c.Path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		logf("创建配置目录失败：%v", err)
		return
	}
	payload := map[string]any{"farm": c.Get()}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		logf("配置序列化失败：%v", err)
		return
	}
	if err := writePrivateFile(c.Path, append(data, '\n')); err != nil {
		logf("配置写入失败：%v", err)
		return
	}
	if st, err := os.Stat(c.Path); err == nil {
		c.mu.Lock()
		c.modTime = st.ModTime()
		c.mu.Unlock()
	}
}

// backupConfig 在覆盖配置前保留最近 5 份副本；不存在旧配置时不创建空备份。
func backupConfig(path string) error {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for i := 4; i >= 1; i-- {
		src := fmt.Sprintf("%s.bak.%d", path, i)
		dst := fmt.Sprintf("%s.bak.%d", path, i+1)
		if _, err := os.Stat(src); err == nil {
			_ = os.Remove(dst)
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	if err := writePrivateFile(path+".bak.1", raw); err != nil {
		return err
	}
	return writePrivateFile(path+".bak", raw)
}

func configBackupPath(path, name string) (string, bool) {
	if path == "" {
		return "", false
	}
	if name == "config.json.bak" {
		return path + ".bak", true
	}
	for i := 1; i <= 5; i++ {
		if name == fmt.Sprintf("config.json.bak.%d", i) {
			return fmt.Sprintf("%s.bak.%d", path, i), true
		}
	}
	return "", false
}

func listConfigBackups(path string) []map[string]any {
	out := []map[string]any{}
	if path == "" {
		return out
	}
	names := []string{"config.json.bak", "config.json.bak.1", "config.json.bak.2", "config.json.bak.3", "config.json.bak.4", "config.json.bak.5"}
	for _, name := range names {
		p, _ := configBackupPath(path, name)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{"name": name, "bytes": st.Size(), "modifiedAt": st.ModTime().UnixMilli()})
	}
	return out
}

func readConfigBackup(path, name string) (FarmConfig, error) {
	p, ok := configBackupPath(path, name)
	if !ok {
		return FarmConfig{}, fmt.Errorf("无效的备份名称")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return FarmConfig{}, err
	}
	var top struct {
		Farm rawFarm `json:"farm"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return FarmConfig{}, fmt.Errorf("备份配置格式错误")
	}
	return migrateFarm(top.Farm), nil
}

// writePrivateFile 写入仅当前用户可读写的文件，并收紧已有文件的权限。
func writePrivateFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".private-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func tightenPrivateFile(path string) {
	if path != "" {
		_ = os.Chmod(path, 0o600)
	}
}

// ReloadIfChanged 外部修改 config.json 后热加载（30 秒轮询，避免依赖 fsnotify）。
func (c *Config) ReloadIfChanged() (bool, error) {
	if c.Path == "" {
		return false, nil
	}
	st, err := os.Stat(c.Path)
	if err != nil {
		return false, err
	}
	c.mu.RLock()
	last := c.modTime
	c.mu.RUnlock()
	if !st.ModTime().After(last) {
		return false, nil
	}
	raw, err := os.ReadFile(c.Path)
	if err != nil {
		return false, err
	}
	tightenPrivateFile(c.Path)
	var top struct {
		Farm rawFarm `json:"farm"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return false, err
	}
	farm := migrateFarm(top.Farm)
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	c.mu.Lock()
	c.Farm = farm
	c.modTime = st.ModTime()
	c.mu.Unlock()
	return true, nil
}

func remarksPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "remarks.json")
}

func loadRemarks(dataDir string) map[string]string {
	out := map[string]string{}
	path := remarksPath(dataDir)
	if path == "" {
		return out
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	tightenPrivateFile(path)
	_ = json.Unmarshal(raw, &out)
	return out
}

// remarksWriteMu keeps concurrent remark updates from overwriting each other.
var remarksWriteMu sync.Mutex

func saveRemark(dataDir string, selfID int64, remark string) error {
	remarksWriteMu.Lock()
	defer remarksWriteMu.Unlock()
	path := remarksPath(dataDir)
	if path == "" {
		return fmt.Errorf("数据目录不可用")
	}
	remarks := loadRemarks(dataDir)
	key := fmt.Sprintf("%d", selfID)
	value := strings.TrimSpace(remark)
	if value == "" {
		delete(remarks, key)
	} else {
		remarks[key] = value
	}
	data, _ := json.MarshalIndent(remarks, "", "  ")
	return writePrivateFile(path, append(data, '\n'))
}
