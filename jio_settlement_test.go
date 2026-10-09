package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type settlementTestProvider struct {
	MockJioProvider
	name       string
	price      float64
	amount     float64
	fail       bool
	called     func()
	balance    *float64
	balanceErr error
	stock      *int
}

func (p *settlementTestProvider) Name() string { return p.name }
func (p *settlementTestProvider) GetProducts(context.Context) ([]SupplierProduct, error) {
	return []SupplierProduct{{ID: "jio", Name: "Jio", PriceUSD: p.price, IsActive: true, Stock: p.stock}}, nil
}
func (p *settlementTestProvider) GetBalance(context.Context) (*SupplierBalance, error) {
	if p.balanceErr != nil {
		return nil, p.balanceErr
	}
	balance := 9999.0
	if p.balance != nil {
		balance = *p.balance
	}
	return &SupplierBalance{Supported: true, Balance: balance, Currency: "USD"}, nil
}
func (p *settlementTestProvider) GetOfferLink(ctx context.Context, _, _ string) (string, error) {
	p.called()
	if p.fail {
		return "", errors.New("sold out")
	}
	recordJioPurchaseCost(ctx, &SupplierPurchaseResult{AmountUSD: p.amount})
	return "https://example.com/redeemed", nil
}

func TestJioSettlementAfterSuccessfulFallback(t *testing.T) {
	initTestDB(t)
	jioProvidersMu.Lock()
	previous := jioProviders
	jioProviders = make(map[string]JioProvider)
	jioProvidersMu.Unlock()
	t.Cleanup(func() { jioProvidersMu.Lock(); jioProviders = previous; jioProvidersMu.Unlock() })
	_, err := db.Exec("INSERT INTO admins (id, username, password_hash, role, jio_balance, created_at, updated_at) VALUES (991, 'settlement_test', 'hash', 'admin', 1, NOW(), NOW()) ON DUPLICATE KEY UPDATE jio_balance = 1")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"jio_dispatch_strategy": "auto_lowest_cost", "jio_active_provider": "expensive", "jio_pricing_mode": "fixed_markup", "jio_pricing_exchange_rate": "10", "jio_pricing_fixed_markup": "2", "jio_pricing_round_precision": "2", "maintenance_mode_jio": "off"} {
		if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, NOW())", key, value); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	callback := func(name string) func() {
		return func() {
			calls = append(calls, name)
			balance, err := GetAdminJioBalance(991)
			if err != nil || balance != 1 {
				t.Errorf("wallet charged before upstream success: %v, %v", balance, err)
			}
		}
	}
	RegisterJioProvider(&settlementTestProvider{name: "cheap", price: 0.1, fail: true, called: callback("cheap")})
	RegisterJioProvider(&settlementTestProvider{name: "expensive", price: 0.2, amount: 0.3, called: callback("expensive")})
	for _, key := range []string{"SETTLEMENT-1", "SETTLEMENT-2"} {
		if _, err := db.Exec("INSERT INTO system_keys (system_key, vendor, vendor_key, status, service_type, creator_id, created_at, updated_at) VALUES (?, 'mock', '', 'active', 'jio', 991, NOW(), NOW())", key); err != nil {
			t.Fatal(err)
		}
	}
	redeem := func(key string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		handleJioRedeem(rr, httptest.NewRequest(http.MethodPost, "/api/jio/redeem", strings.NewReader(`{"card_secret":"`+key+`"}`)))
		return rr
	}
	rr := redeem("SETTLEMENT-1")
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem failed: %s", rr.Body.String())
	}
	if strings.Join(calls, ",") != "cheap,expensive" {
		t.Fatalf("unexpected purchase priority: %v", calls)
	}
	balance, _ := GetAdminJioBalance(991)
	if balance != -4 {
		t.Fatalf("expected actual receipt cost 0.3*10+2, balance -4; got %v", balance)
	}
	var salePrice, consumed float64
	_ = db.QueryRow("SELECT sale_price FROM orders WHERE card_secret = 'SETTLEMENT-1'").Scan(&salePrice)
	_ = db.QueryRow("SELECT -amount FROM jio_wallet_transactions WHERE card_secret = 'SETTLEMENT-1' AND type = 'consume'").Scan(&consumed)
	if salePrice != 5 || consumed != 5 {
		t.Fatalf("order/ledger mismatch: %v/%v", salePrice, consumed)
	}
	if rr := redeem("SETTLEMENT-1"); rr.Code != http.StatusOK {
		t.Fatalf("repeat failed: %s", rr.Body.String())
	}
	if len(calls) != 2 {
		t.Fatal("duplicate upstream purchase")
	}
	if balance, _, err := AddJioWalletBalance(991, 1, "admin_recharge", "partial debt repayment", 0, nil, ""); err != nil || balance != -3 {
		t.Fatalf("partial recharge of negative wallet failed: balance=%v err=%v", balance, err)
	}
	for _, startingBalance := range []float64{-4, 0} {
		_, _ = db.Exec("UPDATE admins SET jio_balance = ? WHERE id = 991", startingBalance)
		rr := redeem("SETTLEMENT-2")
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "联系管理员充值") {
			t.Fatalf("nonpositive balance not rejected: %s", rr.Body.String())
		}
	}
	_, _ = db.Exec("UPDATE admins SET jio_balance = 1 WHERE id = 991")
	p, _ := GetJioProvider("expensive")
	p.(*settlementTestProvider).fail = true
	rr = redeem("SETTLEMENT-2")
	if !strings.Contains(rr.Body.String(), "库存不足") {
		t.Fatalf("missing stock error: %s", rr.Body.String())
	}
	balance, _ = GetAdminJioBalance(991)
	if balance != 1 {
		t.Fatalf("failed purchase changed wallet: %v", balance)
	}
}

func TestJioPurchaseReceiptIsolation(t *testing.T) {
	a, b := &jioPurchaseReceipt{CostUSD: 0.2}, &jioPurchaseReceipt{CostUSD: 0.7}
	ctx := context.WithValue(context.Background(), jioPurchaseReceiptKey{}, a)
	recordJioPurchaseCost(ctx, &SupplierPurchaseResult{AmountUSD: 0.5})
	if a.CostUSD != 0.5 || b.CostUSD != 0.7 {
		t.Fatal("receipt not isolated")
	}
	recordJioPurchaseCost(ctx, &SupplierPurchaseResult{})
	if a.CostUSD != 0.5 {
		t.Fatal("empty receipt overwrote supplier cost")
	}
}
