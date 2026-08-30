package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

var validMethods = map[string]bool{
	http.MethodGet: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodDelete: true, http.MethodPatch: true,
}

const (
	maxLoginBody       = 16 << 10
	maxAdminJSONBody   = 2 << 20
	maxTestRequestBody = 1 << 20
	maxDynamicBody     = 10 << 20
)

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	writeJSON(w, map[string]interface{}{"success": false, "error": message})
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeJSONError(w, http.StatusOK, "不支持的请求方法")
}

func bodyTooLarge(w http.ResponseWriter) {
	writeJSONError(w, http.StatusOK, "请求体过大")
}

func isBodyTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// writeJSON 统一 JSON 响应
func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("写入 JSON 响应失败: %v", err)
	}
}

// securityHeaders 为所有响应统一添加基础安全响应头。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; base-uri 'self'; frame-ancestors 'none'")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		next.ServeHTTP(w, r)
	})
}

// noCacheHeaders 禁止动态页面和 API 响应被浏览器或中间缓存存储。
func noCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

// handleLogin 登录接口
func handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(loginPage))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	ip := clientIP(r)

	if lockedUntil := checkLocked(ip); lockedUntil > 0 {
		writeJSON(w, map[string]interface{}{"success": false, "lockedUntil": lockedUntil})
		return
	}

	if err := r.ParseForm(); err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": "无效的表单数据"})
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	if verifyLogin(username, password, adminUser, adminPassHash) {
		clearState(ip)
		// 创建会话并写入 cookie
		token, err := createSession()
		if err != nil {
			writeJSON(w, map[string]interface{}{"success": false, "error": "创建会话失败"})
			return
		}
		setSessionCookie(w, token)
		writeJSON(w, map[string]interface{}{"success": true})
	} else {
		lockedUntil := recordFail(ip)
		if lockedUntil > 0 {
			writeJSON(w, map[string]interface{}{"success": false, "lockedUntil": lockedUntil})
			return
		}
		writeJSON(w, map[string]interface{}{"success": false})
	}
}

// handleLogout 退出登录
func handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	destroySession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleHome 首页（登录页）；其他路径走动态端点分发
func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(loginPage))
		return
	}
	// 动态端点分发（匹配数据库中的端点）
	handleDynamicEndpoint(w, r)
}

// handlePanel 管理面板
func handlePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(panelPage))
}

// handleListEndpoints GET /api/endpoints
func handleListEndpoints(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	eps, err := listEndpoints()
	if err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"success": true, "endpoints": eps})
}

func decodeEndpoint(w http.ResponseWriter, r *http.Request) (Endpoint, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxAdminJSONBody)
	var ep Endpoint
	if err := json.NewDecoder(r.Body).Decode(&ep); err != nil {
		if isBodyTooLarge(err) {
			bodyTooLarge(w)
		} else {
			writeJSON(w, map[string]interface{}{"success": false, "error": "无效的 JSON"})
		}
		return Endpoint{}, false
	}
	if err := validateEndpoint(&ep); err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return Endpoint{}, false
	}
	return ep, true
}

func validateEndpoint(ep *Endpoint) error {
	if !validMethods[ep.Method] {
		return errors.New("method 必须是 GET、POST、PUT、DELETE 或 PATCH")
	}
	if ep.Path == "" || ep.Path[0] != '/' {
		return errors.New("path 不能为空且必须以 / 开头")
	}
	if ep.Kind == "" {
		ep.Kind = "static"
	}
	if ep.Kind != "static" && ep.Kind != "tengo" {
		return errors.New("kind 必须是 static 或 tengo")
	}
	if ep.Kind == "tengo" {
		if ep.Source == "" {
			return errors.New("Tengo 源码不能为空")
		}
		if ep.ContentType == "" {
			ep.ContentType = "auto"
		}
		if err := compileTengoSource(ep.Source); err != nil {
			return fmt.Errorf("Tengo 编译失败: %w", err)
		}
	}
	return nil
}

func endpointIDFromPath(r *http.Request, action string) (int64, error) {
	value := strings.TrimPrefix(r.URL.Path, "/api/endpoints/"+action+"/")
	if value == "" || strings.Contains(value, "/") {
		return 0, errors.New("端点 ID 无效")
	}
	return strconv.ParseInt(value, 10, 64)
}

func handleCreateEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	ep, ok := decodeEndpoint(w, r)
	if !ok {
		return
	}
	id, err := createEndpoint(ep)
	if err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	ep.ID = id
	writeJSON(w, map[string]interface{}{"success": true, "endpoint": ep})
}

func handleUpdateEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		methodNotAllowed(w, http.MethodPut+", "+http.MethodPatch)
		return
	}
	id, err := endpointIDFromPath(r, "update")
	if err != nil || id <= 0 {
		writeJSON(w, map[string]interface{}{"success": false, "error": "端点 ID 无效"})
		return
	}
	ep, ok := decodeEndpoint(w, r)
	if !ok {
		return
	}
	if err := updateEndpoint(id, ep); err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	ep.ID = id
	writeJSON(w, map[string]interface{}{"success": true, "endpoint": ep})
}

func handleDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	id, err := endpointIDFromPath(r, "delete")
	if err != nil || id <= 0 {
		writeJSON(w, map[string]interface{}{"success": false, "error": "端点 ID 无效"})
		return
	}
	if err := deleteEndpoint(id); err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{"success": true})
}

// handleTestEndpoint 测试尚未保存的 Tengo 云函数端点。
func handleTestEndpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTestRequestBody)
	var req struct {
		Source      string                 `json:"source"`
		Method      string                 `json:"method"`
		Path        string                 `json:"path"`
		ContentType string                 `json:"content_type"`
		Query       map[string]interface{} `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if isBodyTooLarge(err) {
			bodyTooLarge(w)
			return
		}
		writeJSON(w, map[string]interface{}{"success": false, "error": "无效的 JSON"})
		return
	}
	result, err := executeTengo(req.Source, functionRequest{
		Method: req.Method, Path: req.Path, Type: req.ContentType, Query: req.Query,
	})
	if err != nil {
		writeJSON(w, map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if req.ContentType != "" && req.ContentType != "auto" {
		result.Headers["Content-Type"] = req.ContentType
	}
	writeJSON(w, map[string]interface{}{"success": true, "result": map[string]interface{}{"status": http.StatusOK, "headers": result.Headers, "body": result.Body, "body_base64": result.BodyBase64}})
}

// handleDynamicEndpoint 动态端点分发（匹配数据库中的端点）
func handleDynamicEndpoint(w http.ResponseWriter, r *http.Request) {
	ep, err := findEndpoint(r.URL.Path, r.Method)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("查询动态端点失败: %v", err)
		writeJSON(w, map[string]interface{}{"success": false, "error": "查询端点失败"})
		return
	}
	body := ep.Body
	responseHeaders := map[string]string{}
	var responseBody []byte
	if ep.Kind == "static" {
		cachedBody, cachedContentType, ok := staticCache.Get(endpointCacheKey(ep.Path, ep.Method))
		if !ok {
			cachedBody, cachedContentType, err = loadStaticResponse(ep.Path, ep.Method)
		}
		if err != nil {
			log.Printf("读取静态端点响应失败: %v", err)
			writeJSONError(w, http.StatusOK, "读取端点响应失败")
			return
		}
		responseBody = cachedBody
		if ep.ContentType == "" && cachedContentType != "" {
			ep.ContentType = cachedContentType
		}
	}
	if ep.Kind == "tengo" {
		r.Body = http.MaxBytesReader(w, r.Body, maxDynamicBody)
		requestBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			if isBodyTooLarge(readErr) {
				bodyTooLarge(w)
				return
			}
			writeJSONError(w, http.StatusOK, "读取请求体失败")
			return
		}
		result, executeErr := executeTengo(ep.Source, requestToFunctionRequest(r, requestBody, ep.ContentType))
		if executeErr != nil {
			writeJSONError(w, http.StatusOK, executeErr.Error())
			return
		}
		body = result.Body
		responseBody = result.BodyBytes
		responseHeaders = result.Headers
	}
	if ep.Kind == "static" && ep.ContentType == "" {
		ep.ContentType = "application/json"
	}
	for key, value := range responseHeaders {
		w.Header().Set(key, value)
	}
	if ep.ContentType != "" && ep.ContentType != "auto" {
		w.Header().Set("Content-Type", ep.ContentType)
	}
	w.WriteHeader(http.StatusOK)
	if responseBody != nil {
		if _, err := w.Write(responseBody); err != nil {
			log.Printf("写入动态端点二进制响应失败: %v", err)
		}
		return
	}
	if _, err := w.Write([]byte(body)); err != nil {
		log.Printf("写入动态端点响应失败: %v", err)
	}
}
