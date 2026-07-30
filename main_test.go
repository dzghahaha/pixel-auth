package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

func initTestDB(t *testing.T) {
	// DSN for test database
	testDSN := os.Getenv("MYSQL_TEST_DSN")
	if testDSN == "" {
		if normalDSN := os.Getenv("MYSQL_DSN"); normalDSN != "" {
			if normalCfg, err := mysql.ParseDSN(normalDSN); err == nil {
				normalCfg.DBName = "pixel_auth_test"
				testDSN = normalCfg.FormatDSN()
			}
		}
	}
	if testDSN == "" {
		testDSN = "root:d2dsoft_123@tcp(212.129.244.194:3306)/pixel_auth_test?parseTime=true&loc=Local"
	}

	cfg, err := mysql.ParseDSN(testDSN)
	if err != nil {
		t.Skipf("Skipping integration test; invalid DSN: %v", err)
		return
	}

	dbName := cfg.DBName
	if dbName == "" {
		dbName = "pixel_auth_test"
	}

	cfg.DBName = ""
	cfg.Timeout = 5 * time.Second
	serverDSN := cfg.FormatDSN()

	// Connect to server to ensure test database exists
	tempDB, err := sql.Open("mysql", serverDSN)
	if err != nil {
		t.Skipf("Skipping integration test; cannot connect to MySQL server: %v", err)
		return
	}
	_, errDB := tempDB.Exec("CREATE DATABASE IF NOT EXISTS `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci")
	tempDB.Close()
	if errDB != nil {
		t.Skipf("Skipping integration test; cannot create test database: %v", errDB)
		return
	}

	// Connect to test database
	var errOpen error
	db, errOpen = sql.Open("mysql", testDSN)
	if errOpen != nil {
		t.Fatalf("Error opening test database connection: %v", errOpen)
	}

	// Ping database
	if err := db.Ping(); err != nil {
		t.Skipf("Skipping integration test; database ping failed: %v", err)
		return
	}

	// Re-create tables
	_, _ = db.Exec("DROP TABLE IF EXISTS system_settings")
	_, _ = db.Exec("DROP TABLE IF EXISTS key_vendors")
	createTables()

	// Clear tables for test isolation
	if _, err := db.Exec("DELETE FROM account_records"); err != nil {
		t.Logf("Failed to clear account_records: %v", err)
	}
	if _, err := db.Exec("DELETE FROM orders"); err != nil {
		t.Logf("Failed to clear orders: %v", err)
	}
	if _, err := db.Exec("DELETE FROM system_keys"); err != nil {
		t.Logf("Failed to clear system_keys: %v", err)
	}
	if _, err := db.Exec("DELETE FROM key_orders"); err != nil {
		t.Logf("Failed to clear key_orders: %v", err)
	}
	if _, err := db.Exec("DELETE FROM card_stock"); err != nil {
		t.Logf("Failed to clear card_stock: %v", err)
	}
}

func TestMySQLStorage(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Test Insert Card Order
	cardSecret := "TEST-CARD-SECRET-123"
	now := time.Now().Round(time.Second)

	res, err := db.Exec("INSERT INTO orders (card_secret, mode, created_at, updated_at) VALUES (?, ?, ?, ?)",
		cardSecret, "single", now, now)
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	orderID, _ := res.LastInsertId()

	// Test Insert Account Record
	_, err = db.Exec("INSERT INTO account_records (order_id, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "testuser@gmail.com", "password123", "2FAKEY", "pending", "排队中", now, now)
	if err != nil {
		t.Fatalf("failed to insert record: %v", err)
	}

	// Query it back
	var username, password, twoFactor, status, message string
	err = db.QueryRow("SELECT username, password, two_factor, status, message FROM account_records WHERE order_id = ?", orderID).
		Scan(&username, &password, &twoFactor, &status, &message)
	if err != nil {
		t.Fatalf("failed to query record: %v", err)
	}

	if username != "testuser@gmail.com" || password != "password123" || twoFactor != "2FAKEY" || status != "pending" {
		t.Errorf("scanned query record does not match inserted values")
	}
}

func TestHandleSubmitAndQueryAPI(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Test query for non-existing card
	reqQuery := httptest.NewRequest(http.MethodGet, "/api/query?card_secret=NON-EXIST", nil)
	rrQuery := httptest.NewRecorder()
	handleQuery(rrQuery, reqQuery)

	if rrQuery.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rrQuery.Code)
	}

	var queryResp map[string]interface{}
	if err := json.Unmarshal(rrQuery.Body.Bytes(), &queryResp); err != nil {
		t.Fatalf("failed to parse query response: %v", err)
	}

	if queryResp["success"] != true {
		t.Errorf("expected success: true")
	}

	records, ok := queryResp["records"].([]interface{})
	if !ok || len(records) != 0 {
		t.Errorf("expected records to be an empty array, got %v", queryResp["records"])
	}

	// 2. Test Submit API
	_, errDbInsert := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"VALID-CARD", "mock", "MOCK-VENDOR-KEY", "active", "VALID-CARD", time.Now(), time.Now())
	if errDbInsert != nil {
		t.Fatalf("failed to insert mock system key: %v", errDbInsert)
	}

	subReq := SubmitRequest{
		CardSecret: "VALID-CARD",
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Errorf("expected submit status 200, got %d", rrSubmit.Code)
	}

	var submitResp map[string]interface{}
	if err := json.Unmarshal(rrSubmit.Body.Bytes(), &submitResp); err != nil {
		t.Fatalf("failed to parse submit response: %v", err)
	}

	if submitResp["success"] != true {
		t.Errorf("expected submit success to be true")
	}

	// 3. Test query for newly submitted card
	reqQuery2 := httptest.NewRequest(http.MethodGet, "/api/query?card_secret=VALID-CARD", nil)
	rrQuery2 := httptest.NewRecorder()
	handleQuery(rrQuery2, reqQuery2)

	if rrQuery2.Code != http.StatusOK {
		t.Errorf("expected query status 200, got %d", rrQuery2.Code)
	}

	var queryResp2 map[string]interface{}
	if err := json.Unmarshal(rrQuery2.Body.Bytes(), &queryResp2); err != nil {
		t.Fatalf("failed to parse query response: %v", err)
	}

	records2, ok := queryResp2["records"].([]interface{})
	if !ok || len(records2) != 1 {
		t.Fatalf("expected records to have 1 entry, got %v", queryResp2["records"])
	}

	rec := records2[0].(map[string]interface{})
	if rec["username"] != "user@example.com" {
		t.Errorf("expected username 'user@example.com', got '%s'", rec["username"])
	}
	if rec["status"] != "pending" {
		t.Errorf("expected status 'pending', got '%s'", rec["status"])
	}
}

func TestInvalidSubmitRequests(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Test missing card secret
	subReq := SubmitRequest{
		CardSecret: "",
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rrSubmit.Code)
	}

	if !strings.Contains(rrSubmit.Body.String(), "卡密不能为空") {
		t.Errorf("expected error message about card secret, got %s", rrSubmit.Body.String())
	}

	// 2. Test empty accounts list
	subReq2 := SubmitRequest{
		CardSecret: "SOME-CARD",
		Mode:       "single",
		Accounts:   nil,
	}

	bodyBytes2, _ := json.Marshal(subReq2)
	reqSubmit2 := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes2))
	rrSubmit2 := httptest.NewRecorder()
	handleSubmit(rrSubmit2, reqSubmit2)

	if rrSubmit2.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rrSubmit2.Code)
	}

	if !strings.Contains(rrSubmit2.Body.String(), "账号列表不能为空") {
		t.Errorf("expected error message about empty accounts list, got %s", rrSubmit2.Body.String())
	}
}

func TestSubmitFilterOnPreviousPasswordErrors(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	sysKey := "FILTER-TEST-KEY"
	_, err := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		sysKey, "mock", "MOCK-VENDOR-KEY", "active", sysKey, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert mock system key: %v", err)
	}

	// Create a mock order to associate with account_records
	res, err := db.Exec("INSERT INTO orders (card_secret, mode, vendor, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"PREV-CARD-SECRET", "single", "mock", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	orderID, _ := res.LastInsertId()

	// 1. Insert a failed record with "请输入正确的密码" for test_user@gmail.com (pwd: "wrong_pass123")
	_, err = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "PREV-CARD-SECRET", "test_user@gmail.com", "wrong_pass123", "12345678901234567890123456789012", "failed", "请输入正确的密码", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert failed account record: %v", err)
	}

	// 2. Insert a failed record with "请输入正确的邮箱账号" for bad_email@gmail.com
	_, err = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "PREV-CARD-SECRET", "bad_email@gmail.com", "pass123", "12345678901234567890123456789012", "failed", "请输入正确的邮箱账号", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert failed account record: %v", err)
	}

	// 3. Submit with test_user@gmail.com and SAME password -> Should now SUCCEED (200) instead of failing
	subReq := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "test_user@gmail.com",
		Password:  "wrong_pass123", // same password
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d. Body: %s", rrSubmit.Code, rrSubmit.Body.String())
	}

	// Clean up records and orders created by the submission to avoid active order blocks for the next step
	_, _ = db.Exec("DELETE FROM account_records WHERE card_secret = ?", sysKey)
	_, _ = db.Exec("DELETE FROM orders WHERE card_secret = ?", sysKey)

	// 4. Submit with test_user@gmail.com and DIFFERENT password -> Should SUCCEED (returns 200)
	subReqDiffPwd := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReqDiffPwd.Accounts = append(subReqDiffPwd.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "test_user@gmail.com",
		Password:  "new_pass_456", // different password
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytesDiff, _ := json.Marshal(subReqDiffPwd)
	reqSubmitDiff := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytesDiff))
	rrSubmitDiff := httptest.NewRecorder()
	handleSubmit(rrSubmitDiff, reqSubmitDiff)

	if rrSubmitDiff.Code != http.StatusOK {
		t.Errorf("expected status 200 for different password, got %d. Body: %s", rrSubmitDiff.Code, rrSubmitDiff.Body.String())
	}

	// Clean up records and orders created by the submission to avoid active order blocks for the next step
	_, _ = db.Exec("DELETE FROM account_records WHERE card_secret = ?", sysKey)
	_, _ = db.Exec("DELETE FROM orders WHERE card_secret = ?", sysKey)

	// 5. Submit with bad_email@gmail.com and DIFFERENT password -> Should now SUCCEED (200) instead of failing
	subReq2 := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq2.Accounts = append(subReq2.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "bad_email@gmail.com",
		Password:  "another_pwd_789", // different password
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes2, _ := json.Marshal(subReq2)
	reqSubmit2 := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes2))
	rrSubmit2 := httptest.NewRecorder()
	handleSubmit(rrSubmit2, reqSubmit2)

	if rrSubmit2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d. Body: %s", rrSubmit2.Code, rrSubmit2.Body.String())
	}
}

func TestDuplicateSubmitPreventedWhenQueuingOrRunning(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Create system key mapping
	sysKey := "DUP-TEST-KEY"
	_, errDbInsert := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		sysKey, "mock", "MOCK-VENDOR-KEY", "active", sysKey, time.Now(), time.Now())
	if errDbInsert != nil {
		t.Fatalf("failed to insert mock system key: %v", errDbInsert)
	}

	// 2. First submit (sets status to pending)
	subReq := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user1@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Errorf("expected first submit status 200, got %d", rrSubmit.Code)
	}

	// 3. Second submit (should fail since first submission is still pending)
	subReq2 := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq2.Accounts = append(subReq2.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user2@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes2, _ := json.Marshal(subReq2)
	reqSubmit2 := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes2))
	rrSubmit2 := httptest.NewRecorder()
	handleSubmit(rrSubmit2, reqSubmit2)

	if rrSubmit2.Code != http.StatusBadRequest {
		t.Errorf("expected second submit status 400, got %d", rrSubmit2.Code)
	}

	if !strings.Contains(rrSubmit2.Body.String(), "该卡密对应的订单已经在排队或执行中") {
		t.Errorf("expected error message about active orders, got %s", rrSubmit2.Body.String())
	}
}

func TestConcurrentSubmitLock(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Create active system key
	sysKey := "CONCURRENT-LOCK-KEY"
	_, errDbInsert := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		sysKey, "mock", "MOCK-VENDOR-KEY", "active", sysKey, time.Now(), time.Now())
	if errDbInsert != nil {
		t.Fatalf("failed to insert mock system key: %v", errDbInsert)
	}

	// 2. Submit concurrent requests
	// We manually lock the key in activeSubmissions first to simulate another request processing it
	activeSubmissions.Store(sysKey, struct{}{})
	defer activeSubmissions.Delete(sysKey)

	// Now try to submit the same key. It should fail immediately with the locking message.
	subReq := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusBadRequest {
		t.Errorf("expected concurrent submit status 400, got %d", rrSubmit.Code)
	}

	if !strings.Contains(rrSubmit.Body.String(), "该卡密正在处理中，请勿重复提交") {
		t.Errorf("expected error message about processing key, got %s", rrSubmit.Body.String())
	}
}

func TestVendorIntegration(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Setup mock vendor API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Read error", http.StatusInternalServerError)
			return
		}

		var reqMap map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &reqMap); err != nil {
			http.Error(w, "Bad JSON", http.StatusBadRequest)
			return
		}

		action := reqMap["action"].(string)
		w.Header().Set("Content-Type", "application/json")

		if action == "submit_task" {
			// Mock submission response
			resp := map[string]interface{}{
				"success": true,
				"task_id": "TK-MOCK-12345",
				"message": "提交成功",
			}
			json.NewEncoder(w).Encode(resp)
		} else if action == "get_status" {
			// Mock query status response
			resp := map[string]interface{}{
				"success": true,
				"data": map[string]interface{}{
					"task_id":       reqMap["task_id"].(string),
					"status":        "Success",
					"message":       "执行成功",
					"has_offer_url": true,
					"offer_url":     "https://pass.aisale.one/offer/123",
				},
			}
			json.NewEncoder(w).Encode(resp)
		} else if action == "cancel_task" {
			resp := map[string]interface{}{
				"success": true,
				"message": "Task cancelled successfully",
			}
			json.NewEncoder(w).Encode(resp)
		}
	}))
	defer mockServer.Close()

	// Direct vendor Base URL to the mock server
	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// Insert vendor mapping for test card secret
	_, errDbInsert := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		"ck-test-secret-999", "pass.aisale.one", "REAL-CDKEY-123", "active", "ck-test-secret-999", time.Now(), time.Now())
	if errDbInsert != nil {
		t.Fatalf("failed to insert test system key: %v", errDbInsert)
	}

	// 2. Submit order using a ck-prefix card secret
	subReq := SubmitRequest{
		CardSecret: "ck-test-secret-999",
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "vendor_test@example.com",
		Password:  "password",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Errorf("expected submit status 200, got %d. Body: %s", rrSubmit.Code, rrSubmit.Body.String())
	}

	// Verify order has vendor "pass.aisale.one" and record has task_id
	var vendor string
	err := db.QueryRow("SELECT vendor FROM orders WHERE card_secret = ?", "ck-test-secret-999").Scan(&vendor)
	if err != nil {
		t.Fatalf("failed to query order vendor: %v", err)
	}
	if vendor != "pass.aisale.one" {
		t.Errorf("expected vendor to be 'pass.aisale.one', got '%s'", vendor)
	}

	var taskID, status string
	err = db.QueryRow("SELECT task_id, status FROM account_records r JOIN orders o ON r.order_id = o.id WHERE o.card_secret = ?", "ck-test-secret-999").
		Scan(&taskID, &status)
	if err != nil {
		t.Fatalf("failed to query record: %v", err)
	}
	if taskID != "TK-MOCK-12345" {
		t.Errorf("expected task_id to be 'TK-MOCK-12345', got '%s'", taskID)
	}
	if status != "pending" {
		t.Errorf("expected initial status to be 'pending', got '%s'", status)
	}

	// 3. Query the status, which should trigger the mock server call and update the DB
	reqQuery := httptest.NewRequest(http.MethodGet, "/api/query?card_secret=ck-test-secret-999", nil)
	rrQuery := httptest.NewRecorder()
	handleQuery(rrQuery, reqQuery)

	if rrQuery.Code != http.StatusOK {
		t.Errorf("expected query status 200, got %d", rrQuery.Code)
	}

	var queryResp map[string]interface{}
	if err := json.Unmarshal(rrQuery.Body.Bytes(), &queryResp); err != nil {
		t.Fatalf("failed to parse query response: %v", err)
	}

	records, ok := queryResp["records"].([]interface{})
	if !ok || len(records) != 1 {
		t.Fatalf("expected records to have 1 entry, got %v", queryResp["records"])
	}

	rec := records[0].(map[string]interface{})
	if rec["status"] != "success" {
		t.Errorf("expected updated status 'success', got '%s'", rec["status"])
	}
	if rec["discount_url"] != "https://pass.aisale.one/offer/123" {
		t.Errorf("expected updated discount_url 'https://pass.aisale.one/offer/123', got '%s'", rec["discount_url"])
	}
}

