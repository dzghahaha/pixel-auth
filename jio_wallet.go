package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInsufficientBalance 表示储值账户余额不足错误
var ErrInsufficientBalance = errors.New("账户储值余额不足")

// JioWalletTransaction 表示储值钱包流水记录结构
type JioWalletTransaction struct {
	ID            int64     `json:"id"`
	AdminID       int64     `json:"admin_id"`
	AdminUsername string    `json:"admin_username,omitempty"`
	AdminNickname string    `json:"admin_nickname,omitempty"`
	Type          string    `json:"type"` // "recharge", "admin_recharge", "admin_deduct", "consume", "refund"
	Amount        float64   `json:"amount"`
	BalanceBefore float64   `json:"balance_before"`
	BalanceAfter  float64   `json:"balance_after"`
	OrderID       *int64    `json:"order_id,omitempty"`
	CardSecret    string    `json:"card_secret,omitempty"`
	Remark        string    `json:"remark"`
	OperatorID    *int64    `json:"operator_id,omitempty"`
	OperatorName  string    `json:"operator_name,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// GetAdminJioBalance 获取指定管理员的 Jio 储值钱包余额
func GetAdminJioBalance(adminID int64) (float64, error) {
	var balance float64
	err := db.QueryRow("SELECT COALESCE(jio_balance, 0.00) FROM admins WHERE id = ?", adminID).Scan(&balance)
	if err != nil {
		return 0, err
	}
	return balance, nil
}

// AddJioWalletBalance 为指定管理员增加或扣减钱包余额（支持正数充值、负数扣款、代充、退款等）
func AddJioWalletBalance(adminID int64, amount float64, txType string, remark string, operatorID int64, orderID *int64, cardSecret string) (float64, int64, error) {
	if amount == 0 {
		return 0, 0, errors.New("变动金额不能为 0")
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	var balanceBefore float64
	errQuery := tx.QueryRow("SELECT COALESCE(jio_balance, 0.00) FROM admins WHERE id = ? FOR UPDATE", adminID).Scan(&balanceBefore)
	if errQuery != nil {
		return 0, 0, fmt.Errorf("查询管理员账户失败: %w", errQuery)
	}

	balanceAfter := math.Round((balanceBefore+amount)*100) / 100
	if balanceAfter < 0 {
		return 0, 0, fmt.Errorf("扣减金额超限：当前可用余额为 ￥%.2f，扣减后不能小于 0", balanceBefore)
	}

	_, errUpdate := tx.Exec("UPDATE admins SET jio_balance = ?, updated_at = NOW() WHERE id = ?", balanceAfter, adminID)
	if errUpdate != nil {
		return 0, 0, fmt.Errorf("更新钱包余额失败: %w", errUpdate)
	}

	if txType == "" {
		if amount > 0 {
			txType = "admin_recharge"
		} else {
			txType = "admin_deduct"
		}
	}

	resLog, errLog := tx.Exec(`
		INSERT INTO jio_wallet_transactions 
		(admin_id, type, amount, balance_before, balance_after, order_id, card_secret, remark, operator_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NOW())`,
		adminID, txType, amount, balanceBefore, balanceAfter, orderID, cardSecret, remark, operatorID)
	if errLog != nil {
		return 0, 0, fmt.Errorf("记录流水失败: %w", errLog)
	}

	txID, _ := resLog.LastInsertId()

	if errCommit := tx.Commit(); errCommit != nil {
		return 0, 0, fmt.Errorf("提交事务失败: %w", errCommit)
	}

	return balanceAfter, txID, nil
}

// DeductJioWalletForCardRedeem 在 C 端兑换时根据卡密归属校验并扣减对应管理员的储值余额
func DeductJioWalletForCardRedeem(cardSecret string, salePrice float64) (int64, int64, error) {
	if salePrice <= 0 {
		// 免消费或未设置价格
		return 0, 0, nil
	}

	// 1. 查询卡密归属创建人 creator_id
	var creatorID sql.NullInt64
	errQueryKey := db.QueryRow("SELECT creator_id FROM system_keys WHERE system_key = ?", cardSecret).Scan(&creatorID)
	if errQueryKey != nil {
		log.Printf("[JioWallet] 查询卡密 %s 归属异常: %v", cardSecret, errQueryKey)
	}

	var targetAdminID int64 = 1 // 默认退化为首个管理员 (admin)
	if creatorID.Valid && creatorID.Int64 > 0 {
		targetAdminID = creatorID.Int64
	} else {
		// 若卡密无归属人，尝试读取第一个超级管理员
		var firstAdminID int64
		if errAdmin := db.QueryRow("SELECT id FROM admins WHERE role = 'admin' ORDER BY id ASC LIMIT 1").Scan(&firstAdminID); errAdmin == nil {
			targetAdminID = firstAdminID
		}
	}

	// 2. 开启事务扣减并记录流水
	tx, err := db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("开启扣费事务失败: %w", err)
	}
	defer tx.Rollback()

	var balanceBefore float64
	var username string
	errBalance := tx.QueryRow("SELECT username, COALESCE(jio_balance, 0.00) FROM admins WHERE id = ? FOR UPDATE", targetAdminID).
		Scan(&username, &balanceBefore)
	if errBalance != nil {
		return 0, 0, fmt.Errorf("未找到卡密归属账户 (ID: %d): %w", targetAdminID, errBalance)
	}

	// 3. 校验余额是否足够本次消费
	if balanceBefore < salePrice {
		log.Printf("[JioWallet] 卡密 %s 归属用户 %s (ID: %d) 余额不足: 当前 ￥%.2f, 本次需要 ￥%.2f",
			cardSecret, username, targetAdminID, balanceBefore, salePrice)
		return targetAdminID, 0, ErrInsufficientBalance
	}

	balanceAfter := math.Round((balanceBefore-salePrice)*100) / 100

	_, errUpdate := tx.Exec("UPDATE admins SET jio_balance = ?, updated_at = NOW() WHERE id = ?", balanceAfter, targetAdminID)
	if errUpdate != nil {
		return targetAdminID, 0, fmt.Errorf("扣减余额失败: %w", errUpdate)
	}

	remark := fmt.Sprintf("C端卡密兑换消费扣减 (单价: ￥%.2f)", salePrice)
	resLog, errLog := tx.Exec(`
		INSERT INTO jio_wallet_transactions 
		(admin_id, type, amount, balance_before, balance_after, card_secret, remark, operator_id, created_at)
		VALUES (?, 'consume', ?, ?, ?, ?, ?, NULL, NOW())`,
		targetAdminID, -salePrice, balanceBefore, balanceAfter, cardSecret, remark)
	if errLog != nil {
		return targetAdminID, 0, fmt.Errorf("写入消费流水失败: %w", errLog)
	}

	txID, _ := resLog.LastInsertId()

	if errCommit := tx.Commit(); errCommit != nil {
		return targetAdminID, 0, fmt.Errorf("提交扣费事务失败: %w", errCommit)
	}

	log.Printf("[JioWallet] 卡密 %s 扣费成功: 归属用户 %s, 扣除 ￥%.2f, 变动后余额 ￥%.2f (流水ID: %d)",
		cardSecret, username, salePrice, balanceAfter, txID)

	return targetAdminID, txID, nil
}

// RefundJioWalletForCardRedeem 上游调用失败时，自动将之前扣减的余额退回给管理员
func RefundJioWalletForCardRedeem(adminID int64, salePrice float64, cardSecret string, reason string) error {
	if adminID <= 0 || salePrice <= 0 {
		return nil
	}

	remark := fmt.Sprintf("兑换失败自动退还扣费 (原因: %s)", reason)
	_, _, err := AddJioWalletBalance(adminID, salePrice, "refund", remark, 0, nil, cardSecret)
	if err != nil {
		log.Printf("[JioWallet] CRITICAL: 自动退还卡密 %s 扣费失败: %v", cardSecret, err)
		return err
	}
	log.Printf("[JioWallet] 卡密 %s 扣费已全额退还至用户 ID %d: ￥%.2f", cardSecret, adminID, salePrice)
	return nil
}

// handleAdminJioWalletSummary 获取当前登录管理员的钱包概况与核心统计指标
func handleAdminJioWalletSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 GET 请求",
		})
		return
	}

	adminID, ok := getAdminID(r)
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "未登录或登录已过期",
		})
		return
	}

	var username, nickname, role string
	var balance float64
	errQuery := db.QueryRow("SELECT username, nickname, role, COALESCE(jio_balance, 0.00) FROM admins WHERE id = ?", adminID).
		Scan(&username, &nickname, &role, &balance)
	if errQuery != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询管理员信息失败",
		})
		return
	}

	// 统计今日消费、累计消费、累计充值
	var todayConsume, totalConsume, totalRecharge float64

	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())

	_ = db.QueryRow(`
		SELECT COALESCE(ABS(SUM(amount)), 0.00) 
		FROM jio_wallet_transactions 
		WHERE admin_id = ? AND type = 'consume' AND created_at >= ?`, adminID, todayStart).Scan(&todayConsume)

	_ = db.QueryRow(`
		SELECT COALESCE(ABS(SUM(amount)), 0.00) 
		FROM jio_wallet_transactions 
		WHERE admin_id = ? AND type = 'consume'`, adminID).Scan(&totalConsume)

	_ = db.QueryRow(`
		SELECT COALESCE(SUM(amount), 0.00) 
		FROM jio_wallet_transactions 
		WHERE admin_id = ? AND type IN ('recharge', 'admin_recharge')`, adminID).Scan(&totalRecharge)

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]interface{}{
			"admin_id":       adminID,
			"username":       username,
			"nickname":       nickname,
			"role":           role,
			"balance":        balance,
			"today_consume":  todayConsume,
			"total_consume":  totalConsume,
			"total_recharge": totalRecharge,
		},
	})
}

// handleAdminJioWalletTransactions 分页获取资金流水明细（普通用户看自己的，超级管理员可查全部或按用户筛选）
func handleAdminJioWalletTransactions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 GET 请求",
		})
		return
	}

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

	// 解析查询参数
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	txType := strings.TrimSpace(r.URL.Query().Get("type"))
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	filterAdminID := strings.TrimSpace(r.URL.Query().Get("admin_id"))
	startDate := strings.TrimSpace(r.URL.Query().Get("start_date"))
	endDate := strings.TrimSpace(r.URL.Query().Get("end_date"))

	var whereClauses []string
	var args []interface{}

	whereClauses = append(whereClauses, "1=1")

	// 权限隔离：非超级管理员强制只能看本人
	if role != "admin" {
		whereClauses = append(whereClauses, "t.admin_id = ?")
		args = append(args, adminID)
	} else if filterAdminID != "" {
		whereClauses = append(whereClauses, "t.admin_id = ?")
		args = append(args, filterAdminID)
	}

	if txType != "" {
		whereClauses = append(whereClauses, "t.type = ?")
		args = append(args, txType)
	}

	if query != "" {
		pattern := "%" + query + "%"
		whereClauses = append(whereClauses, "(t.card_secret LIKE ? OR t.remark LIKE ?)")
		args = append(args, pattern, pattern)
	}

	if startDate != "" {
		whereClauses = append(whereClauses, "t.created_at >= ?")
		args = append(args, startDate+" 00:00:00")
	}

	if endDate != "" {
		whereClauses = append(whereClauses, "t.created_at <= ?")
		args = append(args, endDate+" 23:59:59")
	}

	whereSQL := strings.Join(whereClauses, " AND ")

	// 统计总数
	var total int64
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM jio_wallet_transactions t WHERE %s", whereSQL)
	if errCount := db.QueryRow(countQuery, args...).Scan(&total); errCount != nil {
		log.Printf("Error counting wallet transactions: %v", errCount)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询流水总数失败",
		})
		return
	}

	// 查询列表
	dataQuery := fmt.Sprintf(`
		SELECT t.id, t.admin_id, COALESCE(a.username, '') AS admin_username, COALESCE(a.nickname, '') AS admin_nickname,
		       t.type, t.amount, t.balance_before, t.balance_after, t.order_id, t.card_secret, t.remark,
		       t.operator_id, COALESCE(op.username, '') AS operator_name, t.created_at
		FROM jio_wallet_transactions t
		LEFT JOIN admins a ON t.admin_id = a.id
		LEFT JOIN admins op ON t.operator_id = op.id
		WHERE %s
		ORDER BY t.id DESC
		LIMIT ? OFFSET ?`, whereSQL)

	dataArgs := append(args, pageSize, offset)
	rows, errRows := db.Query(dataQuery, dataArgs...)
	if errRows != nil {
		log.Printf("Error querying wallet transactions: %v", errRows)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询流水记录失败",
		})
		return
	}
	defer rows.Close()

	var records []JioWalletTransaction
	for rows.Next() {
		var r JioWalletTransaction
		var orderID sql.NullInt64
		var operatorID sql.NullInt64

		errScan := rows.Scan(
			&r.ID, &r.AdminID, &r.AdminUsername, &r.AdminNickname,
			&r.Type, &r.Amount, &r.BalanceBefore, &r.BalanceAfter, &orderID, &r.CardSecret, &r.Remark,
			&operatorID, &r.OperatorName, &r.CreatedAt,
		)
		if errScan != nil {
			log.Printf("Error scanning wallet transaction row: %v", errScan)
			continue
		}

		if orderID.Valid {
			r.OrderID = &orderID.Int64
		}
		if operatorID.Valid {
			r.OperatorID = &operatorID.Int64
		}

		records = append(records, r)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"records":   records,
	})
}

// handleAdminJioWalletCreatePayOrder 用户调用支付接口发起充值订单 (通过 Epay 或 虎皮椒)
func handleAdminJioWalletCreatePayOrder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 POST 请求",
		})
		return
	}

	adminID, ok := getAdminID(r)
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "未登录或登录已过期",
		})
		return
	}

	var req struct {
		Amount float64 `json:"amount"`
		Type   string  `json:"type"` // "wxpay" or "alipay"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "请求格式无效",
		})
		return
	}

	if req.Amount <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "充值金额必须大于 0",
		})
		return
	}

	if req.Type == "" {
		req.Type = "wxpay"
	}

	outTradeNo := fmt.Sprintf("JW%d%04d", time.Now().Unix(), time.Now().Nanosecond()%10000)
	totalAmountStr := fmt.Sprintf("%.2f", req.Amount)
	now := time.Now()

	payMethod := getSetting("pay_method", "epay")

	_, errInsert := db.Exec(`
		INSERT INTO jio_wallet_orders (out_trade_no, admin_id, amount, pay_type, pay_method, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)`,
		outTradeNo, adminID, req.Amount, req.Type, payMethod, now, now)
	if errInsert != nil {
		log.Printf("Failed to insert jio_wallet_order: %v\n", errInsert)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "生成充值订单失败",
		})
		return
	}

	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	notifyURL := fmt.Sprintf("%s://%s/api/pay/notify", scheme, r.Host)
	returnURL := fmt.Sprintf("%s://%s/admin/jio_wallet.html?out_trade_no=%s", scheme, r.Host, outTradeNo)

	var payURL string
	if payMethod == "xunhupay" {
		gateway := getSetting("xunhupay_url", "https://api.xunhupay.com")
		var appID, secret string
		if req.Type == "wxpay" {
			appID = getSetting("xunhupay_wx_appid", "")
			secret = getSetting("xunhupay_wx_secret", "")
		} else if req.Type == "alipay" {
			appID = getSetting("xunhupay_alipay_appid", "")
			secret = getSetting("xunhupay_alipay_secret", "")
		}

		if appID == "" || secret == "" {
			respondJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "支付通道未配置，请联系管理员",
			})
			return
		}

		xunhu := NewXunhuPay(appID, secret, gateway)
		data, err := xunhu.CreatePayment(outTradeNo, req.Amount, fmt.Sprintf("Jio储值钱包充值 ￥%.2f", req.Amount), notifyURL, returnURL, "")
		if err != nil {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("发起支付失败: %v", err),
			})
			return
		}

		var ok bool
		payURL, ok = data["url"].(string)
		if !ok || payURL == "" {
			respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": "未获取到有效的支付链接",
			})
			return
		}
	} else {
		pid := getSetting("epay_pid", "1668")
		key := getSetting("epay_key", "")
		apiURL := getSetting("epay_url", "https://pay.vansdesign.cn/")
		wxChannel := getSetting("epay_wx_channel", "201906181353")
		alipayChannel := getSetting("epay_alipay_channel", "")

		params := map[string]string{
			"pid":          pid,
			"type":         req.Type,
			"out_trade_no": outTradeNo,
			"notify_url":   notifyURL,
			"return_url":   returnURL,
			"name":         fmt.Sprintf("Jio储值钱包充值 ￥%.2f", req.Amount),
			"money":        totalAmountStr,
		}

		if req.Type == "wxpay" && wxChannel != "" {
			params["channel"] = wxChannel
		} else if req.Type == "alipay" && alipayChannel != "" {
			params["channel"] = alipayChannel
		}

		sign := calculateEpaySign(params, key)
		params["sign"] = sign
		params["sign_type"] = "MD5"

		submitBase := apiURL
		if !strings.HasSuffix(submitBase, "/") {
			submitBase += "/"
		}
		submitURL := submitBase + "submit.php"

		var queryParts []string
		for k, v := range params {
			queryParts = append(queryParts, fmt.Sprintf("%s=%s", k, url.QueryEscape(v)))
		}
		payURL = submitURL + "?" + strings.Join(queryParts, "&")
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"out_trade_no": outTradeNo,
		"amount":       req.Amount,
		"pay_url":      payURL,
	})
}

// handleAdminJioWalletOrderStatus 轮询检查钱包充值订单支付状态
func handleAdminJioWalletOrderStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 GET 请求",
		})
		return
	}

	outTradeNo := strings.TrimSpace(r.URL.Query().Get("out_trade_no"))
	if outTradeNo == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "out_trade_no 不能为空",
		})
		return
	}

	var status string
	var amount float64
	err := db.QueryRow("SELECT status, amount FROM jio_wallet_orders WHERE out_trade_no = ?", outTradeNo).Scan(&status, &amount)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"message": "未找到该充值订单",
		})
		return
	} else if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "查询数据库失败",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"out_trade_no": outTradeNo,
		"status":       status,
		"amount":       amount,
	})
}

// handleAdminJioWalletSelfRecharge 管理员自助直接充值/扣款 (仅超级管理员可用，普通用户必须走在线支付)
func handleAdminJioWalletSelfRecharge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 POST 请求",
		})
		return
	}

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
	if role != "admin" {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "普通用户请使用在线支付通道进行钱包充值",
		})
		return
	}

	var req struct {
		Amount float64 `json:"amount"`
		Remark string  `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "请求数据格式错误",
		})
		return
	}

	if req.Amount == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "变动金额不能为 0",
		})
		return
	}

	remark := strings.TrimSpace(req.Remark)
	txType := "admin_recharge"
	actionName := "充值"
	if req.Amount < 0 {
		txType = "admin_deduct"
		actionName = "扣款"
		if remark == "" {
			remark = "超级管理员自助扣款"
		}
	} else if remark == "" {
		remark = "超级管理员自助充值"
	}

	newBalance, txID, err := AddJioWalletBalance(adminID, req.Amount, txType, remark, adminID, nil, "")
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("%s失败: %v", actionName, err),
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"message":     fmt.Sprintf("成功%s ￥%.2f，当前余额 ￥%.2f", actionName, math.Abs(req.Amount), newBalance),
		"new_balance": newBalance,
		"tx_id":       txID,
	})
}

