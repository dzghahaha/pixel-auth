package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SupplierBalance 供应商账户余额信息 (上游充值余额)
type SupplierBalance struct {
	Supported bool    `json:"supported"`          // 是否支持在线查询余额
	Balance   float64 `json:"balance"`            // 账户可用余额 (通常为 USD)
	Currency  string  `json:"currency"`           // 货币单位 (如 USD)
	Username  string  `json:"username,omitempty"` // 供应商侧商户账号或用户名
	KeyName   string  `json:"key_name,omitempty"` // API Key 备注标识
	Status    string  `json:"status,omitempty"`   // 状态提示或说明
	RawData   any     `json:"raw_data,omitempty"`
}

// SupplierProduct 供应商提供的商品/激活服务项 (Gemini 兑换链接等)
type SupplierProduct struct {
	ID            string  `json:"id"`                       // 商品/服务唯一标识
	Name          string  `json:"name"`                     // 商品名称
	PriceUSD      float64 `json:"price_usd"`                // 采购成本 (USD)
	StandardPrice float64 `json:"standard_price,omitempty"` // 原价/标准价
	PricingType   string  `json:"pricing_type,omitempty"`   // standard, reseller_special
	DeliveryType  string  `json:"delivery_type,omitempty"`  // activation, stock, supplier_api
	Stock         *int    `json:"stock,omitempty"`          // 当前库存数 (为 nil 表示无限或未知)
	IsActive      bool    `json:"is_active"`                // 是否在线可用
	RawData       any     `json:"raw_data,omitempty"`
}

// SupplierPurchaseRequest 购买下单请求参数
type SupplierPurchaseRequest struct {
	ProductID            string `json:"product_id"`                      // 商品标识或产品 ID
	Quantity             int    `json:"quantity"`                        // 购买数量 (默认 1)
	ActivationIdentifier string `json:"activation_identifier,omitempty"` // 激活账号/邮箱/用户标识
	CustomerReference    string `json:"customer_reference,omitempty"`    // 内部订单编号或关联标识
	IdempotencyKey       string `json:"idempotency_key,omitempty"`       // 幂等键
}

// SupplierPurchaseResult 购买下单结果
type SupplierPurchaseResult struct {
	OrderID      string   `json:"order_id"`                // 上游订单号
	Link         string   `json:"link"`                    // 最终交付的兑换链接或卡券内容
	Items        []string `json:"items,omitempty"`         // 交付条目明细
	AmountUSD    float64  `json:"amount_usd"`              // 订单扣费金额
	BalanceAfter *float64 `json:"balance_after,omitempty"` // 交易后供应商账户剩余余额
	Status       string   `json:"status"`                  // 订单状态 (COMPLETED, PENDING 等)
	RawData      any      `json:"raw_data,omitempty"`
}

// SupplierDepositResult 自助充值单结果 (如 USDT BEP20 充值)
type SupplierDepositResult struct {
	Supported   bool    `json:"supported"`              // 是否支持自助充值
	DepositID   string  `json:"deposit_id"`             // 充值记录单号
	Status      string  `json:"status"`                 // WAITING, CREDITED, FAILED 等
	Network     string  `json:"network"`                // 充值公链网络 (如 BEP20)
	Address     string  `json:"address"`                // 收款充值钱包地址
	PayAmount   float64 `json:"pay_amount"`             // 应支付金额
	PayCurrency string  `json:"pay_currency"`           // 支付币种 (如 USDTBSC)
	ExpiresAt   string  `json:"expires_at,omitempty"`   // 订单有效截止时间
	QRCodeData  string  `json:"qr_code_data,omitempty"` // 收款二维码展示数据
	RawData     any     `json:"raw_data,omitempty"`
}

// JioProvider 定义第三方 Jio 订阅兑换链接获取与综合供应商适配器接口
type JioProvider interface {
	// Name 返回该 Provider 的唯一标识（用于枚举配置）
	Name() string
	// DisplayName 返回界面的友好展示名称
	DisplayName() string
	// GetOfferLink C 端卡密兑换时统一调度的链接获取入口
	GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (link string, err error)
	// GetBalance 查询该供应商账户当前储值余额
	GetBalance(ctx context.Context) (*SupplierBalance, error)
	// GetProducts 查询该供应商当前可用商品列表及 Gemini 链接价格
	GetProducts(ctx context.Context) ([]SupplierProduct, error)
	// Purchase 购买下单接口 (支持直接测试购买并提取交付链接)
	Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error)
	// CreateDeposit 创建自主充值订单 (如 USDT BEP20，不支持则返回 Supported: false)
	CreateDeposit(ctx context.Context, amountUSD float64) (*SupplierDepositResult, error)
	// GetDepositStatus 刷新并查询充值订单入账状态
	GetDepositStatus(ctx context.Context, depositID string) (*SupplierDepositResult, error)
}

var (
	jioProvidersMu sync.RWMutex
	jioProviders   = make(map[string]JioProvider)
)

// RegisterJioProvider 注册第三方 Jio Provider
func RegisterJioProvider(p JioProvider) {
	jioProvidersMu.Lock()
	defer jioProvidersMu.Unlock()
	jioProviders[strings.ToLower(p.Name())] = p
}

// GetJioProvider 根据 Provider 标识获取实例
func GetJioProvider(name string) (JioProvider, error) {
	jioProvidersMu.RLock()
	defer jioProvidersMu.RUnlock()
	p, ok := jioProviders[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("未找到标识为 '%s' 的 Jio 供应商实现", name)
	}
	return p, nil
}

