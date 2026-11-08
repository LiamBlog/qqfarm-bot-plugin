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
type Panel struct {
	Label      string `json:"label"`
	BaseURL    string `json:"baseUrl"`
	AccessCode string `json:"accessCode"`
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
}

type Config struct {
	Farm    FarmConfig
	Path    string
	modTime time.Time
	mu      sync.RWMutex
}

// 兼容旧版单面板字段（baseUrl / accessCode）
type rawFarm struct {
	Enabled        *bool   `json:"enabled"`
	PushOnLogin    *bool   `json:"pushOnLogin"`
	RefreshMinutes *int    `json:"refreshMinutes"`
	StaleHours     *int    `json:"staleHours"`
	AutoRepair     *bool   `json:"autoRepair"`
	RepairMinutes  *int    `json:"repairMinutes"`
	Panels         []Panel `json:"panels"`
	ActivePanel    *int    `json:"activePanel"`
	BaseURL        string  `json:"baseUrl"`
	AccessCode     string  `json:"accessCode"`
}

func normalizePanels(in []Panel) []Panel {
	seen := map[string]bool{}
	out := make([]Panel, 0, len(in))
	for _, p := range in {
		p.Label = strings.TrimSpace(p.Label)
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.AccessCode = strings.TrimSpace(p.AccessCode)
		if p.BaseURL == "" || seen[p.BaseURL] {
			continue
		}
		if p.Label == "" {
			p.Label = p.BaseURL
		}
		seen[p.BaseURL] = true
		out = append(out, p)
	}
	return out
}

func migrateFarm(raw rawFarm) FarmConfig {
	farm := FarmConfig{
		Enabled:       true,
		PushOnLogin:   true,
		RepairMinutes: 30,
		Panels:        normalizePanels(raw.Panels),
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
	return FarmConfig{Enabled: true, PushOnLogin: true, RepairMinutes: 30, Panels: []Panel{}}
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

func (c *Config) Set(farm FarmConfig) {
	c.mu.Lock()
	c.Farm = farm
	c.mu.Unlock()
	c.saveLocked()
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
	if err := os.WriteFile(c.Path, append(data, '\n'), 0o644); err != nil {
		logf("配置写入失败：%v", err)
		return
	}
	if st, err := os.Stat(c.Path); err == nil {
		c.mu.Lock()
		c.modTime = st.ModTime()
		c.mu.Unlock()
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
	var top struct {
		Farm rawFarm `json:"farm"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return false, err
	}
	farm := migrateFarm(top.Farm)
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
	_ = json.Unmarshal(raw, &out)
	return out
}

func saveRemark(dataDir string, selfID int64, remark string) error {
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
	return os.WriteFile(path, append(data, '\n'), 0o644)
}
