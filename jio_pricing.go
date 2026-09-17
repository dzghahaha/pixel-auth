package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// AcczoneServiceItem 表示 api.acczone.xyz /getServices 接口返回的单个激活服务项
type AcczoneServiceItem struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	IsActive  int     `json:"is_active"`
	CreatedAt string  `json:"created_at"`
}

// JioProviderCost 表示各第三方供应商的成本明细项
type JioProviderCost struct {
	ProviderKey    string               `json:"provider_key"`
	ProviderName   string               `json:"provider_name"`
	ServiceKey     string               `json:"service_key"`
	ServiceName    string               `json:"service_name"`
	CostPrice      float64              `json:"cost_price"`
	CostCNY        float64              `json:"cost_cny"`
	IsActive       int                  `json:"is_active"`
	IsSystemActive bool                 `json:"is_system_active"`
	LatencyMs      int64                `json:"latency_ms"`
	Currency       string               `json:"currency"`
	StatusText     string               `json:"status_text"`
	FetchedAt      string               `json:"fetched_at"`
	RawServices    []AcczoneServiceItem `json:"raw_services,omitempty"`
}

// JioPricingConfig Jio 销售价格配置结构体
type JioPricingConfig struct {
	PricingMode     string  `json:"pricing_mode"`     // "fixed" (固定价格) 或 "ratio" (按成本比例计算)
	FixedPrice      float64 `json:"fixed_price"`      // 固定销售单价 (元)
	BenchmarkSource string  `json:"benchmark_source"` // 基准成本来源: "active" (当前系统启用渠道), "lowest" (全渠道最低成本), "acczone", "manual"
	ExchangeRate    float64 `json:"exchange_rate"`    // 美元汇率 (如 7.20)
	Ratio           float64 `json:"ratio"`            // 加价倍数 (如 1.50)
	RoundMode       string  `json:"round_mode"`       // 小数取整规则: "ceil" (向上进位), "floor" (向下舍去), "round" (四舍五入)
	RoundPrecision  int     `json:"round_precision"`  // 小数保留位数: 0 (取整到元), 1 (保留1位小数), 2 (保留2位小数)
	CachedCost      float64 `json:"cached_cost"`      // 最新缓存的成本 (USD)
	CachedAt        string  `json:"cached_at"`        // 成本缓存时间
}

// FetchAcczoneServicesResult 携带耗时与服务项列表
type FetchAcczoneServicesResult struct {
	Services  []AcczoneServiceItem
	LatencyMs int64
}

// FetchAcczoneServices 实时请求 api.acczone.xyz/getServices 获取可用激活服务与成本列表
func FetchAcczoneServices(ctx context.Context) ([]AcczoneServiceItem, error) {
	res, err := FetchAcczoneServicesWithLatency(ctx)
	if err != nil {
		return nil, err
	}
	return res.Services, nil
}

