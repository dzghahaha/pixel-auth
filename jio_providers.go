package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// JioProvider 定义第三方 Jio 订阅兑换链接获取的适配器接口
type JioProvider interface {
	// Name 返回该 Provider 的唯一标识（用于系统设置中的枚举值配置）
	Name() string
	// GetOfferLink 请求上游或按规则生成专属兑换链接
	GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (link string, err error)
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

// GetActiveJioProvider 获取当前系统设置中生效的 Jio Provider（默认降级为 mock）
func GetActiveJioProvider() JioProvider {
	activeName := getSetting("jio_active_provider", "mock")
	p, err := GetJioProvider(activeName)
	if err == nil && p != nil {
		return p
	}
	// Fallback to default mock provider
	if fallback, ok := jioProviders["mock"]; ok {
		return fallback
	}
	return &MockJioProvider{}
}

// MockJioProvider 首期默认的模拟第三方 Provider（支持完整业务闭环及联调测试）
type MockJioProvider struct{}

func (m *MockJioProvider) Name() string {
	return "mock"
}

func (m *MockJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	if strings.TrimSpace(cardSecret) == "" {
		return "", errors.New("卡密不能为空")
	}

	cleanCard := strings.ToUpper(strings.TrimSpace(cardSecret))
	if len(cleanCard) > 6 {
		cleanCard = cleanCard[:6]
	}

	// 生成 8 字节的随机 token 后缀
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		return fmt.Sprintf("https://one.google.com/promo/hasoffer?token=JIO-OFFER-%s", cleanCard), nil
	}
	randomToken := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("https://one.google.com/promo/hasoffer?token=JIO-%s-%s", cleanCard, strings.ToUpper(randomToken)), nil
}

// AcczoneJioProvider 接入 api.acczone.xyz 第三方优惠券链接采购接口
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

func (a *AcczoneJioProvider) GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (string, error) {
	// 1. 读取系统设置中的 jio_provider_config
	configJSON := getSetting("jio_provider_config", "{}")
	var cfg struct {
		APIKey     string `json:"apikey"`
		ServiceKey string `json:"service_key"`
		APIURL     string `json:"api_url"`
	}
	_ = json.Unmarshal([]byte(configJSON), &cfg)

	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		// 备选回退：检查 vendorKey 是否携带 apikey
		apiKey = strings.TrimSpace(vendorKey)
	}
	if apiKey == "" {
		return "", errors.New("未配置 Acczone 的 apikey，请在管理后台「系统设置」-「Jio 订阅第三方配置」中填写 apikey")
	}

	serviceKey := strings.TrimSpace(cfg.ServiceKey)
	if serviceKey == "" {
		serviceKey = "gemini"
	}

	apiURL := strings.TrimSpace(cfg.APIURL)
	if apiURL == "" {
		apiURL = "https://api.acczone.xyz/buyCpn"
	}

	// 2. 构造 GET 请求参数
	reqURL := fmt.Sprintf("%s?apikey=%s&service_key=%s&quantity=1", apiURL, url.QueryEscape(apiKey), url.QueryEscape(serviceKey))

	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if errReq != nil {
		return "", fmt.Errorf("构建 Acczone 请求失败: %w", errReq)
	}
	httpReq.Header.Set("User-Agent", "Pixel-Auth-Client/1.0")

	client := a.client
	if client == nil {
		client = getJioHTTPClient(20 * time.Second)
	}

	resp, errResp := client.Do(httpReq)
	if errResp != nil {
		return "", fmt.Errorf("请求 Acczone 接口网络失败: %w", errResp)
	}
	defer resp.Body.Close()

	bodyBytes, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return "", fmt.Errorf("读取 Acczone 响应失败: %w", errRead)
	}

	bodyStr := string(bodyBytes)

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Acczone 返回 HTTP %d: %s", resp.StatusCode, bodyStr)
	}

	// 3. 尝试解析成功数组响应 [ { "code_value": "..." } ]
	var items []AcczoneCpnItem
	if errArr := json.Unmarshal(bodyBytes, &items); errArr == nil {
		if len(items) == 0 {
			return "", errors.New("Acczone 返回空卡券列表，库存可能已售罄")
		}
		link := strings.TrimSpace(items[0].CodeValue)
		if link == "" {
			return "", errors.New("Acczone 返回的卡券内容为空")
		}
		return link, nil
	}

	// 4. 若不是数组，尝试解析错误对象 { "message": "...", "error": "..." }
	var errObj map[string]interface{}
	if errMap := json.Unmarshal(bodyBytes, &errObj); errMap == nil {
		if msg, ok := errObj["message"].(string); ok && msg != "" {
			return "", fmt.Errorf("Acczone 提示: %s", msg)
		}
		if errVal, ok := errObj["error"].(string); ok && errVal != "" {
			return "", fmt.Errorf("Acczone 提示: %s", errVal)
		}
		if detail, ok := errObj["detail"].(string); ok && detail != "" {
			return "", fmt.Errorf("Acczone 提示: %s", detail)
		}
	}

	return "", fmt.Errorf("解析 Acczone 响应失败: %s", bodyStr)
}

func init() {
	// 默认注册 mock 供应商
	RegisterJioProvider(&MockJioProvider{})
	// 注册 acczone 供应商
	RegisterJioProvider(&AcczoneJioProvider{})
}

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
// 专用于 Jio 第三方 API 上游请求，不影响系统其他网络通信
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

	// 如果未指定协议前缀，自动补全 http://
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
// 严格隔离：仅针对 Jio 第三方调用生效，不影响系统其他模块
// 支持后台选择是否开启代理：当 jio_proxy_enabled == "off" 时强制直连
func getJioHTTPClient(timeout time.Duration) *http.Client {
	enabled := strings.ToLower(strings.TrimSpace(getSetting("jio_proxy_enabled", "")))
	// 只要显式配置为关闭 (off)，则直连（不走代理）
	if enabled == "off" {
		return buildJioHTTPClient("", timeout)
	}

	// 1. 优先通过结构化字段组装代理 URL
	proto := getSetting("jio_proxy_protocol", "http")
	host := getSetting("jio_proxy_host", "")
	port := getSetting("jio_proxy_port", "")
	user := getSetting("jio_proxy_username", "")
	pass := getSetting("jio_proxy_password", "")
	proxyAddr := formatJioProxyURL(proto, host, port, user, pass)

	// 2. 若结构化字段未填写主机，回退读取传统 jio_proxy 完整 URL
	if proxyAddr == "" {
		proxyAddr = strings.TrimSpace(getSetting("jio_proxy", ""))
	}

	// 3. 若均未配置代理地址，保持直连
	if proxyAddr == "" {
		return buildJioHTTPClient("", timeout)
	}

	return buildJioHTTPClient(proxyAddr, timeout)
}
