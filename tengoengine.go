package main

import (
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/d5/tengo/v2"
	"github.com/d5/tengo/v2/stdlib"
)

type functionRequest struct {
	Method  string
	Path    string
	Type    string
	Query   map[string]interface{}
	Body    interface{}
	Headers map[string]interface{}
	RawBody []byte
}

type functionResponse struct {
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body,omitempty"`
	BodyBytes  []byte            `json:"-"`
	BodyBase64 string            `json:"body_base64,omitempty"`
}

var functionExecutionTimeout = 10 * time.Second
var tengoHTTPClient = &http.Client{Timeout: 10 * time.Second}

// tengoHTTPPool limits concurrent upstream requests while allowing excess
// requests to wait until a slot becomes available. The limit is deliberately
// bounded for this small server; waiting remains cancellable by the request
// context and the Tengo execution timeout.
const maxTengoHTTPConcurrency = 8

var tengoHTTPPool = make(chan struct{}, maxTengoHTTPConcurrency)

// 缓存不使用宿主 HTTP 模块的脚本字节码；请求执行时 Clone 并注入 req，
// 避免重复解析编译，同时保持每个请求的全局变量完全隔离。
var tengoCompileCache = struct {
	sync.Mutex
	items map[[sha256.Size]byte]*tengo.Compiled
	order [][sha256.Size]byte
}{items: make(map[[sha256.Size]byte]*tengo.Compiled)}

const maxTengoCompileCacheEntries = 128

const (
	maxTengoSourceBytes             = 256 << 10
	maxTengoHTTPResponseBytes       = 16 << 20
	maxTengoOutputBytes             = 16 << 20
	maxTengoAllocs            int64 = 250_000
	maxTengoConstObjects            = 50_000
)

func tengoObject(v interface{}) (tengo.Object, error) { return tengo.FromInterface(v) }
func tengoStringArg(args []tengo.Object, i int) (string, error) {
	if i >= len(args) {
		return "", errors.New("缺少参数")
	}
	v, ok := tengo.ToString(args[i])
	if !ok {
		return "", fmt.Errorf("参数 %d 必须是字符串", i+1)
	}
	return v, nil
}
func tengoMapArg(o tengo.Object) (map[string]interface{}, error) {
	v := tengo.ToInterface(o)
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil, errors.New("参数必须是对象")
	}
	return m, nil
}
func tengoJSONEncode(args ...tengo.Object) (tengo.Object, error) {
	if len(args) != 1 {
		return nil, errors.New("json.encode 需要一个参数")
	}
	b, e := json.Marshal(tengo.ToInterface(args[0]))
	if e != nil {
		return nil, e
	}
	return &tengo.String{Value: string(b)}, nil
}
func tengoJSONDecode(args ...tengo.Object) (tengo.Object, error) {
	s, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	var v interface{}
	if e = json.Unmarshal([]byte(s), &v); e != nil {
		return nil, e
	}
	return tengoObject(v)
}
func tengoBase64Encode(args ...tengo.Object) (tengo.Object, error) {
	s, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	return &tengo.String{Value: base64.StdEncoding.EncodeToString([]byte(s))}, nil
}
func tengoBase64Decode(args ...tengo.Object) (tengo.Object, error) {
	s, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		return nil, e
	}
	return &tengo.Bytes{Value: b}, nil
}
func tengoHash(algo string, args ...tengo.Object) (tengo.Object, error) {
	s, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	var out []byte
	switch algo {
	case "sha256":
		x := sha256.Sum256([]byte(s))
		out = x[:]
	case "sha512":
		x := sha512.Sum512([]byte(s))
		out = x[:]
	case "md5":
		x := md5.Sum([]byte(s))
		out = x[:]
	}
	return &tengo.String{Value: hex.EncodeToString(out)}, nil
}
func tengoHMAC(args ...tengo.Object) (tengo.Object, error) {
	key, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	msg, e := tengoStringArg(args, 1)
	if e != nil {
		return nil, e
	}
	h := hmac.New(sha256.New, []byte(key))
	h.Write([]byte(msg))
	return &tengo.String{Value: hex.EncodeToString(h.Sum(nil))}, nil
}
func tengoURLQueryEscape(args ...tengo.Object) (tengo.Object, error) {
	s, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	return &tengo.String{Value: url.QueryEscape(s)}, nil
}
func tengoRandomBytes(args ...tengo.Object) (tengo.Object, error) {
	n := 16
	if len(args) > 0 {
		n0, ok := tengo.ToInt(args[0])
		if !ok || n0 < 1 || n0 > 4096 {
			return nil, errors.New("长度必须为 1-4096")
		}
		n = n0
	}
	b := make([]byte, n)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	return &tengo.String{Value: base64.RawURLEncoding.EncodeToString(b)}, nil
}

