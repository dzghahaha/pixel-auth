# Pixel 订阅 Web 服务设计规格说明书 (Spec)

## 1. 概述
本项目是一个基于 Go 语言开发的前后端一体化 Web 服务，配套移动端自适应 H5 页面「Pixel 订阅」。系统提供卡密与账号数据的提交、处理状态查询等功能。

---

## 2. 目录结构
```text
pixel-auth/
├── docs/
│   └── superpowers/
│       └── specs/
│           └── 2026-06-17-pixel-auth-design.md # 本设计文档
├── main.go                                     # Go 后端入口，路由与API处理，本地JSON存储
├── data.json                                   # 本地数据存储（JSON格式，包含排队与处理记录）
└── frontend/                                   # 前端静态资源目录 (使用 go:embed 嵌入)
    ├── index.html                              # 移动端 H5 订阅页面
    ├── style.css                               # 原生 CSS 样式表
    └── app.js                                  # 前端交互及请求逻辑
```

---

## 3. UI 界面与交互规范
H5 页面采用移动端自适应设计，主色调为亮蓝色，警示色为红色，辅色为浅灰色，卡片使用白底圆角简约风格。

### 3.1 页面布局结构（从上至下）
1. **头部区域**：
   - 左侧：菱形图标 + 文字「Pixel 订阅」标题。
   - 右上角：**完全空白**，移除 API 文档按钮、显示排队数量的计时标签。
2. **黄色警示横幅模块**：
   - 左侧：黄色三角感叹号图标。
   - 主文字：「使用前请务必完成以下两步」。
   - 小字备注：「完成后再提交，可显著降低处理失败风险」。
   - 横向两个步骤卡片：
     - ① 蓝色数字 1：「开启二步验证 查看教程」（**移除右侧蓝色跳转箭头，无教程跳转入口**）。
     - ② 蓝色数字 2：「关闭支付资料 前往设置」（**移除右侧蓝色跳转箭头，无教程跳转入口**）。
3. **卡密主表单卡片**：
   - 标题区域：购物车图标 + 「卡密」标题，下方说明小字：「支持：1. 订阅 + 绑卡 2. 获取优惠链接 3. 反重力过扫码」。
   - 卡密输入框：单行输入框，占位文字：「请输入卡密」。
   - 模式切换按钮：双切换按钮，默认激活「单账号提交」，未激活「批量提价」（文字严格遵循「批量提价」）。
   - 动态输入区域：
     - **「单账号提交」模式**：垂直排列三组带 `*` 必填标识的输入项：
       1. 账号
       2. 密码
       3. 身份验证器密钥 (2FA) 或 8 位备用验证码
     - **「批量提价」模式**：显示一个大输入框（Textarea），占位提示文字：
       ```text
       每行一个账号，支持的格式：
       邮箱--密码--2FA
       邮箱--密码--辅助邮箱--2FA
       支持的分隔符：|,----,---,--,-
       ```
   - 提示行：文档图标 + 红色文字「每次提交前，请务必确保支付资料已关闭」（**无右侧跳转箭头**）。
   - 提交按钮：通栏蓝色大按钮，内部带有绿色星标 + 文字「提交处理」。
4. **底部查询模块**：
   - 横向组合：左侧输入框（占位：「输入卡密查询状态」）+ 右侧「查询」按钮。
   - 查询结果列表：若查询到多条账号状态，在此区域以卡片列表形式展示账号、状态标签（待处理、成功、失败）和详细备注信息。
   - 底部提示条：通栏浅蓝色，左侧放大镜图标 + 文字「开放查询・输入卡密即可查看订单」。

---

## 4. 后端设计与 API 接口
后端使用 Go 标准库 `net/http` 提供静态文件托管及 API 服务，数据持久化到本地 `data.json` 中，采用互斥锁保证并发安全。

### 4.1 数据模型 (Data Models)
```go
type AccountRecord struct {
    Username   string    `json:"username"`
    Password   string    `json:"password"`
    TwoFactor  string    `json:"two_factor"`
    ExtraEmail string    `json:"extra_email,omitempty"` // 辅助邮箱（批量模式可能解析出该字段）
    Status     string    `json:"status"`                // "pending" (待处理), "success" (成功), "failed" (失败)
    Message    string    `json:"message"`               // 处理结果备注
    CreatedAt  time.Time `json:"created_at"`
    UpdatedAt  time.Time `json:"updated_at"`
}

type CardOrder struct {
    CardSecret string          `json:"card_secret"` // 卡密（唯一标识）
    Mode       string          `json:"mode"`        // "single" 或 "batch"
    Records    []AccountRecord `json:"records"`     // 卡密关联的账号处理记录
}
```

### 4.2 接口 1: 提交卡密数据
* **请求路径**：`POST /api/submit`
* **Content-Type**：`application/json`
* **请求参数**：
  ```json
  {
    "card_secret": "KM-123456",
    "mode": "single",
    "accounts": [
      {
        "username": "test@gmail.com",
        "password": "mypassword",
        "two_factor": "JBSWY3DPEHPK3PXP"
      }
    ]
  }
  ```
* **逻辑处理**：
  1. 验证卡密不为空。
  2. 若卡密在 `data.json` 中不存在，初始化一条记录。
  3. 将提交的账号追加至该卡密对应的记录列表，初始状态设置为 `pending`。
  4. 持久化至 `data.json`。
* **响应参数**：
  ```json
  {
    "success": true,
    "message": "提交成功，已加入处理队列"
  }
  ```

### 4.4 接口 2: 查询处理状态
* **请求路径**：`GET /api/query?card_secret=KM-123456`
* **逻辑处理**：
  - 从 `data.json` 读取该卡密的所有记录并返回。
* **响应参数**：
  ```json
  {
    "success": true,
    "card_secret": "KM-123456",
    "records": [
      {
        "username": "test@gmail.com",
        "status": "pending",
        "message": "排队处理中",
        "updated_at": "2026-06-17T12:00:00Z"
      }
    ]
  }
  ```

---

## 5. 测试与验证策略
1. **单元测试**：针对后端的数据存储、并发读写、接口逻辑编写 Go 单元测试。
2. **解析测试**：针对批量文本解析规则编写前端/后端测试，确保多种分隔符（如 `|`、`----`、`---`、`--`、`-`）和不同格式（带或不带辅助邮箱）的正常解析。
3. **前端 UI 自适应测试**：模拟不同移动设备屏幕尺寸（iPhone SE, iPhone 12 Pro, Pixel 5）验证页面布局的自适应度，确保无水平滚动条。