func TestConvertKeys(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	convReq := ConvertKeysRequest{
		Vendor:     "pass.aisale.one",
		VendorKeys: []string{"real-key-a", "real-key-b"},
		Multiplier: 2,
		Note:       "test-note-123",
	}

	bodyBytes, _ := json.Marshal(convReq)
	req := httptest.NewRequest(http.MethodPost, "/api/convert_keys", bytes.NewBuffer(bodyBytes))
	rr := httptest.NewRecorder()
	handleConvertKeys(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp ConvertKeysResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse convert response: %v", err)
	}

	if !resp.Success {
		t.Errorf("expected success to be true")
	}

	if len(resp.SystemKeys) != 4 {
		t.Errorf("expected 4 system keys generated, got %d", len(resp.SystemKeys))
	}

	// Verify database rows count
	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM system_keys WHERE vendor = ?", "pass.aisale.one").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query count: %v", err)
	}
	if count != 4 {
		t.Errorf("expected 4 rows in DB, got %d", count)
	}

	var noteCount int
	errNote := db.QueryRow("SELECT COUNT(*) FROM system_keys WHERE vendor = ? AND note = ?", "pass.aisale.one", "test-note-123").Scan(&noteCount)
	if errNote != nil {
		t.Fatalf("failed to query count with note: %v", errNote)
	}
	if noteCount != 4 {
		t.Errorf("expected 4 rows in DB with note = 'test-note-123', got %d", noteCount)
	}

	// Insert mock user to test creator_id override
	var mockUserID int64
	resOverride, errInsertAdmin := db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES ('mockcreator', 'hash', 'user', NOW(), NOW())")
	if errInsertAdmin != nil {
		t.Fatalf("failed to insert mock admin: %v", errInsertAdmin)
	}
	mockUserID, _ = resOverride.LastInsertId()

	// Test Convert Keys with CreatorID override
	convReqOverride := ConvertKeysRequest{
		Vendor:     "pass.aisale.one",
		VendorKeys: []string{"override-key"},
		Multiplier: 1,
		Note:       "override-test",
		CreatorID:  &mockUserID,
	}
	bodyOverrideBytes, _ := json.Marshal(convReqOverride)
	reqOverride := httptest.NewRequest(http.MethodPost, "/api/convert_keys", bytes.NewBuffer(bodyOverrideBytes))
	rrOverride := httptest.NewRecorder()
	handleConvertKeys(rrOverride, reqOverride)

	if rrOverride.Code != http.StatusOK {
		t.Fatalf("expected status 200 for override convert, got %d. Body: %s", rrOverride.Code, rrOverride.Body.String())
	}

	var respOverride ConvertKeysResponse
	json.Unmarshal(rrOverride.Body.Bytes(), &respOverride)
	if len(respOverride.SystemKeys) != 1 {
		t.Fatalf("expected 1 system key generated, got %d", len(respOverride.SystemKeys))
	}

	// Verify creator_id in database
	var dbCreatorID int64
	errQueryCreator := db.QueryRow("SELECT creator_id FROM system_keys WHERE system_key = ?", respOverride.SystemKeys[0]).Scan(&dbCreatorID)
	if errQueryCreator != nil {
		t.Fatalf("failed to query creator_id for system key: %v", errQueryCreator)
	}
	if dbCreatorID != mockUserID {
		t.Errorf("expected creator_id to be %d, got %d", mockUserID, dbCreatorID)
	}
}

func TestResetKeys(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Setup mock vendor API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"success": true,
			"task_id": "TK-MOCK-12345",
			"message": "提交成功",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 1. Convert keys first
	convReq := ConvertKeysRequest{
		Vendor:     "pass.aisale.one",
		VendorKeys: []string{"real-vendor-key-xyz"},
		Multiplier: 1,
	}
	bodyBytes, _ := json.Marshal(convReq)
	reqConv := httptest.NewRequest(http.MethodPost, "/api/convert_keys", bytes.NewBuffer(bodyBytes))
	rrConv := httptest.NewRecorder()
	handleConvertKeys(rrConv, reqConv)

	var convResp ConvertKeysResponse
	json.Unmarshal(rrConv.Body.Bytes(), &convResp)
	oldSysKey := convResp.SystemKeys[0]

	// 2. Submit order using this system key
	subReq := SubmitRequest{
		CardSecret: oldSysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "reset_test_user@gmail.com",
		Password:  "pass123",
		TwoFactor: "12345678901234567890123456789012",
	})
	bodyBytesSub, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytesSub))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Fatalf("failed to submit order: %s", rrSubmit.Body.String())
	}

	// Update task status to failed so it's not considered "running" during reset test
	_, errUpdateTask := db.Exec("UPDATE account_records r JOIN orders o ON r.order_id = o.id SET r.status = 'failed' WHERE o.card_secret = ?", oldSysKey)
	if errUpdateTask != nil {
		t.Fatalf("failed to update task status: %v", errUpdateTask)
	}

	// 3. Reset the system key (also supply a dummy non-existing key and a key that will be deactivated)
	resetReq := ResetKeysRequest{
		OldKeys: []string{oldSysKey, "px-non-exist", ""},
	}
	bodyBytesReset, _ := json.Marshal(resetReq)
	reqReset := httptest.NewRequest(http.MethodPost, "/api/reset_keys", bytes.NewBuffer(bodyBytesReset))
	rrReset := httptest.NewRecorder()
	handleResetKeys(rrReset, reqReset)

	if rrReset.Code != http.StatusOK {
		t.Fatalf("expected reset status 200, got %d. Body: %s", rrReset.Code, rrReset.Body.String())
	}

	var resetResp ResetKeysResponse
	json.Unmarshal(rrReset.Body.Bytes(), &resetResp)

	if !resetResp.Success {
		t.Errorf("expected reset success to be true")
	}

	// We only expect 1 new key in output because "px-non-exist" is invalid and skipped!
	if len(resetResp.NewKeys) != 1 {
		t.Errorf("expected 1 new key, got %d", len(resetResp.NewKeys))
	}
	newSysKey := resetResp.NewKeys[0]

	// 4. Verify DB: Old key must be inactive
	var oldStatus string
	err := db.QueryRow("SELECT status FROM system_keys WHERE system_key = ?", oldSysKey).Scan(&oldStatus)
	if err != nil {
		t.Fatalf("failed to query old key: %v", err)
	}
	if oldStatus != "inactive" {
		t.Errorf("expected old key status 'inactive', got '%s'", oldStatus)
	}

	// New key must be active and point to same vendor and vendor_key
	var newVendor, newVendorKey, newStatus string
	err = db.QueryRow("SELECT vendor, vendor_key, status FROM system_keys WHERE system_key = ?", newSysKey).Scan(&newVendor, &newVendorKey, &newStatus)
	if err != nil {
		t.Fatalf("failed to query new key: %v", err)
	}
	if newVendor != "pass.aisale.one" || newVendorKey != "real-vendor-key-xyz" || newStatus != "active" {
		t.Errorf("new key properties mismatch: vendor=%s, key=%s, status=%s", newVendor, newVendorKey, newStatus)
	}

	// 5. Verify order isolation: the order must remain with the oldSysKey
	var orderCardSecret string
	err = db.QueryRow("SELECT o.card_secret FROM orders o JOIN account_records r ON r.order_id = o.id WHERE r.username = 'reset_test_user@gmail.com'").Scan(&orderCardSecret)
	if err != nil {
		t.Fatalf("failed to query order card secret: %v", err)
	}
	if orderCardSecret != oldSysKey {
		t.Errorf("expected order to remain with oldSysKey '%s', got '%s'", oldSysKey, orderCardSecret)
	}

	// 6. Query order status with the new system key should return 0 records
	reqQueryNew := httptest.NewRequest(http.MethodGet, "/api/query?card_secret="+newSysKey, nil)
	rrQueryNew := httptest.NewRecorder()
	handleQuery(rrQueryNew, reqQueryNew)

	if rrQueryNew.Code != http.StatusOK {
		t.Errorf("expected query status 200 for new key, got %d", rrQueryNew.Code)
	}

	var queryRespNew map[string]interface{}
	json.Unmarshal(rrQueryNew.Body.Bytes(), &queryRespNew)
	recordsNew, _ := queryRespNew["records"].([]interface{})
	if len(recordsNew) != 0 {
		t.Errorf("expected 0 records returned for new key, got %d", len(recordsNew))
	}

	// 7. Query order status with the old system key should return 1 record (since old key can still query its own records)
	reqQueryOld := httptest.NewRequest(http.MethodGet, "/api/query?card_secret="+oldSysKey, nil)
	rrQueryOld := httptest.NewRecorder()
	handleQuery(rrQueryOld, reqQueryOld)

	if rrQueryOld.Code != http.StatusOK {
		t.Errorf("expected query status 200 for old key, got %d", rrQueryOld.Code)
	}

	var queryRespOld map[string]interface{}
	json.Unmarshal(rrQueryOld.Body.Bytes(), &queryRespOld)
	recordsOld, _ := queryRespOld["records"].([]interface{})
	if len(recordsOld) != 1 {
		t.Errorf("expected 1 record returned for old key, got %d", len(recordsOld))
	}
}

func TestResetKeysPreventWhenRunning(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Setup mock vendor API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"success": true,
			"task_id": "TK-MOCK-99999",
			"message": "提交成功",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 2. Convert keys first to get a valid system key
	convReq := ConvertKeysRequest{
		Vendor:     "pass.aisale.one",
		VendorKeys: []string{"running-vendor-key"},
		Multiplier: 1,
	}
	bodyBytes, _ := json.Marshal(convReq)
	reqConv := httptest.NewRequest(http.MethodPost, "/api/convert_keys", bytes.NewBuffer(bodyBytes))
	rrConv := httptest.NewRecorder()
	handleConvertKeys(rrConv, reqConv)

	var convResp ConvertKeysResponse
	json.Unmarshal(rrConv.Body.Bytes(), &convResp)
	runningSysKey := convResp.SystemKeys[0]

	// 3. Submit order using this system key (so it has a task with 'pending' status)
	subReq := SubmitRequest{
		CardSecret: runningSysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "running_test_user@gmail.com",
		Password:  "pass123",
		TwoFactor: "12345678901234567890123456789012",
	})
	bodyBytesSub, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytesSub))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Fatalf("failed to submit order: %s", rrSubmit.Body.String())
	}

	// 4. Attempt to reset the system key. It should fail because the task is still "pending".
	resetReq := ResetKeysRequest{
		OldKeys: []string{runningSysKey},
	}
	bodyBytesReset, _ := json.Marshal(resetReq)
	reqReset := httptest.NewRequest(http.MethodPost, "/api/reset_keys", bytes.NewBuffer(bodyBytesReset))
	rrReset := httptest.NewRecorder()
	handleResetKeys(rrReset, reqReset)

	if rrReset.Code != http.StatusBadRequest {
		t.Errorf("expected reset status 400 Bad Request, got %d. Body: %s", rrReset.Code, rrReset.Body.String())
	}

	var resetResp ResetKeysResponse
	json.Unmarshal(rrReset.Body.Bytes(), &resetResp)

	if resetResp.Success {
		t.Errorf("expected reset success to be false, got true")
	}

	if !strings.Contains(resetResp.Message, "还有正在执行的任务") {
		t.Errorf("expected error message to contain '还有正在执行的任务', got: %s", resetResp.Message)
	}

	// 5. Verify DB: Key must still be active
	var keyStatus string
	err := db.QueryRow("SELECT status FROM system_keys WHERE system_key = ?", runningSysKey).Scan(&keyStatus)
	if err != nil {
		t.Fatalf("failed to query key status: %v", err)
	}
	if keyStatus != "active" {
		t.Errorf("expected system key to remain active, but got status: %s", keyStatus)
	}
}

func TestQuerySyncInvalidatesKey(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Setup mock vendor API server returning Success
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"task_id":       "TK-SYNC-SUCCESS-1",
				"status":        "Success",
				"message":       "订阅成功",
				"has_offer_url": true,
				"offer_url":     "https://pass.aisale.one/offer/xyz",
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 2. Insert active system key
	cardSecret := "CK-SYNC-TEST-KEY"
	_, err := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		cardSecret, "pass.aisale.one", "V-KEY-XYZ", "active", cardSecret, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert system key: %v", err)
	}

	// 3. Insert order
	res, err := db.Exec("INSERT INTO orders (card_secret, mode, vendor, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		cardSecret, "single", "pass.aisale.one", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	orderID, _ := res.LastInsertId()

	// 4. Insert pending account record
	_, err = db.Exec("INSERT INTO account_records (order_id, username, password, two_factor, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "sync_user@example.com", "pwd", "2fa", "pending", "处理中", "TK-SYNC-SUCCESS-1", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert account record: %v", err)
	}

	// 5. Trigger handleQuery to sync and check invalidation
	req := httptest.NewRequest(http.MethodGet, "/api/query?card_secret="+cardSecret, nil)
	rr := httptest.NewRecorder()
	handleQuery(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	// 6. Verify database: system key must be inactive
	var keyStatus string
	err = db.QueryRow("SELECT status FROM system_keys WHERE system_key = ?", cardSecret).Scan(&keyStatus)
	if err != nil {
		t.Fatalf("failed to query system key status: %v", err)
	}
	if keyStatus != "inactive" {
		t.Errorf("expected system key status to be 'inactive', got '%s'", keyStatus)
	}
}

