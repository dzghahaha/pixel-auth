package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type contextKey string
const adminIDKey contextKey = "admin_id"

// LoginRequest represents the parameters for admin login
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// checkAdminSession checks if token is valid and not expired, returning admin_id if valid
func checkAdminSession(token string) (int64, bool) {
	var adminID int64
	var expiresAt time.Time
	err := db.QueryRow("SELECT admin_id, expires_at FROM admin_sessions WHERE token = ?", token).Scan(&adminID, &expiresAt)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("Error querying session token: %v\n", err)
		}
		return 0, false
	}
	if time.Now().After(expiresAt) {
		// Clean up expired session
		_, errDel := db.Exec("DELETE FROM admin_sessions WHERE token = ?", token)
		if errDel != nil {
			log.Printf("Error deleting expired session: %v\n", errDel)
		}
		return 0, false
	}
	return adminID, true
}

func getAdminID(r *http.Request) (int64, bool) {
	adminID, ok := r.Context().Value(adminIDKey).(int64)
	return adminID, ok
}

func hasPermission(adminID int64, permission string) bool {
	var role string
	err := db.QueryRow("SELECT role FROM admins WHERE id = ?", adminID).Scan(&role)
	if err != nil {
		return false
	}
	if role == "admin" {
		return true
	}
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM admin_permissions WHERE admin_id = ? AND permission = ?", adminID, permission).Scan(&count)
	return err == nil && count > 0
}

func requirePermission(permission string, next http.HandlerFunc) http.HandlerFunc {
	return requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := getAdminID(r)
		if !ok {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "请登录后操作",
			})
			return
		}
		if !hasPermission(adminID, permission) {
			respondJSON(w, http.StatusForbidden, map[string]interface{}{
				"success": false,
				"message": "您没有该操作权限",
			})
			return
		}
		next(w, r)
	})
}

func requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return requireAdmin(func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := getAdminID(r)
		if !ok {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "请登录后操作",
			})
			return
		}
		var role string
		err := db.QueryRow("SELECT role FROM admins WHERE id = ?", adminID).Scan(&role)
		if err != nil || role != "admin" {
			respondJSON(w, http.StatusForbidden, map[string]interface{}{
				"success": false,
				"message": "仅超级管理员可进行该操作",
			})
			return
		}
		next(w, r)
	})
}

func requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err != nil || cookie.Value == "" {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "请登录后操作",
			})
			return
		}
		adminID, ok := checkAdminSession(cookie.Value)
		if !ok {
			respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "登录已过期或无效，请重新登录",
			})
			return
		}
		ctx := context.WithValue(r.Context(), adminIDKey, adminID)
		next(w, r.WithContext(ctx))
	}
}

type adminStaticServer struct {
	fileServer http.Handler
}

