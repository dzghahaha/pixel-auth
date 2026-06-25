package main

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

//go:embed frontend/*
var embedFS embed.FS

// AccountRecord represents a single account status within a card order
type AccountRecord struct {
	Username    string     `json:"username"`
	Password    string     `json:"password"`
	TwoFactor   string     `json:"two_factor"`
	ExtraEmail  string     `json:"extra_email,omitempty"`
	Status      string     `json:"status"` // "pending", "success", "failed"
	Message     string     `json:"message"`
	DiscountURL string     `json:"discount_url"`
	Vendor      string     `json:"vendor"`
	TaskID      string     `json:"task_id"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

// CardOrder holds the order history for a single card secret
type CardOrder struct {
	CardSecret string          `json:"card_secret"`
	Mode       string          `json:"mode"`
	Records    []AccountRecord `json:"records"`
}

// Global DB connection pool
var db *sql.DB

// Initialize MySQL database connection and create tables
func initDB() {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		// Default connection details provided by user
		dsn = "root:d2dsoft_123@tcp(212.129.244.194:3306)/pixel_auth?parseTime=true&loc=Local"
	}

	// Parse the DSN dynamically
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		log.Fatalf("Invalid DSN configured: %v", err)
	}

	dbAddr := cfg.Addr
	dbName := cfg.DBName
	if dbName == "" {
		dbName = "pixel_auth"
	}

	// 1. Connect to MySQL without specifying a database to create the database if not exists
	// Ensure we don't hang for 2 minutes by setting a connection timeout (e.g., 5 seconds)
	cfg.Timeout = 5 * time.Second
	cfg.DBName = ""
	serverDSN := cfg.FormatDSN()

	tempDB, err := sql.Open("mysql", serverDSN)
	if err == nil {
		log.Printf("Connecting to MySQL server at %s to ensure database exists...\n", dbAddr)
		_, errDB := tempDB.Exec("CREATE DATABASE IF NOT EXISTS `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
		if errDB != nil {
			log.Printf("Warning: failed to ensure database exists: %v. Proceeding to connect anyway.\n", errDB)
		}
		tempDB.Close()
	}

	// 2. Connect to the actual database
	var errOpen error
	db, errOpen = sql.Open("mysql", dsn)
	if errOpen != nil {
		log.Fatalf("Error opening database connection pool: %v", errOpen)
	}

	// Connection pool tuning
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	// Check connectivity
	log.Printf("Pinging MySQL database at %s...\n", dbAddr)
	var errPing error
	for i := 0; i < 3; i++ {
		errPing = db.Ping()
		if errPing == nil {
			break
		}
		log.Printf("Ping failed, retrying in 2 seconds... (%d/3)\n", i+1)
		time.Sleep(2 * time.Second)
	}
	if errPing != nil {
		log.Fatalf("Failed to ping MySQL database: %v", errPing)
	}
	log.Println("Successfully connected to MySQL database!")

	// 3. Create tables
	createTables()

	// 4. Ensure default admin exists
	ensureDefaultAdmin()
}

