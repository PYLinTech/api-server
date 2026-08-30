package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	maxFailCount    = 5
	lockDuration    = 2 * time.Minute
	failExpireAfter = 30 * time.Minute
)

var (
	adminUser      string
	adminPassHash  string
	loginAttemptMu sync.Mutex
	loginAttempts  = make(map[string]struct {
		failCount   int
		lockedUntil int64
		lastFailAt  int64
	})
)

func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); net.ParseIP(ip) != nil {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	if net.ParseIP(host) == nil {
		return r.RemoteAddr
	}
	return host
}

func checkLocked(ip string) int64 {
	loginAttemptMu.Lock()
	defer loginAttemptMu.Unlock()
	state, ok := loginAttempts[ip]
	if !ok || state.lockedUntil <= 0 {
		return 0
	}
	if time.Now().Unix() >= state.lockedUntil {
		delete(loginAttempts, ip)
		return 0
	}
	return state.lockedUntil
}

func recordFail(ip string) int64 {
	now := time.Now().Unix()
	loginAttemptMu.Lock()
	defer loginAttemptMu.Unlock()
	state := loginAttempts[ip]
	state.failCount++
	state.lastFailAt = now
	if state.failCount >= maxFailCount {
		state.lockedUntil = now + int64(lockDuration.Seconds())
		loginAttempts[ip] = state
		return state.lockedUntil
	}
	state.lockedUntil = 0
	loginAttempts[ip] = state
	return 0
}

func clearState(ip string) { loginAttemptMu.Lock(); delete(loginAttempts, ip); loginAttemptMu.Unlock() }

func cleanupLoginAttempts() {
	for {
		time.Sleep(5 * time.Minute)
		now := time.Now().Unix()
		loginAttemptMu.Lock()
		for ip, state := range loginAttempts {
			if state.lockedUntil > 0 && now >= state.lockedUntil {
				delete(loginAttempts, ip)
			} else if state.lockedUntil <= 0 && state.lastFailAt > 0 && now-state.lastFailAt >= int64(failExpireAfter.Seconds()) {
				delete(loginAttempts, ip)
			}
		}
		loginAttemptMu.Unlock()
	}
}

func ensureEnv() {
	if data, err := os.ReadFile(".env"); err == nil {
		user, passHash := "", ""
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
			if len(parts) != 2 {
				continue
			}
			switch strings.TrimSpace(parts[0]) {
			case "ADMIN_USER":
				user = strings.TrimSpace(parts[1])
			case "ADMIN_PASS_HASH":
				passHash = strings.TrimSpace(parts[1])
			}
		}
		if user != "" && passHash != "" {
			return
		}
	} else if !os.IsNotExist(err) {
		log.Fatalf("检查 .env 失败: %v", err)
	}
	// 通过环境变量 ADMIN_USER / ADMIN_PASSWORD（明文）初始化管理员配置，
	// 用于 Docker/脚本部署：程序负责生成与自身一致的 bcrypt 哈希，避免脚本重复实现。
	if user := os.Getenv("ADMIN_USER"); user != "" {
		if pass := os.Getenv("ADMIN_PASSWORD"); pass != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
			if err == nil {
				content := fmt.Sprintf("# api-server 自动生成配置\nADMIN_USER=%s\nADMIN_PASS_HASH=%s\nSTATIC_CACHE_RATIO=2\n", user, base64.StdEncoding.EncodeToString(hash))
				if err := os.WriteFile(".env", []byte(content), 0600); err == nil {
					log.Printf("已通过环境变量初始化管理员配置")
					return
				}
			}
		}
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		log.Fatal(".env 不存在或配置不完整，当前不是交互式终端，无法初始化管理员配置")
	}
	fmt.Println(".env配置不存在或不完整！")
	fmt.Println("准备交互式创建配置...")
	fmt.Println("++++++++++++++++++++++++")
	reader := bufio.NewReader(os.Stdin)
	readValue := func(prompt string) string {
		fmt.Print(prompt)
		value, err := reader.ReadString('\n')
		if err != nil && len(value) == 0 {
			log.Fatalf("读取初始化配置失败: %v", err)
		}
		return strings.TrimSpace(value)
	}
	admin := readValue("首次启动，请输入管理员账号: ")
	if admin == "" {
		log.Fatal("管理员账号不能为空")
	}
	password := readValue("请输入管理员密码: ")
	if len(password) < 6 {
		log.Fatal("管理员密码长度不能少于 6 位")
	}
	confirm := readValue("请再次输入管理员密码: ")
	if password != confirm {
		log.Fatal("两次输入的管理员密码不一致")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("生成管理员密码配置失败: %v", err)
	}
	content := fmt.Sprintf("# api-server 自动生成配置\nADMIN_USER=%s\nADMIN_PASS_HASH=%s\nSTATIC_CACHE_RATIO=2\n", admin, base64.StdEncoding.EncodeToString(hash))
	if err := os.WriteFile(".env", []byte(content), 0600); err != nil {
		log.Fatalf("写入 .env 失败: %v", err)
	}
	fmt.Println("配置写入成功，管理员账号和密码如下，请妥善保存：")
	fmt.Printf("管理员账号：%s\n管理员密码：%s\n", admin, password)
}

func loadEnv() (string, string) {
	user, passHash := "", ""
	data, err := os.ReadFile(".env")
	if err != nil {
		log.Fatalf("读取 .env 失败: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		switch key {
		case "ADMIN_USER":
			user = val
		case "ADMIN_PASS_HASH":
			passHash = val
		}
	}
	if user == "" || passHash == "" {
		log.Fatal("ADMIN_USER 或 ADMIN_PASS_HASH 未在 .env 中配置")
	}
	if decoded, err := base64.StdEncoding.DecodeString(passHash); err == nil {
		passHash = string(decoded)
	}
	return user, passHash
}

func verifyLogin(username, password, adminUser, adminPassHash string) bool {
	return subtle.ConstantTimeCompare([]byte(username), []byte(adminUser)) == 1 && bcrypt.CompareHashAndPassword([]byte(adminPassHash), []byte(password)) == nil
}
