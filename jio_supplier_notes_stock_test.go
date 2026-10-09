package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcczoneStockDrivesAutoSelection(t *testing.T) {
	initTestDB(t)
	jioProvidersMu.Lock()
	previous := jioProviders
	jioProviders = map[string]JioProvider{"acczone": &AcczoneJioProvider{}}
	jioProvidersMu.Unlock()
	t.Cleanup(func() {
		jioProvidersMu.Lock()
		jioProviders = previous
		jioProvidersMu.Unlock()
		autoLowestCostCache.Lock()
		autoLowestCostCache.candidates = nil
		autoLowestCostCache.Unlock()
	})
	stockJSON := `,"stock":0`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getServices":
			fmt.Fprintf(w, `[{"key":"gemini","name":"Gemini","price":0.4,"is_active":1%s}]`, stockJSON)
		case "/getBalance":
			fmt.Fprint(w, `{"balance":10,"username":"stock-test"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	if err := SaveSupplierConfig("acczone", map[string]interface{}{"apikey": "test-key", "service_key": "gemini", "api_url": server.URL + "/buyCpn"}); err != nil {
		t.Fatal(err)
	}
	provider, _ := GetJioProvider("acczone")
	for _, tc := range []struct {
		stock              string
		count              int
		present, available bool
	}{
		{`,"stock":0`, 0, true, false},
		{`,"stock":7`, 7, true, true},
		{``, 0, false, false},
	} {
		stockJSON = tc.stock
		products, err := provider.GetProducts(context.Background())
		if err != nil || len(products) != 1 {
			t.Fatalf("fetch failed: %+v %v", products, err)
		}
		if (products[0].Stock != nil) != tc.present {
			t.Fatalf("stock presence lost: %+v", products[0])
		}
		if tc.present && *products[0].Stock != tc.count {
			t.Fatalf("stock was not mapped: %+v", products[0])
		}
		candidates, err := EvaluateEligibleSuppliers(context.Background(), true)
		if err != nil || len(candidates) != 1 || candidates[0].Available != tc.available {
			t.Fatalf("incorrect stock eligibility: %+v %v", candidates, err)
		}
	}
}

func TestJioSupplierNotesPersistWithoutChangingCredentials(t *testing.T) {
	initTestDB(t)
	cookie := createTestAdminSession(t, "supplier-note-admin", "user", []string{"jio_suppliers"})
	if err := SaveSupplierConfig("acczone", map[string]interface{}{"apikey": "keep-test-key", "service_key": "gemini"}); err != nil {
		t.Fatal(err)
	}
	note := "备用账号\n<script>alert('x')</script>"
	for _, text := range []string{note, "更新备注", ""} {
		body, _ := json.Marshal(map[string]string{"provider": "acczone", "note": text})
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/admin/jio/suppliers/note", strings.NewReader(string(body)))
		req.AddCookie(cookie)
		requirePermission("jio_suppliers", handleAdminJioSupplierNote)(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("note save failed: %s", rr.Body.String())
		}
		if got := getSetting("jio_supplier_note_acczone", ""); got != text {
			t.Fatalf("note not persisted: %q", got)
		}
		if cfg := GetSupplierConfig("acczone"); cfg["apikey"] != "keep-test-key" || cfg["service_key"] != "gemini" {
			t.Fatalf("credentials changed: %+v", cfg)
		}
	}
	for _, body := range []string{`{"provider":"unknown","note":"text"}`, `{"provider":"acczone","note":"` + strings.Repeat("长", 501) + `"}`} {
		rr := httptest.NewRecorder()
		handleAdminJioSupplierNote(rr, httptest.NewRequest(http.MethodPost, "/api/admin/jio/suppliers/note", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid note accepted: %s", rr.Body.String())
		}
	}
	rr := httptest.NewRecorder()
	requirePermission("jio_suppliers", handleAdminJioSupplierNote)(rr, httptest.NewRequest(http.MethodPost, "/api/admin/jio/suppliers/note", strings.NewReader(`{"provider":"acczone","note":"unauthorized"}`)))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous note write accepted: %d", rr.Code)
	}
}