func createTables() {
	ordersDDL := `
	CREATE TABLE IF NOT EXISTS orders (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		card_secret VARCHAR(128) NOT NULL,
		mode VARCHAR(32) NOT NULL,
		vendor VARCHAR(64) NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_card_secret (card_secret)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	recordsDDL := `
	CREATE TABLE IF NOT EXISTS account_records (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		order_id BIGINT UNSIGNED NOT NULL,
		username VARCHAR(255) NOT NULL,
		password VARCHAR(255) NOT NULL,
		two_factor VARCHAR(255) NOT NULL,
		extra_email VARCHAR(255) DEFAULT NULL,
		status VARCHAR(32) NOT NULL,
		message VARCHAR(500) DEFAULT NULL,
		discount_url VARCHAR(500) DEFAULT NULL,
		task_id VARCHAR(128) NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		completed_at DATETIME DEFAULT NULL,
		KEY idx_order_id (order_id),
		CONSTRAINT fk_order_id FOREIGN KEY (order_id) REFERENCES orders (id) ON DELETE CASCADE
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	systemKeysDDL := `
	CREATE TABLE IF NOT EXISTS system_keys (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		system_key VARCHAR(128) NOT NULL,
		vendor VARCHAR(64) NOT NULL,
		vendor_key VARCHAR(128) NOT NULL,
		status VARCHAR(32) NOT NULL DEFAULT 'active',
		original_key VARCHAR(128) NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_system_key (system_key),
		KEY idx_vendor_key (vendor_key),
		KEY idx_original_key (original_key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	adminsDDL := `
	CREATE TABLE IF NOT EXISTS admins (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		username VARCHAR(128) NOT NULL UNIQUE,
		password_hash VARCHAR(255) NOT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	adminSessionsDDL := `
	CREATE TABLE IF NOT EXISTS admin_sessions (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		token VARCHAR(64) NOT NULL UNIQUE,
		admin_id BIGINT UNSIGNED NOT NULL,
		expires_at DATETIME NOT NULL,
		created_at DATETIME NOT NULL,
		CONSTRAINT fk_admin_id FOREIGN KEY (admin_id) REFERENCES admins (id) ON DELETE CASCADE,
		KEY idx_expires_at (expires_at)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	log.Println("Ensuring database tables 'orders', 'account_records', 'system_keys', 'admins', and 'admin_sessions' exist...")
	if _, err := db.Exec(ordersDDL); err != nil {
		log.Fatalf("Error creating orders table: %v", err)
	}

	if _, err := db.Exec(recordsDDL); err != nil {
		log.Fatalf("Error creating account_records table: %v", err)
	}

	if _, err := db.Exec(systemKeysDDL); err != nil {
		log.Fatalf("Error creating system_keys table: %v", err)
	}

	if _, err := db.Exec(adminsDDL); err != nil {
		log.Fatalf("Error creating admins table: %v", err)
	}

	if _, err := db.Exec(adminSessionsDDL); err != nil {
		log.Fatalf("Error creating admin_sessions table: %v", err)
	}
	log.Println("Tables verified/created successfully.")

	// Run migrations to ensure fields exist in existing databases
	var hasVendor bool
	errCheck := db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'orders' 
		  AND COLUMN_NAME = 'vendor'
	`).Scan(&hasVendor)
	if errCheck == nil && !hasVendor {
		log.Println("Adding 'vendor' column to 'orders' table...")
		if _, err := db.Exec("ALTER TABLE orders ADD COLUMN vendor VARCHAR(64) NOT NULL DEFAULT ''"); err != nil {
			log.Printf("Warning: failed to add vendor column: %v\n", err)
		}
	}

	var hasTaskID bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'account_records' 
		  AND COLUMN_NAME = 'task_id'
	`).Scan(&hasTaskID)
	if errCheck == nil && !hasTaskID {
		log.Println("Adding 'task_id' column to 'account_records' table...")
		if _, err := db.Exec("ALTER TABLE account_records ADD COLUMN task_id VARCHAR(128) NOT NULL DEFAULT ''"); err != nil {
			log.Printf("Warning: failed to add task_id column: %v\n", err)
		}
	}

	var hasOriginalKey bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'system_keys' 
		  AND COLUMN_NAME = 'original_key'
	`).Scan(&hasOriginalKey)
	if errCheck == nil && !hasOriginalKey {
		log.Println("Adding 'original_key' column to 'system_keys' table...")
		if _, err := db.Exec("ALTER TABLE system_keys ADD COLUMN original_key VARCHAR(128) NOT NULL DEFAULT ''"); err != nil {
			log.Printf("Warning: failed to add original_key column: %v\n", err)
		} else {
			// Add index for original_key
			if _, err := db.Exec("ALTER TABLE system_keys ADD KEY idx_original_key (original_key)"); err != nil {
				log.Printf("Warning: failed to add idx_original_key: %v\n", err)
			}
		}
	}

	// Always ensure existing keys with empty original_key are backfilled with their own system_key
	if _, err := db.Exec("UPDATE system_keys SET original_key = system_key WHERE original_key = ''"); err != nil {
		log.Printf("Warning: failed to populate original_key for existing keys: %v\n", err)
	}
}

