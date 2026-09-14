# Pixel 与 Jio 双订阅体系架构设计文档

**文档标识**: `docs/superpowers/specs/2026-09-14-pixel-and-jio-dual-subscription-system-design.md`  
**创建日期**: 2026-09-14  
**状态**: Approved / Ready for Implementation Planning  

---

## 1. 背景与目标

本系统原为单一的 Google One Pixel 订阅处理系统（涉及卡密输入、Google 账号密码及 2FA 提交、设备编排或第三方上游自动化代绑）。  
现需在系统中扩充新增 **Jio 订阅服务**，将系统整体升级为 **Pixel 与 Jio 双订阅并行的综合订阅管理与兑换平台**。

### 核心业务目标
1. **业务与数据双分类**：卡密（`system_keys`、`card_stock`）与订单（`orders`）支持按业务类型分类（`pixel` 与 `jio`）。
2. **C 端独立兑换页面 (`/jio.html`)**：
   - 用户只需输入卡密，即可在下方获取一串订阅兑换链接；
   - 包含明确规范的 3 步使用指引与 6~12 小时失效警示提示；
   - 提供高可读性等宽链接展示、一键复制到剪贴板与直接新标签页打开功能。
3. **可扩展的第三方 Provider 架构（策略模式）**：
   - 针对 Jio 获取订阅链接依赖第三方 API 的特性，建立可扩展的 Provider 接口及枚举注册中心；
   - 后台系统设置支持切换启用哪一个第三方 Provider，便于后续对接具体第三方厂商 API 文档。
4. **管理端协同分类**：
   - 密钥管理、订单管理均支持按业务类型筛选与彩色徽章标识；
   - 卡密生成与密钥转换支持选择生成/录入卡密的业务类型。

---

## 2. 数据模型与数据库迁移方案

### 2.1 数据库结构升级
系统在初始化启动时通过 `db.go` 执行平滑变更（`ALTER TABLE ... ADD COLUMN` 容错）：

1. **`system_keys` 表**：
   - 新增字段：`service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
   - 新增索引：`KEY idx_sk_service_type (service_type)`
   - 历史存量数据自动归属于 `pixel`。

2. **`card_stock` 表**：
   - 新增字段：`service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
   - 新增索引：`KEY idx_cs_service_type (service_type)`

3. **`orders` 表**：
   - 新增字段：`service_type VARCHAR(32) NOT NULL DEFAULT 'pixel'`
   - 新增索引：`KEY idx_orders_service_type (service_type)`

4. **`account_records` 表**：
   - Jio 模式复用现有字段：`discount_url` 存储获取到的 Jio 订阅链接，`status` 标记为 `success`，`message` 记录为 `兑换链接获取成功`，`username` 填 `-`，`completed_at` 记当前时间。

5. **`system_settings` 表**：
   - 预设项：`jio_active_provider`（默认 `'mock'`）
   - 预设项：`jio_provider_config`（预留 JSON 格式配置）

---

## 3. 后端架构与接口设计

### 3.1 Jio Provider 策略架构 (`jio_providers.go`)

```go
// JioProvider 定义第三方获取链接标准适配器接口
type JioProvider interface {
    Name() string
    GetOfferLink(ctx context.Context, cardSecret, vendorKey string) (link string, err error)
}
```

- **注册器结构**：
  - `var jioProviders = map[string]JioProvider{}`
  - `func RegisterJioProvider(p JioProvider)`
  - `func GetActiveJioProvider() JioProvider`
- **首期实现**：
  - `MockJioProvider`（名称：`"mock"`）：基于测试规则生成规范格式的 Mock 链接（如 `https://one.google.com/promo/hasoffer?token=...`），保证首期自动化测试与完整业务闭环。
  - 预留接口实现槽位，后续获得真实 API 文档后热插拔接入。

### 3.2 C 端专属兑换接口
- **路径**：`POST /api/jio/redeem`
- **权限**：公开公开限流接口
- **请求体**：
  ```json
  {
    "card_secret": "JIO-SAMPLE-CARD-KEY"
  }
  ```
- **处理逻辑**：
  1. 校验入参 `card_secret` 非空；
  2. 查询 `system_keys`：
     - 若卡密不存在，返回 `400 "卡密不存在，请检查输入"`；
     - 若卡密 `service_type != 'jio'`，返回 `400 "此卡密非 Jio 专用卡密，请前往对应页面使用"`；
     - 若状态已为 `inactive`，查询 `orders` 和 `account_records`，若历史已有该卡密成功的兑换记录，则返回已有记录（幂等回显）；若无成功记录则报错 `400 "卡密已作废或已被使用"`；
  3. 开启单并发卡密内存锁（防重提交）；
  4. 调度当前启用的 `JioProvider.GetOfferLink` 获取兑换链接；
  5. 数据库事务落盘：
     - `UPDATE system_keys SET status = 'inactive', updated_at = NOW() WHERE system_key = ?`；
     - 插入 `orders (card_secret, mode, vendor, service_type, created_at, updated_at)`；
     - 插入 `account_records (order_id, card_secret, username, password, two_factor, status, message, discount_url, completed_at, ...)`；
  6. 返回成功 JSON 与数据：
     ```json
     {
       "success": true,
       "message": "获取成功",
       "data": {
         "card_secret": "JIO-SAMPLE-CARD-KEY",
         "offer_url": "https://one.google.com/promo/...",
         "created_at": "2026-09-14 16:00:00"
       }
     }
     ```

