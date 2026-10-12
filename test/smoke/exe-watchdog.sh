#!/usr/bin/env bash
# Independent release-exe test (Windows-only): proxycheck watchdog E2E + core sanity (18083/18084 + mock 9099).
# Run A = ARMED path: fake reg earlier in PATH (per-process only, no system change) reports
#         ProxyEnable=1 + ProxyServer set, NO_PROXY/ProxyOverride "missing"; env NO_PROXY stripped.
# Run B = QUIET path: real registry, env NO_PROXY stripped -> watchdog must stay silent
#         (quiet on any clean machine: CI runners have no system proxy; a dev box with the system
#         proxy on stays quiet as long as user-env NO_PROXY covers loopback).
set -u
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) ;; *) echo "SKIP: exe-watchdog is Windows-only (proxycheck reads the registry)"; exit 0;; esac
cd "$(dirname "$0")/../.."
OUT=test/smoke/out
mkdir -p "$OUT"
CFG="$(pwd -W 2>/dev/null || pwd)/test/smoke/config.json"
FAKE=$(mktemp -d)

MOCK_PID=""
PROXY_PID=""
trap 'for pid in $MOCK_PID $PROXY_PID; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done; wait 2>/dev/null; rm -rf "$FAKE"; true' EXIT

{ for l in '@echo off' \
  'if "%~4"=="ProxyEnable" goto pe' \
  'if "%~4"=="ProxyServer" goto ps' \
  'exit /b 1' \
  ':pe' \
  'echo.' \
  'echo HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings' \
  'echo     ProxyEnable    REG_DWORD    0x1' \
  'exit /b 0' \
  ':ps' \
  'echo.' \
  'echo HKEY_CURRENT_USER\Software\Microsoft\Windows\CurrentVersion\Internet Settings' \
  'echo     ProxyServer    REG_SZ    10.10.1.105:8118' \
  'exit /b 0'; do printf '%s\r\n' "$l"; done; } > "$FAKE/reg.bat"

fail=0
ck() { if [ "$1" = "$2" ]; then echo "PASS: $3"; else echo "FAIL: $3 (got [$1] want [$2])"; fail=1; fi; }

# curl-based probe: any HTTP answer (even 404/429) means occupied; connection refused (exit 7) means free.
probe() { curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1$2"; }
for p in 9099 18083 18084; do
  probe "$p" / && { echo "ABORT: $p busy"; exit 1; }
done
# Build the mock once: killing a "go run" wrapper would orphan the real child process.
go build -o "$OUT/mock.exe" ./test
"$OUT/mock.exe" >"$OUT/mock.log" 2>&1 &
MOCK_PID=$!

wait_ports() { for i in $(seq 1 40); do
  probe 18083 /__logs && probe 18084 / && return 0 # console endpoint / mux 404: no flight created
  sleep 0.5
done; return 1; }
kill_proxy() {
  [ -n "$PROXY_PID" ] && kill "$PROXY_PID" 2>/dev/null; wait "$PROXY_PID" 2>/dev/null; PROXY_PID=""
  sleep 1
  # Insurance against MSYS kill quirks: if the exe still squats on 18083, taskkill the port owner,
  # otherwise the next wait_ports would latch onto the zombie and the run would silently test it.
  local wpid; wpid=$(netstat -ano | grep ':18083 ' | grep LISTENING | awk '{print $NF}' | head -1)
  [ -n "$wpid" ] && { taskkill //F //PID "$wpid" >/dev/null 2>&1; sleep 1; }
  return 0
}

echo "=== Run A: armed path (fake reg + env NO_PROXY stripped) ==="
# Launch via exec-subshell, not an `env` wrapper: MSYS `env` spawns the native exe as a grandchild
# and killing the wrapper PID can orphan it (observed in practice: a zombie still holding
# 18083/18084 after the trap ran). exec replaces the subshell, so $PROXY_PID IS the exe itself.
( PATH="$FAKE:$PATH"; unset NO_PROXY no_proxy; exec ./release/proxy429.exe -config "$CFG" -no-tray ) >"$OUT/exe-a.log" 2>&1 &
PROXY_PID=$!
wait_ports || { echo "ABORT: ports never up (A)"; exit 1; }
sleep 1
curl -s --max-time 10 http://127.0.0.1:18083/__logs/data > "$OUT/data-a.json"
code1=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 -X POST http://127.0.0.1:18083/v1/messages -H "Content-Type: application/json" -d '{"model":"smoke","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"hi"}]}')
code2=$(curl -s -o /dev/null -w "%{http_code}" --max-time 30 -X POST http://127.0.0.1:18084/v1/responses -H "Content-Type: application/json" -d '{"model":"gpt-5-codex","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}')
curl -s --max-time 10 http://127.0.0.1:18083/__logs > "$OUT/page-a.html"
# The version stamp mirrors build.sh: "release <tag>" when HEAD is exactly a version tag (CD tag builds),
# else "beta <HEAD short hash>-<build HHMM>". Pin it so a stale exe is caught.
if TAG=$(git describe --exact-match --tags HEAD 2>/dev/null); then
  vexp='"version":"release '"$TAG"
else
  vexp='"version":"beta '"$(git rev-parse --short HEAD)"'-'
fi
if grep -q "$vexp" "$OUT/data-a.json"; then v=1; else v=0; fi
ck "$v" "1" "version stamp matches HEAD build"
ck "$code1" "200" "anthropic port proxies"
ck "$code2" "200" "responses port proxies"
ck "$(grep -o '"hijackRisk":true' "$OUT/data-a.json" | head -1)" '"hijackRisk":true' "hijackRisk armed in /__logs/data"
# Assert against the log ring in /__logs/data (the canonical store the console polls), not the
# stderr capture: this is a GUI-subsystem exe launched through MSYS layers, where a single stderr
# write can vanish while the ring (fetched over HTTP from the process under test) is reliable.
# data-a.json is one compact JSON line, so count occurrences with grep -o, not lines with grep -c.
ck "$(grep -o '\[proxycheck\] system proxy is on' "$OUT/data-a.json" | wc -l)" "1" "armed transition log line"
ck "$(grep -o '10\.10\.1\.105:8118' "$OUT/data-a.json" | head -1)" "10.10.1.105:8118" "log line carries proxy address"
bw=$(grep -c 'id="hijackWarn"' "$OUT/page-a.html"); [ "$bw" -ge 1 ] && bw=1
ck "$bw" "1" "banner div present in page"
js=$(grep -c 'd\.hijackRisk' "$OUT/page-a.html"); [ "$js" -ge 1 ] && js=1
ck "$js" "1" "poll JS toggles banner on hijackRisk"
kill_proxy

echo "=== Run B: quiet path (real registry + env NO_PROXY stripped) ==="
( unset NO_PROXY no_proxy; exec ./release/proxy429.exe -config "$CFG" -no-tray ) >"$OUT/exe-b.log" 2>&1 &
PROXY_PID=$!
wait_ports || { echo "ABORT: ports never up (B)"; exit 1; }
sleep 1
curl -s --max-time 10 http://127.0.0.1:18083/__logs/data > "$OUT/data-b.json"
ck "$(grep -c '"hijackRisk"' "$OUT/data-b.json")" "0" "hijackRisk absent (omitempty, quiet)"
# Ring target here too: a lost stderr write would make the zero-count pass vacuously.
ck "$(grep -o '\[proxycheck\]' "$OUT/data-b.json" | wc -l)" "0" "zero proxycheck log lines (quiet)"
kill_proxy

[ $fail -eq 0 ] && echo "EXE TEST PASS" || { echo "EXE TEST FAIL"; exit 1; }