func ensureDefaultAdmin() {
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM admins").Scan(&count)
	if err != nil {
		log.Fatalf("Error checking admins count: %v", err)
	}

	if count == 0 {
		// Generate 16-char random password
		const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("Failed to generate random password: %v", err)
		}
		for i := range b {
			b[i] = charset[b[i]%byte(len(charset))]
		}
		password := string(b)

		hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			log.Fatalf("Failed to bcrypt admin password: %v", err)
		}

		now := time.Now()
		_, errInsert := db.Exec("INSERT INTO admins (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)",
			"admin", string(hashedPassword), now, now)
		if errInsert != nil {
			log.Fatalf("Failed to insert default admin: %v", errInsert)
		}

		log.Println("========================================================================")
		log.Println("[ADMIN INIT] 已成功初始化默认管理员账号：")
		log.Printf("用户名: admin\n")
		log.Printf("初始密码: %s\n", password)
		log.Println("请妥善保管该密码。若需修改密码，请直接修改 admins 表。")
		log.Println("========================================================================")
	}
}

var vendorBaseURL = "https://pass.aisale.one/gateway.php"

type VendorSubmitRequest struct {
	Action   string `json:"action"`
	CDKey    string `json:"cdkey"`
	Email    string `json:"email"`
	Password string `json:"password"`
	TwoFA    string `json:"twofa"`
	TaskType string `json:"task_type"`
	Lang     string `json:"lang"`
}

type VendorSubmitResponse struct {
	Success bool   `json:"success"`
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

type VendorQueryRequest struct {
	Action string `json:"action"`
	CDKey  string `json:"cdkey"`
	TaskID string `json:"task_id"`
	Lang   string `json:"lang"`
}

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

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Post(vendorBaseURL, "application/json", bytes.NewBuffer(jsonData))
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

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(vendorBaseURL, "application/json", bytes.NewBuffer(jsonData))
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

type ConvertKeysRequest struct {
	Vendor     string   `json:"vendor"`
	VendorKeys []string `json:"vendor_keys"`
	Multiplier int      `json:"multiplier"`
}

type ConvertKeysResponse struct {
	Success      bool     `json:"success"`
	SystemKeys   []string `json:"system_keys"`
	OriginalKeys []string `json:"original_keys,omitempty"`
	Message      string   `json:"message"`
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

	if len(req.VendorKeys) == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "第三方密钥列表不能为空",
		})
		return
	}

	if req.Multiplier <= 0 {
		req.Multiplier = 1
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

			var originalKey string
			for {
				originalKey = generateSystemKey()
				var count int
				err := tx.QueryRow("SELECT COUNT(*) FROM system_keys WHERE original_key = ?", originalKey).Scan(&count)
				if err == nil && count == 0 {
					break
				}
			}

			_, errInsert := tx.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
				sysKey, req.Vendor, vKey, "active", originalKey, now, now)
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

type ResetKeysRequest struct {
	OldKeys []string `json:"old_keys"`
}

type ResetKeysResponse struct {
	Success      bool     `json:"success"`
	NewKeys      []string `json:"new_keys"`
	OriginalKeys []string `json:"original_keys,omitempty"`
	Message      string   `json:"message"`
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
		errQuery := tx.QueryRow("SELECT vendor, vendor_key, status, original_key FROM system_keys WHERE system_key = ?", oldKey).
			Scan(&vendor, &vendorKey, &status, &originalKey)

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

		_, errInsert := tx.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
			newKey, vendor, vendorKey, "active", originalKey, now, now)
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

// JSON Submission Request struct
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