// GetAllJioProviders 获取系统已注册的所有正式 Jio 供应商实例列表
func GetAllJioProviders() []JioProvider {
	jioProvidersMu.RLock()
	defer jioProvidersMu.RUnlock()
	// 按照正式商用顺序返回: vente, acczone, 其他真实提供商
	order := []string{"vente", "acczone"}
	seen := make(map[string]bool)
	var list []JioProvider

	for _, k := range order {
		if p, ok := jioProviders[k]; ok {
			list = append(list, p)
			seen[k] = true
		}
	}
	for k, p := range jioProviders {
		// 彻底排除测试用的 mock 供应商
		if !seen[k] && strings.ToLower(k) != "mock" {
			list = append(list, p)
		}
	}
	return list
}

// GetActiveJioProvider 获取当前系统设置中生效的 Jio Provider（默认降级为 vente 或 acczone）
func GetActiveJioProvider() JioProvider {
	activeName := strings.ToLower(strings.TrimSpace(getSetting("jio_active_provider", "vente")))
	p, err := GetJioProvider(activeName)
	if err == nil && p != nil && p.Name() != "mock" {
		return p
	}
	// Fallback to default vente or acczone provider
	if fallback, ok := jioProviders["vente"]; ok {
		return fallback
	}
	if fallback, ok := jioProviders["acczone"]; ok {
		return fallback
	}
	return &VenteJioProvider{}
}

// =========================================================================
// Jio 供应商交付调度策略 (Dispatch Strategy)
// =========================================================================

const (
	StrategySpecific       = "specific"         // 1. 指定供应商
	StrategyAutoLowestCost = "auto_lowest_cost" // 2. 自动选择 (优先价格低-库存足够)
)

// GetJioDispatchStrategy 获取当前系统选择策略
func GetJioDispatchStrategy() string {
	st := strings.ToLower(strings.TrimSpace(getSetting("jio_dispatch_strategy", StrategySpecific)))
	if st != StrategyAutoLowestCost {
		return StrategySpecific
	}
	return StrategyAutoLowestCost
}

// CandidateSupplier 评估后的候选供应商
type CandidateSupplier struct {
	Provider    JioProvider `json:"-"`
	Name        string      `json:"name"`
	DisplayName string      `json:"display_name"`
	PriceUSD    float64     `json:"price_usd"`
	Stock       *int        `json:"stock"`
	Available   bool        `json:"available"`
	ProductID   string      `json:"product_id"`
	Reason      string      `json:"reason"`
}

var autoLowestCostCache struct {
	sync.RWMutex
	lastEvaluated time.Time
	candidates    []CandidateSupplier
}

// EvaluateEligibleSuppliers 并发评估所有供应商候选（筛选库存充足并按价格从小到大排序）
func EvaluateEligibleSuppliers(ctx context.Context, forceRefresh bool) ([]CandidateSupplier, error) {
	if !forceRefresh {
		autoLowestCostCache.RLock()
		if time.Since(autoLowestCostCache.lastEvaluated) < 25*time.Second && len(autoLowestCostCache.candidates) > 0 {
			cached := make([]CandidateSupplier, len(autoLowestCostCache.candidates))
			copy(cached, autoLowestCostCache.candidates)
			autoLowestCostCache.RUnlock()
			return cached, nil
		}
		autoLowestCostCache.RUnlock()
	}

	allProviders := GetAllJioProviders()
	var candidates []CandidateSupplier
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range allProviders {
		wg.Add(1)
		go func(prov JioProvider) {
			defer wg.Done()
			subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()

			prods, err := prov.GetProducts(subCtx)
			mu.Lock()
			defer mu.Unlock()

			cand := CandidateSupplier{
				Provider:    prov,
				Name:        strings.ToLower(prov.Name()),
				DisplayName: prov.DisplayName(),
				Available:   false,
			}

			if err != nil {
				cand.Reason = fmt.Sprintf("获取商品异常: %v", err)
				candidates = append(candidates, cand)
				return
			}

			if len(prods) == 0 {
				cand.Reason = "商品列表为空"
				candidates = append(candidates, cand)
				return
			}

			var geminiProd *SupplierProduct
			for i := range prods {
				if strings.Contains(strings.ToLower(prods[i].Name), "gemini") {
					geminiProd = &prods[i]
					break
				}
			}
			if geminiProd == nil {
				geminiProd = &prods[0]
			}

			cand.PriceUSD = geminiProd.PriceUSD
			cand.Stock = geminiProd.Stock
			cand.ProductID = geminiProd.ID

			hasStock := true
			if geminiProd.Stock != nil && *geminiProd.Stock <= 0 {
				hasStock = false
			}

			if !geminiProd.IsActive {
				cand.Reason = "商品未启用"
				candidates = append(candidates, cand)
				return
			}

			if !hasStock {
				cand.Reason = "上游库存已售罄"
				candidates = append(candidates, cand)
				return
			}

			cand.Available = true
			cand.Reason = "库存充足就绪"
			candidates = append(candidates, cand)
		}(p)
	}

	wg.Wait()

	// 排序：优先 Available=true，且 PriceUSD 升序排列
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Available != candidates[j].Available {
			return candidates[i].Available
		}
		return candidates[i].PriceUSD < candidates[j].PriceUSD
	})

	autoLowestCostCache.Lock()
	autoLowestCostCache.lastEvaluated = time.Now()
	autoLowestCostCache.candidates = make([]CandidateSupplier, len(candidates))
	copy(autoLowestCostCache.candidates, candidates)
	autoLowestCostCache.Unlock()

	return candidates, nil
}

