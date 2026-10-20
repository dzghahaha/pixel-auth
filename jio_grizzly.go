package main

// Port of jio-gemini-otp/jio.py. A purchase owns its cookies and activation ID;
// credentials, phone numbers, OTPs and upstream response bodies are never logged.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const grizzlyAPI = "https://api.grizzlysms.com/stubs/handler_api.php"
const grizzlyProductID = "jio"

type grizzlyConfig struct {
	APIKey, APIURL, JioURL string
	MaxPrice               float64
	Timeout                time.Duration
	MaxAttempts            int
}

func loadGrizzlyConfig(raw map[string]interface{}) (grizzlyConfig, error) {
	cfg := grizzlyConfig{APIURL: grizzlyAPI, JioURL: "https://www.jio.com", MaxPrice: 0.208, Timeout: 300 * time.Second, MaxAttempts: 5}
	cfg.APIKey, _ = raw["api_key"].(string)
	cfg.APIKey = strings.TrimSpace(cfg.APIKey)
	if cfg.APIKey == "" {
		return cfg, errors.New("请先配置 Grizzly API Key")
	}
	// Endpoint overrides support private gateways and local integration tests.
	for key, target := range map[string]*string{"api_url": &cfg.APIURL, "jio_url": &cfg.JioURL} {
		if value, ok := raw[key].(string); ok && strings.TrimSpace(value) != "" {
			u, err := url.Parse(strings.TrimSpace(value))
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return cfg, fmt.Errorf("Grizzly %s 地址无效", key)
			}
			*target = strings.TrimRight(u.String(), "/")
		}
	}
	for key, target := range map[string]*float64{"max_price": &cfg.MaxPrice} {
		if value, exists := raw[key]; exists {
			n, err := strconv.ParseFloat(fmt.Sprint(value), 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 100 {
				return cfg, errors.New("Grizzly 号码最高采购价必须大于 0 且不超过 100 USD")
			}
			*target = n
		}
	}
	for key, bounds := range map[string][3]int{"timeout_seconds": {60, 480, 300}, "max_attempts": {1, 20, 5}} {
		n := bounds[2]
		if value, exists := raw[key]; exists {
			parsed, err := strconv.Atoi(fmt.Sprint(value))
			if err != nil || parsed < bounds[0] || parsed > bounds[1] {
				return cfg, fmt.Errorf("Grizzly %s 必须在 %d–%d 之间", key, bounds[0], bounds[1])
			}
			n = parsed
		}
		if key == "timeout_seconds" {
			cfg.Timeout = time.Duration(n) * time.Second
		} else {
			cfg.MaxAttempts = n
		}
	}
	return cfg, nil
}

type GrizzlyJioProvider struct{}

func (*GrizzlyJioProvider) Name() string        { return "grizzly" }
func (*GrizzlyJioProvider) DisplayName() string { return "Grizzly" }

type grizzlyClient struct {
	cfg                                    grizzlyConfig
	http                                   *http.Client
	stockInterval, otpInterval, otpTimeout time.Duration
}

func newGrizzlyClient(cfg grizzlyConfig) *grizzlyClient {
	return &grizzlyClient{cfg: cfg, http: getJioHTTPClient(20 * time.Second), stockInterval: 15 * time.Second, otpInterval: 5 * time.Second, otpTimeout: 180 * time.Second}
}

func (g *grizzlyClient) request(ctx context.Context, action string, params url.Values) (string, error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("api_key", g.cfg.APIKey)
	params.Set("action", action)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.cfg.APIURL+"?"+params.Encode(), nil)
	if err != nil {
		return "", errors.New("Grizzly 请求配置无效")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// net/url errors include the API key in the URL. Never propagate them.
		return "", errors.New("Grizzly 网络请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Grizzly HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", errors.New("Grizzly 响应读取失败")
	}
	return strings.TrimSpace(string(body)), nil
}

func grizzlyAPIError(text string) error {
	// Only known protocol codes may leave the adapter.
	for _, code := range []string{"BAD_KEY", "NO_BALANCE", "NO_NUMBERS", "BAD_SERVICE", "BAD_ACTION", "NO_ACTIVATION", "STATUS_CANCEL", "SERVICE_UNAVAILABLE_REGION", "ERROR_SQL"} {
		if text == code {
			return fmt.Errorf("Grizzly: %s", code)
		}
	}
	return errors.New("Grizzly 返回无效响应")
}