// handleAdminJioWalletAdminRecharge 超级管理员为其他用户充值或设置负数扣款 (仅超级管理员可用)
func handleAdminJioWalletAdminRecharge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"message": "仅支持 POST 请求",
		})
		return
	}

	operatorID, ok := getAdminID(r)
	if !ok {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "未登录或登录已过期",
		})
		return
	}

	var opRole string
	_ = db.QueryRow("SELECT role FROM admins WHERE id = ?", operatorID).Scan(&opRole)
	if opRole != "admin" {
		respondJSON(w, http.StatusForbidden, map[string]interface{}{
			"success": false,
			"message": "仅超级管理员有权执行人工充值或扣款操作",
		})
		return
	}

	var req struct {
		TargetAdminID int64   `json:"target_admin_id"`
		Amount        float64 `json:"amount"`
		Remark        string  `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "请求数据格式错误",
		})
		return
	}

	if req.TargetAdminID <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "请选择有效的操作目标用户",
		})
		return
	}

	if req.Amount == 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "变动金额不能为 0",
		})
		return
	}

	var targetUsername string
	errTarget := db.QueryRow("SELECT username FROM admins WHERE id = ?", req.TargetAdminID).Scan(&targetUsername)
	if errTarget != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "目标用户不存在",
		})
		return
	}

	remark := strings.TrimSpace(req.Remark)
	txType := "admin_recharge"
	actionName := "充值"
	if req.Amount < 0 {
		txType = "admin_deduct"
		actionName = "扣款"
		if remark == "" {
			remark = "管理员人工扣款"
		}
	} else if remark == "" {
		remark = "管理员人工代充入账"
	}

	newBalance, txID, err := AddJioWalletBalance(req.TargetAdminID, req.Amount, txType, remark, operatorID, nil, "")
	if err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": fmt.Sprintf("%s失败: %v", actionName, err),
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"message":         fmt.Sprintf("成功为用户 [%s] %s ￥%.2f，该账户当前余额 ￥%.2f", targetUsername, actionName, math.Abs(req.Amount), newBalance),
		"target_admin_id": req.TargetAdminID,
		"target_username": targetUsername,
		"new_balance":     newBalance,
		"tx_id":           txID,
	})
}