// SelectJioProviderForOffer 按照当前系统调度策略（指定或自动最低价库存足）为 C 端兑换调度获取优惠链接
func SelectJioProviderForOffer(ctx context.Context, cardSecret, vendorKey string) (link string, chosenName string, err error) {
	strategy := GetJioDispatchStrategy()

	// 1. 指定供应商模式
	if strategy == StrategySpecific {
		p := GetActiveJioProvider()
		chosenName = p.Name()
		l, e := p.GetOfferLink(ctx, cardSecret, vendorKey)
		return l, chosenName, e
	}

	// 2. 自动选择模式 (优先价格低-库存足够)
	candidates, errEval := EvaluateEligibleSuppliers(ctx, false)
	if errEval != nil || len(candidates) == 0 {
		p := GetActiveJioProvider()
		l, e := p.GetOfferLink(ctx, cardSecret, vendorKey)
		return l, p.Name(), e
	}

	var lastErr error
	for _, cand := range candidates {
		if !cand.Available {
			continue
		}

		link, err := cand.Provider.GetOfferLink(ctx, cardSecret, vendorKey)
		if err == nil && link != "" {
			if cand.PriceUSD > 0 {
				updateJioCachedCost(cand.PriceUSD, time.Now().Format("2006-01-02 15:04:05"))
			}
			return link, cand.Name, nil
		}

		lastErr = err
		fmt.Printf("[JioDispatch] 候选渠道 %s 下单失败，容灾尝试下一个候选: %v\n", cand.Name, err)
	}

	fallback := GetActiveJioProvider()
	linkFallback, errFallback := fallback.GetOfferLink(ctx, cardSecret, vendorKey)
	if errFallback == nil {
		return linkFallback, fallback.Name(), nil
	}

	if lastErr != nil {
		return "", fallback.Name(), lastErr
	}
	return "", fallback.Name(), errFallback
}

// =========================================================================
// 多供应商配置持久化管理器
// =========================================================================

// GetAllSuppliersConfig 读取所有供应商的配置集合 map[providerName]configMap
func GetAllSuppliersConfig() map[string]map[string]interface{} {
	result := make(map[string]map[string]interface{})

	raw := strings.TrimSpace(getSetting("jio_suppliers_config", "{}"))
	if raw != "" && raw != "{}" {
		_ = json.Unmarshal([]byte(raw), &result)
	}

	// 兼容旧版单一配置: jio_provider_config
	oldConfigJSON := strings.TrimSpace(getSetting("jio_provider_config", "{}"))
	if oldConfigJSON != "" && oldConfigJSON != "{}" {
		var oldMap map[string]interface{}
		if err := json.Unmarshal([]byte(oldConfigJSON), &oldMap); err == nil && len(oldMap) > 0 {
			active := strings.ToLower(strings.TrimSpace(getSetting("jio_active_provider", "acczone")))
			if _, exists := result[active]; !exists {
				result[active] = oldMap
			}
			if _, exists := result["acczone"]; !exists {
				// 旧版通常就是 acczone 的参数
				result["acczone"] = oldMap
			}
		}
	}

	return result
}

// GetSupplierConfig 获取特定供应商的配置项 map
func GetSupplierConfig(providerName string) map[string]interface{} {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	all := GetAllSuppliersConfig()
	if cfg, ok := all[providerName]; ok && cfg != nil {
		return cfg
	}
	return make(map[string]interface{})
}

// SaveSupplierConfig 保存单个供应商的配置
func SaveSupplierConfig(providerName string, config map[string]interface{}) error {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	if providerName == "" {
		return errors.New("供应商名称不能为空")
	}

	all := GetAllSuppliersConfig()
	all[providerName] = config

	data, err := json.Marshal(all)
	if err != nil {
		return fmt.Errorf("序列化多供应商配置失败: %w", err)
	}

	rawJSON := string(data)
	_, errDB := db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('jio_suppliers_config', ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()", rawJSON, rawJSON)
	if errDB != nil {
		return fmt.Errorf("保存供应商配置至数据库失败: %w", errDB)
	}

	// 兼容处理：如果是当前激活的 Provider，同时同步一份到 jio_provider_config
	active := strings.ToLower(strings.TrimSpace(getSetting("jio_active_provider", "mock")))
	if active == providerName || providerName == "acczone" {
		singleData, _ := json.Marshal(config)
		singleJSON := string(singleData)
		_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('jio_provider_config', ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()", singleJSON, singleJSON)
	}

	return nil
}

// =========================================================================
// 1. Mock 模拟供应商实现 (MockJioProvider)
// =========================================================================

// MockJioProvider 测试环境模拟供应商
type MockJioProvider struct{}

func (m *MockJioProvider) Name() string {
	return "mock"
}

func (m *MockJioProvider) DisplayName() string {
	return "测试模拟供应商 (Mock Provider)"
}

func (m *MockJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	if strings.TrimSpace(cardSecret) == "" {
		return "", errors.New("卡密不能为空")
	}

	cleanCard := strings.ToUpper(strings.TrimSpace(cardSecret))
	if len(cleanCard) > 6 {
		cleanCard = cleanCard[:6]
	}

	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Sprintf("https://one.google.com/promo/hasoffer?token=JIO-OFFER-%s", cleanCard), nil
	}
	randomToken := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("https://one.google.com/promo/hasoffer?token=JIO-%s-%s", cleanCard, strings.ToUpper(randomToken)), nil
}

func (m *MockJioProvider) GetBalance(ctx context.Context) (*SupplierBalance, error) {
	return &SupplierBalance{
		Supported: true,
		Balance:   9999.00,
		Currency:  "USD",
		Username:  "mock_tester",
		KeyName:   "Mock Internal Test Key",
		Status:    "模拟账户运行正常 (无限制可用)",
	}, nil
}

func (m *MockJioProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	stock := 999
	return []SupplierProduct{
		{
			ID:            "mock_gemini",
			Name:          "Gemini Advanced 1 Month (Mock Link)",
			PriceUSD:      0.00,
			StandardPrice: 0.00,
			PricingType:   "standard",
			DeliveryType:  "stock",
			Stock:         &stock,
			IsActive:      true,
		},
	}, nil
}

func (m *MockJioProvider) Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error) {
	link, err := m.GetOfferLink(ctx, "MOCK-"+strconv.FormatInt(time.Now().Unix(), 10), "")
	if err != nil {
		return nil, err
	}
	remBalance := 9999.00
	return &SupplierPurchaseResult{
		OrderID:      fmt.Sprintf("MOCK-ORD-%d", time.Now().Unix()),
		Link:         link,
		Items:        []string{link},
		AmountUSD:    0.00,
		BalanceAfter: &remBalance,
		Status:       "COMPLETED",
	}, nil
}