func TestBackgroundSyncInvalidatesKeys(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Setup mock vendor API server returning Success
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"success": true,
			"data": map[string]interface{}{
				"task_id":       "TK-BG-SUCCESS-2",
				"status":        "Success",
				"message":       "订阅成功",
				"has_offer_url": true,
				"offer_url":     "https://pass.aisale.one/offer/bg-success",
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 2. Insert active system key
	cardSecret := "CK-BG-TEST-KEY"
	_, err := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		cardSecret, "pass.aisale.one", "V-KEY-BG", "active", cardSecret, time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert system key: %v", err)
	}

	// 3. Insert order
	res, err := db.Exec("INSERT INTO orders (card_secret, mode, vendor, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		cardSecret, "single", "pass.aisale.one", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	orderID, _ := res.LastInsertId()

	// 4. Insert pending account record
	_, err = db.Exec("INSERT INTO account_records (order_id, username, password, two_factor, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "bg_user@example.com", "pwd", "2fa", "pending", "处理中", "TK-BG-SUCCESS-2", time.Now(), time.Now())
	if err != nil {
		t.Fatalf("failed to insert account record: %v", err)
	}

	// 5. Call syncPendingAndInvalidate directly
	syncPendingAndInvalidate()

	// 6. Verify database: account record status must be 'success'
	var recordStatus string
	err = db.QueryRow("SELECT status FROM account_records WHERE order_id = ?", orderID).Scan(&recordStatus)
	if err != nil {
		t.Fatalf("failed to query account record status: %v", err)
	}
	if recordStatus != "success" {
		t.Errorf("expected account record status to be 'success', got '%s'", recordStatus)
	}

	// 7. Verify database: system key must be inactive
	var keyStatus string
	err = db.QueryRow("SELECT status FROM system_keys WHERE system_key = ?", cardSecret).Scan(&keyStatus)
	if err != nil {
		t.Fatalf("failed to query system key status: %v", err)
	}
	if keyStatus != "inactive" {
		t.Errorf("expected system key status to be 'inactive', got '%s'", keyStatus)
	}
}

func TestOriginalKeyFlow(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Setup mock vendor API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]interface{}{
			"success": true,
			"task_id": "TK-ORIG-11111",
			"message": "提交成功",
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 1. Convert keys to generate first system key
	convReq := ConvertKeysRequest{
		Vendor:     "pass.aisale.one",
		VendorKeys: []string{"orig-flow-vendor-key"},
		Multiplier: 1,
	}
	bodyBytes, _ := json.Marshal(convReq)
	reqConv := httptest.NewRequest(http.MethodPost, "/api/convert_keys", bytes.NewBuffer(bodyBytes))
	rrConv := httptest.NewRecorder()
	handleConvertKeys(rrConv, reqConv)

	var convResp ConvertKeysResponse
	json.Unmarshal(rrConv.Body.Bytes(), &convResp)
	if len(convResp.SystemKeys) != 1 || len(convResp.OriginalKeys) != 1 {
		t.Fatalf("expected 1 system key and 1 original key, got %v and %v", convResp.SystemKeys, convResp.OriginalKeys)
	}
	sysKey1 := convResp.SystemKeys[0]
	orgKey := convResp.OriginalKeys[0]

	// Verify original_key is generated and stored in DB
	var dbOrgKey string
	err := db.QueryRow("SELECT original_key FROM system_keys WHERE system_key = ?", sysKey1).Scan(&dbOrgKey)
	if err != nil {
		t.Fatalf("failed to query original key from DB: %v", err)
	}
	if dbOrgKey != orgKey {
		t.Errorf("expected DB original_key '%s', got '%s'", orgKey, dbOrgKey)
	}

	// 2. Submit first order using sysKey1
	subReq := SubmitRequest{
		CardSecret: sysKey1,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "orig_user1@gmail.com",
		Password:  "pass123",
		TwoFactor: "12345678901234567890123456789012",
	})
	bodyBytesSub, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytesSub))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		t.Fatalf("failed to submit first order: %s", rrSubmit.Body.String())
	}

	// 3. Update task status to failed (so we can reset)
	_, err = db.Exec("UPDATE account_records r JOIN orders o ON r.order_id = o.id SET r.status = 'failed' WHERE o.card_secret = ?", sysKey1)
	if err != nil {
		t.Fatalf("failed to set task status to failed: %v", err)
	}

	// 4. Reset the key (sysKey1 -> sysKey2)
	resetReq := ResetKeysRequest{
		OldKeys: []string{sysKey1},
	}
	bodyBytesReset, _ := json.Marshal(resetReq)
	reqReset := httptest.NewRequest(http.MethodPost, "/api/reset_keys", bytes.NewBuffer(bodyBytesReset))
	rrReset := httptest.NewRecorder()
	handleResetKeys(rrReset, reqReset)

	if rrReset.Code != http.StatusOK {
		t.Fatalf("expected reset status 200, got %d. Body: %s", rrReset.Code, rrReset.Body.String())
	}

	var resetResp ResetKeysResponse
	json.Unmarshal(rrReset.Body.Bytes(), &resetResp)
	if len(resetResp.NewKeys) != 1 || len(resetResp.OriginalKeys) != 1 {
		t.Fatalf("expected 1 new key and 1 original key from reset, got %v and %v", resetResp.NewKeys, resetResp.OriginalKeys)
	}
	sysKey2 := resetResp.NewKeys[0]
	if resetResp.OriginalKeys[0] != orgKey {
		t.Errorf("expected reset original key to be '%s', got '%s'", orgKey, resetResp.OriginalKeys[0])
	}

	// Verify original_key is copied to sysKey2 in DB
	err = db.QueryRow("SELECT original_key FROM system_keys WHERE system_key = ?", sysKey2).Scan(&dbOrgKey)
	if err != nil {
		t.Fatalf("failed to query original key for new key: %v", err)
	}
	if dbOrgKey != orgKey {
		t.Errorf("expected new key original_key to be '%s', got '%s'", orgKey, dbOrgKey)
	}

	// 5. Verify database groupings of sysKey1 and sysKey2
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM system_keys WHERE original_key = ?", orgKey).Scan(&count)
	if err != nil {
		t.Fatalf("failed to query keys by original key: %v", err)
	}
	if count != 2 {
		t.Errorf("expected 2 keys grouped by original_key, got %d", count)
	}

	// 6. Querying using sysKey1 should return 1 record
	reqQuery1 := httptest.NewRequest(http.MethodGet, "/api/query?card_secret="+sysKey1, nil)
	rrQuery1 := httptest.NewRecorder()
	handleQuery(rrQuery1, reqQuery1)

	if rrQuery1.Code != http.StatusOK {
		t.Fatalf("expected query 1 to be 200, got %d", rrQuery1.Code)
	}

	var queryResp1 map[string]interface{}
	json.Unmarshal(rrQuery1.Body.Bytes(), &queryResp1)
	records1, _ := queryResp1["records"].([]interface{})
	if len(records1) != 1 {
		t.Errorf("expected 1 record when querying with sysKey1, got %d", len(records1))
	}

	// 7. Querying using sysKey2 (which has no orders submitted) should return 0 records due to query isolation
	reqQuery2 := httptest.NewRequest(http.MethodGet, "/api/query?card_secret="+sysKey2, nil)
	rrQuery2 := httptest.NewRecorder()
	handleQuery(rrQuery2, reqQuery2)

	if rrQuery2.Code != http.StatusOK {
		t.Fatalf("expected query 2 to be 200, got %d", rrQuery2.Code)
	}

	var queryResp2 map[string]interface{}
	json.Unmarshal(rrQuery2.Body.Bytes(), &queryResp2)
	records2, _ := queryResp2["records"].([]interface{})
	if len(records2) != 0 {
		t.Errorf("expected 0 records when querying with sysKey2, got %d", len(records2))
	}
}

