#!/usr/bin/env bash
# Build the proxy. Version stamp injected as "<git short hash>-<build HHMM>" (e.g. c639d56-1545).
# Cross-platform: produces artifacts under release/ per host GOOS:
#   darwin  -> release/Proxy429.app (LSUIElement menu-bar app, no Dock icon) + ad-hoc signature
#   linux   -> release/proxy429
#   windows -> release/proxy429.exe (GUI subsystem -H=windowsgui, no console window)
# Usage: bash build.sh
# Tray library fyne.io/systray: darwin goes through cgo (AppKit, needs clang); linux/windows are pure Go, no C compiler needed.
set -e
cd "$(dirname "$0")"

# Fall back to ~/go/bin when go isn't on PATH (this machine's go lives there); error out if still missing.
if ! command -v go >/dev/null 2>&1; then
  if [ -x "$HOME/go/bin/go" ]; then export PATH="$HOME/go/bin:$PATH"; else
    echo "error: go not found; install it or add it to PATH first" >&2; exit 1
  fi
fi
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

HASH=$(git rev-parse --short HEAD 2>/dev/null || echo dev)
TIME=$(date +%H%M)
VERSION="$HASH-$TIME"
mkdir -p release

build_bin() {  # $1 = output path, $2 = extra ldflags
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
    # ad-hoc signature: the best option without a developer certificate; first launch needs right-click -> Open in Finder to pass Gatekeeper.
    if codesign -s - --force --deep "$APP" >/dev/null 2>&1; then
      echo "ad-hoc signed (first launch needs right-click -> Open)"
    else
      echo "unsigned (codesign unavailable; run manually: codesign -s - --force $APP)"
    fi
    [ -f docs/usage.md ] && cp docs/usage.md release/ || true
    echo "BUILD_OK version=$VERSION platform=darwin/$(go env GOARCH) -> $APP"
    ;;
  linux)
    export CGO_ENABLED=0
    build_bin release/proxy429 ""
    [ -f docs/usage.md ] && cp docs/usage.md release/ || true
    echo "BUILD_OK version=$VERSION platform=linux/$(go env GOARCH) -> release/proxy429"
    ;;
  windows)
    export CGO_ENABLED=0
    # -H=windowsgui: GUI subsystem, no console window on launch, pure tray operation; logs via the web console or log_file.
    build_bin release/proxy429.exe "-H=windowsgui"
    [ -f docs/usage.md ] && cp docs/usage.md release/ || true
    echo "BUILD_OK version=$VERSION platform=windows/$(go env GOARCH) -> release/proxy429.exe"
    ;;
esac
