package main

import (
	"container/list"
	"database/sql"
	"fmt"
	"log"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

var endpointCache atomic.Value // map[string]Endpoint，保存后整体原子替换

var staticCache *staticResponseCache

func initStaticCache() {
	staticCache = newStaticResponseCache()
}

func init() {
	endpointCache.Store(map[string]Endpoint{})
}

func endpointCacheKey(path, method string) string {
	return method + "\x00" + path
}

func replaceEndpointCache(eps []Endpoint) {
	items := make(map[string]Endpoint, len(eps))
	if staticCache == nil {
		initStaticCache()
	}
	staticCache.Reset()
	for _, endpoint := range eps {
		if endpoint.Kind == "" {
			endpoint.Kind = "static"
		}
		items[endpointCacheKey(endpoint.Path, endpoint.Method)] = endpoint
		if endpoint.Kind == "static" {
			staticCache.Set(endpointCacheKey(endpoint.Path, endpoint.Method), []byte(endpoint.Body), endpoint.ContentType)
			// 路由索引不重复持有静态正文；正文由容量受控的 LRU 缓存承载。
			endpoint.Body = ""
		}
	}
	endpointCache.Store(items)
}

func loadStaticResponse(path, method string) ([]byte, string, error) {
	var body, contentType string
	err := db.QueryRow("SELECT body, content_type FROM api_endpoints WHERE path=? AND method=? AND kind='static'", path, method).Scan(&body, &contentType)
	if err != nil {
		return nil, "", err
	}
	staticCache.Set(endpointCacheKey(path, method), []byte(body), contentType)
	return []byte(body), contentType, nil
}

// staticResponseCache 是按响应字节数限制的 LRU 缓存，避免静态端点请求重复
// 从 Endpoint 对象或 SQLite 读取响应内容。容量默认是 1 MiB×GOMAXPROCS，
// 可通过 STATIC_CACHE_RATIO 使用小数或整数比例自定义。
type staticResponseCache struct {
	capacity int64
	used     int64
	items    map[string]*staticCacheEntry
	order    *list.List
	mu       sync.Mutex
}

type staticCacheEntry struct {
	key         string
	body        []byte
	contentType string

	node *list.Element
}

func newStaticResponseCache() *staticResponseCache {
	ratio := 1.0
	ratioValue := os.Getenv("STATIC_CACHE_RATIO")
	if ratioValue == "" {
		if data, err := os.ReadFile(".env"); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
				if len(parts) == 2 && strings.TrimSpace(parts[0]) == "STATIC_CACHE_RATIO" {
					ratioValue = strings.TrimSpace(parts[1])
					break
				}
			}
		}
	}
	if value, err := strconv.ParseFloat(ratioValue, 64); err == nil && value > 0 {
		ratio = value
	}
	cores := runtime.GOMAXPROCS(0)
	capacity := int64(float64(1<<20) * float64(cores) * ratio)
	if capacity < 1<<20 {
		capacity = 1 << 20
	}
	log.Printf("静态端点内存缓存容量: %.2f MiB (核心数=%d, 比例=%.4g)", float64(capacity)/(1<<20), cores, ratio)
	return &staticResponseCache{capacity: capacity, items: make(map[string]*staticCacheEntry), order: list.New()}
}

func (c *staticResponseCache) Reset() {
	c.mu.Lock()
	c.used = 0
	c.items = make(map[string]*staticCacheEntry)
	c.order.Init()
	c.mu.Unlock()
}

func (c *staticResponseCache) Set(key string, body []byte, contentType string) {
	size := int64(len(body))
	if size > c.capacity {
		return
	}
	copyBody := append([]byte(nil), body...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.used -= int64(len(old.body))
		c.order.Remove(old.node)
		delete(c.items, key)
	}
	for c.used+size > c.capacity {
		old := c.order.Back()
		if old == nil {
			break
		}
		entry := old.Value.(*staticCacheEntry)
		c.used -= int64(len(entry.body))
		delete(c.items, entry.key)
		c.order.Remove(old)
	}
	entry := &staticCacheEntry{key: key, body: copyBody, contentType: contentType}
	entry.node = c.order.PushFront(entry)
	c.items[key] = entry
	c.used += size
}

func (c *staticResponseCache) Get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return nil, "", false
	}
	c.order.MoveToFront(entry.node)
	return entry.body, entry.contentType, true
}