func (m *MockJioProvider) CreateDeposit(ctx context.Context, amountUSD float64) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{
		Supported:   true,
		DepositID:   fmt.Sprintf("mock_dep_%d", time.Now().Unix()),
		Status:      "CREDITED",
		Network:     "BEP20",
		Address:     "0x000000000000000000000000000000000000dEaD",
		PayAmount:   amountUSD,
		PayCurrency: "USDTBSC",
		ExpiresAt:   time.Now().Add(24 * time.Hour).Format("2006-01-02 15:04:05"),
	}, nil
}

func (m *MockJioProvider) GetDepositStatus(ctx context.Context, depositID string) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{
		Supported:   true,
		DepositID:   depositID,
		Status:      "CREDITED",
		Network:     "BEP20",
		Address:     "0x000000000000000000000000000000000000dEaD",
		PayAmount:   10.0,
		PayCurrency: "USDTBSC",
	}, nil
}

// =========================================================================
// 2. Acczone 供应商实现 (AcczoneJioProvider)
// =========================================================================

// AcczoneJioProvider 接入 api.acczone.xyz 第三方卡券采购平台
type AcczoneJioProvider struct {
	client *http.Client
}

type AcczoneCpnItem struct {
	ID            int64       `json:"id"`
	ServiceKey    string      `json:"service_key"`
	CodeType      string      `json:"code_type"`
	CodeValue     string      `json:"code_value"`
	IsUsed        int         `json:"is_used"`
	UsedBy        interface{} `json:"used_by"`
	UsedAt        string      `json:"used_at"`
	ExtractedCode interface{} `json:"extracted_code"`
}

func (a *AcczoneJioProvider) Name() string {
	return "acczone"
}

func (a *AcczoneJioProvider) DisplayName() string {
	return "Acczone 采购平台 (api.acczone.xyz)"
}

func (a *AcczoneJioProvider) getEffectiveConfig() (apiKey, serviceKey, apiURL string) {
	cfg := GetSupplierConfig("acczone")
	if val, ok := cfg["apikey"].(string); ok {
		apiKey = strings.TrimSpace(val)
	}
	if val, ok := cfg["service_key"].(string); ok {
		serviceKey = strings.TrimSpace(val)
	}
	if val, ok := cfg["api_url"].(string); ok {
		apiURL = strings.TrimSpace(val)
	}

	// 兼容旧版 jio_provider_config
	if apiKey == "" || serviceKey == "" {
		oldJSON := getSetting("jio_provider_config", "{}")
		var oldCfg struct {
			APIKey     string `json:"apikey"`
			ServiceKey string `json:"service_key"`
			APIURL     string `json:"api_url"`
		}
		_ = json.Unmarshal([]byte(oldJSON), &oldCfg)
		if apiKey == "" {
			apiKey = strings.TrimSpace(oldCfg.APIKey)
		}
		if serviceKey == "" {
			serviceKey = strings.TrimSpace(oldCfg.ServiceKey)
		}
		if apiURL == "" {
			apiURL = strings.TrimSpace(oldCfg.APIURL)
		}
	}

	if serviceKey == "" {
		serviceKey = "gemini"
	}
	if apiURL == "" {
		apiURL = "https://api.acczone.xyz/buyCpn"
	}
	return
}

func (a *AcczoneJioProvider) GetBalance(ctx context.Context) (*SupplierBalance, error) {
	apiKey, _, apiURL := a.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Acczone 的 apikey，请在供应商配置中设置")
	}

	balanceURL := "https://api.acczone.xyz/getBalance"
	if apiURL != "" {
		if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
			balanceURL = fmt.Sprintf("%s://%s/getBalance", u.Scheme, u.Host)
		}
	}

	reqURL := fmt.Sprintf("%s?apikey=%s", balanceURL, url.QueryEscape(apiKey))
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建 Acczone 余额请求失败: %w", errReq)
	}
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	client := a.client
	if client == nil {
		client = getJioHTTPClient(20 * time.Second)
	}

	resp, errResp := client.Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Acczone 余额接口网络失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Acczone 余额响应失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		var errObj map[string]interface{}
		_ = json.Unmarshal(bodyBytes, &errObj)
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return nil, fmt.Errorf("Acczone 提示 (HTTP %d): %s", resp.StatusCode, msg)
		}
		return nil, fmt.Errorf("Acczone 余额接口返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var accBalResp struct {
		UserID     int64   `json:"user_id"`
		Username   string  `json:"username"`
		FirstName  string  `json:"first_name"`
		Balance    float64 `json:"balance"`
		CreatedAt  string  `json:"created_at"`
		VerifiedAt string  `json:"verified_at"`
		APIKey     string  `json:"apikey"`
	}

	if errJSON := json.Unmarshal(bodyBytes, &accBalResp); errJSON != nil {
		return nil, fmt.Errorf("解析 Acczone 余额响应失败: %w", errJSON)
	}

	uname := accBalResp.Username
	if uname == "" && accBalResp.FirstName != "" {
		uname = accBalResp.FirstName
	} else if uname != "" && accBalResp.FirstName != "" && uname != accBalResp.FirstName {
		uname = fmt.Sprintf("%s (%s)", uname, accBalResp.FirstName)
	}

	return &SupplierBalance{
		Supported: true,
		Balance:   accBalResp.Balance,
		Currency:  "USD",
		Username:  uname,
		KeyName:   maskSecret(accBalResp.APIKey),
		Status:    "正常在线",
		RawData:   accBalResp,
	}, nil
}