func (p *GrizzlyJioProvider) GetBalance(ctx context.Context) (*SupplierBalance, error) {
	cfg, err := loadGrizzlyConfig(GetSupplierConfig(p.Name()))
	if err != nil {
		return nil, err
	}
	g := newGrizzlyClient(cfg)
	defer g.http.CloseIdleConnections()
	text, err := g.request(ctx, "getBalance", nil)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(text, "ACCESS_BALANCE:") {
		return nil, grizzlyAPIError(text)
	}
	balance, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(text, "ACCESS_BALANCE:")), 64)
	if err != nil || math.IsNaN(balance) || math.IsInf(balance, 0) || balance < 0 {
		return nil, errors.New("Grizzly 余额格式无效")
	}
	return &SupplierBalance{Supported: true, Balance: balance, Currency: "USD"}, nil
}

func (p *GrizzlyJioProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	cfg, err := loadGrizzlyConfig(GetSupplierConfig(p.Name()))
	if err != nil {
		return nil, err
	}
	// maxPrice is a conservative quote, not a claim of live number stock.
	return []SupplierProduct{{ID: grizzlyProductID, Name: "Jio Gemini 自动获取链接（号码成本上限）", PriceUSD: cfg.MaxPrice, DeliveryType: "activation", UnlimitedStock: true, IsActive: true}}, nil
}

func (p *GrizzlyJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	result, err := p.Purchase(ctx, SupplierPurchaseRequest{ProductID: grizzlyProductID, Quantity: 1, CustomerReference: cardSecret})
	if err != nil {
		return "", err
	}
	recordJioPurchaseCost(ctx, result)
	return result.Link, nil
}

func (p *GrizzlyJioProvider) Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error) {
	if req.Quantity > 1 || (req.ProductID != "" && req.ProductID != grizzlyProductID) {
		return nil, errors.New("Grizzly 每次仅支持获取一个 Jio Gemini 链接")
	}
	cfg, err := loadGrizzlyConfig(GetSupplierConfig(p.Name()))
	if err != nil {
		return nil, err
	}
	g := newGrizzlyClient(cfg)
	defer g.http.CloseIdleConnections()
	return g.purchase(ctx)
}

func (*GrizzlyJioProvider) CreateDeposit(context.Context, float64) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{Supported: false}, nil
}
func (*GrizzlyJioProvider) GetDepositStatus(context.Context, string) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{Supported: false}, nil
}

type grizzlyActivation struct {
	ID, Phone string
	Cost      float64
}

var grizzlyNoStock = errors.New("Grizzly 暂无号码")
var grizzlyMobilePattern = regexp.MustCompile(`^[6-9][0-9]{9}$`)
var grizzlyDigits = regexp.MustCompile(`\D`)
var grizzlyOTPPattern = regexp.MustCompile(`\b[0-9]{6}\b`)
var grizzlyURLPattern = regexp.MustCompile(`(?i)https?://serviceactivation\.google\.com/subscription/new/([A-Za-z0-9_-]{50,}={0,2})`)

func (g *grizzlyClient) getNumber(ctx context.Context) (*grizzlyActivation, error) {
	// V2 supplies activationCost. Only an explicit unsupported-action response
	// permits a second purchase request; ambiguous network errors must not retry.
	for _, action := range []string{"getNumberV2", "getNumber"} {
		text, err := g.request(ctx, action, url.Values{"service": {"jio"}, "country": {"22"}, "maxPrice": {strconv.FormatFloat(g.cfg.MaxPrice, 'f', -1, 64)}})
		if err != nil {
			return nil, err
		}
		if text == "BAD_ACTION" && action == "getNumberV2" {
			continue
		}
		if text == "NO_NUMBERS" {
			return nil, grizzlyNoStock
		}
		a := &grizzlyActivation{Cost: g.cfg.MaxPrice}
		if strings.HasPrefix(text, "ACCESS_NUMBER:") {
			parts := strings.Split(text, ":")
			if len(parts) == 3 {
				a.ID, a.Phone = parts[1], parts[2]
			}
		} else {
			var obj map[string]any
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			if decoder.Decode(&obj) == nil {
				if obj["activationId"] != nil {
					a.ID = fmt.Sprint(obj["activationId"])
				}
				if obj["phoneNumber"] != nil {
					a.Phone = fmt.Sprint(obj["phoneNumber"])
				}
				if cost, err := strconv.ParseFloat(fmt.Sprint(obj["activationCost"]), 64); err == nil && cost > 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) {
					a.Cost = cost
				}
			}
		}
		if a.ID == "" {
			return nil, grizzlyAPIError(text)
		}
		return a, nil
	}
	return nil, errors.New("Grizzly 不支持号码获取接口")
}