func TestAdminBackendFlow(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Clear admin tables
	_, _ = db.Exec("DELETE FROM admin_sessions")
	_, _ = db.Exec("DELETE FROM admins")

	// Insert test admin
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("testpwd123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	now := time.Now()
	_, err = db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES (?, ?, 'admin', ?, ?)",
		"testadmin", string(hashedPassword), now, now)
	if err != nil {
		t.Fatalf("failed to insert admin: %v", err)
	}

	// 1. Test Login - Failed attempt
	loginReqPayload := `{"username": "testadmin", "password": "wrongpassword"}`
	reqLoginFail := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginReqPayload))
	rrLoginFail := httptest.NewRecorder()
	handleAdminLogin(rrLoginFail, reqLoginFail)

	if rrLoginFail.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for failed login, got %d", rrLoginFail.Code)
	}

	// 2. Test Login - Successful attempt
	loginReqPayload = `{"username": "testadmin", "password": "testpwd123"}`
	reqLoginOK := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginReqPayload))
	rrLoginOK := httptest.NewRecorder()
	handleAdminLogin(rrLoginOK, reqLoginOK)

	if rrLoginOK.Code != http.StatusOK {
		t.Fatalf("expected 200 for successful login, got %d. Body: %s", rrLoginOK.Code, rrLoginOK.Body.String())
	}

	var loginResp map[string]interface{}
	json.Unmarshal(rrLoginOK.Body.Bytes(), &loginResp)
	if loginResp["success"] != true {
		t.Errorf("expected success: true, got %v", loginResp["success"])
	}

	// Get cookie
	cookies := rrLoginOK.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "admin_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("session cookie not found in response")
	}

	// 3. Test Check Session - Authenticated
	reqCheckOK := httptest.NewRequest(http.MethodGet, "/api/admin/check", nil)
	reqCheckOK.AddCookie(sessionCookie)
	rrCheckOK := httptest.NewRecorder()
	handleAdminCheck(rrCheckOK, reqCheckOK)

	if rrCheckOK.Code != http.StatusOK {
		t.Errorf("expected 200 for check session, got %d", rrCheckOK.Code)
	}

	var checkResp map[string]interface{}
	json.Unmarshal(rrCheckOK.Body.Bytes(), &checkResp)
	if checkResp["success"] != true || checkResp["username"] != "testadmin" {
		t.Errorf("expected success: true, username: testadmin, got %v", checkResp)
	}

	// 4. Test Check Session - Unauthenticated (no cookie)
	reqCheckFail := httptest.NewRequest(http.MethodGet, "/api/admin/check", nil)
	rrCheckFail := httptest.NewRecorder()
	handleAdminCheck(rrCheckFail, reqCheckFail)

	var checkFailResp map[string]interface{}
	json.Unmarshal(rrCheckFail.Body.Bytes(), &checkFailResp)
	if checkFailResp["success"] == true {
		t.Errorf("expected success: false for unauthenticated check session")
	}

	// 5. Test Orders Listing API
	// First let's insert a mock order and record
	res, err := db.Exec("INSERT INTO orders (card_secret, mode, created_at, updated_at) VALUES (?, ?, ?, ?)",
		"SYS-CARD-777", "single", now, now)
	if err != nil {
		t.Fatalf("failed to insert mock order: %v", err)
	}
	orderID, _ := res.LastInsertId()

	_, err = db.Exec("INSERT INTO account_records (order_id, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "orderuser@gmail.com", "pass123", "2FA", "pending", "排队中", now, now)
	if err != nil {
		t.Fatalf("failed to insert mock record: %v", err)
	}
	var recordID int64
	err = db.QueryRow("SELECT id FROM account_records WHERE order_id = ?", orderID).Scan(&recordID)
	if err != nil {
		t.Fatalf("failed to query mock record id: %v", err)
	}

	// Query orders - unauthorized (no cookie)
	reqOrdersUnauth := httptest.NewRequest(http.MethodGet, "/api/admin/orders", nil)
	rrOrdersUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminOrders)(rrOrdersUnauth, reqOrdersUnauth)
	if rrOrdersUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized orders query, got %d", rrOrdersUnauth.Code)
	}

	// Query orders - authorized
	reqOrdersAuth := httptest.NewRequest(http.MethodGet, "/api/admin/orders?query=orderuser", nil)
	reqOrdersAuth.AddCookie(sessionCookie)
	rrOrdersAuth := httptest.NewRecorder()
	requireAdmin(handleAdminOrders)(rrOrdersAuth, reqOrdersAuth)

	if rrOrdersAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for orders query, got %d. Body: %s", rrOrdersAuth.Code, rrOrdersAuth.Body.String())
	}

	var ordersResp map[string]interface{}
	json.Unmarshal(rrOrdersAuth.Body.Bytes(), &ordersResp)
	if ordersResp["success"] != true || int(ordersResp["total"].(float64)) != 1 {
		t.Errorf("expected 1 order record in result, got %v", ordersResp)
	}

	// Test start_time / end_time filter
	nowSec := time.Now().Unix()
	urlWithTimeMatch := fmt.Sprintf("/api/admin/orders?query=orderuser&start_time=%d&end_time=%d", nowSec-30, nowSec+30)
	reqOrdersTimeMatch := httptest.NewRequest(http.MethodGet, urlWithTimeMatch, nil)
	reqOrdersTimeMatch.AddCookie(sessionCookie)
	rrOrdersTimeMatch := httptest.NewRecorder()
	requireAdmin(handleAdminOrders)(rrOrdersTimeMatch, reqOrdersTimeMatch)

	var ordersRespTimeMatch map[string]interface{}
	json.Unmarshal(rrOrdersTimeMatch.Body.Bytes(), &ordersRespTimeMatch)
	if ordersRespTimeMatch["success"] != true || int(ordersRespTimeMatch["total"].(float64)) != 1 {
		t.Errorf("expected 1 order record in time matching query, got %v", ordersRespTimeMatch)
	}

	urlWithTimeBefore := fmt.Sprintf("/api/admin/orders?query=orderuser&start_time=%d&end_time=%d", nowSec-60, nowSec-10)
	reqOrdersTimeBefore := httptest.NewRequest(http.MethodGet, urlWithTimeBefore, nil)
	reqOrdersTimeBefore.AddCookie(sessionCookie)
	rrOrdersTimeBefore := httptest.NewRecorder()
	requireAdmin(handleAdminOrders)(rrOrdersTimeBefore, reqOrdersTimeBefore)

	var ordersRespTimeBefore map[string]interface{}
	json.Unmarshal(rrOrdersTimeBefore.Body.Bytes(), &ordersRespTimeBefore)
	if ordersRespTimeBefore["success"] != true || int(ordersRespTimeBefore["total"].(float64)) != 0 {
		t.Errorf("expected 0 order records in time before query, got %v", ordersRespTimeBefore)
	}

	// 5.b Test Order History API
	// Query order history - unauthorized (no cookie)
	reqHistoryUnauth := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/orders/history?order_id=%d", orderID), nil)
	rrHistoryUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminOrderHistory)(rrHistoryUnauth, reqHistoryUnauth)
	if rrHistoryUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized order history query, got %d", rrHistoryUnauth.Code)
	}

	// Query order history - authorized
	reqHistoryAuth := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/orders/history?order_id=%d", orderID), nil)
	reqHistoryAuth.AddCookie(sessionCookie)
	rrHistoryAuth := httptest.NewRecorder()
	requireAdmin(handleAdminOrderHistory)(rrHistoryAuth, reqHistoryAuth)

	if rrHistoryAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for order history query, got %d. Body: %s", rrHistoryAuth.Code, rrHistoryAuth.Body.String())
	}

	var historyResp map[string]interface{}
	json.Unmarshal(rrHistoryAuth.Body.Bytes(), &historyResp)
	if historyResp["success"] != true {
		t.Errorf("expected success: true, got %v", historyResp)
	}
	historyRecords, ok := historyResp["records"].([]interface{})
	if !ok || len(historyRecords) != 1 {
		t.Errorf("expected 1 history record, got %d", len(historyRecords))
	}

	// 6. Test Orders Update API
	updatePayload := fmt.Sprintf(`{"record_id": %d, "status": "success", "message": "已绑定优惠", "discount_url": "https://pixel.sub/offer"}`, recordID)
	reqUpdate := httptest.NewRequest(http.MethodPost, "/api/admin/orders/update", strings.NewReader(updatePayload))
	reqUpdate.AddCookie(sessionCookie)
	rrUpdate := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrdersUpdate)(rrUpdate, reqUpdate)

	if rrUpdate.Code != http.StatusOK {
		t.Fatalf("expected 200 for orders update, got %d. Body: %s", rrUpdate.Code, rrUpdate.Body.String())
	}

	// Verify updated fields in DB
	var dbStatus, dbMessage, dbDiscount string
	err = db.QueryRow("SELECT status, message, discount_url FROM account_records WHERE id = ?", recordID).
		Scan(&dbStatus, &dbMessage, &dbDiscount)
	if err != nil {
		t.Fatalf("failed to query updated record: %v", err)
	}
	if dbStatus != "success" || dbMessage != "已绑定优惠" || dbDiscount != "https://pixel.sub/offer" {
		t.Errorf("record fields not updated correctly in database: status=%s, message=%s, discount=%s", dbStatus, dbMessage, dbDiscount)
	}

	// Insert a test key to ensure keys query and export returns data
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"test-sys-key-export", "ai.deard.fun", "test-vendor-key", "active", 1, "test-sys-key-export", now, now)
	if err != nil {
		t.Fatalf("failed to insert test system key for keys listing: %v", err)
	}

	// 6.b Test Admin Keys Listing API
	// Query keys - unauthorized (no cookie)
	reqKeysUnauth := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	rrKeysUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminKeys)(rrKeysUnauth, reqKeysUnauth)
	if rrKeysUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized keys query, got %d", rrKeysUnauth.Code)
	}

	// Query keys - authorized
	reqKeysAuth := httptest.NewRequest(http.MethodGet, "/api/admin/keys", nil)
	reqKeysAuth.AddCookie(sessionCookie)
	rrKeysAuth := httptest.NewRecorder()
	requireAdmin(handleAdminKeys)(rrKeysAuth, reqKeysAuth)

	if rrKeysAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for keys query, got %d. Body: %s", rrKeysAuth.Code, rrKeysAuth.Body.String())
	}

	var keysResp map[string]interface{}
	json.Unmarshal(rrKeysAuth.Body.Bytes(), &keysResp)
	if keysResp["success"] != true {
		t.Errorf("expected success: true, got %v", keysResp)
	}

	// Test Keys Export API
	reqKeysExport := httptest.NewRequest(http.MethodGet, "/api/admin/keys?export=true", nil)
	reqKeysExport.AddCookie(sessionCookie)
	rrKeysExport := httptest.NewRecorder()
	requireAdmin(handleAdminKeys)(rrKeysExport, reqKeysExport)

	if rrKeysExport.Code != http.StatusOK {
		t.Fatalf("expected 200 for keys export, got %d. Body: %s", rrKeysExport.Code, rrKeysExport.Body.String())
	}

	var exportResp map[string]interface{}
	json.Unmarshal(rrKeysExport.Body.Bytes(), &exportResp)
	if exportResp["success"] != true {
		t.Errorf("expected success: true for keys export, got %v", exportResp)
	}
	exportedKeys, ok := exportResp["keys"].([]interface{})
	if !ok || len(exportedKeys) == 0 {
		t.Errorf("expected exported keys list, got %v", exportResp)
	}

	// Test Admin Keys Creation Time Filter API
	nowFilter := time.Now()
	pastTime := nowFilter.AddDate(0, 0, -10)
	sysKeyOld := "OLDTIME_KEY_TEST_99"
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, created_at, updated_at) VALUES (?, 'ai.deard.fun', 'vk_old', 'active', ?, ?)", sysKeyOld, pastTime, pastTime)
	if err != nil {
		t.Fatalf("failed to insert past system key: %v", err)
	}

	// Filter by start_date = today (should exclude old key)
	todayStr := nowFilter.Format("2006-01-02")
	reqKeysToday := httptest.NewRequest(http.MethodGet, "/api/admin/keys?start_date="+todayStr, nil)
	reqKeysToday.AddCookie(sessionCookie)
	rrKeysToday := httptest.NewRecorder()
	requireAdmin(handleAdminKeys)(rrKeysToday, reqKeysToday)
	if rrKeysToday.Code != http.StatusOK {
		t.Fatalf("expected 200 for start_date filter, got %d", rrKeysToday.Code)
	}
	var respToday map[string]interface{}
	json.Unmarshal(rrKeysToday.Body.Bytes(), &respToday)
	recordsToday, ok := respToday["records"].([]interface{})
	if !ok {
		t.Fatalf("expected records array, got %v", respToday)
	}
	for _, item := range recordsToday {
		m := item.(map[string]interface{})
		if m["system_key"] == sysKeyOld {
			t.Errorf("old key %s should not appear when filtering from %s", sysKeyOld, todayStr)
		}
	}

	// Filter by end_date = 5 days ago (should include old key, exclude today's key)
	fiveDaysAgoStr := nowFilter.AddDate(0, 0, -5).Format("2006-01-02")
	reqKeysPast := httptest.NewRequest(http.MethodGet, "/api/admin/keys?end_date="+fiveDaysAgoStr, nil)
	reqKeysPast.AddCookie(sessionCookie)
	rrKeysPast := httptest.NewRecorder()
	requireAdmin(handleAdminKeys)(rrKeysPast, reqKeysPast)
	if rrKeysPast.Code != http.StatusOK {
		t.Fatalf("expected 200 for end_date filter, got %d", rrKeysPast.Code)
	}
	var respPast map[string]interface{}
	json.Unmarshal(rrKeysPast.Body.Bytes(), &respPast)
	recordsPast, ok := respPast["records"].([]interface{})
	if !ok {
		t.Fatalf("expected records array, got %v", respPast)
	}
	foundOld := false
	for _, item := range recordsPast {
		m := item.(map[string]interface{})
		if m["system_key"] == sysKeyOld {
			foundOld = true
		}
		if m["system_key"] == "test-sys-key-export" {
			t.Errorf("today's key test-sys-key-export should not appear when filtering until %s", fiveDaysAgoStr)
		}
	}
	if !foundOld {
		t.Errorf("expected past key %s to be found in end_date filter result", sysKeyOld)
	}

	// 6.c Test Admin Dashboard Stats API
	// Query stats - unauthorized (no cookie)
	reqStatsUnauth := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/stats", nil)
	rrStatsUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminDashboardStats)(rrStatsUnauth, reqStatsUnauth)
	if rrStatsUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthorized dashboard stats query, got %d", rrStatsUnauth.Code)
	}

	// Query stats - authorized
	reqStatsAuth := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/stats", nil)
	reqStatsAuth.AddCookie(sessionCookie)
	rrStatsAuth := httptest.NewRecorder()
	requireAdmin(handleAdminDashboardStats)(rrStatsAuth, reqStatsAuth)

	if rrStatsAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 for dashboard stats query, got %d. Body: %s", rrStatsAuth.Code, rrStatsAuth.Body.String())
	}

	var statsResp map[string]interface{}
	json.Unmarshal(rrStatsAuth.Body.Bytes(), &statsResp)
	if statsResp["success"] != true {
		t.Errorf("expected success: true for dashboard stats, got %v", statsResp)
	}

	todayData, ok := statsResp["today"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected today statistics data in response, got %v", statsResp)
	}
	if int(todayData["total"].(float64)) != 1 {
		t.Errorf("expected today total orders to be 1, got %v", todayData["total"])
	}
	if int(todayData["success"].(float64)) != 1 {
		t.Errorf("expected today success orders to be 1, got %v", todayData["success"])
	}
	if int(todayData["failed"].(float64)) != 0 {
		t.Errorf("expected today failed orders to be 0, got %v", todayData["failed"])
	}
	if int(todayData["other"].(float64)) != 0 {
		t.Errorf("expected today other orders to be 0, got %v", todayData["other"])
	}
	if todayData["success_rate"].(float64) != 100.0 {
		t.Errorf("expected today success rate to be 100.0, got %v", todayData["success_rate"])
	}

	summary30d, ok := statsResp["summary_30d"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected summary_30d statistics data in response, got %v", statsResp)
	}
	if int(summary30d["total"].(float64)) != 1 {
		t.Errorf("expected 30d total orders to be 1, got %v", summary30d["total"])
	}
	if int(summary30d["success"].(float64)) != 1 {
		t.Errorf("expected 30d success orders to be 1, got %v", summary30d["success"])
	}
	if int(summary30d["failed"].(float64)) != 0 {
		t.Errorf("expected 30d failed orders to be 0, got %v", summary30d["failed"])
	}

	trendData, ok := statsResp["trend"].([]interface{})
	if !ok || len(trendData) != 30 {
		t.Errorf("expected trend to be a slice of 30 days, got %v", statsResp["trend"])
	} else {
		// The last day in the trend should be today, and it should have count: 1
		lastDay, ok := trendData[29].(map[string]interface{})
		if !ok {
			t.Fatalf("expected last trend item to be a map, got %v", trendData[29])
		}
		if int(lastDay["count"].(float64)) != 1 {
			t.Errorf("expected count for today in trend to be 1, got %v", lastDay["count"])
		}
	}

	// Test Order Retry API - Run after stats checks because retry resets status to pending
	retryPayload := fmt.Sprintf(`{"record_id": %d}`, recordID)
	reqRetry := httptest.NewRequest(http.MethodPost, "/api/admin/orders/retry", strings.NewReader(retryPayload))
	reqRetry.AddCookie(sessionCookie)
	rrRetry := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrderRetry)(rrRetry, reqRetry)

	if rrRetry.Code != http.StatusOK {
		t.Fatalf("expected 200 for orders retry, got %d. Body: %s", rrRetry.Code, rrRetry.Body.String())
	}

	// Verify retried fields in DB
	var dbStatusRetry, dbMessageRetry string
	var dbExecCount int
	err = db.QueryRow("SELECT status, message, execution_count FROM account_records WHERE id = ?", recordID).
		Scan(&dbStatusRetry, &dbMessageRetry, &dbExecCount)
	if err != nil {
		t.Fatalf("failed to query retried record: %v", err)
	}
	if dbStatusRetry != "pending" || dbExecCount != 2 {
		t.Errorf("record fields not retried correctly in database: status=%s, execution_count=%d", dbStatusRetry, dbExecCount)
	}

	// 7. Test Logout
	reqLogout := httptest.NewRequest(http.MethodPost, "/api/admin/logout", nil)
	reqLogout.AddCookie(sessionCookie)
	rrLogout := httptest.NewRecorder()
	handleAdminLogout(rrLogout, reqLogout)

	if rrLogout.Code != http.StatusOK {
		t.Errorf("expected 200 for logout, got %d", rrLogout.Code)
	}

	// Check session is deleted from DB
	var sessionCount int
	err = db.QueryRow("SELECT COUNT(*) FROM admin_sessions WHERE token = ?", sessionCookie.Value).Scan(&sessionCount)
	if err != nil {
		t.Fatalf("failed to query sessions table: %v", err)
	}
	if sessionCount != 0 {
		t.Errorf("expected session token to be deleted from database, but it still exists")
	}

	// 8. Test static server blocking /convert.html
	staticFS, err := fs.Sub(embedFS, "frontend")
	if err != nil {
		t.Fatalf("failed to get embed fs: %v", err)
	}
	srv := &adminStaticServer{fileServer: http.FileServer(http.FS(staticFS))}

	reqStaticConvert := httptest.NewRequest(http.MethodGet, "/convert.html", nil)
	rrStaticConvert := httptest.NewRecorder()
	srv.ServeHTTP(rrStaticConvert, reqStaticConvert)
	if rrStaticConvert.Code != http.StatusForbidden {
		t.Errorf("expected 403 Forbidden for /convert.html, got %d", rrStaticConvert.Code)
	}

	// Test static server redirecting unauthenticated /admin/ index
	reqStaticAdmin := httptest.NewRequest(http.MethodGet, "/admin/index.html", nil)
	rrStaticAdmin := httptest.NewRecorder()
	srv.ServeHTTP(rrStaticAdmin, reqStaticAdmin)
	if rrStaticAdmin.Code != http.StatusFound {
		t.Errorf("expected 302 Found redirect for unauthenticated /admin/, got %d", rrStaticAdmin.Code)
	}
}

