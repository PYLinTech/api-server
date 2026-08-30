package main

// Endpoint 表示一个 API 端点
type Endpoint struct {
	ID          int64  `json:"id"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	Kind        string `json:"kind"`   // static / tengo
	Source      string `json:"source"` // Tengo 云函数源码（kind=tengo 时使用）
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}
