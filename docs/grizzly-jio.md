# Grizzly Jio 供应商

## 启用

重新编译并启动服务后，进入「Jio 供应商」→「Grizzly」→「供应商参数」，填写 Grizzly API Key，保存后点击「启用供应商」。无需部署 Python，也不会读取原脚本目录的 `.env` 或历史激活链接。

默认参数：号码最高采购价 **0.208 USD**、单次获取超时 **300 秒**、最多 **5 个号码**。服务固定为 MyJio（`jio`），国家为印度（`22`），使用现有 Jio 专网代理配置。现有供应商默认启用；新增 Grizzly 默认禁用。

Grizzly 展示为「无限库存」。这是本系统的供货模式，不表示接码平台随时有号码：缺货时会轮询等待，直至获取号码或超时。自动优选仍要求供应商启用、配置有效、余额足够，并按价格排序。

号码最高采购价用于优选的保守报价和余额门槛。优先调用 `getNumberV2` 读取实际 `activationCost`；明确不支持该方法时才使用 `getNumber`。上游没有返回费用时，以配置的价格上限作为结算成本。失败号码会尝试取消；已接码或取消失败的号码可能仍产生上游费用，不承诺退款。协议参考：[Grizzly 官方 API 文档](https://grizzlysms.com/docs)。

禁用阻止后续自动调度、指定渠道和手动试买，不中断已发起的兑换。禁用当前指定供应商后，应启用它、指定其他渠道或切换自动优选。

## C 端进度

`POST /api/jio/redeem` 增加可选参数 `async: true`，现有同步调用保持兼容。异步请求验证卡密并预占后返回 HTTP 202：

```json
{"success":true,"data":{"status":"processing","task_id":"<随机任务标识>","progress":0}}
```

使用 `POST /api/jio/redeem/status`，请求体为 `{"task_id":"<任务标识>"}`，查询 `data.status`、`data.progress` 和完成后的 `data.result`。任务标识可访问对应兑换结果，应视同敏感信息，不放入 URL。

前端每 1.5 秒查询一次，并在 sessionStorage 保存任务标识。同一标签页刷新会恢复查询；使用同一卡密再次提交会加入正在执行的任务。网络断开不取消后台兑换。阶段内按预估时间推进百分比，超过估计时间停留在阶段上限；重试不回退。只在链接保存和钱包结算成功后显示 100%。

数据库启动迁移自动创建 `jio_redemption_tasks`。后台任务最长 10 分钟；进度、结果持久化。服务重启不会自动重复执行未完成的采购；超期任务提示人工核查，并保留可能已付费卡密的 `processing` 状态。管理员应核对上游订单、链接和钱包后恢复，不能直接批量解锁重购。

## 验证

新增 Go 测试使用本地 HTTP 模拟服务；集成测试必须设置指向独立测试库的 `MYSQL_TEST_DSN`（现有 `initTestDB` 会重建/清空测试表）。不使用真实 Grizzly Key 或购买号码。

```powershell
$env:MYSQL_TEST_DSN='root@tcp(127.0.0.1:33317)/pixel_auth_grizzly_test?parseTime=true&loc=Local'
go test ./... -run 'Test(Grizzly|JioAsync|JioExpired|JioSupplierDisable)' -count=1
python scripts/test-jio-grizzly-ui.py
```

浏览器测试需要 Python Playwright 和 Chrome，可通过 `JIO_UI_BROWSER` 指定浏览器路径。所有 API 请求均为拦截模拟，涵盖手机端百分比、刷新恢复、重复点击、失败恢复、后台开关、无限库存和配置表单。

本次回归中，`TestJioRedeemSuccess`、`TestAdminSettingsJioProvider`、`TestJioDispatchStrategy` 在原始提交 `a981a38` 上也失败：分别缺少钱包测试数据、仍假定默认渠道为 mock、仍假定自动优选包含 mock。本次新增测试、其余 Jio 相关回归和 `go vet` 通过；未进行真实付费接码验证。
