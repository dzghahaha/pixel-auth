package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJioAutoSelectionRequiresBalanceAndStock(t *testing.T) {
	initTestDB(t)
	jioProvidersMu.Lock()
	previous := jioProviders
	jioProviders = make(map[string]JioProvider)
	jioProvidersMu.Unlock()
	t.Cleanup(func() {
		jioProvidersMu.Lock()
		jioProviders = previous
		jioProvidersMu.Unlock()
		autoLowestCostCache.Lock()
		autoLowestCostCache.candidates = nil
		autoLowestCostCache.Unlock()
	})
	for key, value := range map[string]string{"jio_dispatch_strategy": "auto_lowest_cost", "jio_active_provider": "empty-wallet", "jio_pricing_mode": "fixed_markup", "jio_pricing_fixed_markup": "2", "jio_pricing_exchange_rate": "10", "jio_pricing_round_precision": "2"} {
		if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, NOW())", key, value); err != nil {
			t.Fatal(err)
		}
	}
	zero, underfunded, exact := 0.0, 0.05, 0.3
	noStock := 0
	var calls []string
	providers := []*settlementTestProvider{
		{name: "empty-wallet", price: 0.1, balance: &zero},
		{name: "underfunded", price: 0.15, balance: &underfunded},
		{name: "balance-error", price: 0.2, balanceErr: errors.New("balance unavailable")},
		{name: "sold-out", price: 0.25, stock: &noStock},
		{name: "funded-cheapest", price: 0.3, balance: &exact},
		{name: "funded-expensive", price: 0.6},
	}
	for _, provider := range providers {
		name := provider.name
		provider.called = func() { calls = append(calls, name) }
		RegisterJioProvider(provider)
	}
	candidates, err := EvaluateEligibleSuppliers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 6 || candidates[0].Name != "funded-cheapest" || candidates[1].Name != "funded-expensive" {
		t.Fatalf("unexpected priority: %+v", candidates)
	}
	for i, candidate := range candidates {
		if candidate.Available != (i < 2) {
			t.Fatalf("incorrect eligibility: %+v", candidate)
		}
	}
	link, name, cost, err := selectJioProviderForOfferWithCost(context.Background(), "balance-test", "")
	if err != nil || link == "" || name != "funded-cheapest" || cost != 0.3 || len(calls) != 1 {
		t.Fatalf("wrong purchase: %s %s %v %v calls=%v", link, name, cost, err, calls)
	}
	// Once the cheapest supplier no longer has enough funds, both UI and price use the next eligible supplier.
	exact = 0
	rr := httptest.NewRecorder()
	handleAdminJioSuppliersList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/jio/suppliers?refresh=1", nil))
	var suppliers struct {
		Effective string `json:"current_effective_provider"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &suppliers); err != nil {
		t.Fatal(err)
	}
	if suppliers.Effective != "funded-expensive" {
		t.Fatalf("underfunded supplier remained selected: %s", rr.Body.String())
	}
	info := GetJioSupplierStrategyCostInfo(context.Background(), false)
	if info.ActiveProvider != suppliers.Effective || CalculateJioStrategySalePrice(info, GetJioPricingConfig()) != 8 {
		t.Fatalf("dashboard price disagrees: %+v", info)
	}
	providers[5].fail = true
	calls = nil
	_, _, _, err = selectJioProviderForOfferWithCost(context.Background(), "balance-test-fail", "")
	if !errors.Is(err, ErrAllSuppliersExhausted) || len(calls) != 1 || calls[0] != "funded-expensive" {
		t.Fatalf("ineligible fallback was purchased: calls=%v err=%v", calls, err)
	}
}