func TestSettingsAndConfigFlow(t *testing.T) {
	// Re-initialize DB tables for isolation
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Check default tutorial url exists in DB and can be retrieved via public API /api/config
	reqConfig := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rrConfig := httptest.NewRecorder()
	handleGetConfig(rrConfig, reqConfig)

	if rrConfig.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rrConfig.Code)
	}

	var configResp map[string]interface{}
	if err := json.Unmarshal(rrConfig.Body.Bytes(), &configResp); err != nil {
		t.Fatalf("failed to unmarshal config response: %v", err)
	}

	if configResp["success"] != true {
		t.Errorf("expected success: true, got %v", configResp["success"])
	}

	defaultURL := "https://www.yuque.com/taozi-khqsp/rrub4i/fxm5dgln1rh5iwd1"
	if configResp["two_factor_tutorial_url"] != defaultURL {
		t.Errorf("expected default tutorial URL %q, got %q", defaultURL, configResp["two_factor_tutorial_url"])
	}

	// 2. GET /api/admin/settings without admin authentication should fail with 401
	reqAdminGetUnauth := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	rrAdminGetUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminSettings)(rrAdminGetUnauth, reqAdminGetUnauth)

	if rrAdminGetUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for unauthenticated GET /api/admin/settings, got %d", rrAdminGetUnauth.Code)
	}

	// 3. POST /api/admin/settings without admin authentication should fail with 401
	reqAdminPostUnauth := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(`{"two_factor_tutorial_url":"https://test-url.com"}`))
	rrAdminPostUnauth := httptest.NewRecorder()
	requireAdmin(handleAdminSettings)(rrAdminPostUnauth, reqAdminPostUnauth)

	if rrAdminPostUnauth.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized for unauthenticated POST /api/admin/settings, got %d", rrAdminPostUnauth.Code)
	}

	// 4. Authenticate admin and obtain token
	// Insert test admin if not exists (initDB ensures this but let's be safe)
	var adminCount int
	db.QueryRow("SELECT COUNT(*) FROM admins WHERE username = 'admin'").Scan(&adminCount)
	if adminCount == 0 {
		hashed, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		db.Exec("INSERT INTO admins (username, password_hash, created_at, updated_at) VALUES ('admin', ?, ?, ?)", string(hashed), time.Now(), time.Now())
	} else {
		// Reset password for admin to 'admin123' to make sure
		hashed, _ := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		db.Exec("UPDATE admins SET password_hash = ? WHERE username = 'admin'", string(hashed))
	}

	// Login
	loginBody := `{"username":"admin","password":"admin123"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginBody))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)

	if rrLogin.Code != http.StatusOK {
		t.Fatalf("failed to login: %d", rrLogin.Code)
	}

	// Extract cookie
	cookies := rrLogin.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "admin_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("session cookie not found in login response")
	}

	// 5. GET /api/admin/settings WITH admin authentication
	reqAdminGetAuth := httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	reqAdminGetAuth.AddCookie(sessionCookie)
	rrAdminGetAuth := httptest.NewRecorder()
	requireAdmin(handleAdminSettings)(rrAdminGetAuth, reqAdminGetAuth)

	if rrAdminGetAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for authenticated GET, got %d", rrAdminGetAuth.Code)
	}

	var adminGetResp map[string]interface{}
	if err := json.Unmarshal(rrAdminGetAuth.Body.Bytes(), &adminGetResp); err != nil {
		t.Fatalf("failed to unmarshal admin GET response: %v", err)
	}

	settings, ok := adminGetResp["settings"].(map[string]interface{})
	if !ok {
		t.Fatalf("settings map not found in response: %v", adminGetResp)
	}

	if settings["two_factor_tutorial_url"] != defaultURL {
		t.Errorf("expected default URL %q, got %q", defaultURL, settings["two_factor_tutorial_url"])
	}

	// 6. POST /api/admin/settings WITH admin authentication (Update)
	newTestURL := "https://www.yuque.com/test-new-link"
	updateBody := fmt.Sprintf(`{"two_factor_tutorial_url": %q}`, newTestURL)
	reqAdminPostAuth := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(updateBody))
	reqAdminPostAuth.AddCookie(sessionCookie)
	rrAdminPostAuth := httptest.NewRecorder()
	requireAdmin(handleAdminSettings)(rrAdminPostAuth, reqAdminPostAuth)

	if rrAdminPostAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for authenticated POST, got %d", rrAdminPostAuth.Code)
	}

	var adminPostResp map[string]interface{}
	if err := json.Unmarshal(rrAdminPostAuth.Body.Bytes(), &adminPostResp); err != nil {
		t.Fatalf("failed to unmarshal admin POST response: %v", err)
	}

	if adminPostResp["success"] != true {
		t.Errorf("expected success: true, got %v", adminPostResp["success"])
	}

	// 7. Verify GET /api/config and GET /api/admin/settings return the updated URL
	// Public endpoint check
	rrConfigUpdated := httptest.NewRecorder()
	handleGetConfig(rrConfigUpdated, reqConfig)

	var configRespUpdated map[string]interface{}
	json.Unmarshal(rrConfigUpdated.Body.Bytes(), &configRespUpdated)
	if configRespUpdated["two_factor_tutorial_url"] != newTestURL {
		t.Errorf("expected updated URL %q, got %q", newTestURL, configRespUpdated["two_factor_tutorial_url"])
	}

	// Admin endpoint check
	rrAdminGetAuthUpdated := httptest.NewRecorder()
	requireAdmin(handleAdminSettings)(rrAdminGetAuthUpdated, reqAdminGetAuth)

	var adminGetRespUpdated map[string]interface{}
	json.Unmarshal(rrAdminGetAuthUpdated.Body.Bytes(), &adminGetRespUpdated)
	settingsUpdated := adminGetRespUpdated["settings"].(map[string]interface{})
	if settingsUpdated["two_factor_tutorial_url"] != newTestURL {
		t.Errorf("expected updated URL in admin GET %q, got %q", newTestURL, settingsUpdated["two_factor_tutorial_url"])
	}
}

func TestEpayDirectGeneration(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}

	// 1. Log in as admin to get cookie
	hashedPwd, _ := bcrypt.GenerateFromPassword([]byte("adminpwd"), bcrypt.DefaultCost)
	now := time.Now()
	_, errDb := db.Exec("REPLACE INTO admins (username, password_hash, role, created_at, updated_at) VALUES ('admin', ?, 'admin', ?, ?)", string(hashedPwd), now, now)
	if errDb != nil {
		t.Fatalf("failed to insert test admin: %v", errDb)
	}

	loginBody := `{"username":"admin","password":"adminpwd"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginBody))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)
	cookie := rrLogin.Result().Cookies()[0]

	// 2. Save settings (Epay credentials and key price)
	settingsPayload := `{"epay_url":"https://pay.vansdesign.cn/","epay_pid":"1668","epay_key":"test_epay_key_secret","epay_wx_channel":"201906181353","epay_alipay_channel":"201906181354","key_price":"9.99","two_factor_tutorial_url":"https://test.tutorial/url"}`
	reqSave := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(settingsPayload))
	reqSave.AddCookie(cookie)
	rrSave := httptest.NewRecorder()
	requirePermission("settings", handleAdminSettings)(rrSave, reqSave)
	if rrSave.Code != http.StatusOK {
		t.Fatalf("failed to save settings: %d", rrSave.Code)
	}

	// Pre-insert 3 available keys in card_stock for checkout test
	_, errInsertStock := db.Exec("INSERT INTO card_stock (card_key, vendor, status, note, created_at, updated_at) VALUES ('STOCKKEY1111', 'ai.deard.fun', 'available', 'Epay Purchased', ?, ?), ('STOCKKEY2222', 'ai.deard.fun', 'available', 'Epay Purchased', ?, ?), ('STOCKKEY3333', 'ai.deard.fun', 'available', 'Epay Purchased', ?, ?)", now, now, now, now, now, now)
	if errInsertStock != nil {
		t.Fatalf("failed to insert mock card stock: %v", errInsertStock)
	}

	rowsDbg, _ := db.Query("SELECT id, card_key, status, vendor, note FROM card_stock")
	for rowsDbg.Next() {
		var id int
		var key, statusVal, vendorVal, noteVal string
		rowsDbg.Scan(&id, &key, &statusVal, &vendorVal, &noteVal)
		t.Logf("Dbg CardStock: id=%d, key=%s, status=%s, vendor=%s, note=%s", id, key, statusVal, vendorVal, noteVal)
	}
	rowsDbg.Close()

	// 3. GET /api/pay/config - verify stock is 3 (reflects the inserted keys)
	reqConfig := httptest.NewRequest(http.MethodGet, "/api/pay/config", nil)
	reqConfig.AddCookie(cookie)
	rrConfig := httptest.NewRecorder()
	requirePermission("buy", handleGetPayConfig)(rrConfig, reqConfig)
	if rrConfig.Code != http.StatusOK {
		t.Fatalf("expected 200 for pay config, got %d", rrConfig.Code)
	}
	var configResp map[string]interface{}
	json.Unmarshal(rrConfig.Body.Bytes(), &configResp)
	if configResp["stock"].(float64) != 3 {
		t.Errorf("expected stock to be 3 (matching pre-inserted count), got %v", configResp["stock"])
	}

	// 4. Place a card key order
	buyPayload := `{"quantity": 3, "type": "wxpay"}`
	reqBuy := httptest.NewRequest(http.MethodPost, "/api/pay/buy", strings.NewReader(buyPayload))
	reqBuy.AddCookie(cookie)
	rrBuy := httptest.NewRecorder()
	requirePermission("buy", handlePayBuy)(rrBuy, reqBuy)
	if rrBuy.Code != http.StatusOK {
		t.Fatalf("expected 200 for buy order creation, got %d. Body: %s", rrBuy.Code, rrBuy.Body.String())
	}
	var buyResp map[string]interface{}
	json.Unmarshal(rrBuy.Body.Bytes(), &buyResp)
	outTradeNo := buyResp["out_trade_no"].(string)

	// Verify order in database is pending
	var dbStatus string
	var dbQty int
	errQuery := db.QueryRow("SELECT status, quantity FROM key_orders WHERE out_trade_no = ?", outTradeNo).Scan(&dbStatus, &dbQty)
	if errQuery != nil {
		t.Fatalf("failed to query key_order in DB: %v", errQuery)
	}
	if dbStatus != "pending" || dbQty != 3 {
		t.Errorf("unexpected DB order status/qty: status=%s, qty=%d", dbStatus, dbQty)
	}

	// Verify no keys returned on query when pending (security validation)
	reqQuery := httptest.NewRequest(http.MethodGet, "/api/pay/query?out_trade_no="+outTradeNo, nil)
	reqQuery.AddCookie(cookie)
	rrQueryPending := httptest.NewRecorder()
	requirePermission("buy", handlePayQuery)(rrQueryPending, reqQuery)
	var queryPendingResp map[string]interface{}
	json.Unmarshal(rrQueryPending.Body.Bytes(), &queryPendingResp)
	if _, exists := queryPendingResp["card_keys"]; exists {
		t.Errorf("expected card_keys to be hidden when order is pending, but it was returned")
	}

	// 5. Simulate callback notify (Success payment)
	notifyParams := map[string]string{
		"pid":          "1668",
		"trade_no":     "202607049999",
		"out_trade_no": outTradeNo,
		"type":         "wxpay",
		"name":         "购买卡密 x3",
		"money":        "29.97", // 9.99 * 3
		"trade_status": "TRADE_SUCCESS",
	}
	notifySign := calculateEpaySign(notifyParams, "test_epay_key_secret")
	notifyQuery := fmt.Sprintf("pid=1668&trade_no=202607049999&out_trade_no=%s&type=wxpay&name=%s&money=29.97&trade_status=TRADE_SUCCESS&sign=%s",
		outTradeNo, url.QueryEscape("购买卡密 x3"), notifySign)
	reqNotify := httptest.NewRequest(http.MethodGet, "/api/pay/notify?"+notifyQuery, nil)
	rrNotify := httptest.NewRecorder()
	handlePayNotify(rrNotify, reqNotify)
	if rrNotify.Body.String() != "success" {
		t.Fatalf("expected notify response 'success', got: %s", rrNotify.Body.String())
	}

	// 6. Verify keys created in DB as 'active' and associated with vendor 'ai.deard.fun'
	var activeCount int
	errCount := db.QueryRow("SELECT COUNT(*) FROM system_keys WHERE status = 'active' AND vendor = 'ai.deard.fun' AND note = 'Epay Purchased'").Scan(&activeCount)
	if errCount != nil {
		t.Fatalf("failed to query system_keys count: %v", errCount)
	}
	if activeCount != 3 {
		t.Errorf("expected 3 active keys inserted in DB, got %d", activeCount)
	}

	// 7. Verify query endpoint returns the drawn keys from card_stock pool
	rrQueryPaid := httptest.NewRecorder()
	requirePermission("buy", handlePayQuery)(rrQueryPaid, reqQuery)
	var queryPaidResp map[string]interface{}
	json.Unmarshal(rrQueryPaid.Body.Bytes(), &queryPaidResp)
	if queryPaidResp["status"] != "paid" {
		t.Errorf("expected query status to be paid, got %v", queryPaidResp["status"])
	}
	cardKeys := queryPaidResp["card_keys"].(string)
	keysSplit := strings.Split(strings.TrimSpace(cardKeys), "\n")
	if len(keysSplit) != 3 {
		t.Errorf("expected 3 keys in result, got %d. Content: %s", len(keysSplit), cardKeys)
	}
	for _, key := range keysSplit {
		if !strings.HasPrefix(key, "STOCKKEY") {
			t.Errorf("expected key to be drawn from card_stock pool, got: %s", key)
		}
	}

	// 8. Verify keys marked as 'sold' in card_stock
	var soldCount int
	db.QueryRow("SELECT COUNT(*) FROM card_stock WHERE status = 'sold'").Scan(&soldCount)
	if soldCount != 3 {
		t.Errorf("expected 3 keys in card_stock to be marked as sold, got %d", soldCount)
	}
}

