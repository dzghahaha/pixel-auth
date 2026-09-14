# Pixel 与 Jio 双订阅体系实施计划 (Implementation Plan)

**文档标识**: `docs/superpowers/plans/2026-09-14-pixel-and-jio-dual-subscription-system-plan.md`  
**设计文档依据**: `docs/superpowers/specs/2026-09-14-pixel-and-jio-dual-subscription-system-design.md`  
**日期**: 2026-09-14  

---

## 阶段规划与 WBS 任务分解

### Task 1: 数据库结构平滑迁移与基础 Model 适配
- **目标**: 在 `db.go` 中加入 `service_type` 字段的平滑迁移逻辑与索引，扩充 `CardOrder` 与 `system_keys`、`card_stock` 相关结构。
- **涉及文件**: `db.go`
- **步骤**:
  1. `system_keys`: `ALTER TABLE system_keys ADD COLUMN service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
  2. `card_stock`: `ALTER TABLE card_stock ADD COLUMN service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
  3. `orders`: `ALTER TABLE orders ADD COLUMN service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
  4. 增加相应字段的索引（容错忽略已存在）。
  5. 初始 settings: `jio_active_provider = 'mock'`。
- **验证方式**: 运行 `go build .` 编译无错。

---

### Task 2: Jio Provider 策略模式与适配器架构
- **目标**: 实现可插拔的 Jio Provider 架构，提供注册器与首期 `MockJioProvider`。
- **涉及文件**: 新建 `jio_providers.go`
- **步骤**:
  1. 定义 `JioProvider` 接口；
  2. 实现注册机制 `RegisterJioProvider`、`GetActiveJioProvider`；
  3. 实现首期 `MockJioProvider`，生成有效格式测试链接（如 `https://one.google.com/promo/hasoffer?token=...`）；
  4. 预留第三方 API 适配器类骨架。
- **验证方式**: 编写针对 `JioProvider` 的单元测试。

---

### Task 3: Jio C 端专属核销接口与跨业务查询
- **目标**: 暴露 `POST /api/jio/redeem`，并兼容 `/api/query` 跨业务查询。
- **涉及文件**: `client_handlers.go`, `main.go`
- **步骤**:
  1. 路由注册：`http.HandleFunc("/api/jio/redeem", limit(handleJioRedeem))`
  2. 实现 `handleJioRedeem`：
     - 校验 `card_secret` 非空；
     - 检查 `system_keys`：存在性、`service_type == 'jio'`、状态；
     - 并发防重锁；
     - 调用 Provider 获取兑换链接；
     - 事务写入 `orders` (`service_type='jio'`)、`account_records`，更新卡密为 `inactive`；
     - 幂等处理（已兑换过的返回历史链接）。
  3. 在 `handleQuery` 中返回 `service_type`。
- **验证方式**: 在 `main_test.go` 中编写专用测试用例 `TestJioRedeemSuccess` 与 `TestJioRedeemCardTypeMismatch`，验证通过。

---

### Task 4: C 端独立页面 `frontend/jio.html`
- **目标**: 打造专属于 Jio 订阅的高颜值独立页面，包含规范的三步使用说明与 6~12 小时失效警示。
- **涉及文件**: 新建 `frontend/jio.html`
- **步骤**:
  1. 遵照 `Impeccable` 与系统 Slate & Indigo 调色盘；
  2. 页面顶栏（标题、徽章、订单查询跳转）；
  3. 兑换链接使用说明卡片（①账号登录 ②谷歌浏览器打开 ③蓝色按钮激活，以及 6~12 小时失效提示）；
  4. 卡密输入框与“获取兑换链接”主按钮；
  5. 链接展示卡片（等宽只读输入框、一键复制到剪贴板、直接在新标签页打开）；
  6. 底部快捷查询卡密已有链接。
- **验证方式**: 浏览器中访问 `/jio.html` 进行视觉与交互核验。

---

### Task 5: 管理端分类筛选与卡密生成/转换改造
- **目标**: 管理端全面协同支持 Pixel 与 Jio 业务分类。
- **涉及文件**: `admin_handlers.go`, `vendor.go`, `frontend/admin/keys.html`, `frontend/admin/orders.html`, `frontend/admin/generate.html`, `frontend/admin/convert.html`
- **步骤**:
  1. `admin_handlers.go`:
     - `handleAdminKeys` 支持 `service_type` 过滤与查询输出；
     - `handleAdminOrders` 支持 `service_type` 过滤与查询输出；
     - `handleGenerateStockKeys` 支持传入 `service_type` 参数入库。
  2. `vendor.go`:
     - `handleConvertKeys` 支持传入 `service_type` 参数入库。
  3. `keys.html` & `orders.html`:
     - 增加业务分类筛选下拉框；
     - 表格增加业务分类彩色徽标。
  4. `generate.html` & `convert.html`:
     - 增加业务分类单选/下拉选择项。
- **验证方式**: 针对 `handleAdminKeys` 和 `handleConvertKeys` 进行针对性单测。

---

### Task 6: 完整编译构建与全链路自动化验证
- **目标**: 确保所有代码 0 告警通过 Go 编译，精准测试用例全部通过。
- **步骤**:
  1. 运行 `go test -v -run TestJioRedeem`；
  2. 运行 `go test -v -run TestConvertKeysWithServiceType`；
  3. 运行 `go build -o pixel-auth.exe .` 验证无编译错误；
  4. 提交 Git commit。
