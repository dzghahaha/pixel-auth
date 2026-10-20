# Jio 自有链接

管理员进入 **Jio 供应商 → Jio（自有）**：

1. 在“供应商参数”填写 Jio 链接采购价格（USD，最多四位小数）。客户端售价仍由“Jio 价格管理”计算。
2. 点击“管理链接库存”，每行粘贴一个兑换链接，点击“录入链接”。每次最多 1000 条，每条最多 4096 字节。
3. 启用供应商，然后设为指定主力，或使用自动最低价策略。自有供应商有库存和有效价格即可参与自动优选，无需上游密钥、余额或充值。

导入时清理首尾空白、忽略空行，重复链接（包括已经发放的链接）会被跳过。无效行会阻止整批导入，并显示行号。链接只做格式校验，不向链接目标发送请求。

库存表可以按未使用、已发放筛选，每页 50 条，并显示发放订单和时间。试买会实际消耗库存。正常兑换按录入顺序发放；库存不足时整单失败，不部分发放。

## 数据和接口

启动服务时自动创建 `jio_links` 和 `jio_local_orders`，无需手动执行 SQL。新供应商默认禁用。

- `GET /api/admin/jio/suppliers/local/links?page=1&status=available`：库存统计与分页列表；省略 status 查询全部。
- `POST /api/admin/jio/suppliers/local/links`：JSON `{ "links": "https://example.com/a\nhttps://example.com/b" }`，返回 `added` 和 `duplicates`。
- 定价沿用供应商配置接口，provider 为 `local`，config 为 `{ "price_usd": 0.25 }`。
- 试买沿用供应商购买接口，product_id 为 `jio`，必须提供唯一的 `customer_reference`。同一次购买重试必须使用相同标识和数量。

所有库存管理接口使用现有 `jio_suppliers` 权限。库存发放和交付结果在同一事务中保存，数据库锁及唯一请求摘要防止并发重复发放。卡密兑换重试会优先恢复已提交的本地交付结果。

## 验证

在独立本地 MySQL 测试库通过了输入校验、导入去重、事务回滚、并发领取、幂等重试、自动选择、禁用校验和卡密兑换结算测试。24 个并发请求（12 对重复请求）只发放 12 条链接。卡密重复兑换只发放、扣款一次。

`scripts/test-jio-local-ui.py` 使用模拟 API 验证价格配置、批量录入、错误恢复、链接安全展示与桌面/手机布局；需要 Python Playwright 和 Chrome。

全量测试存在 7 项失败，在提交 `55e77fa` 的独立副本中也复现：TestHandleSubmitAndQueryAPI、TestDuplicateSubmitPreventedWhenQueuingOrRunning、TestVendorIntegration、TestMaintenanceModeAndResume、TestJioRedeemSuccess、TestAdminSettingsJioProvider、TestJioDispatchStrategy。
