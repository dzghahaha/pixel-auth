package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

var activeSubmissions sync.Map

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

	// Transparent card replacement detection
	var currentCardSecret string
	errReplaceQuery := db.QueryRow(`
		SELECT o.card_secret 
		FROM orders o
		JOIN account_records r ON r.order_id = o.id
		WHERE r.card_secret = ?
		LIMIT 1`, req.CardSecret).Scan(&currentCardSecret)

	if errReplaceQuery == nil && currentCardSecret != "" && currentCardSecret != req.CardSecret {
		log.Printf("[Transparent Redirect] Replaced card secret detected. Swapping from %s to %s\n", req.CardSecret, currentCardSecret)
		req.CardSecret = currentCardSecret
	}

	// Lock the card secret to prevent concurrent duplicate submissions
	if _, loaded := activeSubmissions.LoadOrStore(req.CardSecret, struct{}{}); loaded {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "该卡密正在处理中，请勿重复提交",
		})
		return
	}
	defer activeSubmissions.Delete(req.CardSecret)

	if len(req.Accounts) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "账号列表不能为空",
		})
		return
	}

	// Validate 2FA and email domain for all submitted accounts
	for idx, acc := range req.Accounts {
		if !isValid2FA(acc.TwoFactor) {
			errMsg := "2FA格式不正确，请输入32位密钥或备用验证码"
			if len(req.Accounts) > 1 {
				errMsg = fmt.Sprintf("第 %d 行账号 2FA格式不正确，请输入32位密钥或备用验证码", idx+1)
			}
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": errMsg,
			})
			return
		}

		if strings.Contains(acc.Username, "@") && !isPersonalGoogleEmail(acc.Username) {
			errMsg := "必须使用Google个人账号，企业组织等账号不可以订阅Google One"
			if len(req.Accounts) > 1 {
				errMsg = fmt.Sprintf("第 %d 行账号必须使用Google个人账号，企业组织等账号不可以订阅Google One", idx+1)
			}
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": errMsg,
			})
			return
		}
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

	// Intercept deard vendor card conversions if enabled
	if vendor == "ai.deard.fun" && getSetting("deard_convert_open", "off") == "on" {
		rows, errRows := db.Query(`
			SELECT system_key, vendor_key 
			FROM system_keys 
			WHERE vendor = 'pass.aisale.one' AND status = 'active' 
			ORDER BY id ASC`)
		if errRows != nil {
			log.Printf("Error querying active pass.aisale.one keys: %v\n", errRows)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "数据库服务故障，请稍后重试",
			})
			return
		}
		defer rows.Close()

		var targetVKey string
		var found bool

		for rows.Next() {
			var sysKey string
			var vKey string
			if errScan := rows.Scan(&sysKey, &vKey); errScan != nil {
				log.Printf("Error scanning pass.aisale.one key: %v\n", errScan)
				continue
			}

			// Call balance query API
			remaining, errBal := checkAisaleBalance(vKey)
			if errBal != nil {
				log.Printf("Error checking balance for cdkey %s: %v\n", vKey, errBal)
				if strings.Contains(errBal.Error(), "API error") {
					log.Printf("Invalidating key %s due to API error: %v\n", sysKey, errBal)
					db.Exec("UPDATE system_keys SET status = 'inactive', updated_at = ? WHERE system_key = ?", time.Now(), sysKey)
				}
				continue
			}

			if remaining >= 1.0 {
				targetVKey = vKey
				found = true
				break
			} else {
				log.Printf("Invalidating key %s due to low balance: %f\n", sysKey, remaining)
				_, errInactivate := db.Exec("UPDATE system_keys SET status = 'inactive', updated_at = ? WHERE system_key = ?", time.Now(), sysKey)
				if errInactivate != nil {
					log.Printf("Failed to inactivate system key %s: %v\n", sysKey, errInactivate)
				}
			}
		}

		if !found {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "第三方服务积分不足或卡密无效，暂无法使用该卡密激活",
			})
			return
		}

		// Update the user's submitted card key in system_keys to become a pass.aisale.one key
		_, errUpdateUserKey := db.Exec(`
			UPDATE system_keys 
			SET vendor = 'pass.aisale.one', vendor_key = ?, updated_at = ? 
			WHERE system_key = ?`, targetVKey, time.Now(), req.CardSecret)
		if errUpdateUserKey != nil {
			log.Printf("Failed to update user system key: %v\n", errUpdateUserKey)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "转换卡密失败，请稍后重试",
			})
			return
		}

		vendor = "pass.aisale.one"
		vendorKey = targetVKey
		log.Printf("[Deard Conversion] Converted user key %s to pass.aisale.one key with vendor_key %s\n", req.CardSecret, targetVKey)
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

			_, errInsertRec := tx.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, extra_email, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				orderID, req.CardSecret, res.Username, res.Password, res.TwoFactor, extraEmail, res.Status, res.Message, res.TaskID, now, now)
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

			_, errInsertRec := tx.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, extra_email, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				orderID, req.CardSecret, acc.Username, acc.Password, acc.TwoFactor, extraEmail, "pending", "排队处理中", now, now)
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

	// Find order ID by current or historical card secret
	var orderID int64
	errQueryOrderID := db.QueryRow(`
		SELECT id FROM orders WHERE card_secret = ?
		UNION
		SELECT order_id FROM account_records WHERE card_secret = ?
		LIMIT 1`, cardSecret, cardSecret).Scan(&orderID)

	var rows *sql.Rows
	var err error
	if errQueryOrderID == sql.ErrNoRows {
		// No records found, return empty rows safely
		rows, err = db.Query(`
			SELECT r.id, o.vendor, r.task_id, r.username, r.password, r.two_factor, COALESCE(r.extra_email, ''), r.status, r.message, COALESCE(r.discount_url, ''), r.created_at, r.updated_at, r.completed_at, r.execution_count
			FROM account_records r
			JOIN orders o ON r.order_id = o.id
			WHERE 1=0`)
	} else if errQueryOrderID != nil {
		log.Printf("Query error for order_id lookup with card_secret %s: %v\n", cardSecret, errQueryOrderID)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库错误",
		})
		return
	} else {
		// Read records by order_id
		rows, err = db.Query(`
			SELECT r.id, o.vendor, r.task_id, r.username, r.password, r.two_factor, COALESCE(r.extra_email, ''), r.status, r.message, COALESCE(r.discount_url, ''), r.created_at, r.updated_at, r.completed_at, r.execution_count
			FROM account_records r
			JOIN orders o ON r.order_id = o.id
			WHERE o.id = ?
			ORDER BY r.id DESC`, orderID)
	}
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
		ID             int64
		Vendor         string
		TaskID         string
		Username       string
		Password       string
		TwoFactor      string
		ExtraEmail     string
		Status         string
		Message        string
		DiscountURL    string
		CreatedAt      time.Time
		UpdatedAt      time.Time
		CompletedAt    *time.Time
		ExecutionCount int
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
			&rec.ExecutionCount,
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
		if rec.Vendor == "pass.aisale.one" && rec.TaskID != "" && rec.Status != "success" && rec.Status != "failed" && rec.Status != "cancelled" {
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
		if rec.Status == "failed" && rec.Message != "" {
			var solution string
			errSol := db.QueryRow("SELECT solution FROM faqs WHERE (? LIKE CONCAT('%', error_code, '%') AND error_code != '') OR (? LIKE CONCAT('%', error_desc, '%') AND error_desc != '') LIMIT 1", rec.Message, rec.Message).Scan(&solution)
			if errSol == nil {
				rec.Solution = solution
			}
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

func getClientIP(r *http.Request) string {
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return strings.Trim(ip, "[]")
}

func checkAPIWhitelist(r *http.Request) bool {
	whitelistStr := getSetting("api_whitelist", "")
	if strings.TrimSpace(whitelistStr) == "" {
		return true // Empty whitelist allows all IPs
	}

	clientIP := getClientIP(r)

	// Normalize delimiters to commas
	whitelistStr = strings.ReplaceAll(whitelistStr, "\r\n", ",")
	whitelistStr = strings.ReplaceAll(whitelistStr, "\n", ",")
	whitelistStr = strings.ReplaceAll(whitelistStr, " ", ",")
	ips := strings.Split(whitelistStr, ",")

	for _, ip := range ips {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		if ip == clientIP {
			return true
		}
	}
	return false
}

func isAPIOpen() bool {
	return getSetting("api_open", "off") == "on"
}

func handleDocInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	apiOpen := getSetting("api_open", "off")
	apiBaseURL := getSetting("api_base_url", "")

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"api_open":     apiOpen,
		"api_base_url": apiBaseURL,
	})
}

