package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

var vendorBaseURL = "https://pass.aisale.one/gateway.php"

// VendorSubmitRequest represents request payload to submit task to vendor
type VendorSubmitRequest struct {
	Action   string `json:"action"`
	CDKey    string `json:"cdkey"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TwoFA    string `json:"twofa"`
	TaskType string `json:"task_type"`
	Lang     string `json:"lang"`
}

// VendorSubmitResponse represents response payload from vendor submit endpoint
type VendorSubmitResponse struct {
	Success bool   `json:"success"`
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

// VendorQueryRequest represents request payload to query status from vendor
type VendorQueryRequest struct {
	Action string `json:"action"`
	CDKey  string `json:"cdkey"`
	TaskID string `json:"task_id"`
	Lang   string `json:"lang"`
}

// VendorQueryResponse represents response payload from vendor query status endpoint
type VendorQueryResponse struct {
	Success bool `json:"success"`
	Data    struct {
		TaskID      string `json:"task_id"`
		Status      string `json:"status"` // Pending, Running, Success, Failed
		Message     string `json:"message"`
		HasOfferURL bool   `json:"has_offer_url"`
		OfferURL    string `json:"offer_url"`
	} `json:"data"`
	Message string `json:"message"`
}

// ConvertKeysRequest represents request parameters for keys conversion
type ConvertKeysRequest struct {
	Vendor     string   `json:"vendor"`
	VendorKeys []string `json:"vendor_keys"`
	Multiplier int      `json:"multiplier"`
	Note       string   `json:"note"`
	CreatorID  *int64   `json:"creator_id,omitempty"`
}

// ConvertKeysResponse represents response parameters for keys conversion
type ConvertKeysResponse struct {
	Success      bool     `json:"success"`
	SystemKeys   []string `json:"system_keys"`
	OriginalKeys []string `json:"original_keys,omitempty"`
	Message      string   `json:"message"`
}

// ResetKeysRequest represents request parameters for resetting system keys
type ResetKeysRequest struct {
	OldKeys []string `json:"old_keys"`
}

// ResetKeysResponse represents response parameters for resetting system keys
type ResetKeysResponse struct {
	Success      bool     `json:"success"`
	NewKeys      []string `json:"new_keys"`
	OriginalKeys []string `json:"original_keys,omitempty"`
	Message      string   `json:"message"`
}

func submitTaskToVendor(cardSecret, email, password, twofa string) (*VendorSubmitResponse, error) {
	reqBody := VendorSubmitRequest{
		Action:   "submit_task",
		CDKey:    cardSecret,
		Email:    email,
		Password: password,
		TwoFA:    twofa,
		TaskType: "full",
		Lang:     "zh",
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	// Dynamic API url resolution
	apiURL := "https://pass.aisale.one/gateway.php"
	if vendorBaseURL != "https://pass.aisale.one/gateway.php" {
		apiURL = vendorBaseURL
	} else {
		var vendor string
		errVendor := db.QueryRow("SELECT vendor FROM system_keys WHERE system_key = ?", cardSecret).Scan(&vendor)
		if errVendor != nil {
			_ = db.QueryRow("SELECT vendor FROM orders WHERE card_secret = ?", cardSecret).Scan(&vendor)
		}
		if vendor != "" {
			var dbAPIURL string
			errVal := db.QueryRow("SELECT api_url FROM key_vendors WHERE name = ?", vendor).Scan(&dbAPIURL)
			if errVal == nil && dbAPIURL != "" {
				apiURL = dbAPIURL
			}
		}
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var vendorResp VendorSubmitResponse
	if err := json.Unmarshal(bodyBytes, &vendorResp); err != nil {
		return nil, err
	}

	return &vendorResp, nil
}

func queryTaskFromVendor(cardSecret, taskID string) (*VendorQueryResponse, error) {
	reqBody := VendorQueryRequest{
		Action: "get_status",
		CDKey:  cardSecret,
		TaskID: taskID,
		Lang:   "zh",
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	// Dynamic API url resolution
	apiURL := "https://pass.aisale.one/gateway.php"
	if vendorBaseURL != "https://pass.aisale.one/gateway.php" {
		apiURL = vendorBaseURL
	} else {
		var vendor string
		errVendor := db.QueryRow("SELECT vendor FROM system_keys WHERE system_key = ?", cardSecret).Scan(&vendor)
		if errVendor != nil {
			_ = db.QueryRow("SELECT vendor FROM orders WHERE card_secret = ?", cardSecret).Scan(&vendor)
		}
		if vendor != "" {
			var dbAPIURL string
			errVal := db.QueryRow("SELECT api_url FROM key_vendors WHERE name = ?", vendor).Scan(&dbAPIURL)
			if errVal == nil && dbAPIURL != "" {
				apiURL = dbAPIURL
			}
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var vendorResp VendorQueryResponse
	if err := json.Unmarshal(bodyBytes, &vendorResp); err != nil {
		return nil, err
	}

	return &vendorResp, nil
}

// VendorCancelRequest represents request payload to cancel task from vendor
type VendorCancelRequest struct {
	Action string `json:"action"`
	CDKey  string `json:"cdkey"`
	TaskID string `json:"task_id"`
	Lang   string `json:"lang"`
}

// VendorCancelResponse represents response payload from vendor cancel endpoint
type VendorCancelResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func cancelTaskOnVendor(cardSecret, taskID string) (*VendorCancelResponse, error) {
	reqBody := VendorCancelRequest{
		Action: "cancel_task",
		CDKey:  cardSecret,
		TaskID: taskID,
		Lang:   "zh",
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	// Dynamic API url resolution
	apiURL := "https://pass.aisale.one/gateway.php"
	if vendorBaseURL != "https://pass.aisale.one/gateway.php" {
		apiURL = vendorBaseURL
	} else {
		var vendor string
		errVendor := db.QueryRow("SELECT vendor FROM system_keys WHERE system_key = ?", cardSecret).Scan(&vendor)
		if errVendor != nil {
			_ = db.QueryRow("SELECT vendor FROM orders WHERE card_secret = ?", cardSecret).Scan(&vendor)
		}
		if vendor != "" {
			var dbAPIURL string
			errVal := db.QueryRow("SELECT api_url FROM key_vendors WHERE name = ?", vendor).Scan(&dbAPIURL)
			if errVal == nil && dbAPIURL != "" {
				apiURL = dbAPIURL
			}
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(apiURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var vendorResp VendorCancelResponse
	if err := json.Unmarshal(bodyBytes, &vendorResp); err != nil {
		return nil, err
	}

	return &vendorResp, nil
}

func generateSystemKey() string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	const length = 12
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "PX" + time.Now().Format("060102150405")
	}
	for i := range b {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b)
}

func handleConvertKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "读取请求失败",
		})
		return
	}

	var req ConvertKeysRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "解析JSON失败",
		})
		return
	}

	if req.Vendor == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "供应商不能为空",
		})
		return
	}

	if req.Vendor != "ai.deard.fun" && len(req.VendorKeys) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "第三方密钥列表不能为空",
		})
		return
	}

	if req.Multiplier <= 0 {
		req.Multiplier = 1
	}

	adminID, ok := getAdminID(r)
	var creatorID interface{} = nil
	if ok {
		creatorID = adminID
	}

	if req.CreatorID != nil {
		var exists bool
		err := db.QueryRow("SELECT COUNT(*) > 0 FROM admins WHERE id = ?", *req.CreatorID).Scan(&exists)
		if err == nil && exists {
			creatorID = *req.CreatorID
		} else {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "指定的创建用户不存在",
			})
			return
		}
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("Error starting transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库启动事务失败",
		})
		return
	}
	defer tx.Rollback()

	now := time.Now()
	var generatedKeys []string
	var originalKeys []string

	if req.Vendor == "ai.deard.fun" {
		for i := 0; i < req.Multiplier; i++ {
			var sysKey string
			for {
				sysKey = generateSystemKey()
				var count int
				err := tx.QueryRow("SELECT COUNT(*) FROM system_keys WHERE system_key = ?", sysKey).Scan(&count)
				if err == nil && count == 0 {
					break
				}
			}

			originalKey := sysKey

			_, errInsert := tx.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at, note, creator_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
				sysKey, req.Vendor, "", "active", originalKey, now, now, req.Note, creatorID)
			if errInsert != nil {
				log.Printf("Error inserting system key: %v\n", errInsert)
				respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"success": false,
					"message": "存储密钥映射失败",
				})
				return
			}
			generatedKeys = append(generatedKeys, sysKey)
			originalKeys = append(originalKeys, originalKey)
		}
	} else {
		for _, vKey := range req.VendorKeys {
			vKey = strings.TrimSpace(vKey)
			if vKey == "" {
				continue
			}

			for i := 0; i < req.Multiplier; i++ {
				var sysKey string
				for {
					sysKey = generateSystemKey()
					var count int
					err := tx.QueryRow("SELECT COUNT(*) FROM system_keys WHERE system_key = ?", sysKey).Scan(&count)
					if err == nil && count == 0 {
						break
					}
				}

				originalKey := sysKey

				_, errInsert := tx.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at, note, creator_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
					sysKey, req.Vendor, vKey, "active", originalKey, now, now, req.Note, creatorID)
				if errInsert != nil {
					log.Printf("Error inserting system key: %v\n", errInsert)
					respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
						"success": false,
						"message": "存储密钥映射失败",
					})
					return
				}
				generatedKeys = append(generatedKeys, sysKey)
				originalKeys = append(originalKeys, originalKey)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "提交事务失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, ConvertKeysResponse{
		Success:      true,
		SystemKeys:   generatedKeys,
		OriginalKeys: originalKeys,
		Message:      "转换密钥成功",
	})
}

func handleResetKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "读取请求失败",
		})
		return
	}

	var req ResetKeysRequest
	if err := json.Unmarshal(body, &req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "解析JSON失败",
		})
		return
	}

	if len(req.OldKeys) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "旧卡密列表不能为空",
		})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		log.Printf("Error starting transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库启动事务失败",
		})
		return
	}
	defer tx.Rollback()

	now := time.Now()
	var newKeys []string
	var originalKeys []string

	for _, oldKey := range req.OldKeys {
		oldKey = strings.TrimSpace(oldKey)
		if oldKey == "" {
			continue
		}

		var vendor string
		var vendorKey string
		var status string
		var originalKey string
		var note string
		var creatorID sql.NullInt64
		errQuery := tx.QueryRow("SELECT vendor, vendor_key, status, original_key, note, creator_id FROM system_keys WHERE system_key = ?", oldKey).
			Scan(&vendor, &vendorKey, &status, &originalKey, &note, &creatorID)

		if errQuery == sql.ErrNoRows {
			continue
		} else if errQuery != nil {
			log.Printf("Error querying system key %s: %v\n", oldKey, errQuery)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "数据库查询卡密错误",
			})
			return
		}

		if status != "active" {
			continue
		}

		// Fallback for legacy key if original_key is empty
		if originalKey == "" {
			originalKey = oldKey
		}

		// Check if there are active tasks (status NOT in 'success', 'failed') associated with this key (card_secret)
		var runningCount int
		errRunningCheck := tx.QueryRow(`
			SELECT COUNT(*) 
			FROM account_records r
			JOIN orders o ON r.order_id = o.id
			WHERE o.card_secret = ? AND r.status NOT IN ('success', 'failed')`, oldKey).Scan(&runningCount)
		if errRunningCheck != nil {
			log.Printf("Error checking active tasks for key %s: %v\n", oldKey, errRunningCheck)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "数据库查询任务状态错误",
			})
			return
		}
		if runningCount > 0 {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("该卡密 %s 还有正在执行的任务，无法重置", oldKey),
			})
			return
		}

		_, errUpdate := tx.Exec("UPDATE system_keys SET status = 'inactive', updated_at = ? WHERE system_key = ?", now, oldKey)
		if errUpdate != nil {
			log.Printf("Error updating old key %s status: %v\n", oldKey, errUpdate)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "更新旧卡密状态失败",
			})
			return
		}

		var newKey string
		for {
			newKey = generateSystemKey()
			var count int
			err := tx.QueryRow("SELECT COUNT(*) FROM system_keys WHERE system_key = ?", newKey).Scan(&count)
			if err == nil && count == 0 {
				break
			}
		}

		_, errInsert := tx.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at, note, creator_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
			newKey, vendor, vendorKey, "active", originalKey, now, now, note, creatorID)
		if errInsert != nil {
			log.Printf("Error inserting new system key: %v\n", errInsert)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "存储新系统卡密失败",
			})
			return
		}

		newKeys = append(newKeys, newKey)
		originalKeys = append(originalKeys, originalKey)
	}

	if err := tx.Commit(); err != nil {
		log.Printf("Error committing transaction: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "提交事务失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, ResetKeysResponse{
		Success:      true,
		NewKeys:      newKeys,
		OriginalKeys: originalKeys,
		Message:      "卡密重置成功",
	})
}