func tengoHTTP(ctx context.Context, method string, args ...tengo.Object) (tengo.Object, error) {
	u, e := tengoStringArg(args, 0)
	if e != nil {
		return nil, e
	}
	var options map[string]interface{}
	if len(args) > 1 {
		if _, undefined := args[1].(*tengo.Undefined); undefined {
			args = args[:1]
		} else {
			options, e = tengoMapArg(args[1])
			if e != nil {
				return nil, e
			}
		}
	}
	var body io.Reader
	headers := http.Header{}
	if options != nil {
		if v, ok := options["body"]; ok {
			body = strings.NewReader(fmt.Sprint(v))
		}
		if hs, ok := options["headers"].(map[string]interface{}); ok {
			for k, v := range hs {
				headers.Set(k, fmt.Sprint(v))
			}
		}
	}
	req, e := http.NewRequestWithContext(ctx, method, u, body)
	if e != nil {
		return nil, e
	}
	req.Header = headers
	select {
	case tengoHTTPPool <- struct{}{}:
		defer func() { <-tengoHTTPPool }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	resp, e := tengoHTTPClient.Do(req)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, maxTengoHTTPResponseBytes+1))
	if e != nil {
		return nil, e
	}
	if len(b) > maxTengoHTTPResponseBytes {
		return nil, errors.New("上游响应体超过 16 MiB")
	}
	responseHeaders := make(map[string]interface{}, len(resp.Header))
	for key, values := range resp.Header {
		if len(values) > 0 {
			responseHeaders[key] = values[0]
		}
	}
	return tengoObject(map[string]interface{}{"status": resp.StatusCode, "headers": responseHeaders, "body": string(b), "body_bytes": b})
}

