package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// XunhuPay 虎皮椒支付 SDK
type XunhuPay struct {
	AppID   string
	Secret  string
	Gateway string
	Client  *http.Client
}

// NewXunhuPay 创建 SDK 实例
func NewXunhuPay(appID, secret, gateway string) *XunhuPay {
	if gateway == "" {
		gateway = "https://api.xunhupay.com"
	}
	gateway = strings.TrimRight(gateway, "/")
	return &XunhuPay{
		AppID:   appID,
		Secret:  secret,
		Gateway: gateway,
		Client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// MakeSign 生成签名 hash
func (x *XunhuPay) MakeSign(params map[string]string) string {
	var keys []string
	for k, v := range params {
		if k != "hash" && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
	}
	signStr := strings.Join(parts, "&") + x.Secret

	hash := md5.Sum([]byte(signStr))
	return hex.EncodeToString(hash[:])
}

func nonceStr() string {
	const charset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 32)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// postJSON 发送 POST JSON 请求
func (x *XunhuPay) postJSON(uri string, params map[string]string) (map[string]interface{}, error) {
	params["hash"] = x.MakeSign(params)

	jsonBody, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("[xunhupay] JSON 编码失败: %w", err)
	}

	req, err := http.NewRequest("POST", x.Gateway+uri, strings.NewReader(string(jsonBody)))
	if err != nil {
		return nil, fmt.Errorf("[xunhupay] 创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")

	resp, err := x.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("[xunhupay] HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("[xunhupay] 读取响应失败: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("[xunhupay] JSON 解析失败: %s", string(body))
	}
	return result, nil
}

// CreatePayment 发起支付
func (x *XunhuPay) CreatePayment(tradeOrderID string, totalFee float64, title, notifyURL, returnURL, callbackURL string) (map[string]interface{}, error) {
	params := map[string]string{
		"version":        "1.1",
		"appid":          x.AppID,
		"trade_order_id": tradeOrderID,
		"total_fee":      fmt.Sprintf("%.2f", totalFee),
		"title":          title,
		"notify_url":     notifyURL,
		"return_url":     returnURL,
		"callback_url":   callbackURL,
		"time":           fmt.Sprintf("%d", time.Now().Unix()),
		"nonce_str":      nonceStr(),
	}

	result, err := x.postJSON("/payment/do.html", params)
	if err != nil {
		return nil, err
	}

	if errcode, ok := result["errcode"].(float64); ok && errcode != 0 {
		errmsg, _ := result["errmsg"].(string)
		return nil, fmt.Errorf("[xunhupay] 发起支付失败: %s (code=%.0f)", errmsg, errcode)
	}

	data, _ := result["data"].(map[string]interface{})
	return data, nil
}

// QueryOrder 查询订单状态
func (x *XunhuPay) QueryOrder(tradeOrderID, openOrderID string) (map[string]interface{}, error) {
	if tradeOrderID == "" && openOrderID == "" {
		return nil, fmt.Errorf("tradeOrderID 和 openOrderID 至少传一个")
	}

	params := map[string]string{
		"appid":     x.AppID,
		"time":      fmt.Sprintf("%d", time.Now().Unix()),
		"nonce_str": nonceStr(),
	}
	if tradeOrderID != "" {
		params["out_trade_order"] = tradeOrderID
	} else {
		params["open_order_id"] = openOrderID
	}

	return x.postJSON("/payment/query.html", params)
}

// RefundOrder 退款（仅支持全额退款）
func (x *XunhuPay) RefundOrder(tradeOrderID, openOrderID, reason string) (map[string]interface{}, error) {
	if tradeOrderID == "" && openOrderID == "" {
		return nil, fmt.Errorf("tradeOrderID 和 openOrderID 至少传一个")
	}

	params := map[string]string{
		"appid":     x.AppID,
		"reason":    reason,
		"time":      fmt.Sprintf("%d", time.Now().Unix()),
		"nonce_str": nonceStr(),
	}
	if tradeOrderID != "" {
		params["trade_order_id"] = tradeOrderID
	} else {
		params["open_order_id"] = openOrderID
	}

	return x.postJSON("/payment/refund.html", params)
}

// VerifyCallback 验证签名
func (x *XunhuPay) VerifyCallback(postData url.Values) bool {
	received := postData.Get("hash")

	params := make(map[string]string)
	for k, vs := range postData {
		if len(vs) > 0 && k != "hash" && vs[0] != "" {
			params[k] = vs[0]
		}
	}
	expected := x.MakeSign(params)
	return received == expected
}