func TestEpayCancelAndRefund(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}

	hashedPwd, _ := bcrypt.GenerateFromPassword([]byte("adminpwd"), bcrypt.DefaultCost)
	now := time.Now()
	_, _ = db.Exec("REPLACE INTO admins (username, password_hash, role, created_at, updated_at) VALUES ('admin', ?, 'admin', ?, ?)", string(hashedPwd), now, now)

	loginBody := `{"username":"admin","password":"adminpwd"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginBody))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)
	cookie := rrLogin.Result().Cookies()[0]

	// Save settings
	settingsPayload := `{"epay_url":"https://pay.vansdesign.cn/","epay_pid":"1668","epay_key":"test_epay_key_secret","epay_wx_channel":"201906181353","epay_alipay_channel":"201906181354","key_price":"10.00"}`
	reqSave := httptest.NewRequest(http.MethodPost, "/api/admin/settings", strings.NewReader(settingsPayload))
	reqSave.AddCookie(cookie)
	rrSave := httptest.NewRecorder()
	requirePermission("settings", handleAdminSettings)(rrSave, reqSave)
	if rrSave.Code != http.StatusOK {
		t.Fatalf("failed to save settings: %d", rrSave.Code)
	}

	// Pre-insert keys to card_stock
	_, errInsertStock := db.Exec("INSERT INTO card_stock (card_key, vendor, status, note, created_at, updated_at) VALUES ('STOCKKEYR1', 'ai.deard.fun', 'available', 'Refund Test', ?, ?), ('STOCKKEYR2', 'ai.deard.fun', 'available', 'Refund Test', ?, ?), ('STOCKKEYR3', 'ai.deard.fun', 'available', 'Refund Test', ?, ?)", now, now, now, now, now, now)
	if errInsertStock != nil {
		t.Fatalf("failed to insert stock keys: %v", errInsertStock)
	}

	// 1. Place a card key order to lock keys
	buyPayload := `{"quantity": 2, "type": "wxpay"}`
	reqBuy := httptest.NewRequest(http.MethodPost, "/api/pay/buy", strings.NewReader(buyPayload))
	reqBuy.AddCookie(cookie)
	rrBuy := httptest.NewRecorder()
	requirePermission("buy", handlePayBuy)(rrBuy, reqBuy)
	if rrBuy.Code != http.StatusOK {
		t.Fatalf("expected 200 for buy order creation, got %d. Body: %s", rrBuy.Code, rrBuy.Body.String())
	}
	var buyResp map[string]interface{}
	json.Unmarshal(rrBuy.Body.Bytes(), &buyResp)
	outTradeNo := buyResp["out_trade_no"].(string)

	// Verify that 2 stock keys are locked
	var lockedCount int
	db.QueryRow("SELECT COUNT(*) FROM card_stock WHERE status = 'locked'").Scan(&lockedCount)
	if lockedCount != 2 {
		t.Errorf("expected 2 locked keys in stock pool, got %d", lockedCount)
	}

	// 2. Try to place a second order while first is unpaid
	reqBuy2 := httptest.NewRequest(http.MethodPost, "/api/pay/buy", strings.NewReader(buyPayload))
	reqBuy2.AddCookie(cookie)
	rrBuy2 := httptest.NewRecorder()
	requirePermission("buy", handlePayBuy)(rrBuy2, reqBuy2)
	if rrBuy2.Code == http.StatusOK {
		t.Errorf("expected buy order creation to be blocked while there is an unpaid order, but got 200")
	}

	// 3. Cancel the order manually
	cancelPayload := fmt.Sprintf(`{"out_trade_no": "%s"}`, outTradeNo)
	reqCancel := httptest.NewRequest(http.MethodPost, "/api/pay/cancel", strings.NewReader(cancelPayload))
	reqCancel.AddCookie(cookie)
	rrCancel := httptest.NewRecorder()
	requirePermission("buy", handlePayCancel)(rrCancel, reqCancel)
	if rrCancel.Code != http.StatusOK {
		t.Fatalf("expected 200 for cancellation, got %d. Body: %s", rrCancel.Code, rrCancel.Body.String())
	}

	// Verify keys are unlocked back to available
	var availableCount int
	db.QueryRow("SELECT COUNT(*) FROM card_stock WHERE status = 'available'").Scan(&availableCount)
	if availableCount != 3 {
		t.Errorf("expected all 3 keys to be available after cancel, got %d", availableCount)
	}

	// 4. Place a new order (should succeed now)
	reqBuyNew := httptest.NewRequest(http.MethodPost, "/api/pay/buy", strings.NewReader(buyPayload))
	reqBuyNew.AddCookie(cookie)
	rrBuyNew := httptest.NewRecorder()
	requirePermission("buy", handlePayBuy)(rrBuyNew, reqBuyNew)
	if rrBuyNew.Code != http.StatusOK {
		t.Fatalf("expected 200 for new buy order, got %d. Body: %s", rrBuyNew.Code, rrBuyNew.Body.String())
	}
	var buyNewResp map[string]interface{}
	json.Unmarshal(rrBuyNew.Body.Bytes(), &buyNewResp)
	newOutTradeNo := buyNewResp["out_trade_no"].(string)

	// Simulate payment callback notify
	notifyParams := map[string]string{
		"pid":          "1668",
		"trade_no":     "202607049999",
		"out_trade_no": newOutTradeNo,
		"type":         "wxpay",
		"name":         "购买卡密 x2",
		"money":        "20.00",
		"trade_status": "TRADE_SUCCESS",
	}
	notifySign := calculateEpaySign(notifyParams, "test_epay_key_secret")
	notifyQuery := fmt.Sprintf("pid=1668&trade_no=202607049999&out_trade_no=%s&type=wxpay&name=%s&money=20.00&trade_status=TRADE_SUCCESS&sign=%s",
		newOutTradeNo, url.QueryEscape("购买卡密 x2"), notifySign)
	reqNotify := httptest.NewRequest(http.MethodGet, "/api/pay/notify?"+notifyQuery, nil)
	rrNotify := httptest.NewRecorder()
	handlePayNotify(rrNotify, reqNotify)

	// 5. Test Refund API on the paid order
	refundPayload := fmt.Sprintf(`{"out_trade_no": "%s"}`, newOutTradeNo)
	reqRefund := httptest.NewRequest(http.MethodPost, "/api/admin/pay/refund", strings.NewReader(refundPayload))
	reqRefund.AddCookie(cookie)
	rrRefund := httptest.NewRecorder()
	requirePermission("buy", handleAdminPayRefund)(rrRefund, reqRefund)
	if rrRefund.Code != http.StatusOK {
		t.Fatalf("expected 200 for refund, got %d. Body: %s", rrRefund.Code, rrRefund.Body.String())
	}

	// Verify order status is refunded in DB
	var dbStatusNew string
	db.QueryRow("SELECT status FROM key_orders WHERE out_trade_no = ?", newOutTradeNo).Scan(&dbStatusNew)
	if dbStatusNew != "refunded" {
		t.Errorf("expected refunded status, got %s", dbStatusNew)
	}

	// Verify old system keys are inactive
	var inactiveCount int
	db.QueryRow("SELECT COUNT(*) FROM system_keys WHERE status = 'inactive'").Scan(&inactiveCount)
	if inactiveCount != 2 {
		t.Errorf("expected 2 system keys deactivated, got %d", inactiveCount)
	}

	// Verify new keys are generated and returned to card_stock as available
	var finalAvailable int
	db.QueryRow("SELECT COUNT(*) FROM card_stock WHERE status = 'available'").Scan(&finalAvailable)
	// We started with 3. Order 2 was placed (1 left). Then 2 keys were refunded and new ones added back.
	// So 1 + 2 = 3 available keys!
	if finalAvailable != 3 {
		t.Errorf("expected 3 available keys back in card_stock pool, got %d", finalAvailable)
	}
}

func TestAdminKeysInvalidate(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Clear admin and keys tables
	_, _ = db.Exec("DELETE FROM admin_sessions")
	_, _ = db.Exec("DELETE FROM admins")
	_, _ = db.Exec("DELETE FROM system_keys")

	// Insert test admin
	hashedAdminPassword, _ := bcrypt.GenerateFromPassword([]byte("adminpass123"), bcrypt.DefaultCost)
	now := time.Now()
	resAdmin, err := db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES (?, ?, 'admin', ?, ?)",
		"testadmin", string(hashedAdminPassword), now, now)
	if err != nil {
		t.Fatalf("failed to insert admin: %v", err)
	}
	adminID, _ := resAdmin.LastInsertId()

	// Insert test user (regular user)
	hashedUserPassword, _ := bcrypt.GenerateFromPassword([]byte("userpass123"), bcrypt.DefaultCost)
	resUser, err := db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES (?, ?, 'user', ?, ?)",
		"testuser", string(hashedUserPassword), now, now)
	if err != nil {
		t.Fatalf("failed to insert user: %v", err)
	}
	userID, _ := resUser.LastInsertId()

	// Add keys permission to user so they pass requirePermission check
	_, _ = db.Exec("INSERT INTO admin_permissions (admin_id, permission, created_at) VALUES (?, 'keys', ?)", userID, now)

	// Log in as admin to get cookie
	loginPayload := `{"username": "testadmin", "password": "adminpass123"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginPayload))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)
	adminCookie := rrLogin.Result().Cookies()[0]

	// Log in as user to get cookie
	loginPayloadUser := `{"username": "testuser", "password": "userpass123"}`
	reqLoginUser := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginPayloadUser))
	rrLoginUser := httptest.NewRecorder()
	handleAdminLogin(rrLoginUser, reqLoginUser)
	userCookie := rrLoginUser.Result().Cookies()[0]

	// Insert test keys: one owned by admin (creator_id = adminID), one owned by user (creator_id = userID)
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"key-admin", "ai.deard.fun", "vendor-key-1", "active", adminID, "key-admin", now, now)
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"key-user", "ai.deard.fun", "vendor-key-2", "active", userID, "key-user", now, now)

	// 1. Test Invalidate key-admin as Admin - Should succeed
	invalidatePayload := `{"system_key": "key-admin"}`
	req1 := httptest.NewRequest(http.MethodPost, "/api/admin/keys/invalidate", strings.NewReader(invalidatePayload))
	req1.AddCookie(adminCookie)
	rr1 := httptest.NewRecorder()
	requirePermission("keys", handleAdminKeysInvalidate)(rr1, req1)

	if rr1.Code != http.StatusOK {
		t.Errorf("expected 200, got %d. Body: %s", rr1.Code, rr1.Body.String())
	}
	var resp1 map[string]interface{}
	json.Unmarshal(rr1.Body.Bytes(), &resp1)
	if resp1["success"] != true {
		t.Errorf("expected success: true, got %v", resp1["success"])
	}

	// Verify key-admin is inactive
	var statusAdminKey string
	db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'key-admin'").Scan(&statusAdminKey)
	if statusAdminKey != "inactive" {
		t.Errorf("expected inactive, got %s", statusAdminKey)
	}

	// 2. Test Invalidate key-user as Admin - Should succeed (admin has permission for all keys)
	invalidatePayload2 := `{"system_key": "key-user"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/admin/keys/invalidate", strings.NewReader(invalidatePayload2))
	req2.AddCookie(adminCookie)
	rr2 := httptest.NewRecorder()
	requirePermission("keys", handleAdminKeysInvalidate)(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr2.Code)
	}

	// Reset key-user back to active for testing user role
	_, _ = db.Exec("UPDATE system_keys SET status = 'active' WHERE system_key = 'key-user'")

	// 3. Test Invalidate key-admin as User - Should fail (User doesn't own key-admin)
	req3 := httptest.NewRequest(http.MethodPost, "/api/admin/keys/invalidate", strings.NewReader(invalidatePayload))
	req3.AddCookie(userCookie)
	rr3 := httptest.NewRecorder()
	requirePermission("keys", handleAdminKeysInvalidate)(rr3, req3)

	if rr3.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d. Body: %s", rr3.Code, rr3.Body.String())
	}

	// 4. Test Invalidate key-user as User - Should succeed (User owns key-user)
	req4 := httptest.NewRequest(http.MethodPost, "/api/admin/keys/invalidate", strings.NewReader(invalidatePayload2))
	req4.AddCookie(userCookie)
	rr4 := httptest.NewRecorder()
	requirePermission("keys", handleAdminKeysInvalidate)(rr4, req4)

	if rr4.Code != http.StatusOK {
		t.Errorf("expected 200, got %d. Body: %s", rr4.Code, rr4.Body.String())
	}

	var statusUserKey string
	db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'key-user'").Scan(&statusUserKey)
	if statusUserKey != "inactive" {
		t.Errorf("expected inactive, got %s", statusUserKey)
	}
}

func TestAdminOrderReplaceResubmit(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Clear relevant tables
	_, _ = db.Exec("DELETE FROM admin_sessions")
	_, _ = db.Exec("DELETE FROM admins")
	_, _ = db.Exec("DELETE FROM account_records")
	_, _ = db.Exec("DELETE FROM orders")
	_, _ = db.Exec("DELETE FROM system_keys")

	now := time.Now()

	// 1. Insert test admin
	hashedAdminPassword, _ := bcrypt.GenerateFromPassword([]byte("adminpass123"), bcrypt.DefaultCost)
	resAdmin, err := db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES (?, ?, 'admin', ?, ?)",
		"testadmin", string(hashedAdminPassword), now, now)
	if err != nil {
		t.Fatalf("failed to insert admin: %v", err)
	}
	adminID, _ := resAdmin.LastInsertId()

	// Add orders permission
	_, _ = db.Exec("INSERT INTO admin_permissions (admin_id, permission, created_at) VALUES (?, 'orders', ?)", adminID, now)

	// Log in as admin to get cookie
	loginPayload := `{"username": "testadmin", "password": "adminpass123"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginPayload))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)
	adminCookie := rrLogin.Result().Cookies()[0]

	// 2. Insert keys in system_keys
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"old-key", "ai.deard.fun", "old-vendor-key", "active", adminID, "old-key", now, now)
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"new-key", "ai.deard.fun", "new-vendor-key", "active", adminID, "new-key", now, now)
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"used-key", "ai.deard.fun", "used-vendor-key", "active", adminID, "used-key", now, now)

	// 3. Create an order with old-key
	resOrder, errOrder := db.Exec("INSERT INTO orders (card_secret, vendor, mode, creator_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		"old-key", "ai.deard.fun", "single", adminID, now, now)
	if errOrder != nil {
		t.Fatalf("failed to create order: %v", errOrder)
	}
	orderID, _ := resOrder.LastInsertId()

	// Create an order with used-key to test key usage validation
	_, _ = db.Exec("INSERT INTO orders (card_secret, vendor, mode, creator_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		"used-key", "ai.deard.fun", "single", adminID, now, now)

	// 4. Create account records for the first order
	_, _ = db.Exec("INSERT INTO account_records (order_id, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "user@example.com", "pass123", "twofa123", "running", "正在处理中", now, now)

	// 5. Test Replace & Resubmit requests
	// Case A: Replace with the same key
	payloadSame := fmt.Sprintf(`{"order_id": %d, "new_card_secret": "old-key"}`, orderID)
	reqSame := httptest.NewRequest(http.MethodPost, "/api/admin/orders/replace", strings.NewReader(payloadSame))
	reqSame.AddCookie(adminCookie)
	rrSame := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrderReplaceResubmit)(rrSame, reqSame)
	if rrSame.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for same key, got %d. Body: %s", rrSame.Code, rrSame.Body.String())
	}

	// Case B: Replace with already used key
	payloadUsed := fmt.Sprintf(`{"order_id": %d, "new_card_secret": "used-key"}`, orderID)
	reqUsed := httptest.NewRequest(http.MethodPost, "/api/admin/orders/replace", strings.NewReader(payloadUsed))
	reqUsed.AddCookie(adminCookie)
	rrUsed := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrderReplaceResubmit)(rrUsed, reqUsed)
	if rrUsed.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for already used key, got %d. Body: %s", rrUsed.Code, rrUsed.Body.String())
	}

	// Case C: Replace with invalid key (doesn't exist)
	payloadInvalid := fmt.Sprintf(`{"order_id": %d, "new_card_secret": "non-existent-key"}`, orderID)
	reqInvalid := httptest.NewRequest(http.MethodPost, "/api/admin/orders/replace", strings.NewReader(payloadInvalid))
	reqInvalid.AddCookie(adminCookie)
	rrInvalid := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrderReplaceResubmit)(rrInvalid, reqInvalid)
	if rrInvalid.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-existent key, got %d. Body: %s", rrInvalid.Code, rrInvalid.Body.String())
	}

	// Case D: Replace with valid active key (new-key) - Should succeed
	payloadValid := fmt.Sprintf(`{"order_id": %d, "new_card_secret": "new-key"}`, orderID)
	reqValid := httptest.NewRequest(http.MethodPost, "/api/admin/orders/replace", strings.NewReader(payloadValid))
	reqValid.AddCookie(adminCookie)
	rrValid := httptest.NewRecorder()
	requireSuperAdmin(handleAdminOrderReplaceResubmit)(rrValid, reqValid)
	if rrValid.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid replace, got %d. Body: %s", rrValid.Code, rrValid.Body.String())
	}

	// 6. Verification
	// Old key should be inactive
	var oldKeyStatus string
	db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'old-key'").Scan(&oldKeyStatus)
	if oldKeyStatus != "inactive" {
		t.Errorf("expected old-key status 'inactive', got: %s", oldKeyStatus)
	}

	// New key should still be active
	var newKeyStatus string
	db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'new-key'").Scan(&newKeyStatus)
	if newKeyStatus != "active" {
		t.Errorf("expected new-key status 'active', got: %s", newKeyStatus)
	}

	// Order card secret should be updated
	var updatedSecret string
	var updatedVendor string
	db.QueryRow("SELECT card_secret, vendor FROM orders WHERE id = ?", orderID).Scan(&updatedSecret, &updatedVendor)
	if updatedSecret != "new-key" || updatedVendor != "ai.deard.fun" {
		t.Errorf("expected order secret updated to 'new-key' and vendor 'ai.deard.fun', got secret='%s', vendor='%s'", updatedSecret, updatedVendor)
	}

	// Check records in account_records: there should be 2 records
	rows, errQuery := db.Query("SELECT id, status, message FROM account_records WHERE order_id = ? ORDER BY id ASC", orderID)
	if errQuery != nil {
		t.Fatalf("failed to query account records: %v", errQuery)
	}
	defer rows.Close()

	type Rec struct {
		ID      int64
		Status  string
		Message string
	}
	var records []Rec
	for rows.Next() {
		var r Rec
		rows.Scan(&r.ID, &r.Status, &r.Message)
		records = append(records, r)
	}

	if len(records) != 2 { // 1 original (updated to failed status with message "切换卡密订阅") + 1 new pending submission
		t.Errorf("expected 2 records in account_records for order, got %d: %+v", len(records), records)
	} else {
		// First record is original updated to failed
		if records[0].Status != "failed" || records[0].Message != "切换卡密订阅" {
			t.Errorf("expected first record failed with '切换卡密订阅', got status=%s, message=%s", records[0].Status, records[0].Message)
		}
		// Second record is the new pending submission
		if records[1].Status != "pending" || records[1].Message != "排队处理中" {
			t.Errorf("expected second record pending with '排队处理中', got status=%s, message=%s", records[1].Status, records[1].Message)
		}
	}
}

func TestOpenAPIs(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Initially API is OFF (by default)
	reqQuery := httptest.NewRequest(http.MethodGet, "/api/open/query?card_secret=test-card", nil)
	rrQuery := httptest.NewRecorder()
	handleOpenQuery(rrQuery, reqQuery)
	if rrQuery.Code != http.StatusForbidden {
		t.Errorf("expected 403 when API is off, got %d", rrQuery.Code)
	}

	// 2. Set API ON in database settings
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_open', 'on', ?) ON DUPLICATE KEY UPDATE setting_value='on'", time.Now())

	// Test when whitelist is empty (allows all)
	_, _ = db.Exec("DELETE FROM system_settings WHERE setting_key = 'api_whitelist'")
	rrQuery2 := httptest.NewRecorder()
	handleOpenQuery(rrQuery2, reqQuery)
	if rrQuery2.Code != http.StatusOK { // 200 OK because query endpoint returns empty slice for invalid cards
		t.Errorf("expected 200 when whitelist empty and card invalid, got %d. Body: %s", rrQuery2.Code, rrQuery2.Body.String())
	}

	// 3. Set API whitelist to a specific IP (e.g. 192.168.1.1)
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_whitelist', '192.168.1.1', ?) ON DUPLICATE KEY UPDATE setting_value='192.168.1.1'", time.Now())

	// Request from non-whitelisted IP (standard localhost request)
	reqQueryIP := httptest.NewRequest(http.MethodGet, "/api/open/query?card_secret=test-card", nil)
	reqQueryIP.RemoteAddr = "127.0.0.1:12345"
	rrQueryIP := httptest.NewRecorder()
	handleOpenQuery(rrQueryIP, reqQueryIP)
	if rrQueryIP.Code != http.StatusForbidden {
		t.Errorf("expected 403 when request IP not whitelisted, got %d. Body: %s", rrQueryIP.Code, rrQueryIP.Body.String())
	}

	// Request with whitelisted IP in X-Forwarded-For header
	reqQueryFF := httptest.NewRequest(http.MethodGet, "/api/open/query?card_secret=test-card", nil)
	reqQueryFF.Header.Set("X-Forwarded-For", "192.168.1.1, 10.0.0.1")
	rrQueryFF := httptest.NewRecorder()
	handleOpenQuery(rrQueryFF, reqQueryFF)
	if rrQueryFF.Code != http.StatusOK { // Whitelist check passes via X-Forwarded-For, returns 200 with empty list
		t.Errorf("expected 200 when whitelisted IP matches, got %d. Body: %s", rrQueryFF.Code, rrQueryFF.Body.String())
	}

	// Test open reset API when API is off
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_open', 'off', ?) ON DUPLICATE KEY UPDATE setting_value='off'", time.Now())
	reqResetOff := httptest.NewRequest(http.MethodPost, "/api/open/reset", strings.NewReader(`{"old_keys":["key1"]}`))
	rrResetOff := httptest.NewRecorder()
	handleOpenReset(rrResetOff, reqResetOff)
	if rrResetOff.Code != http.StatusForbidden {
		t.Errorf("expected 403 for open reset when API is off, got %d", rrResetOff.Code)
	}

	// Test open reset API when API is on
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_open', 'on', ?) ON DUPLICATE KEY UPDATE setting_value='on'", time.Now())
	_, _ = db.Exec("DELETE FROM system_settings WHERE setting_key = 'api_whitelist'")

	// Insert active system key to reset
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, creator_id, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, 1, ?, ?, ?)",
		"key-to-reset-open", "ai.deard.fun", "vendor-key-1", "active", "key-to-reset-open", time.Now(), time.Now())

	reqResetOn := httptest.NewRequest(http.MethodPost, "/api/open/reset", strings.NewReader(`{"old_keys":["key-to-reset-open"]}`))
	rrResetOn := httptest.NewRecorder()
	handleOpenReset(rrResetOn, reqResetOn)
	if rrResetOn.Code != http.StatusOK {
		t.Fatalf("expected 200 for open reset when API is on, got %d. Body: %s", rrResetOn.Code, rrResetOn.Body.String())
	}

	var resetOpenResp ResetKeysResponse
	json.Unmarshal(rrResetOn.Body.Bytes(), &resetOpenResp)
	if !resetOpenResp.Success || len(resetOpenResp.NewKeys) != 1 {
		t.Errorf("expected successful open reset and 1 new key, got %v", resetOpenResp)
	}

	// --- Test Open Cancel API ---
	// 1. Test when API is OFF
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_open', 'off', ?) ON DUPLICATE KEY UPDATE setting_value='off'", time.Now())
	reqCancelOff := httptest.NewRequest(http.MethodPost, "/api/open/cancel", strings.NewReader(`{"card_secret":"test-card", "username":"test@gmail.com"}`))
	rrCancelOff := httptest.NewRecorder()
	handleOpenCancel(rrCancelOff, reqCancelOff)
	if rrCancelOff.Code != http.StatusForbidden {
		t.Errorf("expected 403 for open cancel when API is off, got %d", rrCancelOff.Code)
	}

	// 2. Test when API is ON
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('api_open', 'on', ?) ON DUPLICATE KEY UPDATE setting_value='on'", time.Now())
	reqCancelOn := httptest.NewRequest(http.MethodPost, "/api/open/cancel", strings.NewReader(`{"card_secret":"test-card", "username":"test@gmail.com"}`))
	rrCancelOn := httptest.NewRecorder()
	handleOpenCancel(rrCancelOn, reqCancelOn)
	// Should not be 403 (should be 404 because key/account doesn't exist, which means it passed the API open/whitelist guard check!)
	if rrCancelOn.Code != http.StatusNotFound {
		t.Errorf("expected 404 (not found) for open cancel with invalid params when API is on, got %d. Body: %s", rrCancelOn.Code, rrCancelOn.Body.String())
	}
}