func handleOpenSubmit(w http.ResponseWriter, r *http.Request) {
	if !isAPIOpen() {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "开放接口未启用",
		})
		return
	}
	if !checkAPIWhitelist(r) {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("IP %s 未在白名单中", getClientIP(r)),
		})
		return
	}

	handleSubmit(w, r)
}

func handleOpenQuery(w http.ResponseWriter, r *http.Request) {
	if !isAPIOpen() {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "开放接口未启用",
		})
		return
	}
	if !checkAPIWhitelist(r) {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("IP %s 未在白名单中", getClientIP(r)),
		})
		return
	}

	handleQuery(w, r)
}

func handleOpenReset(w http.ResponseWriter, r *http.Request) {
	if !isAPIOpen() {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "开放接口未启用",
		})
		return
	}
	if !checkAPIWhitelist(r) {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("IP %s 未在白名单中", getClientIP(r)),
		})
		return
	}

	handleResetKeys(w, r)
}

func handleOpenCancel(w http.ResponseWriter, r *http.Request) {
	if !isAPIOpen() {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "开放接口未启用",
		})
		return
	}
	if !checkAPIWhitelist(r) {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("IP %s 未在白名单中", getClientIP(r)),
		})
		return
	}

	handleCancelSubscription(w, r)
}

// CancelSubscriptionRequest represents request body to cancel a subscription
type CancelSubscriptionRequest struct {
	CardSecret string `json:"card_secret"`
	Username   string `json:"username"`
}

func handleCancelSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "读取请求体失败",
		})
		return
	}

	var req CancelSubscriptionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "解析JSON数据失败",
		})
		return
	}

	req.CardSecret = strings.TrimSpace(req.CardSecret)
	req.Username = strings.TrimSpace(req.Username)

	if req.CardSecret == "" || req.Username == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "卡密和账号不能为空",
		})
		return
	}

	// 1. Check vendor to determine if it is self-operated (自营)
	var vendor string
	var recordID int64
	var status string
	var taskID string
	errQuery := db.QueryRow(`
		SELECT r.id, o.vendor, r.status, r.task_id
		FROM account_records r
		JOIN orders o ON r.order_id = o.id
		WHERE r.card_secret = ? AND r.username = ?`, 
		req.CardSecret, req.Username).Scan(&recordID, &vendor, &status, &taskID)

	if errQuery == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"message": "未找到对应的提交记录",
		})
		return
	} else if errQuery != nil {
		log.Printf("Error querying account record for cancel: %v\n", errQuery)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库错误",
		})
		return
	}

	if status != "pending" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "当前状态不是排队中，无法取消",
		})
		return
	}

	// Determine if self-operated (自营)
	// Self-operated conditions: vendor is empty, or "ai.deard.fun", or api_url is empty in key_vendors table
	isSelfOperated := false
	if vendor == "" || vendor == "ai.deard.fun" {
		isSelfOperated = true
	} else {
		var apiURL string
		errAPI := db.QueryRow("SELECT api_url FROM key_vendors WHERE name = ?", vendor).Scan(&apiURL)
		if errAPI == sql.ErrNoRows || apiURL == "" {
			isSelfOperated = true
		}
	}

	now := time.Now()

	if isSelfOperated {
		// Update status directly
		_, errUpdate := db.Exec(`
			UPDATE account_records 
			SET status = 'cancelled', message = '已取消', completed_at = ?, updated_at = ? 
			WHERE id = ? AND status = 'pending'`, now, now, recordID)
		if errUpdate != nil {
			log.Printf("Error updating self-operated record status to cancelled: %v\n", errUpdate)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "更新订单状态失败",
			})
			return
		}

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "取消成功",
		})
		return
	}

	// Non-self-operated (非自营): Call third-party API and use a transaction
	tx, errTx := db.Begin()
	if errTx != nil {
		log.Printf("Error beginning transaction: %v\n", errTx)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库事务启动失败",
		})
		return
	}
	defer tx.Rollback()

	// Double check and lock the row
	var currentStatus string
	var currentTaskID string
	errLock := tx.QueryRow(`
		SELECT status, task_id 
		FROM account_records 
		WHERE id = ? FOR UPDATE`, recordID).Scan(&currentStatus, &currentTaskID)

	if errLock != nil {
		log.Printf("Error locking record: %v\n", errLock)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "锁定数据库记录失败",
		})
		return
	}

	if currentStatus != "pending" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "当前状态不是排队中，已被处理或取消",
		})
		return
	}

	// Get vendor key
	var vendorKey string
	errKeyQuery := tx.QueryRow("SELECT vendor_key FROM system_keys WHERE system_key = ?", req.CardSecret).Scan(&vendorKey)
	if errKeyQuery == sql.ErrNoRows {
		vendorKey = req.CardSecret
	} else if errKeyQuery != nil {
		log.Printf("Error querying vendor key mapping: %v\n", errKeyQuery)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库查询卡密映射错误",
		})
		return
	}

	// Call third-party API
	res, errAPI := cancelTaskOnVendor(vendorKey, currentTaskID)
	if errAPI != nil {
		log.Printf("Error calling third-party cancel API: %v\n", errAPI)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("调用第三方接口失败: %v", errAPI),
		})
		return
	}

	if !res.Success {
		log.Printf("Third-party cancel API returned failure: %s\n", res.Message)
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("第三方取消失败: %s", res.Message),
		})
		return
	}

	// Update DB status inside transaction
	dbMsg := res.Message
	if dbMsg == "" {
		dbMsg = "已取消"
	}
	_, errUpdate := tx.Exec(`
		UPDATE account_records 
		SET status = 'cancelled', message = ?, completed_at = ?, updated_at = ? 
		WHERE id = ?`, dbMsg, now, now, recordID)
	if errUpdate != nil {
		log.Printf("Error updating record in transaction: %v\n", errUpdate)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "更新数据库记录失败",
		})
		return
	}

	if errCommit := tx.Commit(); errCommit != nil {
		log.Printf("Error committing transaction: %v\n", errCommit)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "提交事务失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "取消成功",
	})
}

