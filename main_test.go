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
	createTables()

	// Clear tables for test isolation
	_, _ = db.Exec("DELETE FROM account_records")
	_, _ = db.Exec("DELETE FROM orders")
	_, _ = db.Exec("DELETE FROM system_keys")
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
		TwoFactor: "2FASECRET",
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
		TwoFactor: "2FASECRET",
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
		TwoFactor: "2FASECRET",
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
		TwoFactor: "2FA",
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
	err = db.QueryRow("SELECT card_secret FROM orders o JOIN account_records r ON r.order_id = o.id WHERE r.username = 'reset_test_user@gmail.com'").Scan(&orderCardSecret)
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
		TwoFactor: "2FA",
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
		TwoFactor: "2FA",
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
	_, err = db.Exec("INSERT INTO admins (username, password_hash, created_at, updated_at) VALUES (?, ?, ?, ?)",
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
	requireAdmin(handleAdminOrdersUpdate)(rrUpdate, reqUpdate)

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
