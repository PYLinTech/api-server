package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	ensureEnv()
	adminUser, adminPassHash = loadEnv()
	initStaticCache()
	initDB()
	go cleanupLoginAttempts()

	// 公开路由
	http.HandleFunc("/login", handleLogin)
	http.HandleFunc("/logout", handleLogout)

	// 需要登录的路由（鉴权中间件）
	http.HandleFunc("/panel", requireAuth(handlePanel))
	http.HandleFunc("/api/endpoints", requireAuth(handleListEndpoints))
	http.HandleFunc("/api/endpoints/create", requireAuth(handleCreateEndpoint))
	http.HandleFunc("/api/endpoints/update/", requireAuth(handleUpdateEndpoint))
	http.HandleFunc("/api/endpoints/delete/", requireAuth(handleDeleteEndpoint))

	http.HandleFunc("/api/endpoints/test", requireAuth(handleTestEndpoint))

	// 首页（登录页）+ 动态端点分发（合一处理）
	http.HandleFunc("/", handleHome)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	log.Printf("api-server 启动于 :%s", port)
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           noCacheHeaders(securityHeaders(http.DefaultServeMux)),
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 3 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
