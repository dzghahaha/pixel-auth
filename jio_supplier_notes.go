package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleAdminJioSupplierNote updates only the note, keeping credentials and product settings intact.
func handleAdminJioSupplierNote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false, "message": "仅支持 POST 请求"})
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "备注参数无效"})
		return
	}
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if _, err := GetJioProvider(provider); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "供应商不存在"})
		return
	}
	note := strings.TrimSpace(req.Note)
	if len([]rune(note)) > 500 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "备注最多 500 个字符"})
		return
	}
	key := "jio_supplier_note_" + provider
	if _, err := db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()", key, note, note); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": "保存备注失败，请重试"})
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"success": true, "note": note, "message": "备注已保存"})
}