func tengoBuiltin(fn func(...tengo.Object) (tengo.Object, error)) *tengo.BuiltinFunction {
	return &tengo.BuiltinFunction{Value: fn}
}
func tengoModules(ctx context.Context) *tengo.ModuleMap {
	m := stdlib.GetModuleMap("math", "text", "times", "rand", "fmt", "hex", "enum")
	m.AddBuiltinModule("json", map[string]tengo.Object{"encode": tengoBuiltin(tengoJSONEncode), "decode": tengoBuiltin(tengoJSONDecode)})
	m.AddBuiltinModule("base64", map[string]tengo.Object{"encode": tengoBuiltin(tengoBase64Encode), "decode": tengoBuiltin(tengoBase64Decode)})
	m.AddBuiltinModule("url", map[string]tengo.Object{"query_escape": tengoBuiltin(tengoURLQueryEscape)})
	m.AddBuiltinModule("crypto", map[string]tengo.Object{"md5": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHash("md5", a...) }), "sha256": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHash("sha256", a...) }), "sha512": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHash("sha512", a...) }), "hmac_sha256": tengoBuiltin(tengoHMAC), "random_bytes": tengoBuiltin(tengoRandomBytes)})
	m.AddBuiltinModule("http", map[string]tengo.Object{"get": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHTTP(ctx, "GET", a...) }), "post": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHTTP(ctx, "POST", a...) }), "put": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHTTP(ctx, "PUT", a...) }), "patch": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHTTP(ctx, "PATCH", a...) }), "delete": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) { return tengoHTTP(ctx, "DELETE", a...) })})
	m.AddBuiltinModule("response", map[string]tengo.Object{"json": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) {
		if len(a) < 1 {
			return nil, errors.New("response.json 需要对象")
		}
		return tengoObject(map[string]interface{}{"content": tengo.ToInterface(a[0]), "type": "json"})
	}), "text": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) {
		s, e := tengoStringArg(a, 0)
		if e != nil {
			return nil, e
		}
		return tengoObject(map[string]interface{}{"content": s, "type": "text"})
	}), "binary": tengoBuiltin(func(a ...tengo.Object) (tengo.Object, error) {
		if len(a) < 2 {
			return nil, errors.New("response.binary 需要数据和 Content-Type")
		}
		b, ok := tengo.ToByteSlice(a[0])
		if !ok {
			if s, ok2 := tengo.ToString(a[0]); ok2 {
				b = []byte(s)
			} else {
				return nil, errors.New("二进制数据无效")
			}
		}
		ct, e := tengoStringArg(a, 1)
		if e != nil {
			return nil, e
		}
		return tengoObject(map[string]interface{}{"content": b, "type": "binary", "content_type": ct})
	})})
	return m
}

func compileTengo(code string, reqObj tengo.Object, ctx context.Context) (*tengo.Compiled, error) {
	// The HTTP module uses its own bounded request context, so its module
	// closure is safe to bind once in cached bytecode. Each execution still
	// receives a fresh req object and a cloned compiled VM state.
	cacheable := true
	key := sha256.Sum256([]byte(code))
	if cacheable {
		tengoCompileCache.Lock()
		base := tengoCompileCache.items[key]
		tengoCompileCache.Unlock()
		if base != nil {
			compiled := base.Clone()
			if err := compiled.Set("req", reqObj); err != nil {
				return nil, err
			}
			return compiled, nil
		}
	}

	script := tengo.NewScript([]byte(code))
	script.EnableFileImport(false)
	script.SetMaxAllocs(maxTengoAllocs)
	script.SetMaxConstObjects(maxTengoConstObjects)
	script.SetImports(tengoModules(context.Background()))
	if err := script.Add("req", reqObj); err != nil {
		return nil, err
	}
	compiled, err := script.Compile()
	if err != nil || !cacheable {
		return compiled, err
	}

	tengoCompileCache.Lock()
	if existing := tengoCompileCache.items[key]; existing != nil {
		compiled = existing.Clone()
		_ = compiled.Set("req", reqObj)
	} else {
		if len(tengoCompileCache.order) >= maxTengoCompileCacheEntries {
			oldest := tengoCompileCache.order[0]
			delete(tengoCompileCache.items, oldest)
			tengoCompileCache.order = tengoCompileCache.order[1:]
		}
		tengoCompileCache.items[key] = compiled
		tengoCompileCache.order = append(tengoCompileCache.order, key)
		compiled = compiled.Clone()
	}
	tengoCompileCache.Unlock()
	return compiled, nil
}

func warmTengoCache(eps []Endpoint) {
	for _, ep := range eps {
		if ep.Kind != "tengo" || ep.Source == "" {
			continue
		}
		if err := compileTengoSource(ep.Source); err != nil {
			log.Printf("启动预编译 Tengo 端点失败 id=%d path=%s: %v", ep.ID, ep.Path, err)
		}
	}
}

func compileTengoSource(code string) error {
	reqObj, err := tengoObject(map[string]interface{}{
		"path": "", "type": "auto", "query": map[string]interface{}{},
		"body": "", "raw_body": []byte{}, "headers": map[string]interface{}{},
	})
	if err != nil {
		return err
	}
	_, err = compileTengo(code, reqObj, context.Background())
	return err
}