func (a *AcczoneJioProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	services, err := FetchAcczoneServices(ctx)
	if err != nil {
		return nil, err
	}

	var products []SupplierProduct
	for _, s := range services {
		products = append(products, SupplierProduct{
			ID:       s.Key,
			Name:     s.Name,
			PriceUSD: s.Price,
			IsActive: s.IsActive == 1,
			RawData:  s,
		})
	}
	return products, nil
}

func (a *AcczoneJioProvider) Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error) {
	apiKey, defaultServiceKey, apiURL := a.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Acczone 的 apikey，请在供应商配置中填写")
	}

	targetServiceKey := strings.TrimSpace(req.ProductID)
	if targetServiceKey == "" {
		targetServiceKey = defaultServiceKey
	}

	qty := req.Quantity
	if qty <= 0 {
		qty = 1
	}

	reqURL := fmt.Sprintf("%s?apikey=%s&service_key=%s&quantity=%d",
		apiURL, url.QueryEscape(apiKey), url.QueryEscape(targetServiceKey), qty)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建 Acczone 请求失败: %w", errReq)
	}
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	client := a.client
	if client == nil {
		client = getJioHTTPClient(20 * time.Second)
	}

	resp, errResp := client.Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Acczone 接口网络失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Acczone 响应失败: %w", errRead)
	}

	bodyStr := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Acczone 返回 HTTP %d: %s", resp.StatusCode, bodyStr)
	}

	var items []AcczoneCpnItem
	if errArr := json.Unmarshal(bodyBytes, &items); errArr == nil {
		if len(items) == 0 {
			return nil, errors.New("Acczone 返回空卡券列表，库存可能已售罄")
		}
		var links []string
		for _, item := range items {
			val := strings.TrimSpace(item.CodeValue)
			if val != "" {
				links = append(links, val)
			}
		}
		if len(links) == 0 {
			return nil, errors.New("Acczone 返回的卡券内容为空")
		}
		return &SupplierPurchaseResult{
			OrderID:   fmt.Sprintf("%d", items[0].ID),
			Link:      links[0],
			Items:     links,
			Status:    "COMPLETED",
			RawData:   items,
		}, nil
	}

	// 尝试解析错误响应
	var errObj map[string]interface{}
	if errMap := json.Unmarshal(bodyBytes, &errObj); errMap == nil {
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return nil, fmt.Errorf("Acczone 提示: %s", msg)
		}
		if errVal, ok := errObj["error"].(string); ok && errVal != "" {
			return nil, fmt.Errorf("Acczone 提示: %s", errVal)
		}
		if detail, ok := errObj["detail"].(string); ok && detail != "" {
			return nil, fmt.Errorf("Acczone 提示: %s", detail)
		}
	}

	return nil, fmt.Errorf("解析 Acczone 响应失败: %s", bodyStr)
}

func (a *AcczoneJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	apiKey, _, _ := a.getEffectiveConfig()
	if apiKey == "" && vendorKey != "" {
		apiKey = vendorKey
	}
	if apiKey == "" {
		return "", errors.New("未配置 Acczone 的 apikey，请在管理后台「系统设置」或「Jio 供应商管理」中填写 apikey")
	}

	res, err := a.Purchase(ctx, SupplierPurchaseRequest{
		Quantity: 1,
	})
	if err != nil {
		return "", err
	}
	return res.Link, nil
}

func (a *AcczoneJioProvider) CreateDeposit(ctx context.Context, amountUSD float64) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{
		Supported: false,
	}, errors.New("Acczone 暂未开放自动充值接口，请登录 Acczone 官网进行充值")
}

func (a *AcczoneJioProvider) GetDepositStatus(ctx context.Context, depositID string) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{
		Supported: false,
	}, errors.New("Acczone 暂未开放自动充值接口")
}

// =========================================================================
// 3. Vente 供应商实现 (VenteJioProvider) - 基于 OpenAPI 3.0.3 规范对接
// =========================================================================

// VenteJioProvider 接入 VenteBot Reseller API 供应商
type VenteJioProvider struct {
	client *http.Client
}

func (v *VenteJioProvider) Name() string {
	return "vente"
}

func (v *VenteJioProvider) DisplayName() string {
	return "Vente 采购平台 (VenteBot Reseller API)"
}

// getEffectiveConfig 读取 Vente 供应商当前有效配置
func (v *VenteJioProvider) getEffectiveConfig() (apiKey, baseURL, productID string) {
	cfg := GetSupplierConfig("vente")
	if val, ok := cfg["api_key"].(string); ok {
		apiKey = strings.TrimSpace(val)
	}
	if apiKey == "" {
		if val, ok := cfg["apikey"].(string); ok {
			apiKey = strings.TrimSpace(val)
		}
	}
	if val, ok := cfg["base_url"].(string); ok {
		baseURL = strings.TrimSpace(val)
	}
	if val, ok := cfg["product_id"].(string); ok {
		productID = strings.TrimSpace(val)
	} else if valFloat, ok := cfg["product_id"].(float64); ok {
		productID = strconv.FormatInt(int64(valFloat), 10)
	}

	if baseURL == "" {
		baseURL = "https://api.ventebot.com"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return
}

func (v *VenteJioProvider) getClient() *http.Client {
	if v.client != nil {
		return v.client
	}
	return getJioHTTPClient(25 * time.Second)
}

// GetBalance 查询 Vente 账户及储值钱包余额: GET /api/reseller/me
func (v *VenteJioProvider) GetBalance(ctx context.Context) (*SupplierBalance, error) {
	apiKey, baseURL, _ := v.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Vente 的 api_key，请在「Jio 供应商管理」中设置")
	}

	reqURL := baseURL + "/api/reseller/me"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建 Vente 请求失败: %w", errReq)
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Vente 接口失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Vente 响应失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(bodyBytes, &errResp)
		if errResp.Message != "" {
			return nil, fmt.Errorf("Vente 报错 (HTTP %d, %s): %s", resp.StatusCode, errResp.Code, errResp.Message)
		}
		return nil, fmt.Errorf("Vente 返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var meResp struct {
		Success       bool    `json:"success"`
		UserTelegramID int64   `json:"user_telegram_id"`
		Username      *string `json:"username"`
		FirstName     *string `json:"first_name"`
		WalletBalance float64 `json:"wallet_balance"`
		KeyName       string  `json:"key_name"`
		KeyPrefix     string  `json:"key_prefix"`
	}
	if errJSON := json.Unmarshal(bodyBytes, &meResp); errJSON != nil {
		return nil, fmt.Errorf("解析 Vente 用户余额响应失败: %w", errJSON)
	}

	uname := ""
	if meResp.Username != nil {
		uname = *meResp.Username
	}

	return &SupplierBalance{
		Supported: true,
		Balance:   meResp.WalletBalance,
		Currency:  "USD",
		Username:  uname,
		KeyName:   meResp.KeyName,
		Status:    "正常在线",
		RawData:   meResp,
	}, nil
}

