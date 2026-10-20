package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func grizzlyFixture(t *testing.T, failure string) (*grizzlyClient, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var events []string
	var numbers, polls int
	link := "https://serviceactivation.google.com/subscription/new/" + strings.Repeat("a", 60) + "=="
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		action := r.URL.Query().Get("action")
		events = append(events, action+":"+r.URL.Query().Get("status")+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch action {
		case "getNumberV2", "getNumber":
			numbers++
			if r.URL.Query().Get("maxPrice") != "0.208" || r.URL.Query().Get("service") != "jio" || r.URL.Query().Get("country") != "22" {
				t.Error("missing purchase constraints")
			}
			if failure == "no_stock" {
				fmt.Fprint(w, "NO_NUMBERS")
				return
			}
			if failure == "bad_key" {
				fmt.Fprint(w, "BAD_KEY")
				return
			}
			if failure == "ambiguous" {
				w.WriteHeader(503)
				return
			}
			if failure == "legacy" && action == "getNumberV2" {
				fmt.Fprint(w, "BAD_ACTION")
				return
			}
			if failure == "legacy" {
				fmt.Fprint(w, "ACCESS_NUMBER:123:919876543210")
				return
			}
			fmt.Fprintf(w, `{"activationId":"%d","phoneNumber":"919876543210","activationCost":"0.15"}`, numbers)
		case "setStatus":
			fmt.Fprint(w, "ACCESS_READY")
		case "getStatus":
			polls++
			if failure == "raw_sms" {
				fmt.Fprint(w, "Your Jio verification code is 123456")
				return
			}
			if failure == "wait_retry" {
				fmt.Fprint(w, "STATUS_WAIT_RETRY:123456")
				return
			}
			if failure == "otp_timeout" || polls == 1 {
				fmt.Fprint(w, "STATUS_WAIT_CODE")
				return
			}
			fmt.Fprint(w, "STATUS_OK:Your OTP is 123456")
		default:
			switch {
			case strings.Contains(r.URL.Path, "/number/"):
				if len(r.Cookies()) > 0 {
					t.Error("cookies leaked between purchases")
				}
				if failure == "not_jio" || (failure == "first_not_jio" && numbers == 1) {
					fmt.Fprint(w, `{}`)
					return
				}
				fmt.Fprint(w, `{"primaryService":"PREPAID"}`)
			case strings.HasSuffix(r.URL.Path, "sendOtp"):
				var payload map[string]string
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["mobileNumber"] != "9876543210" || payload["loginFlowType"] != "MOBILE" {
					t.Error("wrong OTP payload")
				}
				http.SetCookie(w, &http.Cookie{Name: "flow", Value: "session", Path: "/"})
				fmt.Fprint(w, `{}`)
			case strings.HasSuffix(r.URL.Path, "validateOtp"):
				cookie, err := r.Cookie("flow")
				if err != nil || cookie.Value != "session" {
					t.Error("OTP lost session")
				}
				var payload map[string]string
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["otp"] != "123456" {
					t.Error("invalid OTP extraction")
				}
				if failure == "verify" {
					fmt.Fprint(w, `{"errorMessage":"invalid"}`)
					return
				}
				http.SetCookie(w, &http.Cookie{Name: "auth", Value: "valid", Path: "/"})
				fmt.Fprint(w, `{}`)
			case strings.HasSuffix(r.URL.Path, "authJsonData"):
				if _, err := r.Cookie("auth"); err != nil {
					t.Error("auth cookie missing")
				}
				fmt.Fprint(w, `{"loginFlag":"true"}`)
			case strings.HasSuffix(r.URL.Path, "google-ai"):
				if failure == "no_link" {
					fmt.Fprint(w, `{"redirectionURL":"https://evil.example/"}`)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"redirectionURL": url.PathEscape(url.PathEscape(link))})
			default:
				fmt.Fprint(w, `{"errorCode":"200"}`)
			}
		}
	}))
	t.Cleanup(server.Close)
	g := &grizzlyClient{cfg: grizzlyConfig{APIKey: "secret-do-not-leak", APIURL: server.URL + "/sms", JioURL: server.URL, MaxPrice: 0.208, Timeout: time.Second, MaxAttempts: 1}, http: server.Client(), stockInterval: time.Millisecond, otpInterval: time.Millisecond, otpTimeout: 20 * time.Millisecond}
	return g, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), events...) }
}

