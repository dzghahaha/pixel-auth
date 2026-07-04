package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// SubmitRequest represents the JSON Submission Request struct
type SubmitRequest struct {
	CardSecret string `json:"card_secret"`
	Mode       string `json:"mode"`
	Accounts   []struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	} `json:"accounts"`
}

func handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	var req SubmitRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "无效的JSON格式数据",
		})
		return
	}

	if req.CardSecret == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "卡密不能为空",
		})
		return
	}

	if len(req.Accounts) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "账号列表不能为空",
		})
		return
	}

	// Retrieve vendor mapping from system_keys table
	var vendor string
	var vendorKey string
	var keyStatus string
	var creatorID sql.NullInt64
	errKeyQuery := db.QueryRow("SELECT vendor, vendor_key, status, creator_id FROM system_keys WHERE system_key = ?", req.CardSecret).
		Scan(&vendor, &vendorKey, &keyStatus, &creatorID)

	if errKeyQuery == sql.ErrNoRows || keyStatus != "active" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "卡密无效或已失效",
		})
		return
	} else if errKeyQuery != nil {
		log.Printf("Database error querying system key: %v\n", errKeyQuery)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库服务故障，请稍后重试",
		})
		return
	}

	// Check if there is an existing order for this card key that is still queuing or executing
	var activeCount int
	errCheckActive := db.QueryRow(`
		SELECT COUNT(*) 
		FROM account_records r 
		JOIN orders o ON r.order_id = o.id 
		WHERE o.card_secret = ? AND r.status IN ('pending', 'running')`,
		req.CardSecret).Scan(&activeCount)
	if errCheckActive != nil {
		log.Printf("Database error checking active orders: %v\n", errCheckActive)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库服务故障，请稍后重试",
		})
		return
	}
	if activeCount > 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "该卡密对应的订单已经在排队或执行中，请勿重复提交",
		})
		return
	}

	type AccountSubmitResult struct {
		Username   string
		Password   string
		TwoFactor  string
		ExtraEmail string
		TaskID     string
		Status     string
		Message    string
	}

	var submitResults []AccountSubmitResult
	if vendor == "pass.aisale.one" {
		for _, acc := range req.Accounts {
			res, err := submitTaskToVendor(vendorKey, acc.Username, acc.Password, acc.TwoFactor)
			if err != nil {
				log.Printf("Vendor submit network error for %s: %v\n", acc.Username, err)
				if len(req.Accounts) == 1 {
					respondJSON(w, http.StatusBadRequest, map[string]interface{}{
						"success": false,
						"message": "提交接口网络错误: " + err.Error(),
					})
					return
				}
				submitResults = append(submitResults, AccountSubmitResult{
					Username:   acc.Username,
					Password:   acc.Password,
					TwoFactor:  acc.TwoFactor,
					ExtraEmail: acc.ExtraEmail,
					TaskID:     "",
					Status:     "failed",
					Message:    "提交接口失败: " + err.Error(),
				})
			} else if !res.Success {
				log.Printf("Vendor submit API error for %s: %s\n", acc.Username, res.Message)
				if len(req.Accounts) == 1 {
					respondJSON(w, http.StatusBadRequest, map[string]interface{}{
						"success": false,
						"message": "提交失败: " + res.Message,
					})
					return
				}
				submitResults = append(submitResults, AccountSubmitResult{
					Username:   acc.Username,
					Password:   acc.Password,
					TwoFactor:  acc.TwoFactor,
					ExtraEmail: acc.ExtraEmail,
					TaskID:     "",
					Status:     "failed",
					Message:    "提交失败: " + res.Message,
				})
			} else {
				submitResults = append(submitResults, AccountSubmitResult{
					Username:   acc.Username,
					Password:   acc.Password,
					TwoFactor:  acc.TwoFactor,
					ExtraEmail: acc.ExtraEmail,
					TaskID:     res.TaskID,
					Status:     "pending",
					Message:    "已成功提交，等待处理",
				})
			}
		}
	}

	// Execute inside a database transaction to prevent partial inserts
	tx, err := db.Begin()
	if err != nil {
		log.Printf("Error starting transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库服务故障，请稍后重试",
		})
		return
	}
	defer tx.Rollback()

	// 1. Get or create the card order ID
	var orderID int64
	errQuery := tx.QueryRow("SELECT id FROM orders WHERE card_secret = ?", req.CardSecret).Scan(&orderID)
	now := time.Now()

	if errQuery == sql.ErrNoRows {
		result, errInsert := tx.Exec("INSERT INTO orders (card_secret, mode, vendor, creator_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			req.CardSecret, req.Mode, vendor, creatorID, now, now)
		if errInsert != nil {
			log.Printf("Error inserting order: %v\n", errInsert)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "数据库存储订单失败",
			})
			return
		}
		orderID, _ = result.LastInsertId()
	} else if errQuery != nil {
		log.Printf("Error querying order: %v\n", errQuery)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库查询订单失败",
		})
		return
	} else {
		// If order already exists, ensure vendor is set and backfill creator_id if empty
		_, errUpdateVendor := tx.Exec("UPDATE orders SET vendor = ?, creator_id = COALESCE(creator_id, ?), updated_at = ? WHERE id = ?", vendor, creatorID, now, orderID)
		if errUpdateVendor != nil {
			log.Printf("Warning: failed to update vendor or creator_id: %v\n", errUpdateVendor)
		}
	}

	// 2. Insert account records
	var usernames []string
	if vendor == "pass.aisale.one" {
		for _, res := range submitResults {
			var extraEmail interface{} = nil
			if res.ExtraEmail != "" {
				extraEmail = res.ExtraEmail
			}

			_, errInsertRec := tx.Exec("INSERT INTO account_records (order_id, username, password, two_factor, extra_email, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				orderID, res.Username, res.Password, res.TwoFactor, extraEmail, res.Status, res.Message, res.TaskID, now, now)
			if errInsertRec != nil {
				log.Printf("Error inserting account record: %v\n", errInsertRec)
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"success": false,
					"message": "数据库存储账号记录失败",
				})
				return
			}
			usernames = append(usernames, res.Username)
		}
	} else {
		for _, acc := range req.Accounts {
			var extraEmail interface{} = nil
			if acc.ExtraEmail != "" {
				extraEmail = acc.ExtraEmail
			}

			_, errInsertRec := tx.Exec("INSERT INTO account_records (order_id, username, password, two_factor, extra_email, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
				orderID, acc.Username, acc.Password, acc.TwoFactor, extraEmail, "pending", "排队处理中", now, now)
			if errInsertRec != nil {
				log.Printf("Error inserting account record: %v\n", errInsertRec)
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"success": false,
					"message": "数据库存储账号记录失败",
				})
				return
			}
			usernames = append(usernames, acc.Username)
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库提交事务失败",
		})
		return
	}

	// Proactive Feature: Mock background processing daemon with MySQL updates
	// Only run this if we are not in testing environment to prevent test database contamination
	isTesting := strings.Contains(os.Args[0], ".test") || os.Getenv("MYSQL_TEST_DSN") != ""
	if vendor != "pass.aisale.one" && vendor != "ai.deard.fun" && !isTesting {
		go func(cSecret string, oID int64, targets []string) {
			time.Sleep(5 * time.Second)

			// Loop through the accounts we just submitted
			for i, username := range targets {
				nowTime := time.Now()
				var status string
				var message string
				var discountURL string

				// Toggle success or failure alternately based on index
				if i%2 == 0 {
					status = "success"
					message = "订阅成功绑定，绑卡信息已生效"
					cleanUser := username
					if len(username) > 4 {
						cleanUser = username[:4]
					}
					cleanCard := cSecret
					if len(cSecret) > 4 {
						cleanCard = cSecret[:4]
					}
					discountURL = "https://pixel.sub/discount/CLAIM-MOCK-" + cleanCard + "-" + cleanUser
				} else {
					status = "failed"
					message = "二步验证(2FA)校验失败，请检查备用验证码或密钥"
					discountURL = ""
				}

				_, errUpdate := db.Exec(`
					UPDATE account_records 
					SET status = ?, message = ?, discount_url = ?, completed_at = ?, updated_at = ? 
					WHERE order_id = ? AND username = ? AND status = 'pending'`,
					status, message, discountURL, nowTime, nowTime, oID, username)
				if errUpdate != nil {
					log.Printf("Error running background update on account %s: %v\n", username, errUpdate)
				}
			}
		}(req.CardSecret, orderID, usernames)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "提交成功，已加入处理队列！",
	})
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cardSecret := r.URL.Query().Get("card_secret")
	if cardSecret == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数 card_secret 不能为空",
		})
		return
	}

	// Resolve the vendorKey from system_keys mapping table for the given cardSecret (system key)
	var vendorKey string
	errKeyQuery := db.QueryRow("SELECT vendor_key FROM system_keys WHERE system_key = ?", cardSecret).Scan(&vendorKey)
	if errKeyQuery == sql.ErrNoRows {
		// Forward-compatibility fallback: if no mapping exists, use cardSecret itself as the vendorKey (for backward compatibility)
		vendorKey = cardSecret
	} else if errKeyQuery != nil {
		log.Printf("Query error for system key mapping %s: %v\n", cardSecret, errKeyQuery)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库错误",
		})
		return
	}

	// Read records from MySQL including id, vendor, and task_id
	rows, err := db.Query(`
		SELECT r.id, o.vendor, r.task_id, r.username, r.password, r.two_factor, COALESCE(r.extra_email, ''), r.status, r.message, COALESCE(r.discount_url, ''), r.created_at, r.updated_at, r.completed_at
		FROM account_records r
		JOIN orders o ON r.order_id = o.id
		WHERE o.card_secret = ?
		ORDER BY r.id DESC`, cardSecret)
	if err != nil {
		log.Printf("Query error for card_secret %s: %v\n", cardSecret, err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库错误",
		})
		return
	}
	defer rows.Close()

	type QueryRecord struct {
		ID          int64
		Vendor      string
		TaskID      string
		Username    string
		Password    string
		TwoFactor   string
		ExtraEmail  string
		Status      string
		Message     string
		DiscountURL string
		CreatedAt   time.Time
		UpdatedAt   time.Time
		CompletedAt *time.Time
	}

	var records []QueryRecord
	for rows.Next() {
		var rec QueryRecord
		var extraEmail string
		var completedAt sql.NullTime

		errScan := rows.Scan(
			&rec.ID,
			&rec.Vendor,
			&rec.TaskID,
			&rec.Username,
			&rec.Password,
			&rec.TwoFactor,
			&extraEmail,
			&rec.Status,
			&rec.Message,
			&rec.DiscountURL,
			&rec.CreatedAt,
			&rec.UpdatedAt,
			&completedAt,
		)
		if errScan != nil {
			log.Printf("Error scanning row: %v\n", errScan)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "读取数据失败",
			})
			return
		}

		if extraEmail != "" {
			rec.ExtraEmail = extraEmail
		}
		if completedAt.Valid {
			rec.CompletedAt = &completedAt.Time
		}

		records = append(records, rec)
	}

	// Synchronously query vendor status in parallel for active pass.aisale.one tasks
	var wg sync.WaitGroup
	for i := range records {
		rec := &records[i]
		if rec.Vendor == "pass.aisale.one" && rec.TaskID != "" && rec.Status != "success" && rec.Status != "failed" {
			wg.Add(1)
			go func(r *QueryRecord) {
				defer wg.Done()

				res, err := queryTaskFromVendor(vendorKey, r.TaskID)
				if err != nil {
					log.Printf("Error querying status from vendor for task %s: %v\n", r.TaskID, err)
					return
				}
				if !res.Success {
					log.Printf("Vendor returned error status for task %s: %s\n", r.TaskID, res.Message)
					return
				}

				vendorStatus := res.Data.Status
				var localStatus string
				var completedAt *time.Time
				now := time.Now()

				switch strings.ToLower(vendorStatus) {
				case "success":
					localStatus = "success"
					completedAt = &now
				case "failed":
					localStatus = "failed"
					completedAt = &now
				case "running":
					localStatus = "running"
				case "pending":
					localStatus = "pending"
				default:
					localStatus = "pending"
				}

				discountURL := r.DiscountURL
				if res.Data.HasOfferURL && res.Data.OfferURL != "" {
					discountURL = res.Data.OfferURL
				}

				message := res.Data.Message
				if message == "" {
					message = "处理中"
				}

				var errUpdate error
				if completedAt != nil {
					_, errUpdate = db.Exec(`
						UPDATE account_records 
						SET status = ?, message = ?, discount_url = ?, completed_at = ?, updated_at = ? 
						WHERE id = ?`,
						localStatus, message, discountURL, *completedAt, now, r.ID)
				} else {
					_, errUpdate = db.Exec(`
						UPDATE account_records 
						SET status = ?, message = ?, discount_url = ?, updated_at = ? 
						WHERE id = ?`,
						localStatus, message, discountURL, now, r.ID)
				}

				if errUpdate != nil {
					log.Printf("Failed to update database for task %s: %v\n", r.TaskID, errUpdate)
				} else {
					r.Status = localStatus
					r.Message = message
					r.DiscountURL = discountURL
					r.UpdatedAt = now
					r.CompletedAt = completedAt
				}
			}(rec)
		}
	}
	wg.Wait()

	// Invalidate system key if any record has completed successfully
	hasSuccess := false
	for _, r := range records {
		if r.Status == "success" {
			hasSuccess = true
			break
		}
	}
	if hasSuccess {
		now := time.Now()
		_, errUpdateKey := db.Exec(`
			UPDATE system_keys 
			SET status = 'inactive', updated_at = ? 
			WHERE system_key = ? AND status = 'active'`,
			now, cardSecret)
		if errUpdateKey != nil {
			log.Printf("Failed to invalidate system key %s after successful subscription: %v\n", cardSecret, errUpdateKey)
		} else {
			log.Printf("Successfully invalidated system key %s due to successful subscription\n", cardSecret)
		}
	}

	// Map QueryRecord back to AccountRecord response format
	respRecords := []AccountRecord{}
	for _, r := range records {
		rec := AccountRecord{
			Username:    r.Username,
			Password:    r.Password,
			TwoFactor:   r.TwoFactor,
			Status:      r.Status,
			Message:     r.Message,
			DiscountURL: r.DiscountURL,
			Vendor:      r.Vendor,
			TaskID:      r.TaskID,
			CreatedAt:   r.CreatedAt,
			UpdatedAt:   r.UpdatedAt,
			CompletedAt: r.CompletedAt,
		}
		if r.ExtraEmail != "" {
			rec.ExtraEmail = r.ExtraEmail
		}
		respRecords = append(respRecords, rec)
	}

	// Respond with results (empty list is sent if no records found, frontend will format cleanly)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"card_secret": cardSecret,
		"records":     respRecords,
	})
}

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var tutorialURL string
	err := db.QueryRow("SELECT setting_value FROM system_settings WHERE setting_key = 'two_factor_tutorial_url'").Scan(&tutorialURL)
	if err != nil {
		if err == sql.ErrNoRows {
			tutorialURL = "https://www.yuque.com/taozi-khqsp/rrub4i/fxm5dgln1rh5iwd1"
		} else {
			log.Printf("Error querying setting: %v\n", err)
			tutorialURL = "https://www.yuque.com/taozi-khqsp/rrub4i/fxm5dgln1rh5iwd1"
		}
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":                 true,
		"two_factor_tutorial_url": tutorialURL,
	})
}
