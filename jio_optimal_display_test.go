package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJioOptimalDisplayAndPriceShareSupplier(t *testing.T) {
	initTestDB(t)
	adminCookie := createTestAdminSession(t, "jio_display_admin", "admin", []string{"dashboard"})
	for key, value := range map[string]string{
		"jio_dispatch_strategy": "auto_lowest_cost", "jio_active_provider": "acczone",
		"jio_pricing_mode": "fixed_markup", "jio_pricing_fixed_markup": "2",
		"jio_pricing_exchange_rate": "7", "jio_pricing_round_precision": "2",
		"jio_pricing_cached_cost": "9",
	} {
		if _, err := db.Exec("REPLACE INTO system_settings (setting_key, setting_value, updated_at) VALUES (?, ?, NOW())", key, value); err != nil {
			t.Fatal(err)
		}
	}
	autoLowestCostCache.RLock()
	previous, previousTime := autoLowestCostCache.candidates, autoLowestCostCache.lastEvaluated
	autoLowestCostCache.RUnlock()
	t.Cleanup(func() {
		autoLowestCostCache.Lock()
		autoLowestCostCache.candidates, autoLowestCostCache.lastEvaluated = previous, previousTime
		autoLowestCostCache.Unlock()
	})
	for _, empty := range []bool{false, true} {
		candidates := []CandidateSupplier{
			{Name: "aivault", DisplayName: "AIVault", PriceUSD: 0.35, Available: !empty},
			{Name: "vente", DisplayName: "Vente", PriceUSD: 0.45, Available: !empty},
			{Name: "acczone", DisplayName: "Acczone", PriceUSD: 0.1, Available: false},
		}
		autoLowestCostCache.Lock()
		autoLowestCostCache.candidates, autoLowestCostCache.lastEvaluated = candidates, time.Now()
		autoLowestCostCache.Unlock()
		rr := httptest.NewRecorder()
		handleAdminJioSuppliersList(rr, httptest.NewRequest(http.MethodGet, "/api/admin/jio/suppliers", nil))
		var suppliers struct {
			Effective   string  `json:"current_effective_provider"`
			OptimalCost float64 `json:"optimal_cost_usd"`
			Suppliers   []struct {
				Name   string `json:"name"`
				Active bool   `json:"is_active"`
			} `json:"suppliers"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &suppliers); err != nil {
			t.Fatal(err)
		}
		wantProvider, wantCost, wantSale := "aivault", 0.35, 4.45
		if empty {
			wantProvider, wantCost, wantSale = "", 0, 0
		}
		if suppliers.Effective != wantProvider || suppliers.OptimalCost != wantCost {
			t.Fatalf("wrong preferred supplier: %+v", suppliers)
		}
		for _, supplier := range suppliers.Suppliers {
			if supplier.Active != (supplier.Name == wantProvider) {
				t.Fatalf("incorrect badge: %+v", supplier)
			}
		}
		rr = httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/admin/dashboard/stats", nil)
		request.AddCookie(adminCookie)
		requireAdmin(handleAdminDashboardStats)(rr, request)
		if rr.Code != http.StatusOK {
			t.Fatalf("dashboard failed: %s", rr.Body.String())
		}
		var dashboard struct {
			Pricing struct {
				Provider  string  `json:"active_provider"`
				Cost      float64 `json:"cost_usd"`
				Sale      float64 `json:"sale_price"`
				Available bool    `json:"price_available"`
			} `json:"jio_pricing"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &dashboard); err != nil {
			t.Fatal(err)
		}
		if dashboard.Pricing.Provider != wantProvider || dashboard.Pricing.Cost != wantCost || dashboard.Pricing.Sale != wantSale || dashboard.Pricing.Available == empty {
			t.Fatalf("dashboard and preferred supplier diverged: %+v", dashboard.Pricing)
		}
		if got := GetCurrentJioSalePrice(); got != wantSale {
			t.Fatalf("current price=%v, want=%v", got, wantSale)
		}
	}
}

func TestOptimalJioSupplierSkipsUnavailableAndInvalidPrice(t *testing.T) {
	candidates := []CandidateSupplier{
		{Name: "invalid", Available: true},
		{Name: "first", Available: true, PriceUSD: 0.35},
		{Name: "second", Available: true, PriceUSD: 0.45},
		{Name: "sold-out", Available: false, PriceUSD: 0.1},
	}
	if chosen := optimalJioSupplier(candidates); chosen == nil || chosen.Name != "first" {
		t.Fatalf("wrong priority: %+v", chosen)
	}
	if chosen := optimalJioSupplier(candidates[3:]); chosen != nil {
		t.Fatalf("selected sold-out supplier: %+v", chosen)
	}
}
