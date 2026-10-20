package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestParseLocalJioLinks(t *testing.T) {
	links, duplicate, err := parseLocalJioLinks(" https://example.com/a?sig=A%2Bb \r\n\nhttps://example.com/b\nhttps://example.com/a?sig=A%2Bb")
	if err != nil || len(links) != 2 || duplicate != 1 || links[0] != "https://example.com/a?sig=A%2Bb" {
		t.Fatalf("parse: %v %d %v", links, duplicate, err)
	}
	for _, input := range []string{"", "https://example.com/a\njavascript:alert(1)", "https://user:pass@example.com/a", "https:///a", "https://example.com/a b", "https://example.com/" + strings.Repeat("x", 4096), strings.Repeat("https://example.com/a\n", 1001)} {
		if _, _, err := parseLocalJioLinks(input); err == nil {
			t.Fatalf("accepted invalid batch length %d", len(input))
		}
	}
}

func TestLocalJioPrice(t *testing.T) {
	for _, price := range []any{nil, "0.1", float64(0), -1.0, math.NaN(), math.Inf(1), 1000001.0} {
		if _, err := localJioPrice(map[string]interface{}{"price_usd": price}); err == nil {
			t.Fatalf("accepted %v", price)
		}
	}
	if value, err := localJioPrice(map[string]interface{}{"price_usd": 0.123456}); err != nil || value != 0.1235 {
		t.Fatalf("rounding: %v %v", value, err)
	}
}

func setupLocalJioTest(t *testing.T) *LocalJioProvider {
	t.Helper()
	initTestDB(t)
	for _, table := range []string{"jio_links", "jio_local_orders"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveSupplierConfig("local", map[string]interface{}{"price_usd": 0.25}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO system_settings (setting_key,setting_value,updated_at) VALUES ('jio_supplier_enabled_local','on',NOW()) ON DUPLICATE KEY UPDATE setting_value='on',updated_at=NOW()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(invalidateJioSupplierQuotes)
	return &LocalJioProvider{}
}

func TestLocalJioInventoryTransactions(t *testing.T) {
	p := setupLocalJioTest(t)
	ctx := context.Background()
	if n, d, e := importLocalJioLinks(ctx, "https://example.com/a\r\nhttps://example.com/b\nhttps://example.com/a"); e != nil || n != 2 || d != 1 {
		t.Fatalf("import %d %d %v", n, d, e)
	}
	if _, _, e := importLocalJioLinks(ctx, "https://example.com/c\nbad"); e == nil {
		t.Fatal("invalid batch succeeded")
	}
	if _, e := p.Purchase(ctx, SupplierPurchaseRequest{Quantity: 3, CustomerReference: "short"}); e == nil {
		t.Fatal("partial order issued")
	}
	var orders int
	if e := db.QueryRow(`SELECT COUNT(*) FROM jio_local_orders`).Scan(&orders); e != nil || orders != 0 {
		t.Fatalf("failed order not rolled back: %d %v", orders, e)
	}
	request := SupplierPurchaseRequest{Quantity: 2, CustomerReference: "one"}
	first, e := p.Purchase(ctx, request)
	if e != nil {
		t.Fatal(e)
	}
	if len(first.Items) != 2 || first.AmountUSD != 0.5 {
		t.Fatalf("bad receipt %+v", first)
	}
	repeat, e := p.Purchase(ctx, request)
	if e != nil || repeat.OrderID != first.OrderID || repeat.Link != first.Link {
		t.Fatalf("retry %+v %v", repeat, e)
	}
	request.Quantity = 1
	if _, e = p.Purchase(ctx, request); e == nil {
		t.Fatal("changed retry quantity accepted")
	}
	if n, d, e := importLocalJioLinks(ctx, "https://example.com/a"); e != nil || n != 0 || d != 1 {
		t.Fatalf("reimport issued %d %d %v", n, d, e)
	}
	products, e := p.GetProducts(ctx)
	if e != nil || *products[0].Stock != 0 {
		t.Fatalf("stock %+v %v", products, e)
	}
	if _, e = p.Purchase(ctx, SupplierPurchaseRequest{Quantity: 1, CustomerReference: "empty"}); e == nil {
		t.Fatal("empty stock sold")
	}
}

func TestLocalJioConcurrentDelivery(t *testing.T) {
	p := setupLocalJioTest(t)
	ctx := context.Background()
	var batch []string
	for i := 0; i < 12; i++ {
		batch = append(batch, fmt.Sprintf("https://example.com/%d", i))
	}
	if _, _, err := importLocalJioLinks(ctx, strings.Join(batch, "\n")); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result *SupplierPurchaseResult
		err    error
	}
	results := make(chan outcome, 24)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, e := p.Purchase(ctx, SupplierPurchaseRequest{Quantity: 1, CustomerReference: fmt.Sprintf("request-%d", i/2)})
			results <- outcome{r, e}
		}(i)
	}
	wg.Wait()
	close(results)
	seen := map[string]string{}
	hits := map[string]int{}
	for result := range results {
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		r := result.result
		if old, ok := seen[r.Link]; ok && old != r.OrderID {
			t.Error("link issued to two orders")
		}
		seen[r.Link] = r.OrderID
		hits[r.OrderID]++
	}
	if len(seen) != 12 || len(hits) != 12 {
		t.Fatalf("expected 12 orders: %d %d", len(seen), len(hits))
	}
	for _, n := range hits {
		if n != 2 {
			t.Fatal("retry was not reused")
		}
	}
}