// checkAdminSession checks if token is valid and not expired, returning admin_id if valid
func checkAdminSession(token string) (int64, bool) {
	var adminID int64
	var expiresAt time.Time
	err := db.QueryRow("SELECT admin_id, expires_at FROM admin_sessions WHERE token = ?", token).Scan(&adminID, &expiresAt)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("Error querying session token: %v\n", err)
		}
		return 0, false
	}
	if time.Now().After(expiresAt) {
		// Clean up expired session
		_, errDel := db.Exec("DELETE FROM admin_sessions WHERE token = ?", token)
		if errDel != nil {
			log.Printf("Error deleting expired session: %v\n", errDel)
		}
		return 0, false
	}
	return adminID, true
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err != nil || cookie.Value == "" {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "请登录后操作",
			})
			return
		}
		_, ok := checkAdminSession(cookie.Value)
		if !ok {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "登录已过期或无效，请重新登录",
			})
			return
		}
		next(w, r)
	}
}

type adminStaticServer struct {
	fileServer http.Handler
}

func (h *adminStaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/convert.html" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		if path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
			return
		}

		if path == "/admin/login.html" {
			cookie, err := r.Cookie("admin_session")
			if err == nil && cookie.Value != "" {
				if _, ok := checkAdminSession(cookie.Value); ok {
					http.Redirect(w, r, "/admin/index.html", http.StatusFound)
					return
				}
			}
			h.fileServer.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie("admin_session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/admin/login.html", http.StatusFound)
			return
		}
		if _, ok := checkAdminSession(cookie.Value); !ok {
			http.SetCookie(w, &http.Cookie{
				Name:     "admin_session",
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
			})
			http.Redirect(w, r, "/admin/login.html", http.StatusFound)
			return
		}
	}
	h.fileServer.ServeHTTP(w, r)
}

type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "无效的请求格式数据",
		})
		return
	}

	var adminID int64
	var pwdHash string
	err := db.QueryRow("SELECT id, password_hash FROM admins WHERE username = ?", req.Username).Scan(&adminID, &pwdHash)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "用户名或密码错误",
		})
		return
	} else if err != nil {
		log.Printf("Error querying admin: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库服务故障，请稍后重试",
		})
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(pwdHash), []byte(req.Password))
	if err != nil {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "用户名或密码错误",
		})
		return
	}

	// Generate token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "生成Session Token失败",
		})
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// Store in database with 24 hours expiry
	expiresAt := time.Now().Add(24 * time.Hour)
	now := time.Now()
	_, errInsert := db.Exec("INSERT INTO admin_sessions (token, admin_id, expires_at, created_at) VALUES (?, ?, ?, ?)",
		token, adminID, expiresAt, now)
	if errInsert != nil {
		log.Printf("Error saving admin session: %v\n", errInsert)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "保存Session失败",
		})
		return
	}

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "登录成功",
	})
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_session")
	if err == nil && cookie.Value != "" {
		_, errDel := db.Exec("DELETE FROM admin_sessions WHERE token = ?", cookie.Value)
		if errDel != nil {
			log.Printf("Error deleting session on logout: %v\n", errDel)
		}
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "退出登录成功",
	})
}

func handleAdminCheck(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("admin_session")
	if err != nil || cookie.Value == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}
	adminID, ok := checkAdminSession(cookie.Value)
	if !ok {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}

	var username string
	errQuery := db.QueryRow("SELECT username FROM admins WHERE id = ?", adminID).Scan(&username)
	if errQuery != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":  true,
		"username": username,
	})
}