// GetProducts 获取 Vente 产品目录及 Gemini 链接价格: GET /api/reseller/products
func (v *VenteJioProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	apiKey, baseURL, _ := v.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Vente 的 api_key，请在「Jio 供应商管理」中设置")
	}

	reqURL := baseURL + "/api/reseller/products?lang=zh"
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建 Vente 请求失败: %w", errReq)
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Vente 产品列表失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Vente 响应失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Vente 产品接口返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var catResp struct {
		Success  bool `json:"success"`
		Products []struct {
			ID               int      `json:"id"`
			Name             string   `json:"name"`
			Description      string   `json:"description"`
			PriceUSD         float64  `json:"price_usd"`
			StandardPriceUSD *float64 `json:"standard_price_usd"`
			PricingType      string   `json:"pricing_type"`
			DeliveryType     string   `json:"delivery_type"`
			Stock            *int     `json:"stock"`
			APITest          bool     `json:"api_test"`
		} `json:"products"`
	}

	if errJSON := json.Unmarshal(bodyBytes, &catResp); errJSON != nil {
		return nil, fmt.Errorf("解析 Vente 产品列表失败: %w", errJSON)
	}

	var results []SupplierProduct
	for _, p := range catResp.Products {
		stdPrice := p.PriceUSD
		if p.StandardPriceUSD != nil {
			stdPrice = *p.StandardPriceUSD
		}
		results = append(results, SupplierProduct{
			ID:            strconv.Itoa(p.ID),
			Name:          p.Name,
			PriceUSD:      p.PriceUSD,
			StandardPrice: stdPrice,
			PricingType:   p.PricingType,
			DeliveryType:  p.DeliveryType,
			Stock:         p.Stock,
			IsActive:      true,
			RawData:       p,
		})
	}
	return results, nil
}

// Purchase 购买下单接口: POST /api/reseller/orders
func (v *VenteJioProvider) Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error) {
	apiKey, baseURL, defaultProductID := v.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Vente 的 api_key")
	}

	productIDStr := strings.TrimSpace(req.ProductID)
	if productIDStr == "" {
		productIDStr = defaultProductID
	}

	// 如果仍然未指定 product_id，尝试自动从产品列表中找到 gemini 相关的产品
	if productIDStr == "" {
		products, errProds := v.GetProducts(ctx)
		if errProds == nil && len(products) > 0 {
			for _, p := range products {
				if strings.Contains(strings.ToLower(p.Name), "gemini") {
					productIDStr = p.ID
					break
				}
			}
			if productIDStr == "" {
				productIDStr = products[0].ID
			}
		}
	}

	productIDInt, errConv := strconv.Atoi(productIDStr)
	if errConv != nil || productIDInt <= 0 {
		return nil, fmt.Errorf("无效的 Vente 商品 ID '%s'，请在供应商配置中指定正确的商品 ID", productIDStr)
	}

	qty := req.Quantity
	if qty <= 0 {
		qty = 1
	}

	idempKey := strings.TrimSpace(req.IdempotencyKey)
	if idempKey == "" {
		idempKey = fmt.Sprintf("pix-%d-%04d", time.Now().UnixNano(), time.Now().Nanosecond()%10000)
	}

	custRef := strings.TrimSpace(req.CustomerReference)
	if custRef == "" {
		custRef = fmt.Sprintf("pixel-%d", time.Now().Unix())
	}

	payload := map[string]interface{}{
		"product_id":         productIDInt,
		"quantity":           qty,
		"customer_reference": custRef,
		"idempotency_key":    idempKey,
	}
	if req.ActivationIdentifier != "" {
		payload["activation_identifier"] = req.ActivationIdentifier
	}

	payloadBytes, _ := json.Marshal(payload)
	reqURL := baseURL + "/api/reseller/orders"

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payloadBytes))
	if errReq != nil {
		return nil, fmt.Errorf("构建 Vente 下单请求失败: %w", errReq)
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Vente 下单网络失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Vente 下单响应失败: %w", errRead)
	}

	if resp.StatusCode == http.StatusPaymentRequired { // 402 Insufficient wallet balance
		return nil, errors.New("Vente 供应商账户余额不足，请及时充值")
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var errResp struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(bodyBytes, &errResp)
		if errResp.Message != "" {
			return nil, fmt.Errorf("Vente 下单失败 (%s): %s", errResp.Code, errResp.Message)
		}
		return nil, fmt.Errorf("Vente 下单返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var orderResp struct {
		Success      bool     `json:"success"`
		Status       string   `json:"status"`
		BalanceAfter *float64 `json:"balance_after"`
		UnitPrice    *float64 `json:"unit_price"`
		Total        *float64 `json:"total"`
		Order        struct {
			ID          int64   `json:"id"`
			Status      string  `json:"status"`
			ProductID   int     `json:"product_id"`
			ProductName string  `json:"product_name"`
			Quantity    int     `json:"quantity"`
			AmountUSD   float64 `json:"amount_usd"`
			Items       []struct {
				ID          int64  `json:"id"`
				AccountData string `json:"account_data"`
			} `json:"items"`
		} `json:"order"`
	}

	if errJSON := json.Unmarshal(bodyBytes, &orderResp); errJSON != nil {
		return nil, fmt.Errorf("解析 Vente 下单响应失败: %w", errJSON)
	}

	orderIDStr := strconv.FormatInt(orderResp.Order.ID, 10)
	var links []string
	for _, item := range orderResp.Order.Items {
		accountData := strings.TrimSpace(item.AccountData)
		if accountData != "" {
			// 如果数据中包含 URL，提取出 URL，否则保留原始数据
			extracted := extractURLFromText(accountData)
			if extracted != "" {
				links = append(links, extracted)
			} else {
				links = append(links, accountData)
			}
		}
	}

	// 若尚未返回 items 且状态仍在处理，尝试快速轮询一次 GET /api/reseller/orders/{order_id}
	if len(links) == 0 && orderResp.Order.ID > 0 && orderResp.Order.Status != "CANCELLED" {
		time.Sleep(1 * time.Second)
		polledOrder, errPoll := v.fetchOrderDetails(ctx, orderResp.Order.ID)
		if errPoll == nil && polledOrder != nil {
			for _, item := range polledOrder.Items {
				accData := strings.TrimSpace(item.AccountData)
				if accData != "" {
					extracted := extractURLFromText(accData)
					if extracted != "" {
						links = append(links, extracted)
					} else {
						links = append(links, accData)
					}
				}
			}
		}
	}

	finalLink := ""
	if len(links) > 0 {
		finalLink = links[0]
	}

	amount := orderResp.Order.AmountUSD
	if amount <= 0 && orderResp.Total != nil {
		amount = *orderResp.Total
	}

	return &SupplierPurchaseResult{
		OrderID:      orderIDStr,
		Link:         finalLink,
		Items:        links,
		AmountUSD:    amount,
		BalanceAfter: orderResp.BalanceAfter,
		Status:       orderResp.Order.Status,
		RawData:      orderResp,
	}, nil
}