func TestCancelSubscription(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Setup mock vendor API server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		var reqMap map[string]interface{}
		json.Unmarshal(bodyBytes, &reqMap)
		action := reqMap["action"].(string)
		w.Header().Set("Content-Type", "application/json")

		if action == "cancel_task" {
			taskID := reqMap["task_id"].(string)
			if taskID == "TK-FAIL-CANCEL" {
				resp := map[string]interface{}{
					"success": false,
					"message": "Vendor cancellation error",
				}
				json.NewEncoder(w).Encode(resp)
			} else {
				resp := map[string]interface{}{
					"success": true,
					"message": "Task cancelled successfully",
				}
				json.NewEncoder(w).Encode(resp)
			}
		}
	}))
	defer mockServer.Close()

	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	now := time.Now()

	// --- A. Self-Operated Cancel Test ---
	// 1. Insert self-operated key
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"ck-self-cancel", "ai.deard.fun", "", "active", "ck-self-cancel", now, now)

	// 2. Insert order
	resOrder, _ := db.Exec("INSERT INTO orders (card_secret, vendor, mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"ck-self-cancel", "ai.deard.fun", "single", now, now)
	orderID, _ := resOrder.LastInsertId()

	// 3. Insert pending account record
	_, _ = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderID, "ck-self-cancel", "selfuser@gmail.com", "pwd123", "2FA", "pending", "排队处理中", now, now)

	// 4. Request Cancel
	cancelReq := CancelSubscriptionRequest{
		CardSecret: "ck-self-cancel",
		Username:   "selfuser@gmail.com",
	}
	bodyBytes, _ := json.Marshal(cancelReq)
	req := httptest.NewRequest(http.MethodPost, "/api/query/cancel", bytes.NewBuffer(bodyBytes))
	rr := httptest.NewRecorder()
	handleCancelSubscription(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected self cancel 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var respCancel map[string]interface{}
	json.Unmarshal(rr.Body.Bytes(), &respCancel)
	if respCancel["success"] != true {
		t.Errorf("expected success: true, got %v", respCancel)
	}

	// Verify status in DB: should be 'cancelled'
	var selfStatus, selfMessage string
	db.QueryRow("SELECT status, message FROM account_records WHERE card_secret = ? AND username = ?", "ck-self-cancel", "selfuser@gmail.com").Scan(&selfStatus, &selfMessage)
	if selfStatus != "cancelled" || selfMessage != "已取消" {
		t.Errorf("expected status 'cancelled' and message '已取消', got '%s'/'%s'", selfStatus, selfMessage)
	}

	// --- B. Non-Self-Operated Cancel Test (Success case) ---
	// 1. Insert non-self-operated key (pass.aisale.one)
	_, _ = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		"ck-vendor-cancel", "pass.aisale.one", "V-KEY-CANCEL-1", "active", "ck-vendor-cancel", now, now)

	// 2. Insert order
	resOrderVendor, _ := db.Exec("INSERT INTO orders (card_secret, vendor, mode, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"ck-vendor-cancel", "pass.aisale.one", "single", now, now)
	orderIDVendor, _ := resOrderVendor.LastInsertId()

	// 3. Insert pending account record with task_id
	_, _ = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderIDVendor, "ck-vendor-cancel", "vendoruser@gmail.com", "pwd123", "2FA", "pending", "排队处理中", "TK-OK-CANCEL", now, now)

	// 4. Request Cancel
	cancelReqVendor := CancelSubscriptionRequest{
		CardSecret: "ck-vendor-cancel",
		Username:   "vendoruser@gmail.com",
	}
	bodyBytesVendor, _ := json.Marshal(cancelReqVendor)
	reqVendor := httptest.NewRequest(http.MethodPost, "/api/query/cancel", bytes.NewBuffer(bodyBytesVendor))
	rrVendor := httptest.NewRecorder()
	handleCancelSubscription(rrVendor, reqVendor)

	if rrVendor.Code != http.StatusOK {
		t.Errorf("expected vendor cancel 200, got %d. Body: %s", rrVendor.Code, rrVendor.Body.String())
	}

	// Verify status in DB: should be 'cancelled'
	var vendorStatus, vendorMessage string
	db.QueryRow("SELECT status, message FROM account_records WHERE card_secret = ? AND username = ?", "ck-vendor-cancel", "vendoruser@gmail.com").Scan(&vendorStatus, &vendorMessage)
	if vendorStatus != "cancelled" || vendorMessage != "Task cancelled successfully" {
		t.Errorf("expected status 'cancelled' and message 'Task cancelled successfully', got '%s' (msg: '%s')", vendorStatus, vendorMessage)
	}

	// --- C. Non-Self-Operated Cancel Test (Vendor Failure / Rollback case) ---
	// 1. Insert pending account record with a failing task_id
	_, _ = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, task_id, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		orderIDVendor, "ck-vendor-cancel", "vendorfailuser@gmail.com", "pwd123", "2FA", "pending", "排队处理中", "TK-FAIL-CANCEL", now, now)

	// 2. Request Cancel
	cancelReqFail := CancelSubscriptionRequest{
		CardSecret: "ck-vendor-cancel",
		Username:   "vendorfailuser@gmail.com",
	}
	bodyBytesFail, _ := json.Marshal(cancelReqFail)
	reqFail := httptest.NewRequest(http.MethodPost, "/api/query/cancel", bytes.NewBuffer(bodyBytesFail))
	rrFail := httptest.NewRecorder()
	handleCancelSubscription(rrFail, reqFail)

	if rrFail.Code == http.StatusOK {
		t.Errorf("expected vendor cancel failure (non-200), got %d. Body: %s", rrFail.Code, rrFail.Body.String())
	}

	// Verify status in DB: should STILL be 'pending' (rolled back!)
	var dbStatusFail string
	db.QueryRow("SELECT status FROM account_records WHERE card_secret = ? AND username = ?", "ck-vendor-cancel", "vendorfailuser@gmail.com").Scan(&dbStatusFail)
	if dbStatusFail != "pending" {
		t.Errorf("expected status 'pending' (rolled back), got '%s'", dbStatusFail)
	}
}

func TestInFlightLimiter(t *testing.T) {
	initTestDB(t)

	// Manually lock the key for IP 127.0.0.1 and path /api/query
	key := "127.0.0.1:/api/query"
	if !GlobalLimiter.TryAcquire(key) {
		t.Fatalf("failed to acquire lock in test setup")
	}
	defer GlobalLimiter.Release(key)

	// Now send a request to /api/query from 127.0.0.1
	req := httptest.NewRequest(http.MethodGet, "/api/query?card_secret=test", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()

	// Run it through the wrapped handleQuery
	limit(handleQuery)(rr, req)

	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429 Too Many Requests, got %d", rr.Code)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	if resp["success"] != false || resp["message"] != "请求正在处理中，请勿重复提交" {
		t.Errorf("expected standard rate limit error message, got: %v", resp)
	}
}

func TestDeardCardConversion(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Start a mock HTTP server representing the pass.aisale.one gateway
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		json.Unmarshal(body, &req)

		action := req["action"].(string)
		w.Header().Set("Content-Type", "application/json")

		if action == "get_balance" {
			cdkey := req["cdkey"].(string)
			if cdkey == "DEPLETED-KEY" {
				// A key with no balance
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success":        true,
					"remaining_uses": "0.0",
					"total_uses":     "10.0",
				})
			} else if cdkey == "INVALID-KEY" {
				// A key that is invalid
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"message": "CDKey is invalid",
				})
			} else if cdkey == "GOOD-KEY" {
				// A key with balance
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success":        true,
					"remaining_uses": "15.5",
					"total_uses":     "20.0",
				})
			} else {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"success": false,
					"message": "unknown key",
				})
			}
		} else if action == "submit_task" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"task_id": "TK-MOCK-CONV-123",
				"message": "提交成功",
			})
		}
	}))
	defer mockServer.Close()

	// Direct vendor Base URL to the mock server
	oldVendorBaseURL := vendorBaseURL
	vendorBaseURL = mockServer.URL
	defer func() { vendorBaseURL = oldVendorBaseURL }()

	// 2. Configure the db key_vendors to direct pass.aisale.one queries to our mock server
	_, err := db.Exec("INSERT INTO key_vendors (name, display_name, api_url, created_at, updated_at) VALUES ('pass.aisale.one', 'aisale', ?, NOW(), NOW()) ON DUPLICATE KEY UPDATE api_url = ?", mockServer.URL, mockServer.URL)
	if err != nil {
		t.Fatalf("failed to insert/update key vendor: %v", err)
	}

	// 3. Set the deard_convert_open setting to 'on'
	_, err = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('deard_convert_open', 'on', NOW()) ON DUPLICATE KEY UPDATE setting_value = 'on'")
	if err != nil {
		t.Fatalf("failed to set deard_convert_open: %v", err)
	}

	// 4. Insert some third-party keys (one invalid, one depleted, one good) in system_keys
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, 'pass.aisale.one', ?, 'active', ?, NOW(), NOW())",
		"sys-invalid", "INVALID-KEY", "sys-invalid")
	if err != nil {
		t.Fatalf("failed to insert invalid system key: %v", err)
	}
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, 'pass.aisale.one', ?, 'active', ?, NOW(), NOW())",
		"sys-depleted", "DEPLETED-KEY", "sys-depleted")
	if err != nil {
		t.Fatalf("failed to insert depleted system key: %v", err)
	}
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, 'pass.aisale.one', ?, 'active', ?, NOW(), NOW())",
		"sys-good", "GOOD-KEY", "sys-good")
	if err != nil {
		t.Fatalf("failed to insert good system key: %v", err)
	}

	// 5. Insert a user's submitted deard card key
	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, 'ai.deard.fun', '', 'active', ?, NOW(), NOW())",
		"DEARD-USER-KEY", "DEARD-USER-KEY")
	if err != nil {
		t.Fatalf("failed to insert deard key: %v", err)
	}

	// 6. Submit a request using the deard key
	subReq := SubmitRequest{
		CardSecret: "DEARD-USER-KEY",
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "testuser@gmail.com",
		Password:  "pass123",
		TwoFactor: "12345678901234567890123456789012",
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusOK {
		var errResp map[string]interface{}
		json.Unmarshal(rrSubmit.Body.Bytes(), &errResp)
		t.Fatalf("expected submit status 200, got %d, body: %v", rrSubmit.Code, errResp)
	}

	// 7. Verify database updates
	// A. The user's system key "DEARD-USER-KEY" should have been converted to pass.aisale.one with vendor_key = "GOOD-KEY"
	var updatedVendor, updatedVKey string
	err = db.QueryRow("SELECT vendor, vendor_key FROM system_keys WHERE system_key = 'DEARD-USER-KEY'").Scan(&updatedVendor, &updatedVKey)
	if err != nil {
		t.Fatalf("failed to query updated deard key: %v", err)
	}
	if updatedVendor != "pass.aisale.one" || updatedVKey != "GOOD-KEY" {
		t.Errorf("expected deard key vendor to be 'pass.aisale.one' and vendor_key to be 'GOOD-KEY', got vendor: %s, key: %s", updatedVendor, updatedVKey)
	}

	// B. The invalid and depleted keys should have been marked as 'inactive'
	var invalidStatus string
	err = db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'sys-invalid'").Scan(&invalidStatus)
	if err != nil {
		t.Fatalf("failed to query sys-invalid status: %v", err)
	}
	if invalidStatus != "inactive" {
		t.Errorf("expected sys-invalid to be 'inactive', got %s", invalidStatus)
	}

	var depletedStatus string
	err = db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'sys-depleted'").Scan(&depletedStatus)
	if err != nil {
		t.Fatalf("failed to query sys-depleted status: %v", err)
	}
	if depletedStatus != "inactive" {
		t.Errorf("expected sys-depleted to be 'inactive', got %s", depletedStatus)
	}

	// C. The good key should still be 'active'
	var goodStatus string
	err = db.QueryRow("SELECT status FROM system_keys WHERE system_key = 'sys-good'").Scan(&goodStatus)
	if err != nil {
		t.Fatalf("failed to query sys-good status: %v", err)
	}
	if goodStatus != "active" {
		t.Errorf("expected sys-good to be 'active', got %s", goodStatus)
	}

	// D. The order created should have vendor = 'pass.aisale.one'
	var orderVendor string
	err = db.QueryRow("SELECT vendor FROM orders WHERE card_secret = 'DEARD-USER-KEY'").Scan(&orderVendor)
	if err != nil {
		t.Fatalf("failed to query created order vendor: %v", err)
	}
	if orderVendor != "pass.aisale.one" {
		t.Errorf("expected created order vendor to be 'pass.aisale.one', got %s", orderVendor)
	}
}

func Test2FAValidation(t *testing.T) {
	// 1. Test isValid2FA helper function directly
	validCases := []string{
		"12345678901234567890123456789012", // 32 digits (alphanumeric, length 32)
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", // 32 letters/digits (length 32)
		"JBSW Y3DP EHPK 3PXP JBSW Y3DP EHPK 3PXP", // 32 chars with spaces
		"12345678",                         // 8 digits (8 % 8 == 0)
		"1234-5678",                        // 8 digits with dash
		"12345678 87654321",                // 16 digits (16 % 8 == 0)
		"12345678,87654321,11223344",       // 24 digits (24 % 8 == 0)
	}

	for _, tc := range validCases {
		if !isValid2FA(tc) {
			t.Errorf("expected 2FA string %q to be valid, but got invalid", tc)
		}
	}

	invalidCases := []string{
		"",
		"   ",
		"1234567",                          // 7 digits
		"123456789",                        // 9 digits
		"JBSWY3DPEHPK3PXP",                 // 16 alphanumeric chars (neither 32 chars nor pure digits)
		"12345678A",                        // contains letter 'A', length 9
		"1234-5678-9",                      // 9 digits
	}

	for _, tc := range invalidCases {
		if isValid2FA(tc) {
			t.Errorf("expected 2FA string %q to be invalid, but got valid", tc)
		}
	}

	// 2. Test submit API with invalid 2FA
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Create a mock active system key
	sysKey := "2FA-TEST-KEY"
	_, errDbInsert := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, original_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		sysKey, "mock", "MOCK-VENDOR-KEY", "active", sysKey, time.Now(), time.Now())
	if errDbInsert != nil {
		t.Fatalf("failed to insert mock system key: %v", errDbInsert)
	}

	subReq := SubmitRequest{
		CardSecret: sysKey,
		Mode:       "single",
	}
	subReq.Accounts = append(subReq.Accounts, struct {
		Username   string `json:"username"`
		Password   string `json:"password"`
		TwoFactor  string `json:"two_factor"`
		ExtraEmail string `json:"extra_email,omitempty"`
	}{
		Username:  "user@example.com",
		Password:  "password",
		TwoFactor: "1234567", // Invalid 7-digit 2FA
	})

	bodyBytes, _ := json.Marshal(subReq)
	reqSubmit := httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewBuffer(bodyBytes))
	rrSubmit := httptest.NewRecorder()
	handleSubmit(rrSubmit, reqSubmit)

	if rrSubmit.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 Bad Request for invalid 2FA, got %d", rrSubmit.Code)
	}

	if !strings.Contains(rrSubmit.Body.String(), "2FA格式不正确，请输入32位密钥或备用验证码") {
		t.Errorf("expected error message containing '2FA格式不正确，请输入32位密钥或备用验证码', got: %s", rrSubmit.Body.String())
	}
}