func TestLocalJioDispatchAndRecovery(t *testing.T) {
	p := setupLocalJioTest(t)
	ctx := context.Background()
	jioProvidersMu.Lock()
	previous := jioProviders
	jioProviders = map[string]JioProvider{"local": p}
	jioProvidersMu.Unlock()
	t.Cleanup(func() { jioProvidersMu.Lock(); jioProviders = previous; jioProvidersMu.Unlock() })
	if _, _, e := importLocalJioLinks(ctx, "https://example.com/only"); e != nil {
		t.Fatal(e)
	}
	candidates, e := EvaluateEligibleSuppliers(ctx, true)
	if e != nil || len(candidates) != 1 || !candidates[0].Available || candidates[0].BalanceUSD != nil {
		t.Fatalf("local eligibility %+v %v", candidates, e)
	}
	if _, e = db.Exec(`INSERT INTO system_settings (setting_key,setting_value,updated_at) VALUES ('jio_dispatch_strategy','auto_lowest_cost',NOW())`); e != nil {
		t.Fatal(e)
	}
	link, provider, cost, e := selectJioProviderForOfferWithCost(ctx, "card-local", "")
	if e != nil || provider != "local" || cost != 0.25 || link == "" {
		t.Fatalf("dispatch %s %s %f %v", link, provider, cost, e)
	}
	if _, e = db.Exec(`UPDATE system_settings SET setting_value='off' WHERE setting_key='jio_supplier_enabled_local'`); e != nil {
		t.Fatal(e)
	}
	again, _, _, e := selectJioProviderForOfferWithCost(ctx, "card-local", "")
	if e != nil || again != link {
		t.Fatalf("recovery %s %v", again, e)
	}
	if _, e = p.Purchase(ctx, SupplierPurchaseRequest{CustomerReference: "disabled"}); e == nil {
		t.Fatal("disabled supplier sold")
	}
}

func TestLocalJioAdminValidation(t *testing.T) {
	setupLocalJioTest(t)
	for _, body := range []string{`{"links":"bad"}`, `{"links":""}`} {
		rr := httptest.NewRecorder()
		handleAdminJioLocalLinks(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
		if rr.Code != 400 {
			t.Fatalf("validation status %d", rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	handleAdminJioLocalLinks(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"links":"https://example.com/test"}`)))
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	rr = httptest.NewRecorder()
	handleAdminJioLocalLinks(rr, httptest.NewRequest(http.MethodGet, "/?status=available&page=999", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"available":1`) {
		t.Fatal(rr.Body.String())
	}
	rr = httptest.NewRecorder()
	requireSuperAdmin(handleAdminJioLocalLinks)(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code == 200 {
		t.Fatal("unauthenticated inventory access")
	}
}

func TestLocalJioCardRedemptionSettlement(t *testing.T) {
	p := setupLocalJioTest(t)
	isolateJioProviders(t, p)
	for key, value := range map[string]string{"jio_active_provider": "local", "jio_dispatch_strategy": "specific", "jio_pricing_mode": "fixed_markup", "jio_pricing_exchange_rate": "10", "jio_pricing_fixed_markup": "2", "jio_pricing_round_precision": "2", "maintenance_mode_jio": "off"} {
		setJioTestSetting(t, key, value)
	}
	if _, err := db.Exec(`INSERT INTO admins (id,username,password_hash,role,jio_balance,created_at,updated_at) VALUES (991,'local-owner','hash','admin',100,NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO system_keys (system_key,vendor,vendor_key,status,service_type,creator_id,created_at,updated_at) VALUES ('LOCAL-CARD','local','','active','jio',991,NOW(),NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := importLocalJioLinks(context.Background(), "https://example.com/settlement"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		handleJioRedeem(rr, httptest.NewRequest(http.MethodPost, "/api/jio/redeem", strings.NewReader(`{"card_secret":"LOCAL-CARD"}`)))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "https://example.com/settlement") {
			t.Fatalf("redeem %d: %s", rr.Code, rr.Body.String())
		}
	}
	var charges, deliveries int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jio_wallet_transactions WHERE card_secret='LOCAL-CARD' AND type='consume'`).Scan(&charges); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM jio_links WHERE status='issued'`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	balance, err := GetAdminJioBalance(991)
	if err != nil || balance != 95.5 || charges != 1 || deliveries != 1 {
		t.Fatalf("balance=%v charges=%d deliveries=%d err=%v", balance, charges, deliveries, err)
	}
	info := GetJioSupplierStrategyCostInfo(context.Background(), false)
	if info.CostUSD != .25 {
		t.Fatalf("local cached cost mismatch: %+v", info)
	}
}
