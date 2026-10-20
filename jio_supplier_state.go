package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func isJioSupplierEnabled(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	defaultState := "on"
	if name == "grizzly" || name == "local" {
		defaultState = "off"
	}
	return getSetting("jio_supplier_enabled_"+name, defaultState) == "on"
}

func invalidateJioSupplierQuotes() {
	autoLowestCostCache.Lock()
	defer autoLowestCostCache.Unlock()
	autoLowestCostCache.lastEvaluated = time.Time{}
	autoLowestCostCache.candidates = nil
	autoLowestCostCache.confirmed = nil
}

func handleAdminJioSupplierEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Enabled  *bool  `json:"enabled"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Enabled == nil {
		respondJSON(w, 400, map[string]any{"success": false, "message": "请指定供应商和启用状态"})
		return
	}
	name := strings.ToLower(strings.TrimSpace(req.Provider))
	if _, err := GetJioProvider(name); err != nil || name == "mock" {
		respondJSON(w, 400, map[string]any{"success": false, "message": "供应商不存在"})
		return
	}
	if name == "grizzly" && *req.Enabled {
		if _, err := loadGrizzlyConfig(GetSupplierConfig(name)); err != nil {
			respondJSON(w, 400, map[string]any{"success": false, "message": err.Error()})
			return
		}
	}
	if name == "local" && *req.Enabled {
		if _, err := localJioPrice(GetSupplierConfig(name)); err != nil {
			respondJSON(w, 400, map[string]any{"success": false, "message": err.Error()})
			return
		}
	}
	state := "off"
	if *req.Enabled {
		state = "on"
	}
	_, err := db.Exec(`INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()`, "jio_supplier_enabled_"+name, state, state)
	if err != nil {
		respondJSON(w, 500, map[string]any{"success": false, "message": "保存供应商状态失败"})
		return
	}
	invalidateJioSupplierQuotes()
	respondJSON(w, 200, map[string]any{"success": true, "enabled": *req.Enabled, "message": fmt.Sprintf("%s 状态已更新", name)})
}