func (h *adminStaticServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/convert.html" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	if path == "/admin" || strings.HasPrefix(path, "/admin/") {
		if path == "/admin" {
			http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
			return
		}

		if path == "/admin/login.html" || path == "/admin/admin.css" {
			if path == "/admin/login.html" {
				cookie, err := r.Cookie("admin_session")
				if err == nil && cookie.Value != "" {
					if _, ok := checkAdminSession(cookie.Value); ok {
						http.Redirect(w, r, "/admin/dashboard.html", http.StatusFound)
						return
					}
				}
			}
			h.fileServer.ServeHTTP(w, r)
			return
		}

		cookie, err := r.Cookie("admin_session")
		if err != nil || cookie.Value == "" {
			http.Redirect(w, r, "/admin/login.html", http.StatusFound)
			return
		}
		adminID, ok := checkAdminSession(cookie.Value)
		if !ok {
			http.SetCookie(w, &http.Cookie{
				Name:     "admin_session",
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
			})
			http.Redirect(w, r, "/admin/login.html", http.StatusFound)
			return
		}

		// Perform static page access check:
		if strings.HasSuffix(path, ".html") {
			var role string
			err := db.QueryRow("SELECT role FROM admins WHERE id = ?", adminID).Scan(&role)
			if err != nil {
				// User not found in database, clear session and redirect to login
				http.SetCookie(w, &http.Cookie{
					Name:     "admin_session",
					Value:    "",
					Path:     "/",
					MaxAge:   -1,
					HttpOnly: true,
				})
				http.Redirect(w, r, "/admin/login.html", http.StatusFound)
				return
			}

			// Map file path to permission key
			filePermissionMap := map[string]string{
				"/admin/dashboard.html":   "dashboard",
				"/admin/orders.html":      "orders",
				"/admin/keys.html":        "keys",
				"/admin/convert.html":     "convert",
				"/admin/reset.html":       "reset",
				"/admin/generate.html":    "generate",
				"/admin/buy.html":         "buy",
				"/admin/buy_records.html": "buy",
				"/admin/vendors.html":     "vendors",
				"/admin/logs.html":        "logs",
				"/admin/settings.html":    "settings",
			}

			if path == "/admin/users.html" {
				if role != "admin" {
					http.Redirect(w, r, "/admin/dashboard.html", http.StatusFound)
					return
				}
			} else if perm, ok := filePermissionMap[path]; ok {
				if role != "admin" {
					hasPerm := false
					var count int
					err := db.QueryRow("SELECT COUNT(*) FROM admin_permissions WHERE admin_id = ? AND permission = ?", adminID, perm).Scan(&count)
					if err == nil && count > 0 {
						hasPerm = true
					}
					if !hasPerm {
						http.Redirect(w, r, "/admin/dashboard.html", http.StatusFound)
						return
					}
				}
			}
		}
	}
	h.fileServer.ServeHTTP(w, r)
}

func handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "无效的请求格式数据",
		})
		return
	}

	var adminID int64
	var pwdHash string
	err := db.QueryRow("SELECT id, password_hash FROM admins WHERE username = ?", req.Username).Scan(&adminID, &pwdHash)
	if err == sql.ErrNoRows {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "用户名或密码错误",
		})
		return
	} else if err != nil {
		log.Printf("Error querying admin: %v\n", err)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "数据库服务故障，请稍后重试",
		})
		return
	}

	err = bcrypt.CompareHashAndPassword([]byte(pwdHash), []byte(req.Password))
	if err != nil {
		respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"success": false,
			"message": "用户名或密码错误",
		})
		return
	}

	// Generate token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "生成Session Token失败",
		})
		return
	}
	token := hex.EncodeToString(tokenBytes)

	// Store in database with 24 hours expiry
	expiresAt := time.Now().Add(24 * time.Hour)
	now := time.Now()
	_, errInsert := db.Exec("INSERT INTO admin_sessions (token, admin_id, expires_at, created_at) VALUES (?, ?, ?, ?)",
		token, adminID, expiresAt, now)
	if errInsert != nil {
		log.Printf("Error saving admin session: %v\n", errInsert)
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "保存Session失败",
		})
		return
	}

	// Set cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "登录成功",
	})
}

func handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cookie, err := r.Cookie("admin_session")
	if err == nil && cookie.Value != "" {
		_, errDel := db.Exec("DELETE FROM admin_sessions WHERE token = ?", cookie.Value)
		if errDel != nil {
			log.Printf("Error deleting session on logout: %v\n", errDel)
		}
	}

	// Clear cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "退出登录成功",
	})
}

func handleAdminCheck(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("admin_session")
	if err != nil || cookie.Value == "" {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}
	adminID, ok := checkAdminSession(cookie.Value)
	if !ok {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}

	var username string
	var role string
	errQuery := db.QueryRow("SELECT username, role FROM admins WHERE id = ?", adminID).Scan(&username, &role)
	if errQuery != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"success": false,
		})
		return
	}

	permissions := []string{}
	if role == "admin" {
		permissions = []string{"dashboard", "orders", "keys", "convert", "reset", "generate", "buy", "vendors", "settings", "logs"}
	} else {
		rows, err := db.Query("SELECT permission FROM admin_permissions WHERE admin_id = ?", adminID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var p string
				if err := rows.Scan(&p); err == nil {
					permissions = append(permissions, p)
				}
			}
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success":     true,
		"username":    username,
		"role":        role,
		"permissions": permissions,
	})
}