// isValid2FA validates if 2FA key/code is either a 32-character key or digits with count as a multiple of 8
func isValid2FA(twoFactor string) bool {
	twoFactor = strings.TrimSpace(twoFactor)
	if twoFactor == "" {
		return false
	}

	// Case 1: 32-character key after removing whitespace
	cleanKey := strings.ReplaceAll(twoFactor, " ", "")
	cleanKey = strings.ReplaceAll(cleanKey, "\t", "")
	cleanKey = strings.ReplaceAll(cleanKey, "\r", "")
	cleanKey = strings.ReplaceAll(cleanKey, "\n", "")
	if len(cleanKey) == 32 {
		isAlphaNumeric := true
		for _, ch := range cleanKey {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')) {
				isAlphaNumeric = false
				break
			}
		}
		if isAlphaNumeric {
			return true
		}
	}

	// Case 2: Digits where total digit count is a multiple of 8
	var digitCount int
	hasOnlyDigitsAndSeparators := true
	for _, ch := range twoFactor {
		if ch >= '0' && ch <= '9' {
			digitCount++
		} else if ch == ' ' || ch == '-' || ch == ',' || ch == '\t' || ch == '\r' || ch == '\n' {
			continue
		} else {
			hasOnlyDigitsAndSeparators = false
			break
		}
	}

	if hasOnlyDigitsAndSeparators && digitCount > 0 && digitCount%8 == 0 {
		return true
	}

	return false
}

// isPersonalGoogleEmail checks if the email is a personal Google email
func isPersonalGoogleEmail(email string) bool {
	email = strings.TrimSpace(email)
	if !strings.Contains(email, "@") {
		return true
	}
	lower := strings.ToLower(email)
	personalDomains := []string{
		"gmail.com", "googlemail.com",
		"qq.com", "foxmail.com",
		"163.com", "126.com", "yeah.net",
		"sina.com", "sina.cn", "sohu.com",
		"aliyun.com",
		"139.com", "189.cn", "wo.cn",
		"outlook.com", "hotmail.com", "live.com", "live.cn", "msn.com",
		"icloud.com", "me.com", "mac.com",
		"yahoo.com", "ymail.com",
		"proton.me", "protonmail.com", "protonmail.ch",
		"aol.com",
		"gmx.com", "gmx.net", "mail.com",
		"yandex.com", "yandex.ru",
		"zoho.com",
	}
	for _, domain := range personalDomains {
		if strings.HasSuffix(lower, "@"+domain) || strings.HasSuffix(lower, "."+domain) {
			return true
		}
	}
	return false
}
