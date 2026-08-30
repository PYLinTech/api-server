# api-server 运行镜像：只 COPY 工作电脑预编译的二进制，不在镜像内编译
# 前置：先运行 ./build.sh 生成 dist/api-server-linux-<arch>（TARGETARCH: amd64/arm64）
# 用法: docker build -t pylintech/api-server .  （或 ./docker-build.sh --version v0.1.0）

FROM alpine:3.20
ARG TARGETARCH
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
# 按目标架构 COPY 对应预编译二进制（由 build.sh 产出）
COPY dist/api-server-linux-${TARGETARCH} /app/api-server
# 数据目录持久化 .env / api.db（启动时以该目录为工作目录）
RUN mkdir -p /app/data
VOLUME ["/app/data"]
EXPOSE 8081
ENTRYPOINT ["sh", "-c", "cd /app/data && exec /app/api-server"]