// fetchOrderDetails 轮询订单详情: GET /api/reseller/orders/{order_id}
func (v *VenteJioProvider) fetchOrderDetails(ctx context.Context, orderID int64) (*struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Items  []struct {
		ID          int64  `json:"id"`
		AccountData string `json:"account_data"`
	} `json:"items"`
}, error) {
	apiKey, baseURL, _ := v.getEffectiveConfig()
	reqURL := fmt.Sprintf("%s/api/reseller/orders/%d", baseURL, orderID)

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, errReq
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, errResp
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var res struct {
		Success bool `json:"success"`
		Order   struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
			Items  []struct {
				ID          int64  `json:"id"`
				AccountData string `json:"account_data"`
			} `json:"items"`
		} `json:"order"`
	}
	if errJSON := json.NewDecoder(resp.Body).Decode(&res); errJSON != nil {
		return nil, errJSON
	}
	return &res.Order, nil
}

func (v *VenteJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	res, err := v.Purchase(ctx, SupplierPurchaseRequest{
		Quantity:          1,
		CustomerReference: fmt.Sprintf("card_%s", strings.TrimSpace(cardSecret)),
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Link) == "" {
		return "", errors.New("Vente 订单已生成但尚未返回兑换链接，请稍后重试或查看订单状态")
	}
	return res.Link, nil
}

// CreateDeposit 发起 USDT BEP20 充值订单: POST /api/reseller/wallet/deposits
func (v *VenteJioProvider) CreateDeposit(ctx context.Context, amountUSD float64) (*SupplierDepositResult, error) {
	if amountUSD <= 0 {
		return nil, errors.New("充值金额必须大于 0")
	}

	apiKey, baseURL, _ := v.getEffectiveConfig()
	if apiKey == "" {
		return nil, errors.New("未配置 Vente 的 api_key")
	}

	idempKey := fmt.Sprintf("dep-%d-%04d", time.Now().Unix(), time.Now().Nanosecond()%10000)
	payload := map[string]interface{}{
		"amount_usd":      amountUSD,
		"network":         "BEP20",
		"idempotency_key": idempKey,
		"reference":       fmt.Sprintf("wallet-fund-%d", time.Now().Unix()),
	}

	payloadBytes, _ := json.Marshal(payload)
	reqURL := baseURL + "/api/reseller/wallet/deposits"

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payloadBytes))
	if errReq != nil {
		return nil, fmt.Errorf("构建充值请求失败: %w", errReq)
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Vente 充值接口网络失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Vente 充值响应失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("Vente 充值接口返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var depResp struct {
		Success bool `json:"success"`
		Deposit struct {
			DepositID   string   `json:"deposit_id"`
			Status      string   `json:"status"`
			PayAmount   *float64 `json:"pay_amount"`
			PayCurrency string   `json:"pay_currency"`
			Network     string   `json:"network"`
			Address     *string  `json:"address"`
			ExpiresAt   *string  `json:"expires_at"`
		} `json:"deposit"`
	}

	if errJSON := json.Unmarshal(bodyBytes, &depResp); errJSON != nil {
		return nil, fmt.Errorf("解析 Vente 充值响应失败: %w", errJSON)
	}

	payAmt := amountUSD
	if depResp.Deposit.PayAmount != nil {
		payAmt = *depResp.Deposit.PayAmount
	}

	addr := ""
	if depResp.Deposit.Address != nil {
		addr = *depResp.Deposit.Address
	}

	exp := ""
	if depResp.Deposit.ExpiresAt != nil {
		exp = *depResp.Deposit.ExpiresAt
	}

	return &SupplierDepositResult{
		Supported:   true,
		DepositID:   depResp.Deposit.DepositID,
		Status:      depResp.Deposit.Status,
		Network:     depResp.Deposit.Network,
		Address:     addr,
		PayAmount:   payAmt,
		PayCurrency: depResp.Deposit.PayCurrency,
		ExpiresAt:   exp,
		RawData:     depResp.Deposit,
	}, nil
}