func executeTengo(code string, req functionRequest) (functionResponse, error) {
	if len(code) == 0 {
		return functionResponse{}, errors.New("Tengo 源码不能为空")
	}
	if len(code) > maxTengoSourceBytes {
		return functionResponse{}, errors.New("Tengo 源码超过 256 KiB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), functionExecutionTimeout)
	defer cancel()
	reqObj, e := tengoObject(map[string]interface{}{"path": req.Path, "type": req.Type, "query": req.Query, "body": req.Body, "raw_body": req.RawBody, "headers": req.Headers})
	if e != nil {
		return functionResponse{}, e
	}
	compiled, e := compileTengo(code, reqObj, ctx)
	if e != nil {
		return functionResponse{}, fmt.Errorf("编译 Tengo 失败: %w", e)
	}
	if e = compiled.RunContext(ctx); e != nil {
		if ctx.Err() != nil {
			return functionResponse{}, errors.New("Tengo 执行超时")
		}
		return functionResponse{}, fmt.Errorf("Tengo 执行失败: %w", e)
	}
	v := compiled.Get("result")
	if v == nil || v.IsUndefined() {
		return functionResponse{}, errors.New("必须将响应赋值给 result")
	}
	m, e := tengoMapArg(v.Object())
	if e != nil {
		return functionResponse{}, errors.New("result 必须是响应对象")
	}
	out := functionResponse{Headers: map[string]string{}}
	responseType, _ := m["type"].(string)
	content, ok := m["content"]
	if !ok {
		return out, errors.New("result 必须包含 content")
	}
	if responseType == "" {
		return out, errors.New("result 必须包含 type")
	}
	if responseType == "json" {
		b, err := json.Marshal(content)
		if err != nil {
			return out, fmt.Errorf("content 无法序列化为 JSON: %w", err)
		}
		out.Body = string(b)
		out.Headers["Content-Type"] = "application/json; charset=utf-8"
	} else if responseType == "text" {
		out.Body = fmt.Sprint(content)
		out.Headers["Content-Type"] = "text/plain; charset=utf-8"
	} else if responseType == "binary" {
		obj, err := tengoObject(content)
		if err != nil {
			return out, err
		}
		b, ok := tengo.ToByteSlice(obj)
		if !ok {
			if s, yes := content.(string); yes {
				b = []byte(s)
				ok = true
			}
		}
		if !ok {
			return out, errors.New("binary 类型的 content 必须是 bytes 或字符串")
		}
		out.BodyBytes = b
		out.BodyBase64 = base64.StdEncoding.EncodeToString(b)
		out.Headers["Content-Type"] = "application/octet-stream"
		if ct, yes := m["content_type"].(string); yes && ct != "" {
			out.Headers["Content-Type"] = ct
		}
	} else {
		return out, errors.New("type 必须是 json、text 或 binary")
	}
	if req.Type != "" && req.Type != "auto" {
		out.Headers["Content-Type"] = req.Type
	}
	return out, nil
}

func requestToFunctionRequest(r *http.Request, body []byte, responseType string) functionRequest {
	query := make(map[string]interface{}, len(r.URL.Query()))
	for key, values := range r.URL.Query() {
		if len(values) == 1 {
			query[key] = values[0]
		} else {
			query[key] = values
		}
	}
	headers := make(map[string]interface{}, len(r.Header))
	for key, values := range r.Header {
		if len(values) == 1 {
			headers[key] = values[0]
		} else {
			headers[key] = values
		}
	}
	var parsedBody interface{} = string(body)
	if len(body) > 0 && json.Unmarshal(body, &parsedBody) != nil {
		parsedBody = string(body)
	}
	return functionRequest{Method: r.Method, Path: r.URL.Path, Type: responseType, Query: query, Body: parsedBody, RawBody: append([]byte(nil), body...), Headers: headers}
}
