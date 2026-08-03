#!/usr/bin/env bash
# 一次性构建全平台产物：Windows(带图标) + Linux amd64/arm64
# 用法: ./build.sh    （在 Git Bash / WSL / Linux 下运行）
set -e
cd "$(dirname "$0")"

LDFLAGS="-s -w"

echo "==> Windows amd64 (gonx.exe, 带图标)"
# rsrc_windows.syso 会被 windows 构建自动嵌入
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "$LDFLAGS" -o gonx.exe ./cmd/gonx

echo "==> Linux amd64 (gonx-linux-amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$LDFLAGS" -o gonx-linux-amd64 ./cmd/gonx

echo "==> Linux arm64 (gonx-linux-arm64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$LDFLAGS" -o gonx-linux-arm64 ./cmd/gonx

echo ""
echo "构建完成："
ls -la gonx.exe gonx-linux-amd64 gonx-linux-arm64 | awk '{printf "  %-22s %8.1f MB\n", $NF, $5/1048576}'
