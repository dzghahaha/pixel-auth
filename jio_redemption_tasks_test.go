package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type taskTestProvider struct {
	settlementTestProvider
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
	failed  bool
}

func (p *taskTestProvider) GetOfferLink(ctx context.Context, _, _ string) (string, error) {
	p.calls.Add(1)
	close(p.started)
	reportJioProgress(ctx, 70)
	reportJioProgress(ctx, 20) // A new number must not regress the task percentage.
	select {
	case <-p.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if p.failed {
		return "", errors.New("fixture failure")
	}
	return "https://serviceactivation.google.com/subscription/new/" + strings.Repeat("x", 60), nil
}

func setJioTestSetting(t *testing.T, key, value string) {
	t.Helper()
	if _, err := db.Exec("REPLACE INTO system_settings (setting_key,setting_value,updated_at) VALUES (?,?,NOW())", key, value); err != nil {
		t.Fatal(err)
	}
}

func isolateJioProviders(t *testing.T, providers ...JioProvider) {
	t.Helper()
	jioProvidersMu.Lock()
	previous := jioProviders
	jioProviders = map[string]JioProvider{}
	for _, p := range providers {
		jioProviders[p.Name()] = p
	}
	jioProvidersMu.Unlock()
	invalidateJioSupplierQuotes()
	t.Cleanup(func() {
		jioProvidersMu.Lock()
		jioProviders = previous
		jioProvidersMu.Unlock()
		invalidateJioSupplierQuotes()
	})
}

func TestJioSupplierDisableFiltersFreshCachedAndSpecificDispatch(t *testing.T) {
	initTestDB(t)
	cheap := &settlementTestProvider{name: "cheap-enabled-test", price: 0.1, called: func() { t.Error("disabled provider purchased") }}
	expensive := &settlementTestProvider{name: "expensive-enabled-test", price: 0.4, called: func() {}}
	isolateJioProviders(t, cheap, expensive)
	setJioTestSetting(t, "jio_dispatch_strategy", "auto_lowest_cost")
	candidates, _ := EvaluateEligibleSuppliers(context.Background(), true)
	if got := optimalJioSupplier(candidates); got == nil || got.Name != cheap.Name() {
		t.Fatalf("initial selection: %+v", got)
	}
	// Directly change state without clearing cache, simulating another process.
	setJioTestSetting(t, "jio_supplier_enabled_"+cheap.Name(), "off")
	candidates, _ = EvaluateEligibleSuppliers(context.Background(), false)
	if got := optimalJioSupplier(candidates); got == nil || got.Name != expensive.Name() {
		t.Fatalf("cached disabled quote selected: %+v", got)
	}
	_, name, _, err := selectJioProviderForOfferWithCost(context.Background(), "test-card", "")
	if err != nil || name != expensive.Name() {
		t.Fatalf("dispatch: %s %v", name, err)
	}
	setJioTestSetting(t, "jio_dispatch_strategy", "specific")
	setJioTestSetting(t, "jio_active_provider", cheap.Name())
	if _, _, _, err := selectJioProviderForOfferWithCost(context.Background(), "test-card", ""); err == nil {
		t.Fatal("specific bypassed disabled state")
	}
	for _, handler := range []http.HandlerFunc{handleAdminJioSuppliersSwitchActive, handleAdminJioSuppliersStrategy, handleAdminJioSuppliersPurchase} {
		w := httptest.NewRecorder()
		handler(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"provider":"cheap-enabled-test","strategy":"specific"}`)))
		if w.Code != 400 {
			t.Fatalf("admin bypassed disabled state: %d %s", w.Code, w.Body.String())
		}
	}
	setJioTestSetting(t, "jio_supplier_enabled_"+expensive.Name(), "off")
	setJioTestSetting(t, "jio_dispatch_strategy", "auto_lowest_cost")
	if _, _, _, err := selectJioProviderForOfferWithCost(context.Background(), "test-card", ""); !errors.Is(err, ErrAllSuppliersExhausted) {
		t.Fatal("all-disabled dispatch did not fail")
	}
}

func TestGrizzlyUnlimitedCatalogAndEnableSwitch(t *testing.T) {
	initTestDB(t)
	p := &GrizzlyJioProvider{}
	isolateJioProviders(t, p)
	if isJioSupplierEnabled("grizzly") {
		t.Fatal("new supplier enabled without configuration")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("action") != "getBalance" {
			t.Error("catalog tried to buy or check stock")
		}
		_, _ = w.Write([]byte("ACCESS_BALANCE:1.50"))
	}))
	defer server.Close()
	if err := SaveSupplierConfig("grizzly", map[string]any{"api_key": "fixture-key", "api_url": server.URL}); err != nil {
		t.Fatal(err)
	}
	cookie := createTestAdminSession(t, "grizzly-switch-admin", "user", []string{"jio_suppliers"})
	for _, enabled := range []bool{true, false, true} {
		body, _ := json.Marshal(map[string]any{"provider": "grizzly", "enabled": enabled})
		r := httptest.NewRequest("POST", "/", strings.NewReader(string(body)))
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		requirePermission("jio_suppliers", handleAdminJioSupplierEnabled)(w, r)
		if w.Code != 200 || isJioSupplierEnabled("grizzly") != enabled {
			t.Fatalf("switch: %s", w.Body.String())
		}
		candidates, _ := EvaluateEligibleSuppliers(context.Background(), true)
		if len(candidates) != 1 || candidates[0].Available != enabled {
			t.Fatalf("eligibility: %+v", candidates)
		}
		if enabled && (!candidates[0].UnlimitedStock || candidates[0].Stock != nil) {
			t.Fatalf("inventory not unlimited: %+v", candidates)
		}
	}
	products, err := p.GetProducts(context.Background())
	if err != nil || len(products) != 1 || !products[0].UnlimitedStock || products[0].PriceUSD != 0.208 {
		t.Fatalf("catalog: %+v %v", products, err)
	}
}

func TestJioAsyncRedemptionIdempotencyProgressAndSettlement(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "success"
		if failed {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			initTestDB(t)
			_, _ = db.Exec("DELETE FROM jio_redemption_tasks")
			p := &taskTestProvider{settlementTestProvider: settlementTestProvider{name: "async-test", price: 0.208}, started: make(chan struct{}), release: make(chan struct{}), failed: failed}
			isolateJioProviders(t, p)
			var release sync.Once
			t.Cleanup(func() { release.Do(func() { close(p.release) }) })
			setJioTestSetting(t, "jio_active_provider", p.Name())
			setJioTestSetting(t, "jio_dispatch_strategy", "specific")
			setJioTestSetting(t, "jio_pricing_mode", "fixed")
			setJioTestSetting(t, "jio_pricing_fixed_price", "2")
			if _, err := db.Exec(`INSERT INTO admins (id,username,password_hash,role,jio_balance,created_at,updated_at) VALUES (991,'async-admin','hash','admin',100,NOW(),NOW())`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO system_keys (system_key,vendor,vendor_key,status,service_type,creator_id,created_at,updated_at) VALUES ('ASYNC-CARD','mock','','active','jio',991,NOW(),NOW())`); err != nil {
				t.Fatal(err)
			}
			start := func() string {
				requestCtx, cancel := context.WithCancel(context.Background())
				defer cancel() // Browser disconnect must not cancel the worker.
				r := httptest.NewRequest("POST", "/api/jio/redeem", strings.NewReader(`{"card_secret":"ASYNC-CARD","async":true}`)).WithContext(requestCtx)
				w := httptest.NewRecorder()
				handleJioRedeem(w, r)
				if w.Code != 202 {
					t.Fatalf("start: %d %s", w.Code, w.Body.String())
				}
				var response struct {
					Data struct {
						TaskID string `json:"task_id"`
					} `json:"data"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &response)
				return response.Data.TaskID
			}
			id := start()
			select {
			case <-p.started:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not start")
			}
			if duplicate := start(); duplicate != id {
				t.Fatal("duplicate started another task")
			}
			if balance, _ := GetAdminJioBalance(991); balance != 100 {
				t.Fatal("charged before success")
			}
			// Allow the first progress writes to complete.
			deadline := time.Now().Add(3 * time.Second)
			var progress int
			for time.Now().Before(deadline) {
				_ = db.QueryRow("SELECT progress FROM jio_redemption_tasks WHERE task_id=?", id).Scan(&progress)
				if progress == 70 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if progress != 70 {
				t.Fatalf("progress regressed: %d", progress)
			}
			// Disabling affects future dispatch, not the activation already running.
			setJioTestSetting(t, "jio_supplier_enabled_"+p.Name(), "off")
			release.Do(func() { close(p.release) })
			var state string
			deadline = time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				_ = db.QueryRow("SELECT status,progress FROM jio_redemption_tasks WHERE task_id=?", id).Scan(&state, &progress)
				if state != "processing" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			balance, _ := GetAdminJioBalance(991)
			var keyState, link string
			_ = db.QueryRow("SELECT status,COALESCE(discount_url,'') FROM system_keys WHERE system_key='ASYNC-CARD'").Scan(&keyState, &link)
			if failed {
				if state != "failed" || progress >= 100 || balance != 100 || keyState != "active" {
					t.Fatalf("failure recovery: %s %d %v %s", state, progress, balance, keyState)
				}
			} else {
				if state != "success" || progress != 100 || balance != 98 || keyState != "inactive" || link == "" {
					t.Fatalf("settlement: %s %d %v %s", state, progress, balance, keyState)
				}
				w := httptest.NewRecorder()
				handleJioRedeem(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"card_secret":"ASYNC-CARD","async":true}`)))
				if w.Code != 200 || p.calls.Load() != 1 {
					t.Fatal("completed redemption purchased again")
				}
			}
			w := httptest.NewRecorder()
			handleJioRedemptionStatus(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"task_id":"`+id+`"}`)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"result"`) {
				t.Fatalf("durable status: %s", w.Body.String())
			}
		})
	}
}

func TestJioExpiredTaskDoesNotUnlockPossiblyChargedCard(t *testing.T) {
	initTestDB(t)
	id := strings.Repeat("f", 64)
	_, err := db.Exec(`REPLACE INTO jio_redemption_tasks(task_id,card_secret,status,progress,expires_at,created_at,updated_at) VALUES (?,'INTERRUPTED-CARD','processing',95,DATE_SUB(NOW(),INTERVAL 1 MINUTE),NOW(),NOW())`, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO system_keys(system_key,vendor,vendor_key,status,service_type,created_at,updated_at) VALUES ('INTERRUPTED-CARD','mock','','processing','jio',NOW(),NOW())`)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handleJioRedemptionStatus(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"task_id":"`+id+`"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"failed"`) {
		t.Fatal(w.Body.String())
	}
	var state string
	_ = db.QueryRow("SELECT status FROM system_keys WHERE system_key='INTERRUPTED-CARD'").Scan(&state)
	if state != "processing" {
		t.Fatal("interrupted purchase unlocked")
	}
}
