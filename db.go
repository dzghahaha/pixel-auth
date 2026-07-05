package main

import (
	"crypto/rand"
	"database/sql"
	"log"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

// AccountRecord represents a single account status within a card order
type AccountRecord struct {
	Username    string     `json:"username"`
	Password    string     `json:"password"`
	TwoFactor   string     `json:"two_factor"`
	ExtraEmail  string     `json:"extra_email,omitempty"`
	Status      string     `json:"status"` // "pending", "running", "success", "failed"
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
		creator_id BIGINT UNSIGNED DEFAULT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_card_secret (card_secret),
		KEY idx_orders_creator_id (creator_id)
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
		note VARCHAR(255) NOT NULL DEFAULT '',
		creator_id BIGINT UNSIGNED DEFAULT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_system_key (system_key),
		KEY idx_vendor_key (vendor_key),
		KEY idx_original_key (original_key),
		KEY idx_system_keys_creator_id (creator_id)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	adminsDDL := `
	CREATE TABLE IF NOT EXISTS admins (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		username VARCHAR(128) NOT NULL UNIQUE,
		nickname VARCHAR(128) NOT NULL DEFAULT '',
		password_hash VARCHAR(255) NOT NULL,
		role VARCHAR(32) NOT NULL DEFAULT 'user',
		permissions TEXT DEFAULT NULL,
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

	adminPermissionsDDL := `
	CREATE TABLE IF NOT EXISTS admin_permissions (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		admin_id BIGINT UNSIGNED NOT NULL,
		permission VARCHAR(64) NOT NULL,
		created_at DATETIME NOT NULL,
		CONSTRAINT fk_perm_admin_id FOREIGN KEY (admin_id) REFERENCES admins (id) ON DELETE CASCADE,
		UNIQUE KEY idx_admin_permission (admin_id, permission)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	log.Println("Ensuring database tables 'orders', 'account_records', 'system_keys', 'admins', 'admin_sessions', and 'admin_permissions' exist...")
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

	if _, err := db.Exec(adminPermissionsDDL); err != nil {
		log.Fatalf("Error creating admin_permissions table: %v", err)
	}

	systemSettingsDDL := `
	CREATE TABLE IF NOT EXISTS system_settings (
		setting_key VARCHAR(128) PRIMARY KEY,
		setting_value TEXT NOT NULL,
		updated_at DATETIME NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	keyOrdersDDL := `
	CREATE TABLE IF NOT EXISTS key_orders (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		out_trade_no VARCHAR(128) NOT NULL,
		status VARCHAR(32) NOT NULL DEFAULT 'pending',
		quantity INT NOT NULL,
		price DECIMAL(10,2) NOT NULL,
		total_amount DECIMAL(10,2) NOT NULL,
		pay_type VARCHAR(16) NOT NULL DEFAULT 'wxpay',
		creator_id BIGINT UNSIGNED DEFAULT NULL,
		card_keys TEXT DEFAULT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_out_trade_no (out_trade_no)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	cardStockDDL := `
	CREATE TABLE IF NOT EXISTS card_stock (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		card_key VARCHAR(128) NOT NULL,
		vendor VARCHAR(64) NOT NULL DEFAULT 'ai.deard.fun',
		vendor_key VARCHAR(256) NOT NULL DEFAULT '',
		status VARCHAR(32) NOT NULL DEFAULT 'available',
		original_key VARCHAR(128) NOT NULL DEFAULT '',
		note VARCHAR(256) NOT NULL DEFAULT '',
		creator_id BIGINT UNSIGNED DEFAULT NULL,
		order_id BIGINT UNSIGNED DEFAULT NULL,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_card_key (card_key),
		KEY idx_status (status)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	keyVendorsDDL := `
	CREATE TABLE IF NOT EXISTS key_vendors (
		id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		name VARCHAR(64) NOT NULL,
		display_name VARCHAR(128) NOT NULL,
		api_url VARCHAR(256) NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE KEY idx_name (name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;`

	if _, err := db.Exec(adminSessionsDDL); err != nil {
		log.Fatalf("Error creating admin_sessions table: %v", err)
	}

	if _, err := db.Exec(systemSettingsDDL); err != nil {
		log.Fatalf("Error creating system_settings table: %v", err)
	}

	if _, err := db.Exec(keyOrdersDDL); err != nil {
		log.Fatalf("Error creating key_orders table: %v", err)
	}

	if _, err := db.Exec(cardStockDDL); err != nil {
		log.Fatalf("Error creating card_stock table: %v", err)
	}

	if _, err := db.Exec(keyVendorsDDL); err != nil {
		log.Fatalf("Error creating key_vendors table: %v", err)
	}

	// Migration: Add pay_type to key_orders if it doesn't exist (ignores error if column exists)
	_, _ = db.Exec("ALTER TABLE key_orders ADD COLUMN pay_type VARCHAR(16) NOT NULL DEFAULT 'wxpay'")
	_, _ = db.Exec("ALTER TABLE key_orders ADD COLUMN creator_id BIGINT UNSIGNED DEFAULT NULL")

	// Migration: Add columns to card_stock if they don't exist
	_, _ = db.Exec("ALTER TABLE card_stock ADD COLUMN vendor VARCHAR(64) NOT NULL DEFAULT 'ai.deard.fun'")
	_, _ = db.Exec("ALTER TABLE card_stock ADD COLUMN vendor_key VARCHAR(256) NOT NULL DEFAULT ''")
	_, _ = db.Exec("ALTER TABLE card_stock ADD COLUMN original_key VARCHAR(128) NOT NULL DEFAULT ''")
	_, _ = db.Exec("ALTER TABLE card_stock ADD COLUMN note VARCHAR(256) NOT NULL DEFAULT ''")
	_, _ = db.Exec("ALTER TABLE card_stock ADD COLUMN creator_id BIGINT UNSIGNED DEFAULT NULL")

	// Insert default vendors if not present
	defaultVendors := []struct {
		Name        string
		DisplayName string
		APIURL      string
	}{
		{"ai.deard.fun", "系统直接生成 (ai.deard.fun)", ""},
		{"pass.aisale.one", "合作商卡网 (pass.aisale.one)", "https://pass.aisale.one/gateway.php"},
	}
	for _, v := range defaultVendors {
		var count int
		db.QueryRow("SELECT COUNT(*) FROM key_vendors WHERE name = ?", v.Name).Scan(&count)
		if count == 0 {
			db.Exec("INSERT INTO key_vendors (name, display_name, api_url, created_at, updated_at) VALUES (?, ?, ?, NOW(), NOW())",
				v.Name, v.DisplayName, v.APIURL)
		}
	}

	// Insert default settings if not present
	defaultSettings := map[string]string{
		"two_factor_tutorial_url": "https://www.yuque.com/taozi-khqsp/rrub4i/fxm5dgln1rh5iwd1",
		"epay_pid":                "1668",
		"epay_key":                "",
		"epay_url":                "https://pay.vansdesign.cn/",
		"epay_wx_channel":         "201906181353",
		"epay_alipay_channel":     "",
		"key_price":               "9.99",
	}
	for k, v := range defaultSettings {
		var countSettings int
		errCheckSettings := db.QueryRow("SELECT COUNT(*) FROM system_settings WHERE setting_key = ?", k).Scan(&countSettings)
		if errCheckSettings == nil && countSettings == 0 {
			_, errInsert := db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, ?)",
				k, v, time.Now())
			if errInsert != nil {
				log.Printf("Warning: failed to insert default setting %s: %v\n", k, errInsert)
			}
		}
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

	var hasNote bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'system_keys' 
		  AND COLUMN_NAME = 'note'
	`).Scan(&hasNote)
	if errCheck == nil && !hasNote {
		log.Println("Adding 'note' column to 'system_keys' table...")
		if _, err := db.Exec("ALTER TABLE system_keys ADD COLUMN note VARCHAR(255) NOT NULL DEFAULT ''"); err != nil {
			log.Printf("Warning: failed to add note column: %v\n", err)
		}
	}

	// Dynamic Migrations for User Management and Creator ID Isolation
	var hasRole bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'admins' 
		  AND COLUMN_NAME = 'role'
	`).Scan(&hasRole)
	if errCheck == nil && !hasRole {
		log.Println("Adding 'role' column to 'admins' table...")
		if _, err := db.Exec("ALTER TABLE admins ADD COLUMN role VARCHAR(32) NOT NULL DEFAULT 'user'"); err != nil {
			log.Printf("Warning: failed to add role column to admins: %v\n", err)
		} else {
			// Update default 'admin' user to have 'admin' role
			if _, err := db.Exec("UPDATE admins SET role = 'admin' WHERE username = 'admin'"); err != nil {
				log.Printf("Warning: failed to set default admin role: %v\n", err)
			}
		}
	}

	var hasPermissions bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'admins' 
		  AND COLUMN_NAME = 'permissions'
	`).Scan(&hasPermissions)
	if errCheck == nil && !hasPermissions {
		log.Println("Adding 'permissions' column to 'admins' table...")
		if _, err := db.Exec("ALTER TABLE admins ADD COLUMN permissions TEXT DEFAULT NULL"); err != nil {
			log.Printf("Warning: failed to add permissions column to admins: %v\n", err)
		}
	}

	var hasNickname bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'admins' 
		  AND COLUMN_NAME = 'nickname'
	`).Scan(&hasNickname)
	if errCheck == nil && !hasNickname {
		log.Println("Adding 'nickname' column to 'admins' table...")
		if _, err := db.Exec("ALTER TABLE admins ADD COLUMN nickname VARCHAR(128) NOT NULL DEFAULT ''"); err != nil {
			log.Printf("Warning: failed to add nickname column to admins: %v\n", err)
		}
	}

	var hasKeyCreator bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'system_keys' 
		  AND COLUMN_NAME = 'creator_id'
	`).Scan(&hasKeyCreator)
	if errCheck == nil && !hasKeyCreator {
		log.Println("Adding 'creator_id' column to 'system_keys' table...")
		if _, err := db.Exec("ALTER TABLE system_keys ADD COLUMN creator_id BIGINT UNSIGNED DEFAULT NULL"); err != nil {
			log.Printf("Warning: failed to add creator_id column to system_keys: %v\n", err)
		} else {
			// Add index
			if _, err := db.Exec("ALTER TABLE system_keys ADD KEY idx_system_keys_creator_id (creator_id)"); err != nil {
				log.Printf("Warning: failed to add idx_system_keys_creator_id index: %v\n", err)
			}
		}
	}

	var hasOrderCreator bool
	errCheck = db.QueryRow(`
		SELECT COUNT(*) 
		FROM information_schema.COLUMNS 
		WHERE TABLE_SCHEMA = DATABASE() 
		  AND TABLE_NAME = 'orders' 
		  AND COLUMN_NAME = 'creator_id'
	`).Scan(&hasOrderCreator)
	if errCheck == nil && !hasOrderCreator {
		log.Println("Adding 'creator_id' column to 'orders' table...")
		if _, err := db.Exec("ALTER TABLE orders ADD COLUMN creator_id BIGINT UNSIGNED DEFAULT NULL"); err != nil {
			log.Printf("Warning: failed to add creator_id column to orders: %v\n", err)
		} else {
			// Add index
			if _, err := db.Exec("ALTER TABLE orders ADD KEY idx_orders_creator_id (creator_id)"); err != nil {
				log.Printf("Warning: failed to add idx_orders_creator_id index: %v\n", err)
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
		_, errInsert := db.Exec("INSERT INTO admins (username, password_hash, role, permissions, created_at, updated_at) VALUES (?, ?, ?, NULL, ?, ?)",
			"admin", string(hashedPassword), "admin", now, now)
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

func getSetting(key string, defaultVal string) string {
	var val string
	err := db.QueryRow("SELECT setting_value FROM system_settings WHERE setting_key = ?", key).Scan(&val)
	if err != nil {
		return defaultVal
	}
	return val
}

// Connection test helper for unit testing
func setupTestDB(t interface {
	Fatalf(format string, args ...interface{})
}) {
	// Re-initialize using local test DB if required, but standard connection should be mocked or use sqlite.
	// For these tests, we will mock DB operations if needed, or connect directly to standard DB.
}