// initDB 初始化 SQLite 数据库并建表
func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./api.db")
	if err != nil {
		log.Fatal("打开数据库失败:", err)
	}
	// SQLite 的临时存储、页缓存和 mmap 参数是连接级配置；单连接也足够
	// 支撑当前低写入量管理服务，并确保所有请求使用同一组优化参数。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA temp_store=MEMORY",
		"PRAGMA cache_size=-8192",
		"PRAGMA wal_autocheckpoint=1000",
		"PRAGMA mmap_size=67108864",
		"PRAGMA optimize",
	} {
		if _, err := db.Exec(pragma); err != nil {
			log.Fatalf("配置 SQLite 参数失败 (%s): %v", pragma, err)
		}
	}
	var tempStore, cacheSize, walCheckpoint, mmapSize int64
	if err := db.QueryRow("PRAGMA temp_store").Scan(&tempStore); err != nil {
		log.Fatal("读取 SQLite temp_store 失败:", err)
	}
	if err := db.QueryRow("PRAGMA cache_size").Scan(&cacheSize); err != nil {
		log.Fatal("读取 SQLite cache_size 失败:", err)
	}
	if err := db.QueryRow("PRAGMA wal_autocheckpoint").Scan(&walCheckpoint); err != nil {
		log.Fatal("读取 SQLite wal_autocheckpoint 失败:", err)
	}
	if err := db.QueryRow("PRAGMA mmap_size").Scan(&mmapSize); err != nil {
		log.Fatal("读取 SQLite mmap_size 失败:", err)
	}
	log.Printf("SQLite 优化参数: temp_store=%d cache_size=%d wal_autocheckpoint=%d mmap_size=%d", tempStore, cacheSize, walCheckpoint, mmapSize)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS api_endpoints (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	method TEXT NOT NULL DEFAULT 'GET',
	path TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT 'static',
	content_type TEXT NOT NULL DEFAULT 'application/json',

	body TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	UNIQUE(path, method)
);`)
	if err != nil {
		log.Fatal("初始化表失败:", err)
	}
	eps, err := listEndpoints()
	if err != nil {
		log.Fatal("加载端点缓存失败:", err)
	}
	replaceEndpointCache(eps)
	warmTengoCache(eps)
}

// ============ 端点 CRUD ============

func listEndpoints() ([]Endpoint, error) {
	rows, err := db.Query("SELECT id, method, path, content_type, body, kind, source, created_at, updated_at FROM api_endpoints ORDER BY id DESC")
	if err != nil {
		log.Printf("查询端点列表失败: %v", err)
		return nil, err
	}
	defer rows.Close()
	var eps []Endpoint
	for rows.Next() {
		var e Endpoint
		if err := rows.Scan(&e.ID, &e.Method, &e.Path, &e.ContentType, &e.Body, &e.Kind, &e.Source, &e.CreatedAt, &e.UpdatedAt); err != nil {
			log.Printf("读取端点记录失败: %v", err)
			return nil, err
		}
		eps = append(eps, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("遍历端点列表失败: %v", err)
		return nil, err
	}
	return eps, nil
}

// findEndpoint 按路径+方法查找端点（动态端点分发用）
func findEndpoint(path, method string) (*Endpoint, error) {
	endpoint, ok := endpointCache.Load().(map[string]Endpoint)[endpointCacheKey(path, method)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return &endpoint, nil
}

func reloadEndpointCache() error {
	eps, err := listEndpoints()
	if err != nil {
		return err
	}
	replaceEndpointCache(eps)
	warmTengoCache(eps)
	return nil
}

func createEndpoint(e Endpoint) (int64, error) {
	now := time.Now().Unix()
	result, err := db.Exec("INSERT INTO api_endpoints (method, path, content_type, body, kind, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", e.Method, e.Path, e.ContentType, e.Body, e.Kind, e.Source, now, now)
	if err != nil {
		return 0, err
	}
	eps, err := listEndpoints()
	if err != nil {
		return 0, fmt.Errorf("刷新端点缓存失败: %w", err)
	}
	replaceEndpointCache(eps)
	warmTengoCache(eps)
	return result.LastInsertId()
}

func updateEndpoint(id int64, e Endpoint) error {
	result, err := db.Exec("UPDATE api_endpoints SET method=?, path=?, content_type=?, body=?, kind=?, source=?, updated_at=? WHERE id=?", e.Method, e.Path, e.ContentType, e.Body, e.Kind, e.Source, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return reloadEndpointCache()
}

func deleteEndpoint(id int64) error {
	result, err := db.Exec("DELETE FROM api_endpoints WHERE id=?", id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return reloadEndpointCache()
}
