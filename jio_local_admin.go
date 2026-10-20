package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

func parseLocalJioLinks(input string) ([]string, int, error) {
	seen := make(map[string]bool)
	links := make([]string, 0)
	duplicates, count := 0, 0
	for i, line := range strings.Split(input, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		count++
		if count > 1000 {
			return nil, 0, errors.New("每次最多录入 1000 条链接")
		}
		u, err := url.ParseRequestURI(line)
		if err != nil || len(line) > 4096 || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || strings.ContainsAny(line, "\r\t ") {
			return nil, 0, fmt.Errorf("第 %d 行不是有效的 HTTP/HTTPS 链接（最多 4096 字节）", i+1)
		}
		if seen[line] {
			duplicates++
			continue
		}
		seen[line] = true
		links = append(links, line)
	}
	if count == 0 {
		return nil, 0, errors.New("请至少输入一条兑换链接")
	}
	return links, duplicates, nil
}

func importLocalJioLinks(ctx context.Context, input string) (int, int, error) {
	links, duplicates, err := parseLocalJioLinks(input)
	if err != nil {
		return 0, 0, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, errors.New("创建导入事务失败")
	}
	defer tx.Rollback()
	added := 0
	for _, link := range links {
		hash := sha256.Sum256([]byte(link))
		_, err := tx.ExecContext(ctx, `INSERT INTO jio_links (link,link_hash,created_at) VALUES (?,?,NOW())`, link, hash[:])
		if err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				duplicates++
				continue
			}
			return 0, 0, errors.New("导入失败，本批次未保存")
		}
		added++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, errors.New("确认导入失败，可重新提交，重复链接会自动跳过")
	}
	invalidateJioSupplierQuotes()
	return added, duplicates, nil
}

func handleAdminJioLocalLinks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		var req struct {
			Links string `json:"links"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSON(w, 400, map[string]any{"success": false, "message": "录入格式错误或请求超过 8 MB"})
			return
		}
		if _, _, err := parseLocalJioLinks(req.Links); err != nil {
			respondJSON(w, 400, map[string]any{"success": false, "message": err.Error()})
			return
		}
		added, duplicates, err := importLocalJioLinks(r.Context(), req.Links)
		if err != nil {
			respondJSON(w, 500, map[string]any{"success": false, "message": err.Error()})
			return
		}
		respondJSON(w, 200, map[string]any{"success": true, "added": added, "duplicates": duplicates})
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", 405)
		return
	}
	status := r.URL.Query().Get("status")
	if status != "" && status != "available" && status != "issued" {
		respondJSON(w, 400, map[string]any{"success": false, "message": "无效库存状态"})
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 1000000 {
		page = 1000000
	}
	var available, issued int
	err := db.QueryRowContext(r.Context(), `SELECT COALESCE(SUM(status='available'),0),COALESCE(SUM(status='issued'),0) FROM jio_links`).Scan(&available, &issued)
	if err != nil {
		respondJSON(w, 500, map[string]any{"success": false, "message": "读取库存统计失败"})
		return
	}
	total := available + issued
	if status == "available" {
		total = available
	} else if status == "issued" {
		total = issued
	}
	pages := max(1, (total+49)/50)
	page = min(page, pages)
	rows, err := db.QueryContext(r.Context(), `SELECT id,link,status,order_id,created_at,issued_at FROM jio_links WHERE (?='' OR status=?) ORDER BY id DESC LIMIT 50 OFFSET ?`, status, status, (page-1)*50)
	if err != nil {
		respondJSON(w, 500, map[string]any{"success": false, "message": "读取链接列表失败"})
		return
	}
	defer rows.Close()
	items := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var link, state string
		var order sql.NullString
		var created time.Time
		var issuedAt sql.NullTime
		if err = rows.Scan(&id, &link, &state, &order, &created, &issuedAt); err != nil {
			break
		}
		issuedText := ""
		if issuedAt.Valid {
			issuedText = issuedAt.Time.Format("2006-01-02 15:04:05")
		}
		items = append(items, map[string]any{"id": id, "link": link, "status": state, "order_id": order.String, "created_at": created.Format("2006-01-02 15:04:05"), "issued_at": issuedText})
	}
	if err != nil || rows.Err() != nil {
		respondJSON(w, 500, map[string]any{"success": false, "message": "读取链接列表失败"})
		return
	}
	respondJSON(w, 200, map[string]any{"success": true, "items": items, "available": available, "issued": issued, "total": total, "page": page, "pages": pages})
}
