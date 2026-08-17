package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"
)

//go:embed frontend/*
var embedFS embed.FS

func main() {
	// 1. Initialize database connection
	initDB()

	// 2. Setup embedded static files server
	staticFS, err := fs.Sub(embedFS, "frontend")
	if err != nil {
		log.Fatalf("Error accessing embedded frontend files: %v", err)
	}
	fileServer := http.FileServer(http.FS(staticFS))
	http.Handle("/", &adminStaticServer{fileServer: fileServer})

	// 3. API - Submit card secret and accounts
	http.HandleFunc("/api/submit", limit(handleSubmit))

	// 4. API - Query status of a card secret
	http.HandleFunc("/api/query", limit(handleQuery))
	http.HandleFunc("/api/query/cancel", limit(handleCancelSubscription))

	// API - Batch convert third party keys (Admin protected)
	http.HandleFunc("/api/convert_keys", limit(requirePermission("convert", handleConvertKeys)))

	// API - Reset existing system keys and link to new ones (Whitelisted)
	http.HandleFunc("/api/reset_keys", limit(handleResetKeys))

	// Public config API
	http.HandleFunc("/api/config", limit(handleGetConfig))
	http.HandleFunc("/api/doc/info", limit(handleDocInfo))

	// Developer Open APIs
	http.HandleFunc("/api/open/submit", limit(handleOpenSubmit))
	http.HandleFunc("/api/open/query", limit(handleOpenQuery))
	http.HandleFunc("/api/open/reset", limit(handleOpenReset))
	http.HandleFunc("/api/open/cancel", limit(handleOpenCancel))

	// Epay Integration APIs
	http.HandleFunc("/api/pay/submit", limit(handlePaySubmit))
	http.HandleFunc("/api/pay/notify", limit(handlePayNotify))
	http.HandleFunc("/api/pay/config", limit(requirePermission("buy", handleGetPayConfig)))
	http.HandleFunc("/api/pay/buy", limit(requirePermission("buy", handlePayBuy)))
	http.HandleFunc("/api/pay/query", limit(requirePermission("buy", handlePayQuery)))
	http.HandleFunc("/api/pay/history", limit(requirePermission("buy", handlePayHistory)))
	http.HandleFunc("/api/pay/cancel", limit(requirePermission("buy", handlePayCancel)))

	// Admin APIs
	http.HandleFunc("/api/admin/login", limit(handleAdminLogin))
	http.HandleFunc("/api/admin/logout", limit(handleAdminLogout))
	http.HandleFunc("/api/admin/check", limit(handleAdminCheck))
	http.HandleFunc("/api/admin/orders", limit(requirePermission("orders", handleAdminOrders)))
	http.HandleFunc("/api/admin/orders/update", limit(requireSuperAdmin(handleAdminOrdersUpdate)))
	http.HandleFunc("/api/admin/orders/resume_paused", limit(requireSuperAdmin(handleAdminOrdersResumePaused)))
	http.HandleFunc("/api/admin/orders/history", limit(requirePermission("orders", handleAdminOrderHistory)))
	http.HandleFunc("/api/admin/orders/replace", limit(requireSuperAdmin(handleAdminOrderReplaceResubmit)))
	http.HandleFunc("/api/admin/orders/retry", limit(requireSuperAdmin(handleAdminOrderRetry)))
	http.HandleFunc("/api/admin/keys", limit(requirePermission("keys", handleAdminKeys)))
	http.HandleFunc("/api/admin/keys/invalidate", limit(requirePermission("keys", handleAdminKeysInvalidate)))
	http.HandleFunc("/api/admin/generate_stock_keys", limit(requirePermission("generate", handleGenerateStockKeys)))
	http.HandleFunc("/api/admin/pay/refund", limit(requirePermission("buy", handleAdminPayRefund)))
	http.HandleFunc("/api/admin/vendors", limit(requirePermission("vendors", handleAdminVendors)))
	http.HandleFunc("/api/admin/vendors/create", limit(requirePermission("vendors", handleAdminVendorsCreate)))
	http.HandleFunc("/api/admin/vendors/update", limit(requirePermission("vendors", handleAdminVendorsUpdate)))
	http.HandleFunc("/api/admin/vendors/delete", limit(requirePermission("vendors", handleAdminVendorsDelete)))
	http.HandleFunc("/api/admin/dashboard/stats", limit(requirePermission("dashboard", handleAdminDashboardStats)))
	http.HandleFunc("/api/admin/settings", limit(requirePermission("settings", handleAdminSettings)))
	http.HandleFunc("/api/admin/logs", limit(requirePermission("logs", handleAdminLogs)))
	http.HandleFunc("/api/admin/devices/selector", limit(requirePermission("logs", handleAdminDevicesSelector)))
	http.HandleFunc("/api/admin/devices", limit(requirePermission("devices", handleAdminDevices)))
	http.HandleFunc("/api/admin/devices/update_status", limit(requirePermission("devices", handleAdminDevicesUpdateStatus)))
	http.HandleFunc("/api/admin/devices/update", limit(requirePermission("devices", handleAdminDevicesUpdate)))
	http.HandleFunc("/api/admin/devices/create", limit(requirePermission("devices", handleAdminDevicesCreate)))
	http.HandleFunc("/api/admin/devices/delete", limit(requirePermission("devices", handleAdminDevicesDelete)))
	http.HandleFunc("/api/admin/faqs", limit(requirePermission("faqs", handleAdminFAQs)))
	http.HandleFunc("/api/admin/faqs/create", limit(requirePermission("faqs", handleAdminFAQsCreate)))
	http.HandleFunc("/api/admin/faqs/update", limit(requirePermission("faqs", handleAdminFAQsUpdate)))
	http.HandleFunc("/api/admin/faqs/delete", limit(requirePermission("faqs", handleAdminFAQsDelete)))

	// User Management APIs (Super Admin protected)
	http.HandleFunc("/api/admin/users", limit(requireSuperAdmin(handleAdminUsersList)))
	http.HandleFunc("/api/admin/users/create", limit(requireSuperAdmin(handleAdminUsersCreate)))
	http.HandleFunc("/api/admin/users/update", limit(requireSuperAdmin(handleAdminUsersUpdate)))
	http.HandleFunc("/api/admin/users/delete", limit(requireSuperAdmin(handleAdminUsersDelete)))
	http.HandleFunc("/api/admin/users/selector", limit(requireAdmin(handleAdminUsersSelector)))

	// Start background worker for periodic status sync and key invalidation
	go startBackgroundSync(5 * time.Minute)

	// 5. Start server
	port := "8080"
	if p := os.Getenv("PORT"); p != "" {
		port = p
	}
	log.Printf("Server starting on http://localhost:%s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}
