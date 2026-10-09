package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

type timeoutQuoteProvider struct {
	settlementTestProvider
	blocked bool
}

func (p *timeoutQuoteProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	if p.blocked {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.settlementTestProvider.GetProducts(ctx)
}

func TestJioQuoteTimeoutRetainsDisplayButNeverPurchasesFromCache(t *testing.T) {
	initTestDB(t)
	jioProvidersMu.Lock()
	previousProviders := jioProviders
	provider := &timeoutQuoteProvider{settlementTestProvider: settlementTestProvider{name: "quote-test", price: 0.4, called: func() { t.Error("purchased despite failed live evaluation") }}}
	jioProviders = map[string]JioProvider{"quote-test": provider}
	jioProvidersMu.Unlock()
	autoLowestCostCache.Lock()
	previousCandidates, previousTime, previousConfirmed := autoLowestCostCache.candidates, autoLowestCostCache.lastEvaluated, autoLowestCostCache.confirmed
	autoLowestCostCache.candidates, autoLowestCostCache.confirmed = nil, nil
	autoLowestCostCache.Unlock()
	t.Cleanup(func() {
		jioProvidersMu.Lock()
		jioProviders = previousProviders
		jioProvidersMu.Unlock()
		autoLowestCostCache.Lock()
		autoLowestCostCache.candidates, autoLowestCostCache.lastEvaluated, autoLowestCostCache.confirmed = previousCandidates, previousTime, previousConfirmed
		autoLowestCostCache.Unlock()
	})
	for key, value := range map[string]string{"jio_dispatch_strategy": "auto_lowest_cost", "jio_pricing_mode": "fixed_markup", "jio_pricing_exchange_rate": "10", "jio_pricing_fixed_markup": "2"} {
		if _, err := db.Exec("REPLACE INTO system_settings (setting_key,setting_value,updated_at) VALUES (?,?,NOW())", key, value); err != nil {
			t.Fatal(err)
		}
	}
	initial := GetJioSupplierStrategyCostInfo(context.Background(), true)
	if initial.ActiveProvider != "quote-test" || initial.QuoteStale {
		t.Fatalf("no initial quote: %+v", initial)
	}
	provider.blocked = true
	autoLowestCostCache.Lock()
	autoLowestCostCache.lastEvaluated = time.Now().Add(-time.Minute)
	confirmedAt := autoLowestCostCache.confirmed["quote-test"].CheckedAt
	autoLowestCostCache.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	info := GetJioSupplierStrategyCostInfo(ctx, false)
	cancel()
	if info.ActiveProvider != "quote-test" || !info.QuoteStale || info.CostUSD != 0.4 || info.CostCNY != 4 || CalculateJioStrategySalePrice(info, GetJioPricingConfig()) != 6 {
		t.Fatalf("timeout discarded confirmed quote: %+v", info)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
	_, _, _, err := selectJioProviderForOfferWithCost(ctx, "quote-test-card", "")
	cancel()
	if !errors.Is(err, ErrAllSuppliersExhausted) {
		t.Fatalf("purchase used cached quote: %v", err)
	}
	autoLowestCostCache.Lock()
	if !autoLowestCostCache.confirmed["quote-test"].CheckedAt.Equal(confirmedAt) {
		t.Error("failed refresh extended last confirmed time")
	}
	old := autoLowestCostCache.confirmed["quote-test"]
	old.CheckedAt = time.Now().Add(-6 * time.Minute)
	autoLowestCostCache.confirmed["quote-test"] = old
	autoLowestCostCache.Unlock()
	info = GetJioSupplierStrategyCostInfo(context.Background(), false)
	if info.ActiveProvider != "" || !info.EvaluationPending {
		t.Fatalf("expired quote reused or failed query called stock shortage: %+v", info)
	}
	provider.blocked = false
	zero := 0
	provider.stock = &zero
	info = GetJioSupplierStrategyCostInfo(context.Background(), true)
	if info.ActiveProvider != "" || info.EvaluationPending {
		t.Fatalf("confirmed stock shortage ignored: %+v", info)
	}
	provider.balanceErr = errors.New("balance temporary failure")
	info = GetJioSupplierStrategyCostInfo(context.Background(), true)
	if info.ActiveProvider != "" {
		t.Fatalf("old funded supplier revived after confirmed sellout: %+v", info)
	}
}