// FetchAcczoneServicesWithLatency 实时请求并测量上游响应耗时
func FetchAcczoneServicesWithLatency(ctx context.Context) (*FetchAcczoneServicesResult, error) {
	// 读取系统设置中配置的 api_url（若有自定义则替换路径，否则使用默认官方接口）
	configJSON := getSetting("jio_provider_config", "{}")
	var cfg struct {
		APIURL string `json:"api_url"`
	}
	_ = json.Unmarshal([]byte(configJSON), &cfg)

	baseURL := strings.TrimSpace(cfg.APIURL)
	if baseURL == "" {
		baseURL = "https://api.acczone.xyz/getServices"
	} else {
		// 如果填写的完整 URL 是 buyCpn，则替换为 getServices
		if strings.Contains(baseURL, "/buyCpn") {
			baseURL = strings.Replace(baseURL, "/buyCpn", "/getServices", 1)
		} else if !strings.HasSuffix(baseURL, "/getServices") {
			if strings.HasSuffix(baseURL, "/") {
				baseURL = baseURL + "getServices"
			} else {
				baseURL = baseURL + "/getServices"
			}
		}
	}

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if errReq != nil {
		return nil, fmt.Errorf("构建 Acczone 成本查询请求失败: %w", errReq)
	}
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	// 使用支持 Jio 专属代理设置的客户端
	client := getJioHTTPClient(15 * time.Second)
	startTime := time.Now()
	resp, errResp := client.Do(httpReq)
	latencyMs := time.Since(startTime).Milliseconds()
	if errResp != nil {
		return nil, fmt.Errorf("请求 Acczone /getServices 失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return nil, fmt.Errorf("读取 Acczone /getServices 响应体失败: %w", errRead)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Acczone /getServices 返回 HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var services []AcczoneServiceItem
	if errJSON := json.Unmarshal(bodyBytes, &services); errJSON != nil {
		return nil, fmt.Errorf("解析 Acczone /getServices 响应数据失败: %w, 内容: %s", errJSON, string(bodyBytes))
	}

	return &FetchAcczoneServicesResult{
		Services:  services,
		LatencyMs: latencyMs,
	}, nil
}

// GetAllJioProviderCosts 汇总所有第三方供应商的实时成本信息（Acczone、Mock、第三方外部接口等）
func GetAllJioProviderCosts(ctx context.Context) ([]JioProviderCost, error) {
	cfg := GetJioPricingConfig()
	exchangeRate := cfg.ExchangeRate
	if exchangeRate <= 0 {
		exchangeRate = 7.20
	}

	activeProvider := strings.ToLower(strings.TrimSpace(getSetting("jio_active_provider", "mock")))

	var results []JioProviderCost
	nowStr := time.Now().Format("2006-01-02 15:04:05")

	// 1. 获取 Acczone 实时成本
	acczoneRes, errAcczone := FetchAcczoneServicesWithLatency(ctx)
	if errAcczone != nil {
		log.Printf("[JioPricing] 获取 Acczone 实时成本异常: %v", errAcczone)
		fallbackCost := cfg.CachedCost
		if fallbackCost <= 0 {
			fallbackCost = 0.40
		}
		costCNY := math.Round(fallbackCost*exchangeRate*100) / 100
		results = append(results, JioProviderCost{
			ProviderKey:    "acczone",
			ProviderName:   "Acczone 采购平台 (api.acczone.xyz)",
			ServiceKey:     "gemini",
			ServiceName:    "Gemini Link",
			CostPrice:      fallbackCost,
			CostCNY:        costCNY,
			IsActive:       1,
			IsSystemActive: activeProvider == "acczone",
			LatencyMs:      0,
			Currency:       "USD",
			StatusText:     fmt.Sprintf("缓存数据 (上游连接超时: %v)", errAcczone),
			FetchedAt:      cfg.CachedAt,
		})
	} else {
		acczoneServices := acczoneRes.Services
		var foundGemini *AcczoneServiceItem
		for i := range acczoneServices {
			if strings.EqualFold(acczoneServices[i].Key, "gemini") {
				foundGemini = &acczoneServices[i]
				break
			}
		}

		if foundGemini != nil {
			costCNY := math.Round(foundGemini.Price*exchangeRate*100) / 100
			statusTxt := "正常在线"
			if foundGemini.IsActive == 0 {
				statusTxt = "上游暂停"
			}
			results = append(results, JioProviderCost{
				ProviderKey:    "acczone",
				ProviderName:   "Acczone 采购平台 (api.acczone.xyz)",
				ServiceKey:     foundGemini.Key,
				ServiceName:    foundGemini.Name,
				CostPrice:      foundGemini.Price,
				CostCNY:        costCNY,
				IsActive:       foundGemini.IsActive,
				IsSystemActive: activeProvider == "acczone",
				LatencyMs:      acczoneRes.LatencyMs,
				Currency:       "USD",
				StatusText:     statusTxt,
				FetchedAt:      nowStr,
				RawServices:    acczoneServices,
			})

			// 自动更新系统最新缓存成本
			if foundGemini.Price > 0 {
				updateJioCachedCost(foundGemini.Price, nowStr)
			}
		} else {
			fallbackCost := 0.40
			if len(acczoneServices) > 0 {
				fallbackCost = acczoneServices[0].Price
			}
			costCNY := math.Round(fallbackCost*exchangeRate*100) / 100
			results = append(results, JioProviderCost{
				ProviderKey:    "acczone",
				ProviderName:   "Acczone 采购平台 (api.acczone.xyz)",
				ServiceKey:     "gemini",
				ServiceName:    "Gemini Link",
				CostPrice:      fallbackCost,
				CostCNY:        costCNY,
				IsActive:       1,
				IsSystemActive: activeProvider == "acczone",
				LatencyMs:      acczoneRes.LatencyMs,
				Currency:       "USD",
				StatusText:     "在线 (未匹配key=gemini，使用首项)",
				FetchedAt:      nowStr,
				RawServices:    acczoneServices,
			})
		}
	}

	// 2. 加入第三方备用渠道 (External API / 合作商网关，便于扩展多平台)
	extCostUSD := 0.45
	extCostCNY := math.Round(extCostUSD*exchangeRate*100) / 100
	results = append(results, JioProviderCost{
		ProviderKey:    "external_api",
		ProviderName:   "第三方通用采购网关 (External API)",
		ServiceKey:     "gemini",
		ServiceName:    "Gemini Pro Activation",
		CostPrice:      extCostUSD,
		CostCNY:        extCostCNY,
		IsActive:       1,
		IsSystemActive: activeProvider == "external_api",
		LatencyMs:      120,
		Currency:       "USD",
		StatusText:     "备用渠道已就绪",
		FetchedAt:      nowStr,
	})

	// 3. 加入 Mock 供应商
	results = append(results, JioProviderCost{
		ProviderKey:    "mock",
		ProviderName:   "测试环境模拟供应商 (Mock Provider)",
		ServiceKey:     "gemini",
		ServiceName:    "Mock Gemini Link",
		CostPrice:      0.00,
		CostCNY:        0.00,
		IsActive:       1,
		IsSystemActive: activeProvider == "mock",
		LatencyMs:      1,
		Currency:       "USD",
		StatusText:     "测试免成本",
		FetchedAt:      nowStr,
	})

	return results, nil
}

// updateJioCachedCost 更新系统设置中的成本缓存
func updateJioCachedCost(cost float64, cachedAt string) {
	costStr := fmt.Sprintf("%.4f", cost)
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('jio_pricing_cached_cost', ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()", costStr, costStr)
	_, _ = db.Exec("INSERT INTO system_settings (setting_key, setting_value, updated_at) VALUES ('jio_pricing_cached_at', ?, NOW()) ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()", cachedAt, cachedAt)
}

// GetJioPricingConfig 获取当前系统的 Jio 价格配置
func GetJioPricingConfig() JioPricingConfig {
	mode := strings.ToLower(strings.TrimSpace(getSetting("jio_pricing_mode", "ratio")))
	if mode != "fixed" && mode != "ratio" {
		mode = "ratio"
	}

	fixedPrice, _ := strconv.ParseFloat(getSetting("jio_pricing_fixed_price", "5.00"), 64)
	if fixedPrice <= 0 {
		fixedPrice = 5.00
	}

	exchangeRate, _ := strconv.ParseFloat(getSetting("jio_pricing_exchange_rate", "7.20"), 64)
	if exchangeRate <= 0 {
		exchangeRate = 7.20
	}

	ratio, _ := strconv.ParseFloat(getSetting("jio_pricing_ratio", "1.50"), 64)
	if ratio <= 0 {
		ratio = 1.50
	}

	roundMode := strings.ToLower(strings.TrimSpace(getSetting("jio_pricing_round_mode", "round")))
	if roundMode != "ceil" && roundMode != "floor" && roundMode != "round" {
		roundMode = "round"
	}

	roundPrecision, _ := strconv.Atoi(getSetting("jio_pricing_round_precision", "2"))
	if roundPrecision < 0 {
		roundPrecision = 0
	} else if roundPrecision > 4 {
		roundPrecision = 2
	}

	benchmarkSource := strings.ToLower(strings.TrimSpace(getSetting("jio_pricing_benchmark_source", "active")))
	if benchmarkSource != "active" && benchmarkSource != "lowest" && benchmarkSource != "acczone" && benchmarkSource != "manual" {
		benchmarkSource = "active"
	}

	cachedCost, _ := strconv.ParseFloat(getSetting("jio_pricing_cached_cost", "0.40"), 64)
	if cachedCost <= 0 {
		cachedCost = 0.40
	}

	cachedAt := getSetting("jio_pricing_cached_at", "")

	return JioPricingConfig{
		PricingMode:     mode,
		FixedPrice:      fixedPrice,
		BenchmarkSource: benchmarkSource,
		ExchangeRate:    exchangeRate,
		Ratio:           ratio,
		RoundMode:       roundMode,
		RoundPrecision:  roundPrecision,
		CachedCost:      cachedCost,
		CachedAt:        cachedAt,
	}
}

// CalculateJioSalePrice 根据成本与配置，精确计算销售价格
func CalculateJioSalePrice(costPrice float64, cfg JioPricingConfig) float64 {
	// 1. 固定价格算法
	if cfg.PricingMode == "fixed" {
		return math.Round(cfg.FixedPrice*100) / 100
	}

	// 2. 比例计算算法
	effectiveCost := costPrice
	if effectiveCost <= 0 {
		effectiveCost = cfg.CachedCost
	}
	if effectiveCost <= 0 {
		effectiveCost = 0.40
	}

	rate := cfg.ExchangeRate
	if rate <= 0 {
		rate = 7.20
	}

	multiplier := cfg.Ratio
	if multiplier <= 0 {
		multiplier = 1.00
	}

	// 原始计算未取整售价: 成本(USD) * 汇率 * 加价比例
	rawPrice := effectiveCost * rate * multiplier

	precision := cfg.RoundPrecision
	if precision < 0 {
		precision = 0
	} else if precision > 4 {
		precision = 2
	}

	factor := math.Pow(10, float64(precision))
	var calculated float64

	switch strings.ToLower(cfg.RoundMode) {
	case "ceil":
		// 向上进位取整
		calculated = math.Ceil(rawPrice*factor) / factor
	case "floor":
		// 向下舍去取整
		calculated = math.Floor(rawPrice*factor) / factor
	case "round":
		fallthrough
	default:
		// 四舍五入取整
		calculated = math.Round(rawPrice*factor) / factor
	}

	// 标准货币金额规范为 2 位小数
	return math.Round(calculated*100) / 100
}

// GetCurrentJioSalePrice 获取系统当前应使用的 Jio 销售价格 (元)
func GetCurrentJioSalePrice() float64 {
	cfg := GetJioPricingConfig()
	return CalculateJioSalePrice(cfg.CachedCost, cfg)
}

// handleAdminJioCosts 获取各供应商实时成本
func handleAdminJioCosts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 GET 请求",
		})
		return
	}

	costs, err := GetAllJioProviderCosts(r.Context())
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("获取供应商成本失败: %v", err),
		})
		return
	}

	cfg := GetJioPricingConfig()
	activeProvider := getSetting("jio_active_provider", "mock")

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"active_provider": activeProvider,
		"exchange_rate":   cfg.ExchangeRate,
		"costs":           costs,
	})
}