func handleAdminOrders(w http.ResponseWriter, r *http.Request) {
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
	statusFilter := r.URL.Query().Get("status")

	whereClauses := []string{"1=1"}
	var args []interface{}

	if searchTerm != "" {
		whereClauses = append(whereClauses, "(r.username LIKE ? OR o.card_secret LIKE ? OR r.task_id LIKE ?)")
		likeArg := "%" + searchTerm + "%"
		args = append(args, likeArg, likeArg, likeArg)
	}

	if statusFilter != "" {
		whereClauses = append(whereClauses, "r.status = ?")
		args = append(args, statusFilter)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	var totalCount int
	countQuery := fmt.Sprintf(`
		SELECT COUNT(*) 
		FROM orders o
		LEFT JOIN (
			SELECT r1.*
			FROM account_records r1
			INNER JOIN (
				SELECT order_id, MAX(id) as max_id
				FROM account_records
				GROUP BY order_id
			) r2 ON r1.id = r2.max_id
		) r ON o.id = r.order_id
		WHERE %s`, whereSQL)

	errCount := db.QueryRow(countQuery, args...).Scan(&totalCount)
	if errCount != nil {
		log.Printf("Error counting admin orders: %v\n", errCount)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询总数失败",
		})
		return
	}

	dataQuery := fmt.Sprintf(`
		SELECT o.id, o.card_secret, o.mode, COALESCE(r.username, ''), COALESCE(r.password, ''), COALESCE(r.two_factor, ''), COALESCE(r.extra_email, ''), 
		       COALESCE(r.status, ''), COALESCE(r.message, ''), COALESCE(r.discount_url, ''), o.vendor, COALESCE(r.task_id, ''), 
		       o.created_at, o.updated_at, r.completed_at, COALESCE(sk.vendor_key, '') AS vendor_key
		FROM orders o
		LEFT JOIN system_keys sk ON o.card_secret = sk.system_key
		LEFT JOIN (
			SELECT r1.*
			FROM account_records r1
			INNER JOIN (
				SELECT order_id, MAX(id) as max_id
				FROM account_records
				GROUP BY order_id
			) r2 ON r1.id = r2.max_id
		) r ON o.id = r.order_id
		WHERE %s
		ORDER BY o.id DESC
		LIMIT ? OFFSET ?`, whereSQL)

	dataArgs := append(args, pageSize, offset)
	rows, errRows := db.Query(dataQuery, dataArgs...)
	if errRows != nil {
		log.Printf("Error querying admin orders: %v\n", errRows)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询订单列表失败",
		})
		return
	}
	defer rows.Close()

	type AdminOrderRow struct {
		ID          int64      `json:"id"`
		CardSecret  string     `json:"card_secret"`
		Mode        string     `json:"mode"`
		Username    string     `json:"username"`
		Password    string     `json:"password"`
		TwoFactor   string     `json:"two_factor"`
		ExtraEmail  string     `json:"extra_email"`
		Status      string     `json:"status"`
		Message     string     `json:"message"`
		DiscountURL string     `json:"discount_url"`
		Vendor      string     `json:"vendor"`
		TaskID      string     `json:"task_id"`
		CreatedAt   time.Time  `json:"created_at"`
		UpdatedAt   time.Time  `json:"updated_at"`
		CompletedAt *time.Time `json:"completed_at,omitempty"`
		VendorKey   string     `json:"vendor_key"`
	}

	var records []AdminOrderRow
	for rows.Next() {
		var row AdminOrderRow
		var completedAt sql.NullTime
		errScan := rows.Scan(
			&row.ID,
			&row.CardSecret,
			&row.Mode,
			&row.Username,
			&row.Password,
			&row.TwoFactor,
			&row.ExtraEmail,
			&row.Status,
			&row.Message,
			&row.DiscountURL,
			&row.Vendor,
			&row.TaskID,
			&row.CreatedAt,
			&row.UpdatedAt,
			&completedAt,
			&row.VendorKey,
		)
		if errScan != nil {
			log.Printf("Error scanning admin order row: %v\n", errScan)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "解析数据失败",
			})
			return
		}
		if completedAt.Valid {
			row.CompletedAt = &completedAt.Time
		}
		records = append(records, row)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"total":     totalCount,
		"page":      page,
		"page_size": pageSize,
		"records":   records,
	})
}