func grizzlyWait(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (g *grizzlyClient) setStatus(ctx context.Context, id, status string) error {
	text, err := g.request(ctx, "setStatus", url.Values{"id": {id}, "status": {status}})
	if err != nil {
		return err
	}
	if strings.HasPrefix(text, "ACCESS_") || (status == "8" && text == "STATUS_CANCEL") {
		return nil
	}
	return grizzlyAPIError(text)
}

func (g *grizzlyClient) cleanup(id, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := g.setStatus(ctx, id, status); err != nil {
		log.Printf("[Grizzly] activation %s status %s cleanup failed: %v", id, status, err)
	}
}

func (g *grizzlyClient) purchase(parent context.Context) (*SupplierPurchaseResult, error) {
	ctx, cancel := context.WithTimeout(parent, g.cfg.Timeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < g.cfg.MaxAttempts; attempt++ {
		stop := beginJioProgressStage(ctx, 2, 19, 15*time.Second)
		var a *grizzlyActivation
		var err error
		for {
			a, err = g.getNumber(ctx)
			if !errors.Is(err, grizzlyNoStock) {
				break
			}
			if err = grizzlyWait(ctx, g.stockInterval); err != nil {
				break
			}
		}
		stop()
		if err != nil {
			return nil, fmt.Errorf("Grizzly 取号失败: %w", err)
		}
		link, err := g.activate(ctx, a)
		if err == nil {
			return &SupplierPurchaseResult{OrderID: a.ID, Link: link, Items: []string{link}, AmountUSD: a.Cost, Status: "COMPLETED"}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		if attempt+1 < g.cfg.MaxAttempts {
			if grizzlyWait(ctx, 3*time.Second) != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return nil, errors.New("Grizzly 获取链接超时，请稍后重试")
	}
	return nil, fmt.Errorf("Grizzly 已达到号码尝试上限: %w", lastErr)
}

func (g *grizzlyClient) activate(ctx context.Context, a *grizzlyActivation) (link string, err error) {
	defer func() {
		if link == "" {
			g.cleanup(a.ID, "8")
		} else {
			g.cleanup(a.ID, "6")
		}
	}()
	mobile := grizzlyDigits.ReplaceAllString(a.Phone, "")
	if len(mobile) == 12 && strings.HasPrefix(mobile, "91") {
		mobile = mobile[2:]
	}
	if !grizzlyMobilePattern.MatchString(mobile) {
		return "", errors.New("Grizzly 返回的号码不是有效印度号码")
	}
	// Never share cookies across numbers, customers or retries.
	jar, _ := cookiejar.New(nil)
	session := *g.http
	session.Jar = jar
	stop := beginJioProgressStage(ctx, 20, 26, 5*time.Second)
	defer func() { stop() }()
	if err := g.setStatus(ctx, a.ID, "1"); err != nil {
		return "", err
	}
	data, err := g.jioRequest(ctx, &session, "GET", "/api/jio-recharge-service/recharge/mobility/number/"+mobile, "", nil)
	if err != nil {
		return "", err
	}
	if value := data["primaryService"]; value == nil || value == "" || value == false {
		return "", errors.New("号码未通过 Jio 校验")
	}
	_, err = g.jioRequest(ctx, &session, "POST", "/api/jio-login-service/login/sendOtp", "", map[string]string{"mobileNumber": mobile, "loginFlowType": "MOBILE", "alternateNumber": ""})
	if err != nil {
		return "", err
	}
	stop()
	stop = beginJioProgressStage(ctx, 27, 94, 55*time.Second)
	otp, err := g.pollOTP(ctx, a.ID)
	if err != nil {
		return "", err
	}
	stop()
	stop = beginJioProgressStage(ctx, 95, 99, 3*time.Second)
	_, err = g.jioRequest(ctx, &session, "POST", "/api/jio-login-service/login/validateOtp", "", map[string]string{"mobileNumber": mobile, "otp": otp})
	if err != nil {
		return "", err
	}
	dashboard := g.cfg.JioURL + "/selfcare/dashboard/"
	data, err = g.jioRequest(ctx, &session, "GET", "/api/jio-authenticate-service/authenticate/authJsonData", dashboard, nil)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(fmt.Sprint(data["loginFlag"]), "true") {
		return "", errors.New("Jio 会话验证失败")
	}
	// navigate and submit are best-effort steps in the original script.
	_, _ = g.jioRequest(ctx, &session, "GET", "/api/jio-ott-service/ott/subscription/navigate/Z0241", dashboard, nil)
	referer := g.cfg.JioURL + "/selfcare/googleai/?header=no&type=Z0241&source=JIO"
	_, err = g.jioRequest(ctx, &session, "GET", "/api/jio-ott-service/ott/subscription/activate/Z0241?source=JIO", referer, nil)
	if err != nil {
		return "", err
	}
	data, err = g.jioRequest(ctx, &session, "GET", "/api/jio-ott-service/ott/subscription/google-ai", referer, nil)
	if err != nil {
		return "", err
	}
	link = extractGrizzlyActivationURL(fmt.Sprint(data["redirectionURL"]))
	if link == "" {
		return "", errors.New("Jio 未返回有效 Google 激活链接")
	}
	_, _ = g.jioRequest(ctx, &session, "GET", "/api/jio-ott-service/ott/submission/submit", referer, nil)
	return link, nil
}

func (g *grizzlyClient) pollOTP(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.otpTimeout)
	defer cancel()
	for {
		text, err := g.request(ctx, "getStatus", url.Values{"id": {id}})
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(text, "STATUS_OK:") || strings.HasPrefix(text, "STATUS_WAIT_RETRY:") {
			if otp := grizzlyOTPPattern.FindString(strings.SplitN(text, ":", 2)[1]); otp != "" {
				return otp, nil
			}
		} else if text != "STATUS_WAIT_CODE" && text != "STATUS_WAIT_RESEND" {
			// Some compatible endpoints return the SMS body directly.
			if !strings.HasPrefix(text, "STATUS_") {
				if otp := grizzlyOTPPattern.FindString(text); otp != "" {
					return otp, nil
				}
			}
			return "", grizzlyAPIError(text)
		}
		if err := grizzlyWait(ctx, g.otpInterval); err != nil {
			return "", errors.New("等待 Jio 验证码超时")
		}
	}
}

func (g *grizzlyClient) jioRequest(ctx context.Context, session *http.Client, method, path, referer string, payload any) (map[string]any, error) {
	var body io.Reader
	if payload != nil {
		data, _ := json.Marshal(payload)
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.JioURL+path, body)
	if err != nil {
		return nil, errors.New("Jio 请求配置无效")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/120 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", g.cfg.JioURL)
	if referer == "" {
		referer = g.cfg.JioURL + "/selfcare/login/"
	}
	req.Header.Set("Referer", referer)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := session.Do(req)
	if err != nil {
		return nil, errors.New("Jio 网络请求失败")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Jio HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data) != nil || data == nil {
		return nil, errors.New("Jio 返回无效响应")
	}
	if value := data["error"]; value != nil && value != false && value != "" && value != float64(0) {
		return nil, errors.New("Jio 校验失败")
	}
	if value := data["errorMessage"]; value != nil && value != "" {
		return nil, errors.New("Jio 校验失败")
	}
	if code := data["errorCode"]; code != nil && fmt.Sprint(code) != "200" && fmt.Sprint(code) != "0" {
		return nil, errors.New("Jio 业务校验失败")
	}
	switch strings.ToLower(fmt.Sprint(data["status"])) {
	case "failed", "failure", "error", "false":
		return nil, errors.New("Jio 请求未成功")
	}
	for _, key := range []string{"message", "responseMessage", "responseMsg"} {
		message := strings.ToLower(strings.ReplaceAll(fmt.Sprint(data[key]), "_", " "))
		for _, phrase := range []string{"already active", "already activated", "already redeemed", "already claimed", "already availed"} {
			if strings.Contains(message, phrase) {
				return nil, errors.New("该号码的 Jio 优惠已被领取")
			}
		}
	}
	return data, nil
}

func extractGrizzlyActivationURL(value string) string {
	value = html.UnescapeString(value)
	for i := 0; i < 4; i++ {
		decoded, err := url.PathUnescape(value)
		if err != nil || decoded == value {
			break
		}
		value = decoded
	}
	match := grizzlyURLPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return ""
	}
	return "https://serviceactivation.google.com/subscription/new/" + match[1]
}
