# api-server

`api-server` 是一个轻量级、自用的 API 端点服务，支持静态响应和 Tengo 云函数响应。

## 快速安装（一键）

中国大陆用户：

```bash
curl -fL 'https://raw.giteeusercontent.com/PYLinTech/api-server/raw/main/install.sh' -o install.sh && chmod +x install.sh && ./install.sh
```

国际用户：

```bash
curl -fL 'https://raw.githubusercontent.com/PYLinTech/api-server/refs/heads/main/install.sh' -o install.sh && chmod +x install.sh && ./install.sh
```

脚本会拉取 `pylintech/api-server` 镜像、引导创建管理员账号（密码由程序自身生成，写入数据卷）、启动容器。数据（`.env` / `api.db`）存于 Docker 命名卷，不依赖宿主目录。

## 一键发布（镜像）

工作电脑上编译并推送多架构镜像到 `pylintech/api-server`：

```bash
./docker-build.sh --version v0.1.0             # 编译 amd64+arm64 二进制 → 构建多架构镜像 → 推送
./docker-build.sh --version v0.1.0 --no-push   # 仅本地构建（本机架构）
./docker-build.sh --version v0.1.0 --skip-build  # 跳过二进制编译，直接用 dist/ 已有产物
```

发布前请先 `docker login`。

## 功能

- 创建、编辑、删除和测试 API 端点。
- 支持 GET、POST、PUT、PATCH、DELETE 请求方法。
- 支持静态响应和 Tengo 动态端点。
- Tengo 作为云函数运行，读取用户输入和系统输入后生成响应内容。
- 支持查询参数、请求体、请求头、原始请求体和请求路径。
- 支持通过 Tengo `http` 模块访问上游 HTTP 服务。
- 支持 JSON、文本和二进制响应。
- 内置登录认证和单管理员管理面板。
- 使用 SQLite 保存端点配置。

## 快速启动（本机）

首次启动时，程序会检查 `.env`。如果配置不存在或不完整，会在交互式终端引导创建管理员账号，并生成密码哈希配置（写入 `.env`）。

```bash
go build -o api-server .
./api-server
```

默认监听端口为 `8081`，可以通过 `PORT` 修改：

```bash
PORT=8081 ./api-server
```

启动后打开 `http://localhost:8081/login` 登录管理面板，即可创建静态或 Tengo 云函数端点。

## 部署结构

- `Dockerfile`：运行镜像只 `COPY` 预编译二进制（`dist/api-server-linux-<arch>`），不在镜像内编译
- `docker-compose.yml`：端口 8081，数据存于 Docker 命名卷 `api-server-data`（`.env` 与 `api.db`），不依赖宿主目录
- `install.sh`：一键安装（拉取镜像 + 初始化管理员 + 启动容器）
- `build.sh`：工作电脑编译二进制（本机 + `--linux-amd64`/`--linux-arm64` 交叉编译）
- `docker-build.sh`：一键发布（编译目标架构二进制 → 构建多架构镜像并推送到 `pylintech/api-server`）

install.sh 更多命令：

```bash
./install.sh --tag v0.1.0              # 部署指定版本镜像
./install.sh --status                  # 查看状态
./install.sh --restart                 # 重启
./install.sh --stop                    # 停止（保留数据）
./install.sh --logs                    # 查看日志
./install.sh --uninstall               # 卸载（保留数据）
./install.sh --uninstall --purge       # 卸载并删除数据卷
```

## Tengo 云函数

Tengo 只负责读取输入、执行业务逻辑并生成输出，不负责路由、请求方法或服务器响应封装。

```tengo
response := import("response")

result := response.json({
  "success": true,
  "data": req.query
})
```

当需要在分支中覆盖结果时，必须先声明，再使用赋值：

```tengo
response := import("response")

result := response.json({
  "success": true,
  "data": "ok"
})

if req.query.name == undefined {
  result = response.json({
    "success": false,
    "error": "缺少 name"
  })
}
```

所有 map key 使用双引号。响应通过 `response.json`、`response.text` 或 `response.binary` 生成。

`http.get`、`http.post`、`http.put`、`http.patch` 和 `http.delete` 返回上游响应信息，其中包含 `status`、`headers`、`body` 和 `body_bytes`，上游状态码只能用于业务判断。

## 目录

- `main.go`：服务启动与路由注册。
- `handlers.go`：认证接口、端点管理、测试和动态端点处理。
- `tengoengine.go`：Tengo 运行时、模块和响应转换。
- `pages.go`：内嵌管理面板页面。
- `db.go`：SQLite 数据库初始化和端点存储。
- `auth.go`、`session.go`：管理员认证和会话管理。
- `Dockerfile`、`docker-compose.yml`、`.dockerignore`：Docker 部署。
- `install.sh`：一键安装/部署脚本。
- `build.sh`：本地编译脚本。
- `docker-build.sh`：一键发布脚本。
- `start.sh`：启动脚本。

## 开发验证

```bash
gofmt -w *.go
go test ./...
./build.sh                # 编译本机二进制 → dist/api-server
./build.sh --linux-amd64  # 交叉编译 linux/amd64（Docker）
```

## 许可证

本项目基于 [Apache License 2.0](LICENSE) 开源。

Copyright © 2026 PYLinTech（重庆沛雨霖科技有限公司）
