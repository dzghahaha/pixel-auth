package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// handleAdminFAQs lists the FAQs with optional query search and pagination
func handleAdminFAQs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	page := 1
	pageSize := 20
	if p := r.URL.Query().Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := r.URL.Query().Get("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	offset := (page - 1) * pageSize
	searchTerm := r.URL.Query().Get("query")

	whereClauses := []string{"1=1"}
	var args []interface{}

	if searchTerm != "" {
		whereClauses = append(whereClauses, "(error_code LIKE ? OR error_desc LIKE ? OR solution LIKE ?)")
		likeArg := "%" + searchTerm + "%"
		args = append(args, likeArg, likeArg, likeArg)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	// Get total count
	var total int
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM faqs WHERE %s", whereSQL)
	err := db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		log.Printf("Query faqs count error: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询常见问题总数失败",
		})
		return
	}

	// Get paginated records
	selectQuery := fmt.Sprintf("SELECT id, error_code, error_desc, solution, created_at, updated_at FROM faqs WHERE %s ORDER BY id DESC LIMIT ? OFFSET ?", whereSQL)
	queryArgs := append(args, pageSize, offset)

	rows, err := db.Query(selectQuery, queryArgs...)
	if err != nil {
		log.Printf("Query faqs error: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询常见问题列表失败",
		})
		return
	}
	defer rows.Close()

	type FAQ struct {
		ID        int64     `json:"id"`
		ErrorCode string    `json:"error_code"`
		ErrorDesc string    `json:"error_desc"`
		Solution  string    `json:"solution"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
	}

	var faqs []FAQ
	for rows.Next() {
		var f FAQ
		if err := rows.Scan(&f.ID, &f.ErrorCode, &f.ErrorDesc, &f.Solution, &f.CreatedAt, &f.UpdatedAt); err == nil {
			faqs = append(faqs, f)
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"faqs":    faqs,
		"total":   total,
	})
}

// handleAdminFAQsCreate creates a new FAQ entry
func handleAdminFAQsCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ErrorCode string `json:"error_code"`
		ErrorDesc string `json:"error_desc"`
		Solution  string `json:"solution"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数格式错误",
		})
		return
	}

	req.ErrorCode = strings.TrimSpace(req.ErrorCode)
	req.ErrorDesc = strings.TrimSpace(req.ErrorDesc)
	req.Solution = strings.TrimSpace(req.Solution)

	if req.ErrorCode == "" || req.ErrorDesc == "" || req.Solution == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "错误码、错误描述和解决方案不能为空",
		})
		return
	}

	now := time.Now()
	_, err := db.Exec("INSERT INTO faqs (error_code, error_desc, solution, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		req.ErrorCode, req.ErrorDesc, req.Solution, now, now)

	if err != nil {
		log.Printf("Insert faq error: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "错误码已存在或数据库写入失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "新增常见问题成功",
	})
}

// handleAdminFAQsUpdate updates an existing FAQ entry
func handleAdminFAQsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID        int64  `json:"id"`
		ErrorCode string `json:"error_code"`
		ErrorDesc string `json:"error_desc"`
		Solution  string `json:"solution"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数格式错误",
		})
		return
	}

	req.ErrorCode = strings.TrimSpace(req.ErrorCode)
	req.ErrorDesc = strings.TrimSpace(req.ErrorDesc)
	req.Solution = strings.TrimSpace(req.Solution)

	if req.ID <= 0 || req.ErrorCode == "" || req.ErrorDesc == "" || req.Solution == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数不合法",
		})
		return
	}

	now := time.Now()
	_, err := db.Exec("UPDATE faqs SET error_code = ?, error_desc = ?, solution = ?, updated_at = ? WHERE id = ?",
		req.ErrorCode, req.ErrorDesc, req.Solution, now, req.ID)

	if err != nil {
		log.Printf("Update faq error: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "错误码可能已存在或数据库更新失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "更新常见问题成功",
	})
}

// handleAdminFAQsDelete deletes an FAQ entry
func handleAdminFAQsDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		ID int64 `json:"id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数格式错误",
		})
		return
	}

	if req.ID <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数不合法",
		})
		return
	}

	_, err := db.Exec("DELETE FROM faqs WHERE id = ?", req.ID)
	if err != nil {
		log.Printf("Delete faq error: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "删除常见问题失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "删除常见问题成功",
	})
}