// handleAdminJioPricing 价格配置获取与更新
func handleAdminJioPricing(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := GetJioPricingConfig()
		costs, _ := GetAllJioProviderCosts(r.Context())
		activeProvider := getSetting("jio_active_provider", "mock")

		currentSalePrice := CalculateJioSalePrice(cfg.CachedCost, cfg)

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success":            true,
			"config":             cfg,
			"active_provider":    activeProvider,
			"current_sale_price": currentSalePrice,
			"costs":              costs,
		})

	case http.MethodPost:
		var req struct {
			PricingMode     string  `json:"pricing_mode"`
			FixedPrice      float64 `json:"fixed_price"`
			BenchmarkSource string  `json:"benchmark_source"`
			ExchangeRate    float64 `json:"exchange_rate"`
			Ratio           float64 `json:"ratio"`
			RoundMode       string  `json:"round_mode"`
			RoundPrecision  int     `json:"round_precision"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "请求参数格式错误",
			})
			return
		}

		mode := strings.ToLower(strings.TrimSpace(req.PricingMode))
		if mode != "fixed" && mode != "ratio" {
			mode = "ratio"
		}

		benchmarkSource := strings.ToLower(strings.TrimSpace(req.BenchmarkSource))
		if benchmarkSource != "active" && benchmarkSource != "lowest" && benchmarkSource != "acczone" && benchmarkSource != "manual" {
			benchmarkSource = "active"
		}

		if req.FixedPrice < 0 {
			req.FixedPrice = 0
		}
		if req.ExchangeRate <= 0 {
			req.ExchangeRate = 7.20
		}
		if req.Ratio <= 0 {
			req.Ratio = 1.00
		}

		roundMode := strings.ToLower(strings.TrimSpace(req.RoundMode))
		if roundMode != "ceil" && roundMode != "floor" && roundMode != "round" {
			roundMode = "round"
		}

		precision := req.RoundPrecision
		if precision < 0 {
			precision = 0
		} else if precision > 4 {
			precision = 2
		}

		// 保存到 system_settings
		settingsMap := map[string]string{
			"jio_pricing_mode":             mode,
			"jio_pricing_fixed_price":       fmt.Sprintf("%.2f", req.FixedPrice),
			"jio_pricing_benchmark_source": benchmarkSource,
			"jio_pricing_exchange_rate":     fmt.Sprintf("%.4f", req.ExchangeRate),
			"jio_pricing_ratio":             fmt.Sprintf("%.4f", req.Ratio),
			"jio_pricing_round_mode":        roundMode,
			"jio_pricing_round_precision":   strconv.Itoa(precision),
		}

		for k, v := range settingsMap {
			_, err := db.Exec(`
				INSERT INTO system_settings (setting_key, setting_value, updated_at) 
				VALUES (?, ?, NOW()) 
				ON DUPLICATE KEY UPDATE setting_value = ?, updated_at = NOW()`,
				k, v, v)
			if err != nil {
				log.Printf("[JioPricing] 保存设置 %s 失败: %v", k, err)
			}
		}

		newCfg := GetJioPricingConfig()
		currentSalePrice := CalculateJioSalePrice(newCfg.CachedCost, newCfg)

		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success":            true,
			"message":            "Jio 销售价格配置保存成功",
			"config":             newCfg,
			"current_sale_price": currentSalePrice,
		})

	default:
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "方法不支持",
		})
	}
}

// handleAdminOrdersExport 订单管理报表导出 (Excel 兼容 CSV 格式)
func handleAdminOrdersExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 GET 请求",
		})
		return
	}

	// 检查权限
	adminID, ok := getAdminID(r)
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "未登录或登录已过期",
		})
		return
	}

	var role string
	_ = db.QueryRow("SELECT role FROM admins WHERE id = ?", adminID).Scan(&role)
	if role != "admin" && !hasPermission(adminID, "orders") {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "权限不足",
		})
		return
	}

	// 读取筛选参数
	queryParam := strings.TrimSpace(r.URL.Query().Get("query"))
	statusParam := strings.TrimSpace(r.URL.Query().Get("status"))
	serviceTypeParam := strings.TrimSpace(r.URL.Query().Get("service_type"))
	originalKeyParam := strings.TrimSpace(r.URL.Query().Get("original_key"))
	noteParam := strings.TrimSpace(r.URL.Query().Get("note"))
	startTimeParam := strings.TrimSpace(r.URL.Query().Get("start_time"))
	endTimeParam := strings.TrimSpace(r.URL.Query().Get("end_time"))
	creatorIDParam := strings.TrimSpace(r.URL.Query().Get("creator_id"))

	var whereClauses []string
	var args []interface{}

	whereClauses = append(whereClauses, "1=1")

	// 非超级管理员只能查看自己创建的数据
	if role != "admin" {
		whereClauses = append(whereClauses, "o.creator_id = ?")
		args = append(args, adminID)
	} else if creatorIDParam != "" {
		whereClauses = append(whereClauses, "o.creator_id = ?")
		args = append(args, creatorIDParam)
	}

	if queryParam != "" {
		likePattern := "%" + queryParam + "%"
		whereClauses = append(whereClauses, "(o.card_secret LIKE ? OR r.username LIKE ? OR r.task_id LIKE ? OR r.discount_url LIKE ? OR sk.vendor_key LIKE ?)")
		args = append(args, likePattern, likePattern, likePattern, likePattern, likePattern)
	}

	if originalKeyParam != "" {
		whereClauses = append(whereClauses, "sk.original_key LIKE ?")
		args = append(args, "%"+originalKeyParam+"%")
	}

	if noteParam != "" {
		whereClauses = append(whereClauses, "sk.note LIKE ?")
		args = append(args, "%"+noteParam+"%")
	}

	if statusParam != "" {
		whereClauses = append(whereClauses, "r.status = ?")
		args = append(args, statusParam)
	}

	if serviceTypeParam != "" {
		whereClauses = append(whereClauses, "o.service_type = ?")
		args = append(args, serviceTypeParam)
	}

	if startTimeParam != "" {
		var sec int64
		if _, err := fmt.Sscanf(startTimeParam, "%d", &sec); err == nil {
			startTime := time.Unix(sec, 0)
			whereClauses = append(whereClauses, "o.created_at >= ?")
			args = append(args, startTime)
		}
	}

	if endTimeParam != "" {
		var sec int64
		if _, err := fmt.Sscanf(endTimeParam, "%d", &sec); err == nil {
			endTime := time.Unix(sec, 0)
			whereClauses = append(whereClauses, "o.created_at <= ?")
			args = append(args, endTime)
		}
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	exportQuery := fmt.Sprintf(`
		SELECT o.id, o.card_secret, COALESCE(o.service_type, 'pixel'), COALESCE(o.sale_price, 0.00), o.mode,
		       COALESCE(r.username, ''), COALESCE(r.status, ''), COALESCE(r.message, ''), 
		       COALESCE(NULLIF(r.discount_url, ''), sk.discount_url, ''), o.vendor, COALESCE(r.task_id, ''),
		       o.created_at, r.completed_at, COALESCE(sk.vendor_key, ''), COALESCE(sk.note, ''), 
		       COALESCE(sk.original_key, ''), COALESCE(NULLIF(a.nickname, ''), a.username, '') AS creator_name
		FROM orders o
		LEFT JOIN system_keys sk ON o.card_secret = sk.system_key
		LEFT JOIN admins a ON o.creator_id = a.id
		LEFT JOIN (
			SELECT r1.*
			FROM account_records r1
			INNER JOIN (
				SELECT order_id, MAX(id) as max_id
				FROM account_records
				GROUP BY order_id
			) r2 ON r1.id = r2.max_id
		) r ON o.id = r.order_id
		WHERE %s
		ORDER BY o.id DESC
		LIMIT 50000`, whereSQL)

	rows, errRows := db.Query(exportQuery, args...)
	if errRows != nil {
		log.Printf("Error querying admin orders for export: %v\n", errRows)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "导出订单数据查询失败",
		})
		return
	}
	defer rows.Close()

	// 设置 CSV 响应头
	filename := fmt.Sprintf("orders_report_%s.csv", time.Now().Format("20060102_150405"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", url.PathEscape(filename)))

	// 写入 UTF-8 BOM，确保 Windows Excel 打开中文不乱码
	_, _ = w.Write([]byte("\xef\xbb\xbf"))

	csvWriter := csv.NewWriter(w)
	defer csvWriter.Flush()

	// 写入表头
	headers := []string{
		"订单ID",
		"系统卡密",
		"业务类型",
		"销售价格(元)",
		"模式",
		"账号/邮箱",
		"订单状态",
		"状态信息",
		"兑换链接",
		"合作厂商",
		"Task ID",
		"源Key",
		"卡密备注",
		"创建人",
		"首次提交时间",
		"完成时间",
	}
	if err := csvWriter.Write(headers); err != nil {
		log.Printf("Error writing CSV header: %v", err)
		return
	}

	statusMap := map[string]string{
		"pending":   "排队中",
		"running":   "处理中",
		"success":   "成功",
		"failed":    "失败",
		"cancelled": "已取消",
		"paused":    "维护挂起",
	}

	for rows.Next() {
		var id int64
		var cardSecret, serviceType, mode, username, status, msg, discountURL, vendor, taskID string
		var vendorKey, note, originalKey, creatorName string
		var salePrice float64
		var createdAt time.Time
		var completedAt sql.NullTime

		errScan := rows.Scan(
			&id, &cardSecret, &serviceType, &salePrice, &mode,
			&username, &status, &msg, &discountURL, &vendor, &taskID,
			&createdAt, &completedAt, &vendorKey, &note, &originalKey, &creatorName,
		)
		if errScan != nil {
			log.Printf("Error scanning row in export: %v", errScan)
			continue
		}

		if role != "admin" {
			vendor = ""
			taskID = ""
			vendorKey = ""
			originalKey = ""
		}

		statusName := status
		if mapped, ok := statusMap[strings.ToLower(status)]; ok {
			statusName = mapped
		}

		completedStr := ""
		if completedAt.Valid {
			completedStr = completedAt.Time.Format("2006-01-02 15:04:05")
		}

		record := []string{
			strconv.FormatInt(id, 10),
			cardSecret,
			serviceType,
			fmt.Sprintf("%.2f", salePrice),
			mode,
			username,
			statusName,
			msg,
			discountURL,
			vendor,
			taskID,
			originalKey,
			note,
			creatorName,
			createdAt.Format("2006-01-02 15:04:05"),
			completedStr,
		}
		_ = csvWriter.Write(record)
	}
}
