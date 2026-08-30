package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 会话管理（内存存储）
var (
	sessionMu     sync.Mutex
	sessions      = make(map[string]time.Time) // token -> 过期时间
	currentToken  string                       // 当前唯一有效 token
	sessionTTL    = 24 * time.Hour             // 会话有效期 24 小时
	sessionCookie = "api_session"
)

// createSession 登录成功创建会话，返回 token
func createSession() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	sessionMu.Lock()
	if currentToken != "" {
		delete(sessions, currentToken)
	}
	currentToken = token
	sessions[token] = time.Now().Add(sessionTTL)
	sessionMu.Unlock()
	return token, nil
}

// validSession 检查会话是否有效
func validSession(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	sessionMu.Lock()
	defer sessionMu.Unlock()
	if c.Value == "" || c.Value != currentToken {
		return false
	}
	exp, ok := sessions[c.Value]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(sessions, c.Value)
		currentToken = ""
		return false
	}
	return true
}

// destroySession 退出登录清除会话
func destroySession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		sessionMu.Lock()
		delete(sessions, c.Value)
		if c.Value == currentToken {
			currentToken = ""
		}
		sessionMu.Unlock()
	}
	// 清除 cookie
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
	})
}

// setSessionCookie 写入会话 cookie
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAuth 鉴权中间件：未登录跳转登录页（301）
func requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !validSession(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				if err := json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "会话已过期，请重新登录"}); err != nil {
					log.Printf("写入会话过期响应失败: %v", err)
				}
				return
			}
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		next(w, r)
	}
}
