package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

const coldAccountBudget = 250 * time.Millisecond
const coldPanelBudget = 75 * time.Millisecond

// Network/IPC waits never hold cacheMu. Existing snapshots remain readable while
// one worker refreshes each source; a slow remote panel cannot stall the page.
func (a *Admin) botsCached() []Bot {
	a.cacheMu.Lock()
	cached := a.bots
	fresh := cached != nil && time.Since(a.botsAt) < 2*time.Second
	if !fresh && a.botsLoading == nil && time.Since(a.botsAttempt) >= 2*time.Second {
		done := make(chan struct{})
		a.botsLoading = done
		a.botsAttempt = time.Now()
		go func() {
			bots, err := a.bridge.botList()
			a.cacheMu.Lock()
			defer a.cacheMu.Unlock()
			defer close(done)
			a.botsLoading = nil
			if err != nil {
				a.botsError = a.bridge.redact(err.Error())
				return
			}
			if bots == nil {
				bots = []Bot{}
			}
			a.bots = append([]Bot{}, bots...)
			a.botsError = ""
			a.botsAt = time.Now()
		}()
	}
	done := a.botsLoading
	a.cacheMu.Unlock()
	if done != nil {
		budget := 15 * time.Millisecond
		if cached == nil {
			budget = coldAccountBudget
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	return append([]Bot{}, a.bots...)
}
func panelCacheKey(f FarmConfig) string {
	raw, _ := json.Marshal(struct {
		Enabled bool
		Panels  []Panel
	}{f.Enabled, f.Panels})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (a *Admin) panelsCached() []map[string]any {
	farm := a.cfg.Get()
	if !farm.Enabled {
		return []map[string]any{}
	}
	key := panelCacheKey(farm)
	a.cacheMu.Lock()
	if a.panelKey != key {
		a.panelKey = key
		a.panelEpoch++
		a.panels = nil
		a.panelAt = time.Time{}
		a.panelAttempt = time.Time{}
	}
	if len(farm.Panels) == 0 {
		a.panels = []map[string]any{}
		a.panelAt = time.Now()
		a.cacheMu.Unlock()
		return []map[string]any{}
	}
	fresh := a.panels != nil && time.Since(a.panelAt) < 5*time.Second
	if !fresh && a.panelsLoading == nil && time.Since(a.panelAttempt) >= time.Second {
		done := make(chan struct{})
		a.panelsLoading = done
		a.panelAttempt = time.Now()
		epoch := a.panelEpoch
		// Copy-on-write: snapshots already handed to readers are never mutated.
		if a.panels == nil {
			a.panels = make([]map[string]any, len(farm.Panels))
			for i, p := range farm.Panels {
				a.panels[i] = pendingPanel(p)
			}
		}
		next := clonePanelStatus(a.panels)
		for _, p := range next {
			p["refreshing"] = true
		}
		a.panels = next
		go func() {
			a.bridge.PanelStatusEach(func(i int, p map[string]any) {
				currentKey := panelCacheKey(a.cfg.Get())
				a.cacheMu.Lock()
				defer a.cacheMu.Unlock()
				if a.panelEpoch != epoch || a.panelKey != key || key != currentKey || i >= len(a.panels) {
					return
				}
				next := append([]map[string]any{}, a.panels...)
				item := clonePanelValue(p).(map[string]any)
				item["refreshing"] = false
				next[i] = item
				a.panels = next
			})
			a.cacheMu.Lock()
			defer a.cacheMu.Unlock()
			defer close(done)
			a.panelsLoading = nil
			if a.panelEpoch == epoch && a.panelKey == key {
				a.panelAt = time.Now()
			}
		}()
	}
	cold := a.panelAt.IsZero()
	done := a.panelsLoading
	a.cacheMu.Unlock()
	if done != nil {
		budget := 25 * time.Millisecond
		if cold {
			budget = coldPanelBudget
		}
		timer := time.NewTimer(budget)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
	a.cacheMu.Lock()
	snapshot := a.panels
	refreshing := a.panelsLoading != nil

	a.cacheMu.Unlock()
	if snapshot == nil {
		out := make([]map[string]any, 0, len(farm.Panels))
		for _, p := range farm.Panels {
			out = append(out, map[string]any{"label": p.Label, "baseUrl": p.BaseURL, "priority": p.Priority, "mode": p.mode(), "username": p.Username, "hasCode": p.AccessCode != "", "hasPassword": p.Password != "", "hasToken": p.Token != "", "hasAuth": p.hasAuth(), "checking": true, "refreshing": refreshing, "checkedAt": int64(0), "ok": false, "error": "", "accounts": []any{}})
		}
		return out
	}
	out := clonePanelStatus(snapshot)
	for _, p := range out {
		if !refreshing {
			p["refreshing"] = false
		}
		checked, _ := p["checkedAt"].(int64)
		p["stale"] = checked > 0 && time.Since(time.UnixMilli(checked)) > 5*time.Second
	}
	return out
}
func (a *Admin) invalidatePanels() {
	a.cacheMu.Lock()
	a.panelEpoch++
	a.panelAt = time.Time{}
	a.panelAttempt = time.Time{}
	a.cacheMu.Unlock()
}
func (a *Admin) sourceStatus() map[string]any {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()
	return map[string]any{"accountsAt": max(int64(0), a.botsAt.UnixMilli()), "accountsError": a.botsError, "accountsStale": a.botsAt.IsZero() || a.botsError != "", "accountsRefreshing": a.botsLoading != nil, "panelsAt": max(int64(0), a.panelAt.UnixMilli()), "panelsRefreshing": a.panelsLoading != nil}
}

func pendingPanel(p Panel) map[string]any {
	return map[string]any{"label": p.Label, "baseUrl": p.BaseURL, "priority": p.Priority, "mode": p.mode(), "username": p.Username, "hasCode": p.AccessCode != "", "hasPassword": p.Password != "", "hasToken": p.Token != "", "hasAuth": p.hasAuth(), "checking": true, "refreshing": true, "checkedAt": int64(0), "ok": false, "error": "", "accounts": []any{}}
}