### 3.3 订单通用查询接口 (`/api/query`) 兼容
- 返回的订单详情中补全 `service_type` 字段；
- 前端查询页 `/query.html` 当识别到订单为 `jio` 时，直接高亮展示兑换链接卡片及使用指导。

---

## 4. 前端界面设计与交互规范

### 4.1 C 端独立页面 (`/frontend/jio.html`)
- **文件与访问**：位于 `frontend/jio.html`，静态独立页面。
- **视觉风格**：严格遵循 `Impeccable` 与产品设计标准（Slate 50 底色、Indigo 600 主色、极简微质感阴影、4.5:1 对比度保证）。
- **页面模块结构**：
  1. **顶栏 Header**：
     - 左侧：“Jio 订阅” 标题 + “兑换专区” 徽章；
     - 右侧：“订单查询” 按钮。
  2. **使用说明卡片 (Callout Box)**：
     - 标题：`📖 兑换链接使用说明`
     - 步骤列表：
       - ① 手机或者电脑登录好你要领取的账号
       - ② 将收到的兑换链接粘贴到您的谷歌浏览器中打开
       - ③ 然后单击”蓝色按钮“激活优惠。您的将成功激活。
     - 警示条目：`⏳ 提示：兑换链接 6~12 小时内失效，请尽快完成激活！`
  3. **卡密核销卡片**：
     - 单个卡密输入框（占位符：“请输入 Jio 卡密”）；
     - “获取兑换链接” 主操作按钮，带加载状态与防重复点击。
  4. **成功结果展示区域**：
     - 获取成功后平滑展开；
     - 等宽字体只读链接框；
     - “复制链接” 快捷按钮（点击 Toast 反馈“复制成功”）；
     - “直接在谷歌浏览器中打开 ↗” 链接。
  5. **底部快速查询**：
     - 支持输入已核销过的 Jio 卡密快速找回已有链接。

### 4.2 管理后台改造
1. **密钥管理 (`frontend/admin/keys.html`)**：
   - 过滤栏增加【业务分类】筛选：全部 / Pixel 订阅 / Jio 订阅；
   - 列表增加【业务】列：展示彩色状态徽章（`Pixel` 蓝灰 / `Jio` 绿色）；
   - API 查询接口 `/api/admin/keys` 支持 `service_type` 参数。
2. **订单管理 (`frontend/admin/orders.html`)**：
   - 过滤栏增加【业务分类】筛选：全部 / Pixel 订单 / Jio 订单；
   - 表格增加【业务】列展示；
   - API 查询接口 `/api/admin/orders` 支持 `service_type` 参数。
3. **卡密生成 (`frontend/admin/generate.html`)**：
   - 表单增加【业务类型】单选/下拉选择项（默认 Pixel，可选 Jio）；
   - 对应生成的库存卡密写入 `service_type`。
4. **密钥转换 (`frontend/admin/convert.html`)**：
   - 表单增加【业务类型】选择项；
   - 转换生成的系统卡密存库时写入指定 `service_type`。
5. **系统设置 (`frontend/admin/settings.html`)**：
   - 增加 Jio API 配置模块，可选择当前生效的 Provider 枚举。

---

## 5. 质量门禁与测试验证策略

依据 `GEMINI.md` 与工程质量规范，执行如下验证门禁：
1. **隔离单元与集成测试 (`main_test.go`)**：
   - 针对单用例精准运行：`go test -v -run TestJioRedeemSuccess`、`TestJioRedeemCardTypeMismatch`、`TestJioRedeemDuplicateKey`；
   - 验证 `system_keys` 的 `service_type` 过滤与写入；
   - 验证 `orders` 的 `service_type` 分类与关联查询。
2. **Go 编译门禁**：
   - 执行 `go build -o pixel-auth.exe .` 确保零编译告警与错误。
3. **前端代码审查与体验验证**：
   - 验证 `/jio.html` 在桌面与移动端浏览器视图下的响应式布局、文本对比度及交互反馈。

---

## 6. 自审核对表 (Self-Review Checklist)
- [x] **无占位符**：无未定义或 TBD/TODO 的内容。
- [x] **架构一致性**：Provider 模式与数据库字段及 API 对应一致。
- [x] **边界条件覆盖**：涵盖卡密不存在、卡密分类不匹配、卡密重复兑换幂等处理等。
- [x] **用户意图完全对齐**：独立 `/jio.html` 页面、3步指引、6~12小时失效提示、卡密与订单分类。
