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
		go func() {
			panels := a.bridge.PanelStatus()
			at := time.Now()
			for _, p := range panels {
				p["checkedAt"] = at.UnixMilli()
			}
			currentKey := panelCacheKey(a.cfg.Get())
			a.cacheMu.Lock()
			defer a.cacheMu.Unlock()
			defer close(done)
			a.panelsLoading = nil
			if a.panelEpoch == epoch && a.panelKey == key && key == currentKey {
				a.panels = panels
				a.panelAt = at
			}
		}()
	}
	cold := a.panels == nil
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
	at := a.panelAt
	a.cacheMu.Unlock()
	if snapshot == nil {
		out := make([]map[string]any, 0, len(farm.Panels))
		for _, p := range farm.Panels {
			out = append(out, map[string]any{"label": p.Label, "baseUrl": p.BaseURL, "mode": p.mode(), "username": p.Username, "hasCode": p.AccessCode != "", "hasPassword": p.Password != "", "hasToken": p.Token != "", "hasAuth": p.hasAuth(), "checking": true, "refreshing": refreshing, "checkedAt": int64(0), "ok": false, "error": "", "accounts": []any{}})
		}
		return out
	}
	out := clonePanelStatus(snapshot)
	for _, p := range out {
		p["refreshing"] = refreshing
		p["stale"] = time.Since(at) > 5*time.Second
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