func TestGrizzlyPurchaseCookiesLinkCostAndCleanup(t *testing.T) {
	g, events := grizzlyFixture(t, "")
	for i := 0; i < 2; i++ {
		var mu sync.Mutex
		var progress []int
		ctx := context.WithValue(context.Background(), jioProgressKey{}, jioProgressFunc(func(p int) { mu.Lock(); defer mu.Unlock(); progress = append(progress, p) }))
		result, err := g.purchase(ctx)
		if err != nil || result.AmountUSD != 0.15 || !strings.HasSuffix(result.Link, "==") {
			t.Fatalf("purchase: %+v %v", result, err)
		}
		mu.Lock()
		for _, p := range progress {
			if p >= 100 {
				t.Error("100 before settlement")
			}
		}
		if len(progress) < 4 {
			t.Error("missing business progress")
		}
		mu.Unlock()
	}
	joined := strings.Join(events(), " ")
	if strings.Count(joined, "setStatus:6/") != 2 || strings.Contains(joined, "setStatus:8/") {
		t.Fatalf("cleanup: %s", joined)
	}
}

func TestGrizzlyFailuresCancelWithoutDuplicateAcquisition(t *testing.T) {
	for _, failure := range []string{"not_jio", "otp_timeout", "verify", "no_link", "bad_key", "ambiguous", "no_stock"} {
		t.Run(failure, func(t *testing.T) {
			g, events := grizzlyFixture(t, failure)
			if failure == "no_stock" {
				g.cfg.Timeout = 20 * time.Millisecond
			}
			result, err := g.purchase(context.Background())
			if err == nil || result != nil {
				t.Fatalf("unexpected success: %+v", result)
			}
			if strings.Contains(err.Error(), g.cfg.APIKey) {
				t.Fatal("API key exposed")
			}
			joined := strings.Join(events(), " ")
			bought := failure != "bad_key" && failure != "ambiguous" && failure != "no_stock"
			if strings.Contains(joined, "setStatus:8/") != bought || strings.Contains(joined, "setStatus:6/") {
				t.Fatalf("cleanup missing: %s", joined)
			}
			if (failure == "bad_key" || failure == "ambiguous") && strings.Count(joined, "getNumber") != 1 {
				t.Fatal("retried ambiguous purchase")
			}
		})
	}
}

func TestGrizzlyLegacyCostFallbackAndParentCancellation(t *testing.T) {
	g, _ := grizzlyFixture(t, "legacy")
	result, err := g.purchase(context.Background())
	if err != nil || result.AmountUSD != 0.208 {
		t.Fatalf("legacy: %+v %v", result, err)
	}
	g, events := grizzlyFixture(t, "no_stock")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.purchase(ctx); err == nil {
		t.Fatal("cancel ignored")
	}
	if len(events()) != 0 {
		t.Fatal("called supplier after cancellation")
	}
}

func TestGrizzlyRetriesRejectedNumberAndAcceptsCompatibleOTP(t *testing.T) {
	for _, mode := range []string{"first_not_jio", "raw_sms", "wait_retry"} {
		t.Run(mode, func(t *testing.T) {
			g, events := grizzlyFixture(t, mode)
			g.cfg.MaxAttempts = 2
			g.cfg.Timeout = 5 * time.Second
			result, err := g.purchase(context.Background())
			if err != nil || result.Link == "" {
				t.Fatalf("purchase: %+v %v", result, err)
			}
			if mode == "first_not_jio" {
				joined := strings.Join(events(), " ")
				if strings.Count(joined, "getNumberV2") != 2 || strings.Count(joined, "setStatus:8/") != 1 || strings.Count(joined, "setStatus:6/") != 1 {
					t.Fatalf("retry cleanup: %s", joined)
				}
			}
		})
	}
}

func TestGrizzlyConfigAndURLValidation(t *testing.T) {
	for key, value := range map[string]any{"max_price": "NaN", "timeout_seconds": 0, "max_attempts": 21, "api_url": "file:///tmp/key"} {
		if _, err := loadGrizzlyConfig(map[string]any{"api_key": "test", key: value}); err == nil {
			t.Errorf("accepted %s", key)
		}
	}
	if _, err := loadGrizzlyConfig(map[string]any{"api_key": "test"}); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"https://serviceactivation.google.com.evil/subscription/new/" + strings.Repeat("a", 60), "https://serviceactivation.google.com/subscription/new/short", "javascript:alert(1)"} {
		if extractGrizzlyActivationURL(link) != "" {
			t.Error("accepted invalid URL")
		}
	}
}
