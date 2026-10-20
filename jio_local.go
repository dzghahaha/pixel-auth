package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// Local inventory and its receipt are committed together. A unique request hash
// serializes retries even when multiple server processes share the database.
func createJioLocalTables() error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS jio_local_orders (
		 request_hash BINARY(32) PRIMARY KEY,
		 order_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE,
		 quantity INT NOT NULL, result_json MEDIUMTEXT,
		 created_at DATETIME NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS jio_links (
		 id BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
		 link TEXT NOT NULL, link_hash BINARY(32) NOT NULL UNIQUE,
		 status VARCHAR(16) NOT NULL DEFAULT 'available',
		 order_id VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
		 created_at DATETIME NOT NULL, issued_at DATETIME NULL,
		 INDEX idx_jio_links_stock (status, id), INDEX idx_jio_links_order (order_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			return err
		}
	}
	return nil
}

func localJioPrice(cfg map[string]interface{}) (float64, error) {
	price, ok := cfg["price_usd"].(float64)
	if !ok || math.IsNaN(price) || math.IsInf(price, 0) || price < 0.0001 || price > 1000000 {
		return 0, errors.New("请输入有效采购价格（USD，0.0001 至 1000000）")
	}
	return math.Round(price*10000) / 10000, nil
}

type LocalJioProvider struct{}

var errLocalJioCommitUnknown = errors.New("确认发放失败，请使用相同订单标识重试")

func localJioReceipt(ctx context.Context, key string) (*SupplierPurchaseResult, error) {
	hash := sha256.Sum256([]byte(key))
	var raw sql.NullString
	err := db.QueryRowContext(ctx, `SELECT result_json FROM jio_local_orders WHERE request_hash=?`, hash[:]).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("读取自有 Jio 发放记录失败")
	}
	if !raw.Valid {
		return nil, nil
	}
	var result SupplierPurchaseResult
	if err := json.Unmarshal([]byte(raw.String), &result); err != nil {
		return nil, errors.New("读取自有 Jio 发放结果失败")
	}
	return &result, nil
}

func (*LocalJioProvider) Name() string        { return "local" }
func (*LocalJioProvider) DisplayName() string { return "Jio（自有）" }
func (*LocalJioProvider) GetBalance(context.Context) (*SupplierBalance, error) {
	return &SupplierBalance{Supported: false, Status: "平台自有库存，无需上游余额"}, nil
}
func (*LocalJioProvider) CreateDeposit(context.Context, float64) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{Supported: false}, nil
}
func (*LocalJioProvider) GetDepositStatus(context.Context, string) (*SupplierDepositResult, error) {
	return &SupplierDepositResult{Supported: false}, nil
}
func (*LocalJioProvider) GetProducts(ctx context.Context) ([]SupplierProduct, error) {
	var stock int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM jio_links WHERE status='available'`).Scan(&stock); err != nil {
		return nil, errors.New("读取自有 Jio 库存失败")
	}
	price, err := localJioPrice(GetSupplierConfig("local"))
	return []SupplierProduct{{ID: "jio", Name: "Jio 链接", PriceUSD: price, DeliveryType: "stock", Stock: &stock, IsActive: err == nil}}, nil
}
func (p *LocalJioProvider) GetOfferLink(ctx context.Context, cardSecret, _ string) (string, error) {
	if strings.TrimSpace(cardSecret) == "" {
		return "", errors.New("缺少兑换订单标识")
	}
	result, err := p.Purchase(ctx, SupplierPurchaseRequest{ProductID: "jio", Quantity: 1, IdempotencyKey: "redeem:" + cardSecret})
	if err != nil {
		return "", err
	}
	recordJioPurchaseCost(ctx, result)
	return result.Link, nil
}

func (*LocalJioProvider) Purchase(ctx context.Context, req SupplierPurchaseRequest) (*SupplierPurchaseResult, error) {
	if req.ProductID != "" && req.ProductID != "jio" {
		return nil, errors.New("自有供应商仅提供 Jio 链接")
	}
	if req.Quantity == 0 {
		req.Quantity = 1
	}
	if req.Quantity < 1 || req.Quantity > 100 {
		return nil, errors.New("购买数量须为 1 至 100")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(req.CustomerReference)
	}
	if key == "" || len(key) > 512 {
		return nil, errors.New("必须提供有效订单标识")
	}
	if !isJioSupplierEnabled("local") {
		return nil, errors.New("自有 Jio 供应商已禁用")
	}
	// Read configuration before holding a transaction connection, so concurrent
	// purchases cannot exhaust the pool while waiting for another connection.
	price, priceErr := localJioPrice(GetSupplierConfig("local"))
	hash := sha256.Sum256([]byte(key))
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	orderID := "local-" + hex.EncodeToString(token[:])
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, errors.New("创建发放事务失败")
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO jio_local_orders (request_hash,order_id,quantity,created_at) VALUES (?,?,?,NOW()) ON DUPLICATE KEY UPDATE request_hash=request_hash`, hash[:], orderID, req.Quantity); err != nil {
		return nil, errors.New("保存发放订单失败")
	}
	var previous sql.NullString
	var quantity int
	if err = tx.QueryRowContext(ctx, `SELECT order_id,quantity,result_json FROM jio_local_orders WHERE request_hash=? FOR UPDATE`, hash[:]).Scan(&orderID, &quantity, &previous); err != nil {
		return nil, errors.New("读取发放订单失败")
	}
	if quantity != req.Quantity {
		return nil, errors.New("同一订单的购买数量不能改变")
	}
	if previous.Valid {
		var result SupplierPurchaseResult
		if err = json.Unmarshal([]byte(previous.String), &result); err != nil {
			return nil, errors.New("读取已发放结果失败")
		}
		return &result, nil
	}
	if priceErr != nil {
		return nil, priceErr
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,link FROM jio_links WHERE status='available' ORDER BY id LIMIT ? FOR UPDATE SKIP LOCKED`, req.Quantity)
	if err != nil {
		return nil, errors.New("锁定自有 Jio 库存失败")
	}
	ids := make([]int64, 0, req.Quantity)
	items := make([]string, 0, req.Quantity)
	for rows.Next() {
		var id int64
		var link string
		if err = rows.Scan(&id, &link); err != nil {
			rows.Close()
			return nil, errors.New("读取库存失败")
		}
		ids = append(ids, id)
		items = append(items, link)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, errors.New("读取库存失败")
	}
	if len(items) != req.Quantity {
		return nil, errors.New("自有 Jio 链接库存不足")
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `UPDATE jio_links SET status='issued',order_id=?,issued_at=NOW() WHERE id=?`, orderID, id); err != nil {
			return nil, errors.New("更新发放库存失败")
		}
	}
	result := &SupplierPurchaseResult{OrderID: orderID, Link: strings.Join(items, "\n"), Items: items, AmountUSD: math.Round(price*float64(req.Quantity)*10000) / 10000, Status: "COMPLETED"}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("生成发放结果失败: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE jio_local_orders SET result_json=? WHERE request_hash=?`, raw, hash[:]); err != nil {
		return nil, errors.New("保存发放结果失败")
	}
	if err = tx.Commit(); err != nil {
		return nil, errLocalJioCommitUnknown
	}
	invalidateJioSupplierQuotes()
	return result, nil
}
