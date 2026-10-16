# syntax=docker/dockerfile:1

# ============ 构建阶段：从修正版源码编译 ============
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

WORKDIR /src

# 仅复制源码，仓库内闭源旧二进制不再使用
COPY src/go.mod src/go.mod
COPY src/cfnat.go src/cfnat.go

# CGO 关闭以产出静态二进制；armv6/v7 通过 GOARM 区分
RUN apk add --no-cache git && \
    case "$TARGETVARIANT" in \
        v6) export GOARM=6 ;; \
        v7) export GOARM=7 ;; \
    esac && \
    cd src && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w" -o /cfnat .

# ============ 运行阶段 ============
FROM --platform=$TARGETPLATFORM alpine:3.19

COPY --from=builder /cfnat ./cfnat
COPY go.sh ./go.sh
COPY ips-v4.txt ./ips-v4.txt
COPY ips-v6.txt ./ips-v6.txt
COPY locations.json ./locations.json

RUN chmod +x ./cfnat ./go.sh

# 设置环境变量默认值
ENV colo="SJC,LAX,HKG" \
    delay="300" \
    ipnum="10" \
    ips="4" \
    num="10" \
    port="443" \
    probe="60" \
    random="true" \
    task="100" \
    tls="true" \
    code="200" \
    domain="cloudflaremirrors.com/debian"

# 暴露 1234 端口
EXPOSE 1234

# 运行 go.sh 脚本
CMD ["/bin/sh", "./go.sh"]
