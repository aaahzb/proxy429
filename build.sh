#!/usr/bin/env bash
# 构建代理。版本号注入 "git短hash-构建时分"（如 c639d56-1545）。
# 跨平台：按宿主 GOOS 出对应产物到 release/：
#   darwin  -> release/Proxy429.app（LSUIElement 菜单栏应用，无 Dock 图标）+ ad-hoc 签名
#   linux   -> release/proxy429
#   windows -> release/proxy429.exe（GUI 子系统 -H=windowsgui，无控制台窗口）
# 用法：bash build.sh
# 托盘库 fyne.io/systray：darwin 走 cgo（AppKit，需 clang），linux/windows 纯 Go 免 C 编译器。
set -e
cd "$(dirname "$0")"

# go 不在 PATH 时回落到 ~/go/bin（本机 go 装在此处）；仍找不到则报错退出。
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/go/bin/go" ]; then export PATH="$HOME/go/bin:$PATH"; else
    echo "错误：找不到 go，请先安装或加入 PATH" >&2; exit 1
  fi
fi
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

HASH=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
TIME=$(date +%H%M)
VERSION="$HASH-$TIME"
mkdir -p release

build_bin() {  # $1 = 输出路径 $2 = 额外 ldflags
  go build -buildvcs=false -ldflags "-X main.Version=$VERSION $2" -o "$1" .
}

case "$(go env GOOS)" in
  darwin)
    export CGO_ENABLED=1
    build_bin release/proxy429 ""
    APP=release/Proxy429.app
    rm -rf "$APP"
    mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
    mv release/proxy429 "$APP/Contents/MacOS/proxy429"
    cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>Proxy429</string>
  <key>CFBundleDisplayName</key><string>Proxy429</string>
  <key>CFBundleIdentifier</key><string>com.proxy429.app</string>
  <key>CFBundleVersion</key><string>1</string>
  <key>CFBundleShortVersionString</key><string>1.0</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleExecutable</key><string>proxy429</string>
  <key>LSMinimumSystemVersion</key><string>10.13</string>
  <key>LSUIElement</key><true/>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
    # ad-hoc 签名：无开发者证书时的最佳选择；首次启动需在 Finder 右键「打开」过 Gatekeeper。
    if codesign -s - --force --deep "$APP" >/dev/null 2>&1; then
      echo "已 ad-hoc 签名（首次启动需右键->打开）"
    else
      echo "未签名（codesign 不可用，可手动 codesign -s - --force $APP）"
    fi
    [ -f 使用说明.md ] && cp 使用说明.md release/ || true
    echo "BUILD_OK 版本=$VERSION 平台=darwin/$(go env GOARCH) -> $APP"
    ;;
  linux)
    export CGO_ENABLED=0
    build_bin release/proxy429 ""
    [ -f 使用说明.md ] && cp 使用说明.md release/ || true
    echo "BUILD_OK 版本=$VERSION 平台=linux/$(go env GOARCH) -> release/proxy429"
    ;;
  windows)
    export CGO_ENABLED=0
    # -H=windowsgui：GUI 子系统，启动不弹控制台窗口，纯托盘运行；日志看网页控制台或 log_file。
    build_bin release/proxy429.exe "-H=windowsgui"
    [ -f 使用说明.md ] && cp 使用说明.md release/ || true
    echo "BUILD_OK 版本=$VERSION 平台=windows/$(go env GOARCH) -> release/proxy429.exe"
    ;;
esac
