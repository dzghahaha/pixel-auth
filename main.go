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
	http.HandleFunc("/api/submit", handleSubmit)

	// 4. API - Query status of a card secret
	http.HandleFunc("/api/query", handleQuery)

	// API - Batch convert third party keys (Admin protected)
	http.HandleFunc("/api/convert_keys", requirePermission("convert", handleConvertKeys))

	// API - Reset existing system keys and link to new ones (Whitelisted)
	http.HandleFunc("/api/reset_keys", handleResetKeys)

	// Public config API
	http.HandleFunc("/api/config", handleGetConfig)
	http.HandleFunc("/api/doc/info", handleDocInfo)

	// Developer Open APIs
	http.HandleFunc("/api/open/submit", handleOpenSubmit)
	http.HandleFunc("/api/open/query", handleOpenQuery)
	http.HandleFunc("/api/open/reset", handleOpenReset)

	// Epay Integration APIs
	http.HandleFunc("/api/pay/submit", handlePaySubmit)
	http.HandleFunc("/api/pay/notify", handlePayNotify)
	http.HandleFunc("/api/pay/config", requirePermission("buy", handleGetPayConfig))
	http.HandleFunc("/api/pay/buy", requirePermission("buy", handlePayBuy))
	http.HandleFunc("/api/pay/query", requirePermission("buy", handlePayQuery))
	http.HandleFunc("/api/pay/history", requirePermission("buy", handlePayHistory))
	http.HandleFunc("/api/pay/cancel", requirePermission("buy", handlePayCancel))

	// Admin APIs
	http.HandleFunc("/api/admin/login", handleAdminLogin)
	http.HandleFunc("/api/admin/logout", handleAdminLogout)
	http.HandleFunc("/api/admin/check", handleAdminCheck)
	http.HandleFunc("/api/admin/orders", requirePermission("orders", handleAdminOrders))
	http.HandleFunc("/api/admin/orders/update", requirePermission("orders", handleAdminOrdersUpdate))
	http.HandleFunc("/api/admin/orders/history", requirePermission("orders", handleAdminOrderHistory))
	http.HandleFunc("/api/admin/orders/replace", requirePermission("orders", handleAdminOrderReplaceResubmit))
	http.HandleFunc("/api/admin/keys", requirePermission("keys", handleAdminKeys))
	http.HandleFunc("/api/admin/keys/invalidate", requirePermission("keys", handleAdminKeysInvalidate))
	http.HandleFunc("/api/admin/generate_stock_keys", requirePermission("generate", handleGenerateStockKeys))
	http.HandleFunc("/api/admin/pay/refund", requirePermission("buy", handleAdminPayRefund))
	http.HandleFunc("/api/admin/vendors", requirePermission("vendors", handleAdminVendors))
	http.HandleFunc("/api/admin/vendors/create", requirePermission("vendors", handleAdminVendorsCreate))
	http.HandleFunc("/api/admin/vendors/update", requirePermission("vendors", handleAdminVendorsUpdate))
	http.HandleFunc("/api/admin/vendors/delete", requirePermission("vendors", handleAdminVendorsDelete))
	http.HandleFunc("/api/admin/dashboard/stats", requirePermission("dashboard", handleAdminDashboardStats))
	http.HandleFunc("/api/admin/settings", requirePermission("settings", handleAdminSettings))

	// User Management APIs (Super Admin protected)
	http.HandleFunc("/api/admin/users", requireSuperAdmin(handleAdminUsersList))
	http.HandleFunc("/api/admin/users/create", requireSuperAdmin(handleAdminUsersCreate))
	http.HandleFunc("/api/admin/users/update", requireSuperAdmin(handleAdminUsersUpdate))
	http.HandleFunc("/api/admin/users/delete", requireSuperAdmin(handleAdminUsersDelete))

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