type UpdateOrderRequest struct {
	RecordID    int64  `json:"record_id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	DiscountURL string `json:"discount_url"`
}

func handleAdminOrdersUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req UpdateOrderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "无效的请求格式数据",
		})
		return
	}

	if req.RecordID <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "记录ID无效",
		})
		return
	}

	status := strings.ToLower(req.Status)
	if status != "pending" && status != "success" && status != "failed" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "无效的状态值",
		})
		return
	}

	now := time.Now()
	var errUpdate error
	if status == "success" || status == "failed" {
		_, errUpdate = db.Exec(`
			UPDATE account_records 
			SET status = ?, message = ?, discount_url = ?, completed_at = ?, updated_at = ? 
			WHERE id = ?`,
			status, req.Message, req.DiscountURL, now, now, req.RecordID)
	} else {
		_, errUpdate = db.Exec(`
			UPDATE account_records 
			SET status = ?, message = ?, discount_url = ?, completed_at = NULL, updated_at = ? 
			WHERE id = ?`,
			status, req.Message, req.DiscountURL, now, req.RecordID)
	}

	if errUpdate != nil {
		log.Printf("Error updating account record %d: %v\n", req.RecordID, errUpdate)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "更新数据库记录失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "更新成功",
	})
}

func handleAdminOrderHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	orderID := r.URL.Query().Get("order_id")
	if orderID == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "参数 order_id 不能为空",
		})
		return
	}

	rows, err := db.Query(`
		SELECT id, username, password, two_factor, COALESCE(extra_email, ''), 
		       status, message, COALESCE(discount_url, ''), task_id, 
		       created_at, updated_at, completed_at
		FROM account_records
		WHERE order_id = ?
		ORDER BY id DESC`, orderID)
	if err != nil {
		log.Printf("Query error for order history %s: %v\n", orderID, err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库错误",
		})
		return
	}
	defer rows.Close()

	type OrderHistoryRecord struct {
		ID          int64      `json:"id"`
		Username    string     `json:"username"`
		Password    string     `json:"password"`
		TwoFactor   string     `json:"two_factor"`
		ExtraEmail  string     `json:"extra_email"`
		Status      string     `json:"status"`
		Message     string     `json:"message"`
		DiscountURL string     `json:"discount_url"`
		TaskID      string     `json:"task_id"`
		CreatedAt   time.Time  `json:"created_at"`
		UpdatedAt   time.Time  `json:"updated_at"`
		CompletedAt *time.Time `json:"completed_at,omitempty"`
	}

	var records []OrderHistoryRecord
	for rows.Next() {
		var rec OrderHistoryRecord
		var completedAt sql.NullTime
		errScan := rows.Scan(
			&rec.ID,
			&rec.Username,
			&rec.Password,
			&rec.TwoFactor,
			&rec.ExtraEmail,
			&rec.Status,
			&rec.Message,
			&rec.DiscountURL,
			&rec.TaskID,
			&rec.CreatedAt,
			&rec.UpdatedAt,
			&completedAt,
		)
		if errScan != nil {
			log.Printf("Error scanning history row: %v\n", errScan)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "读取数据失败",
			})
			return
		}
		if completedAt.Valid {
			rec.CompletedAt = &completedAt.Time
		}
		records = append(records, rec)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"records": records,
	})
}

func handleAdminKeys(w http.ResponseWriter, r *http.Request) {
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
	statusFilter := r.URL.Query().Get("status")

	whereClauses := []string{"1=1"}
	var args []interface{}

	if searchTerm != "" {
		whereClauses = append(whereClauses, "(system_key LIKE ? OR vendor_key LIKE ? OR original_key LIKE ?)")
		likeArg := "%" + searchTerm + "%"
		args = append(args, likeArg, likeArg, likeArg)
	}

	if statusFilter != "" {
		whereClauses = append(whereClauses, "status = ?")
		args = append(args, statusFilter)
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	var totalCount int
	errCount := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM system_keys WHERE %s", whereSQL), args...).Scan(&totalCount)
	if errCount != nil {
		log.Printf("Error counting system keys: %v\n", errCount)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询总数失败",
		})
		return
	}

	dataQuery := fmt.Sprintf(`
		SELECT id, system_key, vendor, vendor_key, status, original_key, created_at, updated_at
		FROM system_keys
		WHERE %s
		ORDER BY id DESC
		LIMIT ? OFFSET ?`, whereSQL)

	dataArgs := append(args, pageSize, offset)
	rows, errRows := db.Query(dataQuery, dataArgs...)
	if errRows != nil {
		log.Printf("Error querying system keys: %v\n", errRows)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询卡密列表失败",
		})
		return
	}
	defer rows.Close()

	type SystemKeyRow struct {
		ID          int64     `json:"id"`
		SystemKey   string    `json:"system_key"`
		Vendor      string    `json:"vendor"`
		VendorKey   string    `json:"vendor_key"`
		Status      string    `json:"status"`
		OriginalKey string    `json:"original_key"`
		CreatedAt   time.Time `json:"created_at"`
		UpdatedAt   time.Time `json:"updated_at"`
	}

	var records []SystemKeyRow
	for rows.Next() {
		var row SystemKeyRow
		errScan := rows.Scan(
			&row.ID,
			&row.SystemKey,
			&row.Vendor,
			&row.VendorKey,
			&row.Status,
			&row.OriginalKey,
			&row.CreatedAt,
			&row.UpdatedAt,
		)
		if errScan != nil {
			log.Printf("Error scanning system key row: %v\n", errScan)
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "解析数据失败",
			})
			return
		}
		records = append(records, row)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"total":     totalCount,
		"page":      page,
		"page_size": pageSize,
		"records":   records,
	})
}

func main() {
	// 1. Initialize database connection
	initDB()

	// 2. Setup embedded static files server
	staticFS, err := fs.Sub(embedFS, "frontend")
	if err != nil {
		log.Fatalf("Error accessing embedded frontend files: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	http.Handle("/", &adminStaticServer{fileServer: fileServer})

	// 3. API - Submit card secret and accounts
	http.HandleFunc("/api/submit", handleSubmit)

	// 4. API - Query status of a card secret
	http.HandleFunc("/api/query", handleQuery)

	// API - Batch convert third party keys (Admin protected)
	http.HandleFunc("/api/convert_keys", requireAdmin(handleConvertKeys))

	// API - Reset existing system keys and link to new ones (Whitelisted)
	http.HandleFunc("/api/reset_keys", handleResetKeys)

	// Admin APIs
	http.HandleFunc("/api/admin/login", handleAdminLogin)
	http.HandleFunc("/api/admin/logout", handleAdminLogout)
	http.HandleFunc("/api/admin/check", handleAdminCheck)
	http.HandleFunc("/api/admin/orders", requireAdmin(handleAdminOrders))
	http.HandleFunc("/api/admin/orders/update", requireAdmin(handleAdminOrdersUpdate))
	http.HandleFunc("/api/admin/orders/history", requireAdmin(handleAdminOrderHistory))
	http.HandleFunc("/api/admin/keys", requireAdmin(handleAdminKeys))

	// Start background worker for periodic status sync and key invalidation
	go startBackgroundSync(5 * time.Minute)

	// 5. Start server
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	log.Printf("Server starting on http://localhost:%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
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
	errKeyQuery := db.QueryRow("SELECT vendor, vendor_key, status FROM system_keys WHERE system_key = ?", req.CardSecret).
		Scan(&vendor, &vendorKey, &keyStatus)

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
		result, errInsert := tx.Exec("INSERT INTO orders (card_secret, mode, vendor, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
			req.CardSecret, req.Mode, vendor, now, now)
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
		// If order already exists, ensure vendor is set
		_, errUpdateVendor := tx.Exec("UPDATE orders SET vendor = ?, updated_at = ? WHERE id = ? AND (vendor = '' OR vendor IS NULL)", vendor, now, orderID)
		if errUpdateVendor != nil {
			log.Printf("Warning: failed to update vendor: %v\n", errUpdateVendor)
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
	if vendor != "pass.aisale.one" {
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
					localStatus = "pending"
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

func respondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

// Connection test helper for unit testing
func setupTestDB(t interface {
	Fatalf(format string, args ...interface{})
}) {
	// Re-initialize using local test DB if required, but standard connection should be mocked or use sqlite.
	// For these tests, we will mock DB operations if needed, or connect directly to standard DB.
}

func startBackgroundSync(interval time.Duration) {
	ticker := time.NewTicker(interval)
	// Run once immediately on start
	syncPendingAndInvalidate()

	for range ticker.C {
		syncPendingAndInvalidate()
	}
}

func syncPendingAndInvalidate() {
	log.Println("Starting background sync for pending tasks...")
	// 1. Find all pending records that have a vendor and a task ID
	rows, err := db.Query(`
		SELECT r.id, o.card_secret, COALESCE(sk.vendor_key, o.card_secret) AS vendor_key, r.task_id, o.vendor
		FROM account_records r
		JOIN orders o ON r.order_id = o.id
		LEFT JOIN system_keys sk ON o.card_secret = sk.system_key
		WHERE r.status NOT IN ('success', 'failed') AND o.vendor = 'pass.aisale.one' AND r.task_id != ''`)
	if err != nil {
		log.Printf("Background sync: failed to query pending records: %v\n", err)
		return
	}
	defer rows.Close()

	type PendingRecord struct {
		ID         int64
		CardSecret string
		VendorKey  string
		TaskID     string
		Vendor     string
	}

	var pendingList []PendingRecord
	for rows.Next() {
		var pr PendingRecord
		if err := rows.Scan(&pr.ID, &pr.CardSecret, &pr.VendorKey, &pr.TaskID, &pr.Vendor); err != nil {
			log.Printf("Background sync: failed to scan pending record: %v\n", err)
			continue
		}
		pendingList = append(pendingList, pr)
	}
	rows.Close()

	if len(pendingList) > 0 {
		var wg sync.WaitGroup
		for _, pr := range pendingList {
			wg.Add(1)
			go func(record PendingRecord) {
				defer wg.Done()
				res, err := queryTaskFromVendor(record.VendorKey, record.TaskID)
				if err != nil {
					log.Printf("Background sync: error querying vendor for task %s: %v\n", record.TaskID, err)
					return
				}
				if !res.Success {
					log.Printf("Background sync: vendor returned error status for task %s: %s\n", record.TaskID, res.Message)
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
					localStatus = "pending"
				case "pending":
					localStatus = "pending"
				default:
					localStatus = "pending"
				}

				discountURL := ""
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
						localStatus, message, discountURL, *completedAt, now, record.ID)
				} else {
					_, errUpdate = db.Exec(`
						UPDATE account_records 
						SET status = ?, message = ?, discount_url = ?, updated_at = ? 
						WHERE id = ?`,
						localStatus, message, discountURL, now, record.ID)
				}

				if errUpdate != nil {
					log.Printf("Background sync: failed to update database for task %s: %v\n", record.TaskID, errUpdate)
				}
			}(pr)
		}
		wg.Wait()
	}

	// 2. Invalidate keys for any active system keys whose orders have successful subscriptions
	now := time.Now()
	res, err := db.Exec(`
		UPDATE system_keys sk
		JOIN orders o ON sk.system_key = o.card_secret
		JOIN account_records r ON r.order_id = o.id
		SET sk.status = 'inactive', sk.updated_at = ?
		WHERE sk.status = 'active' AND r.status = 'success'`, now)
	if err != nil {
		log.Printf("Background sync: failed to invalidate system keys: %v\n", err)
	} else {
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected > 0 {
			log.Printf("Background sync: successfully invalidated %d active system keys due to successful subscriptions\n", rowsAffected)
		}
	}
}
