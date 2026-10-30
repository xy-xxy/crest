#!/usr/bin/env bash
# 一次性构建全平台产物：Windows(带图标) + Linux amd64/arm64 + Windows 7 amd64
# 用法: ./build.sh    （在 Git Bash / WSL / Linux 下运行）
# 可设置 OUTPUT_DIR 指定产物目录，默认当前项目目录。
# Win7 使用独立 SDK，可通过 WIN7_GOROOT 指定 Go 1.20.14 安装目录。
set -e
cd "$(dirname "$0")"

LDFLAGS="-s -w"
OUTPUT_DIR="${OUTPUT_DIR:-.}"

BUILDFLAGS="-trimpath"   # 不把开发机绝对路径写进二进制

WIN7_GOROOT="${WIN7_GOROOT:-$HOME/.local/share/crest-build/go1.20.14/go}"
if [ ! -x "$WIN7_GOROOT/bin/go" ]; then
    echo "找不到 Win7 工具链，请设置 WIN7_GOROOT 为独立 Go 1.20.14 的安装目录。" >&2
    exit 1
fi
# 转为绝对路径；环境设置只在子进程内生效，不影响原来的三个构建目标。
WIN7_GOROOT="$(cd "$WIN7_GOROOT" && pwd)"
win7_go() {
    GOROOT="$WIN7_GOROOT" GOENV=off GOTOOLCHAIN=local "$WIN7_GOROOT/bin/go" "$@"
}
case "$(win7_go version)" in
    "go version go1.20.14 "*) ;;
    *)
        echo "Win7 构建必须使用 Go 1.20.14，请检查 WIN7_GOROOT。" >&2
        exit 1
        ;;
esac

mkdir -p "$OUTPUT_DIR"

echo "==> Windows amd64 (crest.exe, 带图标)"
# rsrc_windows.syso 会被 windows 构建自动嵌入
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUTPUT_DIR/crest.exe" ./cmd/crest

echo "==> Linux amd64 (crest-linux-amd64)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUTPUT_DIR/crest-linux-amd64" ./cmd/crest

echo "==> Linux arm64 (crest-linux-arm64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUTPUT_DIR/crest-linux-arm64" ./cmd/crest

echo "==> Windows 7 amd64 (crest-win7-amd64.exe, Go 1.20.14)"
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 win7_go build $BUILDFLAGS -ldflags "$LDFLAGS" -o "$OUTPUT_DIR/crest-win7-amd64.exe" ./cmd/crest

echo ""
echo "构建完成："
ls -la "$OUTPUT_DIR/crest.exe" "$OUTPUT_DIR/crest-linux-amd64" "$OUTPUT_DIR/crest-linux-arm64" "$OUTPUT_DIR/crest-win7-amd64.exe" | awk '{printf "  %-22s %8.1f MB\n", $NF, $5/1048576}'
