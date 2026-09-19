package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

func respondJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(data)
}

// InFlightLimiter controls concurrent requests for the same IP and Path
type InFlightLimiter struct {
	mu    sync.Mutex
	locks map[string]bool
}

// GlobalLimiter is the global instance of InFlightLimiter
var GlobalLimiter = NewInFlightLimiter()

func NewInFlightLimiter() *InFlightLimiter {
	return &InFlightLimiter{
		locks: make(map[string]bool),
	}
}

// TryAcquire attempts to lock a key. Returns true if successful, false if already locked.
func (l *InFlightLimiter) TryAcquire(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks[key] {
		return false
	}
	l.locks[key] = true
	return true
}

// Release releases a locked key.
func (l *InFlightLimiter) Release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.locks, key)
}

// InFlightLimitMiddleware blocks duplicate concurrent requests on the same path for the same client IP
func InFlightLimitMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Bypass static pages
		path := r.URL.Path
		if path == "/" || strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".css") || strings.HasSuffix(path, ".js") || strings.HasSuffix(path, ".png") || strings.HasSuffix(path, ".ico") {
			next(w, r)
			return
		}

		// 1. 管理后台只读查询请求（GET /api/admin/...）允许正常的页面级并发，不设全局阻塞
		if r.Method == http.MethodGet && strings.HasPrefix(path, "/api/admin/") {
			next(w, r)
			return
		}

		// 2. 对于携带了具体 query 参数的非 C端核心查询，区分不同参数，避免多渠道并发请求相互阻塞
		key := fmt.Sprintf("%s:%s", getClientIP(r), path)
		if r.URL.RawQuery != "" && path != "/api/query" {
			key = fmt.Sprintf("%s:%s?%s", getClientIP(r), path, r.URL.RawQuery)
		}

		if !GlobalLimiter.TryAcquire(key) {
			respondJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"success": false,
				"message": "请求正在处理中，请勿重复提交",
			})
			return
		}
		defer GlobalLimiter.Release(key)

		next(w, r)
	}
}

// limit wraps an http.HandlerFunc with the concurrent in-flight limit middleware
func limit(f http.HandlerFunc) http.HandlerFunc {
	return InFlightLimitMiddleware(f)
}
