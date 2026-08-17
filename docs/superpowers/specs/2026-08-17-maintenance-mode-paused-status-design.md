# Design Spec: System Maintenance Mode & Paused Order Status

**Date**: 2026-08-17  
**Status**: Approved by User  
**Target System**: Pixel Auth (Go Backend + Vanilla JS/CSS Frontend)

---

## 1. Executive Summary

This feature introduces a **System Maintenance Mode** (`maintenance_mode`) and a new order status **`paused` (维护挂起)** to prevent tasks from entering the automated execution queue during system maintenance. 

When Maintenance Mode is enabled:
- Newly submitted account processing orders (`account_records`) receive the `paused` status instead of `pending`.
- Automated polling and vendor submission routines ignore `paused` orders, avoiding failure errors during maintenance.
- Super administrators can execute a **One-Click Batch Resume** action to transition all `paused` orders to `pending` once maintenance completes.

---

## 2. Status Lifecycle & Validation

### 2.1 New Order Status: `paused`
- **Database table**: `account_records`
- **Allowed Statuses**: `pending`, `running`, `success`, `failed`, `cancelled`, `paused`
- **Behavior**:
  - Excluded from automated vendor submission routines (`syncPendingAndInvalidate`, etc.).
  - Excluded from background orchestrator dispatchers.

### 2.2 System Setting: `maintenance_mode`
- **Database table**: `system_settings`
- **Keys**: `setting_key = 'maintenance_mode'`, `setting_value = 'off' | 'on'` (default `'off'`).
- **Permissions**: Managed via standard settings API (`/api/admin/settings`). Access restricted according to setting menu permissions.

---

## 3. API Specifications

### 3.1 Submission API (`/api/submit`)
- **Behavior Update**:
  - Before inserting `account_records`, check `maintenance_mode` setting value from DB.
  - If `maintenance_mode == "on"`, set initial record status to `"paused"` and message to `"系统维护中，已挂起"`.
  - Otherwise, preserve existing logic (`status = "pending"`, `message = "排队中"`).

### 3.2 Batch Resume API (`POST /api/admin/orders/resume_paused`)
- **Authorization**: **Strictly Restricted to Super Administrators** using `requireSuperAdmin`.
- **Behavior**:
  - Executes SQL query:
    ```sql
    UPDATE account_records 
    SET status = 'pending', message = '恢复排队处理', updated_at = NOW() 
    WHERE status = 'paused'
    ```
- **Response Format**:
  ```json
  {
    "success": true,
    "resumed_count": 12,
    "message": "成功将 12 条挂起订单恢复为排队中"
  }
  ```

### 3.3 Admin Orders API & Update API
- `/api/admin/orders`: Includes `paused` orders in queries and supports `status=paused` filter parameter.
- `/api/admin/orders/update`: Accepts `"paused"` as a valid status value when updated manually by authorized admins.

---

## 4. Frontend UI & Design Specifications

### 4.1 Admin Orders Management (`frontend/admin/orders.html`)
- **Status Filter**: Includes `<option value="paused">维护挂起 (Paused)</option>`.
- **Edit Modal**: Includes `<option value="paused">维护挂起 (paused)</option>`.
- **Status Badge**: Renders `o.status === 'paused'` with `statusClass = 'paused'` and `statusText = '维护挂起'`.
- **Batch Resume Button**:
  - Positioned in top toolbar for Super Administrators (`role === 'admin'`).
  - Triggers confirmation modal: *"确定要将所有挂起的 X 条订单一键恢复为排队中吗？"*.
  - Calls `POST /api/admin/orders/resume_paused` and reloads order list upon success.

### 4.2 System Settings Page (`frontend/admin/settings.html`)
- **Toggle Control**: Adds a switch element for **维护挂起模式 (Maintenance Mode)**.
- **Persistence**: Saved via `POST /api/admin/settings`.

### 4.3 Styling System (`frontend/admin/admin.css`)
- **Badge Styling**: `.status-badge.paused` configured with Amber warning colors:
  - Background: `rgba(245, 158, 11, 0.12)`
  - Text Color: `#b45309`
  - Border Color: `rgba(245, 158, 11, 0.25)`
  - Dot Indicator: `#f59e0b`

---

## 5. Security & Permission Boundaries

- **Public Submission**: Public users can submit tasks while Maintenance Mode is on, but tasks are safely paused without execution risk.
- **Batch Resume Authorization**: Only Super Admins (`role == "admin"`) wrapped with `requireSuperAdmin` can invoke `/api/admin/orders/resume_paused`. Regular operators (`role == "user"`) calling this endpoint receive `403 Forbidden`.

---

## 6. Testing Strategy

1. **Unit & Integration Tests (`main_test.go`)**:
   - Test submitting order with `maintenance_mode = 'on'` verifies record status is `paused`.
   - Test Batch Resume API with superadmin session converts `paused` records to `pending`.
   - Test Batch Resume API with regular user session returns `403 Forbidden`.
   - Test admin manual status update accepts `paused`.