func TestKeyTierPricesCalculation(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Set key_price and key_tier_prices in DB
	now := time.Now()
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('key_price', '10.00', ?) ON DUPLICATE KEY UPDATE setting_value = '10.00'", now)
	tierJSON := `[{"min_qty":10,"price":8.50},{"min_qty":50,"price":7.00}]`
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('key_tier_prices', ?, ?) ON DUPLICATE KEY UPDATE setting_value = ?", tierJSON, now, tierJSON)

	// 2. Test calculateKeyUnitPrice logic
	if price := calculateKeyUnitPrice(1); price != 10.00 {
		t.Errorf("expected price 10.00 for qty=1, got %.2f", price)
	}
	if price := calculateKeyUnitPrice(5); price != 10.00 {
		t.Errorf("expected price 10.00 for qty=5, got %.2f", price)
	}
	if price := calculateKeyUnitPrice(10); price != 8.50 {
		t.Errorf("expected price 8.50 for qty=10, got %.2f", price)
	}
	if price := calculateKeyUnitPrice(30); price != 8.50 {
		t.Errorf("expected price 8.50 for qty=30, got %.2f", price)
	}
	if price := calculateKeyUnitPrice(50); price != 7.00 {
		t.Errorf("expected price 7.00 for qty=50, got %.2f", price)
	}
	if price := calculateKeyUnitPrice(100); price != 7.00 {
		t.Errorf("expected price 7.00 for qty=100, got %.2f", price)
	}
}

func TestAdminFAQsFlow(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// Clear table
	_, _ = db.Exec("DELETE FROM faqs")
	_, _ = db.Exec("DELETE FROM admin_sessions")
	_, _ = db.Exec("DELETE FROM admins")

	// Insert test admin
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte("testpwd123"), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}

	now := time.Now()
	_, err = db.Exec("INSERT INTO admins (username, password_hash, role, created_at, updated_at) VALUES (?, ?, 'admin', ?, ?)",
		"testadminfaq", string(hashedPassword), now, now)
	if err != nil {
		t.Fatalf("failed to insert admin: %v", err)
	}

	// Login to get session cookie
	loginReqPayload := `{"username": "testadminfaq", "password": "testpwd123"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginReqPayload))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)

	cookies := rrLogin.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == "admin_session" {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("session cookie not found in response")
	}

	// 1. Create a FAQ
	createPayload := `{"error_code": "ERR_TEST_FAQ", "error_desc": "测试错误匹配", "solution": "这是一个测试解决方案"}`
	reqCreate := httptest.NewRequest(http.MethodPost, "/api/admin/faqs/create", strings.NewReader(createPayload))
	reqCreate.AddCookie(sessionCookie)
	rrCreate := httptest.NewRecorder()
	requirePermission("faqs", handleAdminFAQsCreate)(rrCreate, reqCreate)

	if rrCreate.Code != http.StatusOK {
		t.Errorf("expected 200 for create FAQ, got %d. Body: %s", rrCreate.Code, rrCreate.Body.String())
	}

	// Verify duplication prevention
	rrCreateDup := httptest.NewRecorder()
	reqCreateDup := httptest.NewRequest(http.MethodPost, "/api/admin/faqs/create", strings.NewReader(createPayload))
	reqCreateDup.AddCookie(sessionCookie)
	requirePermission("faqs", handleAdminFAQsCreate)(rrCreateDup, reqCreateDup)
	if rrCreateDup.Code == http.StatusOK {
		t.Errorf("expected duplication failure, but got 200")
	}

	// 2. List FAQs
	reqList := httptest.NewRequest(http.MethodGet, "/api/admin/faqs?query=ERR_TEST_FAQ", nil)
	reqList.AddCookie(sessionCookie)
	rrList := httptest.NewRecorder()
	requirePermission("faqs", handleAdminFAQs)(rrList, reqList)

	if rrList.Code != http.StatusOK {
		t.Errorf("expected 200 for list FAQ, got %d", rrList.Code)
	}

	var listResp struct {
		Success bool `json:"success"`
		Total   int  `json:"total"`
		FAQs    []struct {
			ID        int64  `json:"id"`
			ErrorCode string `json:"error_code"`
			ErrorDesc string `json:"error_desc"`
			Solution  string `json:"solution"`
		} `json:"faqs"`
	}
	err = json.Unmarshal(rrList.Body.Bytes(), &listResp)
	if err != nil {
		t.Fatalf("failed to unmarshal list response: %v", err)
	}
	if listResp.Total != 1 || len(listResp.FAQs) != 1 {
		t.Errorf("expected 1 FAQ, got total=%d, length=%d", listResp.Total, len(listResp.FAQs))
	}
	faqID := listResp.FAQs[0].ID

	// 3. Update FAQ
	updatePayload := fmt.Sprintf(`{"id": %d, "error_code": "ERR_TEST_FAQ", "error_desc": "测试错误匹配更新", "solution": "这是一个测试解决方案更新"}`, faqID)
	reqUpdate := httptest.NewRequest(http.MethodPost, "/api/admin/faqs/update", strings.NewReader(updatePayload))
	reqUpdate.AddCookie(sessionCookie)
	rrUpdate := httptest.NewRecorder()
	requirePermission("faqs", handleAdminFAQsUpdate)(rrUpdate, reqUpdate)

	if rrUpdate.Code != http.StatusOK {
		t.Errorf("expected 200 for update FAQ, got %d", rrUpdate.Code)
	}

	// 4. Test client-side solution mapping via /api/query
	// Insert a mock order and failed account record
	cardSecret := "CARD_TEST_FAQ_SECRET"
	_, _ = db.Exec("DELETE FROM account_records WHERE card_secret = ?", cardSecret)
	_, _ = db.Exec("DELETE FROM orders WHERE card_secret = ?", cardSecret)
	_, _ = db.Exec("DELETE FROM system_keys WHERE system_key = ?", cardSecret)

	_, err = db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, created_at, updated_at) VALUES (?, 'ai.deard.fun', 'key123', 'active', ?, ?)", cardSecret, now, now)
	if err != nil {
		t.Fatalf("failed to insert system key: %v", err)
	}

	resOrder, err := db.Exec("INSERT INTO orders (card_secret, mode, vendor, created_at, updated_at) VALUES (?, 'single', 'ai.deard.fun', ?, ?)", cardSecret, now, now)
	if err != nil {
		t.Fatalf("failed to insert mock order: %v", err)
	}
	orderID, _ := resOrder.LastInsertId()

	// Insert failed record with message matching error_desc or error_code of FAQ
	// Error desc is "测试错误匹配更新", message contains "遇到了测试错误匹配更新问题"
	_, err = db.Exec("INSERT INTO account_records (order_id, card_secret, username, password, two_factor, status, message, created_at, updated_at) VALUES (?, ?, 'user1@gmail.com', 'pwd', '2fa', 'failed', '遇到了测试错误匹配更新问题', ?, ?)", orderID, cardSecret, now, now)
	if err != nil {
		t.Fatalf("failed to insert mock account record: %v", err)
	}

	// Perform client query
	reqQuery := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/query?card_secret=%s", cardSecret), nil)
	rrQuery := httptest.NewRecorder()
	handleQuery(rrQuery, reqQuery)

	if rrQuery.Code != http.StatusOK {
		t.Errorf("expected 200 for query, got %d", rrQuery.Code)
	}

	var queryResp struct {
		Success bool            `json:"success"`
		Records []AccountRecord `json:"records"`
	}
	err = json.Unmarshal(rrQuery.Body.Bytes(), &queryResp)
	if err != nil {
		t.Fatalf("failed to unmarshal query response: %v", err)
	}
	if len(queryResp.Records) != 1 {
		t.Errorf("expected 1 record, got %d", len(queryResp.Records))
	} else {
		rec := queryResp.Records[0]
		if rec.Solution != "这是一个测试解决方案更新" {
			t.Errorf("expected mapped solution '这是一个测试解决方案更新', got '%s'", rec.Solution)
		}
	}

	// 5. Delete FAQ
	deletePayload := fmt.Sprintf(`{"id": %d}`, faqID)
	reqDelete := httptest.NewRequest(http.MethodPost, "/api/admin/faqs/delete", strings.NewReader(deletePayload))
	reqDelete.AddCookie(sessionCookie)
	rrDelete := httptest.NewRecorder()
	requirePermission("faqs", handleAdminFAQsDelete)(rrDelete, reqDelete)

	if rrDelete.Code != http.StatusOK {
		t.Errorf("expected 200 for delete FAQ, got %d", rrDelete.Code)
	}

	// Clean up
	_, _ = db.Exec("DELETE FROM account_records WHERE card_secret = ?", cardSecret)
	_, _ = db.Exec("DELETE FROM orders WHERE card_secret = ?", cardSecret)
	_, _ = db.Exec("DELETE FROM system_keys WHERE system_key = ?", cardSecret)
}

func TestXunhuPayFlow(t *testing.T) {
	initTestDB(t)
	if db == nil {
		return
	}
	defer db.Close()

	// 1. Create a local mock server for XunhuPay APIs
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json;charset=UTF-8")
		if r.URL.Path == "/payment/do.html" {
			w.Write([]byte(`{"errcode":0,"errmsg":"success","url":"http://mock-payment-url","open_order_id":"12345"}`))
			return
		}
		if r.URL.Path == "/payment/refund.html" {
			w.Write([]byte(`{"errcode":0,"errmsg":"success","data":{"refund_status":"CD"}}`))
			return
		}
	}))
	defer mockServer.Close()

	// 2. Insert mock settings for Xunhupay
	if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES ('pay_method', 'xunhupay', NOW())"); err != nil {
		t.Fatalf("failed to insert pay_method setting: %v", err)
	}
	if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES ('xunhupay_url', ?, NOW())", mockServer.URL); err != nil {
		t.Fatalf("failed to insert xunhupay_url setting: %v", err)
	}
	if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES ('xunhupay_wx_appid', 'wx_test_appid', NOW())"); err != nil {
		t.Fatalf("failed to insert xunhupay_wx_appid setting: %v", err)
	}
	if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES ('xunhupay_wx_secret', 'wx_test_secret', NOW())"); err != nil {
		t.Fatalf("failed to insert xunhupay_wx_secret setting: %v", err)
	}

	var testVal string
	errGet := db.QueryRow("SELECT setting_value FROM system_settings WHERE setting_key = 'pay_method'").Scan(&testVal)
	t.Logf("BEFORE CHECKOUT: pay_method=%s, err=%v", testVal, errGet)

	// 3. Create test admin and login
	hashedPwd, _ := bcrypt.GenerateFromPassword([]byte("adminpwd"), bcrypt.DefaultCost)
	now := time.Now()
	if _, err := db.Exec("REPLACE INTO admins (username, password_hash, role, created_at, updated_at) VALUES ('admin', ?, 'admin', ?, ?)", string(hashedPwd), now, now); err != nil {
		t.Fatalf("failed to insert admin: %v", err)
	}

	loginBody := `{"username":"admin","password":"adminpwd"}`
	reqLogin := httptest.NewRequest(http.MethodPost, "/api/admin/login", strings.NewReader(loginBody))
	rrLogin := httptest.NewRecorder()
	handleAdminLogin(rrLogin, reqLogin)
	cookie := rrLogin.Result().Cookies()[0]

	// 4. Fill card stock pool
	_, _ = db.Exec("DELETE FROM card_stock")
	_, _ = db.Exec("INSERT INTO card_stock (card_key, status, vendor, note, created_at, updated_at) VALUES ('STOCKKEY1111', 'available', 'ai.deard.fun', 'Xunhu Purchased', ?, ?)", now, now)
	_, _ = db.Exec("INSERT INTO card_stock (card_key, status, vendor, note, created_at, updated_at) VALUES ('STOCKKEY2222', 'available', 'ai.deard.fun', 'Xunhu Purchased', ?, ?)", now, now)

	// 5. Initiate Checkout (Buy 2 keys via wxpay)
	buyPayload := `{"quantity": 2, "type": "wxpay"}`
	reqBuy := httptest.NewRequest(http.MethodPost, "/api/pay/buy", strings.NewReader(buyPayload))
	reqBuy.AddCookie(cookie)
	rrBuy := httptest.NewRecorder()
	requirePermission("buy", handlePayBuy)(rrBuy, reqBuy)

	if rrBuy.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d. Body: %s", rrBuy.Code, rrBuy.Body.String())
	}

	var buyResp map[string]interface{}
	json.Unmarshal(rrBuy.Body.Bytes(), &buyResp)
	if buyResp["pay_url"] != "http://mock-payment-url" {
		t.Errorf("expected pay_url 'http://mock-payment-url', got '%v'", buyResp["pay_url"])
	}
	outTradeNo := buyResp["out_trade_no"].(string)

	// Verify order entry contains pay_method = 'xunhupay'
	var dbPayMethod string
	db.QueryRow("SELECT pay_method FROM key_orders WHERE out_trade_no = ?", outTradeNo).Scan(&dbPayMethod)
	if dbPayMethod != "xunhupay" {
		t.Errorf("expected pay_method 'xunhupay' in DB, got '%s'", dbPayMethod)
	}

	// 6. Simulate callback notify (Success payment)
	notifyVals := url.Values{
		"appid":          {"wx_test_appid"},
		"trade_order_id": {outTradeNo},
		"total_fee":      {"19.98"},
		"status":         {"OD"},
		"time":           {fmt.Sprintf("%d", time.Now().Unix())},
	}
	
	// Create signature
	xunhuPayHelper := NewXunhuPay("wx_test_appid", "wx_test_secret", mockServer.URL)
	paramsSign := make(map[string]string)
	for k, vs := range notifyVals {
		paramsSign[k] = vs[0]
	}
	notifyVals.Set("hash", xunhuPayHelper.MakeSign(paramsSign))

	reqNotify := httptest.NewRequest(http.MethodPost, "/api/pay/notify", strings.NewReader(notifyVals.Encode()))
	reqNotify.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rrNotify := httptest.NewRecorder()
	handlePayNotify(rrNotify, reqNotify)

	if rrNotify.Body.String() != "success" {
		t.Fatalf("expected notify response 'success', got: %s", rrNotify.Body.String())
	}

	// Verify order status is paid in DB
	var dbStatus string
	db.QueryRow("SELECT status FROM key_orders WHERE out_trade_no = ?", outTradeNo).Scan(&dbStatus)
	if dbStatus != "paid" {
		t.Errorf("expected paid status, got %s", dbStatus)
	}

	// 7. Test Refund API on the paid order
	refundPayload := fmt.Sprintf(`{"out_trade_no": "%s"}`, outTradeNo)
	reqRefund := httptest.NewRequest(http.MethodPost, "/api/admin/pay/refund", strings.NewReader(refundPayload))
	reqRefund.AddCookie(cookie)
	rrRefund := httptest.NewRecorder()
	requirePermission("buy", handleAdminPayRefund)(rrRefund, reqRefund)
	if rrRefund.Code != http.StatusOK {
		t.Fatalf("expected 200 for refund, got %d. Body: %s", rrRefund.Code, rrRefund.Body.String())
	}
}