// GetDepositStatus 查询充值单状态: GET /api/reseller/wallet/deposits/{deposit_id}?refresh=true
func (v *VenteJioProvider) GetDepositStatus(ctx context.Context, depositID string) (*SupplierDepositResult, error) {
	depositID = strings.TrimSpace(depositID)
	if depositID == "" {
		return nil, errors.New("deposit_id 不能为空")
	}

	apiKey, baseURL, _ := v.getEffectiveConfig()
	reqURL := fmt.Sprintf("%s/api/reseller/wallet/deposits/%s?refresh=true", baseURL, url.PathEscape(depositID))

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建请求失败: %w", errReq)
	}
	httpReq.Header.Set("X-Reseller-Key", apiKey)
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	resp, errResp := v.getClient().Do(httpReq)
	if errResp != nil {
		return nil, fmt.Errorf("请求 Vente 充值查询失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取充值响应失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Vente 充值查询返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res struct {
		Success bool `json:"success"`
		Deposit struct {
			DepositID   string   `json:"deposit_id"`
			Status      string   `json:"status"`
			PayAmount   *float64 `json:"pay_amount"`
			PayCurrency string   `json:"pay_currency"`
			Network     string   `json:"network"`
			Address     *string  `json:"address"`
			ExpiresAt   *string  `json:"expires_at"`
		} `json:"deposit"`
	}
	if errJSON := json.Unmarshal(bodyBytes, &res); errJSON != nil {
		return nil, fmt.Errorf("解析充值状态失败: %w", errJSON)
	}

	payAmt := 0.0
	if res.Deposit.PayAmount != nil {
		payAmt = *res.Deposit.PayAmount
	}
	addr := ""
	if res.Deposit.Address != nil {
		addr = *res.Deposit.Address
	}
	exp := ""
	if res.Deposit.ExpiresAt != nil {
		exp = *res.Deposit.ExpiresAt
	}

	return &SupplierDepositResult{
		Supported:   true,
		DepositID:   res.Deposit.DepositID,
		Status:      res.Deposit.Status,
		Network:     res.Deposit.Network,
		Address:     addr,
		PayAmount:   payAmt,
		PayCurrency: res.Deposit.PayCurrency,
		ExpiresAt:   exp,
		RawData:     res.Deposit,
	}, nil
}

// extractURLFromText 从文本内容中匹配提取第一个 HTTP/HTTPS 链接
var urlRegex = regexp.MustCompile(`https?://[^\s"'<>]+`)

func extractURLFromText(s string) string {
	match := urlRegex.FindString(s)
	return strings.TrimSpace(match)
}

func init() {
	// 仅注册正式商用供应商
	RegisterJioProvider(&VenteJioProvider{})
	RegisterJioProvider(&AcczoneJioProvider{})
}

// =========================================================================
// Jio 代理与网络客户端工具
// =========================================================================

// formatJioProxyURL 根据协议、主机、端口、账号、密码生成代理 URL
func formatJioProxyURL(protocol, host, port, username, password string) string {
	host = strings.TrimSpace(host)
	port = strings.TrimSpace(port)
	if host == "" {
		return ""
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "http"
	}

	hostPort := host
	if port != "" {
		hostPort = fmt.Sprintf("%s:%s", host, port)
	}

	u := &url.URL{
		Scheme: protocol,
		Host:   hostPort,
	}

	username = strings.TrimSpace(username)
	if username != "" {
		if password != "" {
			u.User = url.UserPassword(username, password)
		} else {
			u.User = url.User(username)
		}
	}

	return u.String()
}

// parseJioProxyURL 将代理 URL 解析为协议、主机、端口、账号、密码
func parseJioProxyURL(raw string) (protocol, host, port, username, password string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "http", "", "", "", ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "http", "", "", "", ""
	}
	protocol = strings.ToLower(u.Scheme)
	if protocol == "" {
		protocol = "http"
	}
	host = u.Hostname()
	port = u.Port()
	if u.User != nil {
		username = u.User.Username()
		password, _ = u.User.Password()
	}
	return
}

// buildJioHTTPClient 根据代理地址构造带超时与正向代理的 HTTP Client
func buildJioHTTPClient(proxyAddr string, timeout time.Duration) *http.Client {
	proxyAddr = strings.TrimSpace(proxyAddr)
	if proxyAddr == "" {
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		}
	}

	rawURL := proxyAddr
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}

	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
			},
		}
	}

	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsedURL),
		},
	}
}

// getJioHTTPClient 获取专用于 Jio 第三方调用的 HTTP 客户端
func getJioHTTPClient(timeout time.Duration) *http.Client {
	enabled := strings.ToLower(strings.TrimSpace(getSetting("jio_proxy_enabled", "")))
	if enabled == "off" {
		return buildJioHTTPClient("", timeout)
	}

	proto := getSetting("jio_proxy_protocol", "http")
	host := getSetting("jio_proxy_host", "")
	port := getSetting("jio_proxy_port", "")
	user := getSetting("jio_proxy_username", "")
	pass := getSetting("jio_proxy_password", "")
	proxyAddr := formatJioProxyURL(proto, host, port, user, pass)

	if proxyAddr == "" {
		proxyAddr = strings.TrimSpace(getSetting("jio_proxy", ""))
	}

	if proxyAddr == "" {
		return buildJioHTTPClient("", timeout)
	}

	return buildJioHTTPClient(proxyAddr, timeout)
}
