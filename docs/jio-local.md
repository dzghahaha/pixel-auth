# Jio 自有链接

### 1. 独立库存链接菜单（仅超级管理员）
自有 Jio 兑换链接库存拥有独立的后台菜单与管理页面 **Jio 库存链接 (`/admin/jio_links.html`)**，不再放在 Jio 供应商列表内，且仅限超级管理员 (`role: admin`) 拥有菜单访问与操作权限：
- 顶部概览指标卡：未使用库存链接 (Available)、已发放链接 (Issued)、录入库存总量 (Total)。
- 批量录入：每行粘贴一个兑换链接，单次最多支持 1000 条，每条最多 4096 字节。自动去重防重入，非法行整批拒绝。
- 明细检索与流转：支持按全部/未使用/已发放筛选，支持一键复制链接、分页流转明细展示。

### 2. 供应商配置与采购
管理员进入 **Jio 供应商 → Jio（自有）**：
- 在“供应商参数”填写 Jio 链接采购价格（USD，最多四位小数）。客户端售价仍由“Jio 价格管理”计算。
- 启用供应商，然后设为指定主力，或使用自动最低价策略。自有供应商有库存和有效价格即可参与自动优选，无需上游密钥、余额或充值。

## 数据和接口

启动服务时自动创建 `jio_links` 和 `jio_local_orders`，无需手动执行 SQL。新供应商默认禁用。

- `GET /api/admin/jio/suppliers/local/links?page=1&status=available`：库存统计与分页列表；省略 status 查询全部（严格限制超级管理员 requireSuperAdmin）。
- `POST /api/admin/jio/suppliers/local/links`：JSON `{ "links": ["https://example.com/a", "https://example.com/b"] }`，返回 `added` 和 `duplicate`（严格限制超级管理员 requireSuperAdmin）。
- 定价沿用供应商配置接口，provider 为 `local`，config 为 `{ "price_usd": 0.25 }`。
- 试买沿用供应商购买接口，product_id 为 `jio`，必须提供唯一的 `customer_reference`。同一次购买重试必须使用相同标识和数量。

库存发放和交付结果在同一事务中保存，数据库锁（`FOR UPDATE SKIP LOCKED`）及唯一请求摘要防止并发重复发放。卡密兑换重试会优先恢复已提交的本地交付结果。

## 验证

在独立本地 MySQL 测试库通过了输入校验、导入去重、事务回滚、并发领取、幂等重试、自动选择、禁用校验和卡密兑换结算测试。24 个并发请求（12 对重复请求）只发放 12 条链接。卡密重复兑换只发放、扣款一次。

`scripts/test-jio-local-ui.py` 使用模拟 API 验证价格配置、批量录入、错误恢复、链接安全展示与桌面/手机布局；需要 Python Playwright 和 Chrome。

全量测试存在 7 项失败，在提交 `55e77fa` 的独立副本中也复现：TestHandleSubmitAndQueryAPI、TestDuplicateSubmitPreventedWhenQueuingOrRunning、TestVendorIntegration、TestMaintenanceModeAndResume、TestJioRedeemSuccess、TestAdminSettingsJioProvider、TestJioDispatchStrategy。
