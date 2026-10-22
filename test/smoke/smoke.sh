#!/usr/bin/env bash
# Lineage smoke v3 (cross-platform, CI-ready): chained same-session requests on Anthropic (18083) + Responses (18084) ports.
# Pairs: a1/a2 plain chain (sess A), b1/b2 cache_control breakpoint migration (sess B),
# c1/c2 interior tail-element growth (sess C), r1/r2 Responses chain. a3 = session-less control.
# Portable: curl-based port probes (no netstat/taskkill), cleanup by PID, proxy runs with -no-tray.
# Requires a fresh build first (bash build.sh). CI runs this on ubuntu/windows/macos; locally: bash test/smoke/smoke.sh
# Search: s1 routes via search_fallback (the smoke-sf route's own claudeSearchSummary=forceHigh overrides sf's forceMid
# per field; sf's thinking=low still applies); s2 via enhance_search (legacy summary_*). Both must reach the mock exactly
# once (single-request summary injection, no step1/step2) and stream its search answer back untouched.
set -u
cd "$(dirname "$0")/../.."
CFG="$(pwd -W 2>/dev/null || pwd)/test/smoke/config.json"
OUT=test/smoke/out
mkdir -p "$OUT"

EXE_SUFFIX=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE_SUFFIX=".exe" ;; esac
EXE="release/proxy429$EXE_SUFFIX"
[ -f "$EXE" ] || EXE="release/Proxy429.app/Contents/MacOS/proxy429" # macOS build.sh moves the binary into the .app
[ -f "$EXE" ] || { echo "ABORT: no built proxy found (run bash build.sh first)"; exit 1; }

# curl-based probe: any HTTP answer (even 404/429) means occupied; connection refused (exit 7) means free.
probe() { curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1$2"; }
for p in 9099 18083 18084; do
  if probe "$p" /; then echo "ABORT: $p already listening"; exit 1; fi
done

MOCK_PID=""
PROXY_PID=""
trap 'for pid in $MOCK_PID $PROXY_PID; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done; wait 2>/dev/null; true' EXIT

# Build the mock once: killing a "go run" wrapper would orphan the real child process.
go build -o "$OUT/mock$EXE_SUFFIX" ./test
"$OUT/mock$EXE_SUFFIX" >"$OUT/mock.log" 2>&1 &
MOCK_PID=$!
"$EXE" -config "$CFG" -no-tray &
PROXY_PID=$!

ok=0
for i in $(seq 1 60); do
  a=0; b=0; c=0
  probe 9099 / && a=1        # mock answers 429 on / — fine
  probe 18083 /__logs && b=1 # console endpoint: creates no flight
  probe 18084 / && c=1       # responses mux 404: creates no flight
  [ "$a$b$c" = "111" ] && { ok=1; break; }
  sleep 0.5
done
echo "listeners: 9099=$a 18083=$b 18084=$c"
[ "$ok" = 1 ] || { echo "ABORT: proxy ports never came up"; exit 1; }

# Anthropic port: metadata.user_id inner-JSON session_id (escaped quotes preserved literally).
cat > "$OUT/a1.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-A\"}"},"messages":[{"role":"user","content":"A"}]}
EOF
cat > "$OUT/a2.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-A\"}"},"messages":[{"role":"user","content":"A"},{"role":"assistant","content":"a1"},{"role":"user","content":"B"}]}
EOF
cat > "$OUT/a3.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"A"}]}
EOF
# b pair: ephemeral cache breakpoint rides the first block in b1 and is gone in b2 (client migration).
cat > "$OUT/b1.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-B\"}"},"messages":[{"role":"user","content":[{"type":"text","text":"BA","cache_control":{"type":"ephemeral"}}]}]}
EOF
cat > "$OUT/b2.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-B\"}"},"messages":[{"role":"user","content":[{"type":"text","text":"BA"}]},{"role":"assistant","content":"b1"},{"role":"user","content":"BB"}]}
EOF
# c pair: last message grows a new block INTERIORLY (before TAIL) — not a byte-prefix extension.
cat > "$OUT/c1.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-C\"}"},"messages":[{"role":"user","content":"CU0"},{"role":"assistant","content":"ca0"},{"role":"user","content":[{"type":"text","text":"CX"},{"type":"text","text":"TAIL"}]}]}
EOF
cat > "$OUT/c2.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"metadata":{"user_id":"{\"session_id\":\"smoke-sess-C\"}"},"messages":[{"role":"user","content":"CU0"},{"role":"assistant","content":"ca0"},{"role":"user","content":[{"type":"text","text":"CX"},{"type":"text","text":"NEW"},{"type":"text","text":"TAIL"}]}]}
EOF
# Responses port: alternating roles (the translator merges adjacent same-role messages otherwise).
cat > "$OUT/r1.json" <<'EOF'
{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"smoke-resp-sess","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"A"}]}]}
EOF
cat > "$OUT/r2.json" <<'EOF'
{"model":"gpt-5-codex","stream":true,"prompt_cache_key":"smoke-resp-sess","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"A"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a1"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"B"}]}]}
EOF

for n in a1 a2 a3 b1 b2 c1 c2; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "anthropic $n -> HTTP $code"
done
for n in r1 r2; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18084/v1/responses" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "responses $n -> HTTP $code"
done

# Classifier split smoke: CC security-monitor prefix + Codex guardian prefix, per-source policy cc=off / codex=low; classifier_route uses mock /think so responses carry thinking blocks (strip differential). g3 exercises the classifier-low floor bump (tiny max_tokens raised +1024 upstream-side).
# g1 (id 10): Responses port, claude-fable-5 + reasoning effort high -> translated to adaptive -> off-normalized, then quietly upgraded to adaptive low upstream (rewrite; thinking stripped on return).
cat > "$OUT/g1.json" <<'EOF'
{"model":"claude-fable-5","stream":true,"instructions":"You are judging one planned coding-agent action.\nAssess it.","reasoning":{"effort":"high"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"approve?"}]}]}
EOF
# g2 (id 11): Responses port, bare fable (not adaptive-classified), no reasoning -> off-normalized (fields appended), then quietly upgraded to enabled+2048 (rewrite).
cat > "$OUT/g2.json" <<'EOF'
{"model":"fable","stream":true,"instructions":"You are judging one planned coding-agent action.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"approve?"}]}]}
EOF
# g3 (id 14, fired after k2 to keep the id order): guardian prefix + budget-class bare fable + max_output_tokens=64 ->
# translated max_tokens=64 can't fit the 1024 floor -> classifier-low bumps max_tokens to 1088 upstream-side, budget exactly 1024 (rewrite; stripped on return).
cat > "$OUT/g3.json" <<'EOF'
{"model":"fable","stream":true,"max_output_tokens":64,"instructions":"You are judging one planned coding-agent action.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"approve?"}]}]}
EOF
# k3 (id 15, fired after g3): Anthropic port, CC prefix + ALREADY thinking disabled + cc=off -> pass-through:
# no reasoning_effort append, no rewrite count (the mode is already enforced; re-applying it would be pointless).
cat > "$OUT/k3.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"system":"You are a security monitor. Check this.","thinking":{"type":"disabled"},"messages":[{"role":"user","content":"x"}]}
EOF
# g4 (id 16, fired after k3): guardian prefix + reasoning effort low -> translated low shape already IS the codex=low
# target -> pass-through: no off detour, no upgrade, no strip (the client asked for thinking, so reasoning blocks stay).
cat > "$OUT/g4.json" <<'EOF'
{"model":"fable","stream":true,"instructions":"You are judging one planned coding-agent action.","reasoning":{"effort":"low"},"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"approve?"}]}]}
EOF
# k1 (id 12): Anthropic port, CC prefix + adaptive + output_config effort high -> cc=off: thinking disabled (rewrite).
cat > "$OUT/k1.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"system":"You are a security monitor. Check this.","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"x"}]}
EOF
# k2 (id 13): Anthropic port, CC prefix + enabled budget 2048 -> cc=off rewrites any shape: thinking disabled, budget gone.
cat > "$OUT/k2.json" <<'EOF'
{"model":"smoke","max_tokens":64,"stream":true,"system":"You are a security monitor. Check this.","thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"x"}]}
EOF
for n in g1 g2; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18084/v1/responses" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "responses $n -> HTTP $code"
done
for n in k1 k2; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "anthropic $n -> HTTP $code"
done
code=$(curl -s -o "$OUT/g3.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18084/v1/responses" -H "Content-Type: application/json" --data-binary "@$OUT/g3.json")
echo "responses g3 -> HTTP $code"
code=$(curl -s -o "$OUT/k3.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/k3.json")
echo "anthropic k3 -> HTTP $code"
code=$(curl -s -o "$OUT/g4.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18084/v1/responses" -H "Content-Type: application/json" --data-binary "@$OUT/g4.json")
echo "responses g4 -> HTTP $code"

# Search single-request smoke: s1 (id 17) hits route smoke-sf (no_search) -> search_fallback; the route's own
# claudeSearchSummary=forceHigh overrides sf's forceMid per field, while sf's claudeSearchThinking=low still applies.
# s2 (id 18) hits route smoke-enh -> enhance_search legacy path (summary_level=high, summary_thinking -> low).
cat > "$OUT/s1.json" <<'EOF'
{"model":"smoke-sf","max_tokens":8192,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: smoke weather"}]}
EOF
cat > "$OUT/s2.json" <<'EOF'
{"model":"smoke-enh","max_tokens":8192,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: smoke transit"}]}
EOF
for n in s1 s2; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "anthropic $n -> HTTP $code"
done

sleep 1
curl -s --max-time 10 "http://127.0.0.1:18083/__recentflights" > "$OUT/recent.json"
curl -s --max-time 10 "http://127.0.0.1:18083/__logs" > "$OUT/page.html"

echo "--- /__recentflights id:last pairs:"
pairs=$(sed 's/},{/}\n{/g' "$OUT/recent.json" | sed -n 's/.*"id":\([0-9]*\).*"last":\([0-9]*\).*/\1:\2/p' | grep -v ':0$' | sort -n | tr '\n' ' ' | sed 's/ $//')
echo "[$pairs]"
want="2:1 5:4 7:6 9:8"
echo "--- page HTML markers (want >=1 each):"
grep -c '&lt;-' "$OUT/page.html"; grep -c 'lastTag' "$OUT/page.html"
echo "--- resp bodies head:"
for n in a1 a2 b1 b2 c1 c2 r1 r2; do echo "[$n] $(head -c 100 "$OUT/$n.resp" | tr -d '\r\n')"; done
echo "--- classifier counters (want classifiers=7 noThink=5, CC 3/2, Codex 4/3; k3/g4 are pass-through hits: counted but not rewritten):"
curl -s --max-time 10 "http://127.0.0.1:18083/__logs/data" > "$OUT/data.json"
grep -o '"classifiers[A-Za-z]*":[0-9]*' "$OUT/data.json" | sort
clsok=1
for kv in '"classifiers":7' '"classifierNoThink":5' '"classifiersCC":3' '"classifierRewritesCC":2' '"classifiersCodex":4' '"classifierRewritesCodex":3'; do
  grep -q "$kv" "$OUT/data.json" || { echo "COUNTER MISSING: $kv"; clsok=0; }
done
up=$(grep -oE '"bytesUp":[0-9]+' "$OUT/data.json" | cut -d: -f2)
down=$(grep -oE '"bytesDown":[0-9]+' "$OUT/data.json" | cut -d: -f2)
echo "--- upstream traffic counters: bytesUp=$up bytesDown=$down (want both >0)"
[ "${up:-0}" -gt 0 ] && [ "${down:-0}" -gt 0 ] || { echo "TRAFFIC COUNTER MISSING/ZERO"; clsok=0; }
grep -q '"heatBase":' "$OUT/data.json" || { echo "HEAT heatBase MISSING"; clsok=0; }
grep -q '"heat":\[\[' "$OUT/data.json" || { echo "HEAT heat ARRAY MISSING"; clsok=0; }
grep -qF 'var heatMetric = 2;' "$OUT/page.html" || { echo "HEAT DEFAULT METRIC NOT OUTPUT"; clsok=0; }
g1up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=10&side=up")
echo "$g1up" | grep -q '"type":"adaptive"' && echo "$g1up" | grep -q '"effort":"low"' || { echo "G1 LOW REWRITE MISSING"; clsok=0; }
k1up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=12&side=up")
echo "$k1up" | grep -q '"type":"disabled"' || { echo "K1 OFF REWRITE MISSING"; clsok=0; }
g2up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=11&side=up")
echo "$g2up" | grep -q '"budget_tokens":2048' || { echo "G2 LOW UPGRADE MISSING (want enabled+2048)"; clsok=0; }
! echo "$g2up" | grep -q 'output_config' || { echo "G2 UNEXPECTED output_config"; clsok=0; }
k2up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=13&side=up")
echo "$k2up" | grep -q '"type":"disabled"' && ! echo "$k2up" | grep -q 'budget_tokens' || { echo "K2 OFF REWRITE WRONG"; clsok=0; }
g3up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=14&side=up")
echo "$g3up" | grep -q '"budget_tokens":1024' || { echo "G3 FLOOR BUMP MISSING (want enabled+1024)"; clsok=0; }
echo "$g3up" | grep -q '"max_tokens":1088' || { echo "G3 MAX_TOKENS RAISE MISSING (want 64+1024=1088)"; clsok=0; }
k3up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=15&side=up")
echo "$k3up" | grep -q '"type":"disabled"' || { echo "K3 THINKING LOST (pass-through must keep disabled)"; clsok=0; }
! echo "$k3up" | grep -q 'reasoning_effort' || { echo "K3 REDUNDANT APPEND (already-off must pass through untouched)"; clsok=0; }
grep -q 'thinking_delta' "$OUT/k3.resp" || { echo "K3 THINKING SHOULD PASS THROUGH (off never strips)"; clsok=0; }
g4up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=16&side=up")
! echo "$g4up" | grep -q '"type":"disabled"' || { echo "G4 OFF DETOUR (already-low must not be off-normalized)"; clsok=0; }
grep -q '"type":"reasoning"' "$OUT/g4.resp" || { echo "G4 REASONING SHOULD PASS THROUGH (already-low never strips)"; clsok=0; }
grep -q 'thinking_delta' "$OUT/k1.resp" || { echo "K1 THINKING SHOULD PASS THROUGH (off never strips)"; clsok=0; }
grep -q 'thinking_delta' "$OUT/k2.resp" || { echo "K2 THINKING SHOULD PASS THROUGH (off never strips)"; clsok=0; }
grep -q '"type":"reasoning"' "$OUT/g1.resp" && { echo "G1 REASONING NOT STRIPPED"; clsok=0; }
grep -q '"type":"reasoning"' "$OUT/g2.resp" && { echo "G2 REASONING NOT STRIPPED"; clsok=0; }
grep -q '"type":"reasoning"' "$OUT/g3.resp" && { echo "G3 REASONING NOT STRIPPED"; clsok=0; }
grep -qE "off-.{1,10}low" "$OUT/recent.json" || { echo "OFF->LOW BADGE MISSING in recent.json"; clsok=0; }
# --- search single-request assertions (s1 id 17 = route claudeSearchSummary overriding sf per field; s2 id 18 = legacy via enhance_search):
searchok=1
s1up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=17&side=up")
echo "$s1up" | grep -q 'Perform a web search for the query: smoke weather' || { echo "S1 MARKER LOST upstream"; searchok=0; }
echo "$s1up" | grep -q 'After performing the web search' || { echo "S1 INJECTION MISSING"; searchok=0; }
echo "$s1up" | grep -q 'detailed comprehensive plaintext summary' || { echo "S1 ROUTE OVERRIDE NOT HIGH"; searchok=0; }
echo "$s1up" | grep -q 'medium-detail' && { echo "S1 SF FORCEMID LEAKED (route override lost)"; searchok=0; }
echo "$s1up" | grep -q '"budget_tokens":2048' || { echo "S1 THINKING-LOW REWRITE MISSING"; searchok=0; }
echo "$s1up" | grep -q '"model":"searchmodel"' || { echo "S1 FALLBACK MODEL SWAP MISSING"; searchok=0; }
s2up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=18&side=up")
echo "$s2up" | grep -q 'Perform a web search for the query: smoke transit' || { echo "S2 MARKER LOST upstream"; searchok=0; }
echo "$s2up" | grep -q 'After performing the web search' || { echo "S2 INJECTION MISSING"; searchok=0; }
echo "$s2up" | grep -q 'detailed comprehensive plaintext summary' || { echo "S2 LEVEL NOT HIGH"; searchok=0; }
echo "$s2up" | grep -q 'data points, direct quotes' || { echo "S2 HIGH WORDING MISSING"; searchok=0; }
echo "$s2up" | grep -q 'verbatim' && { echo "S2 UNEXPECTED VERBATIM (max-only wording leaked)"; searchok=0; }
echo "$s2up" | grep -q '"budget_tokens":2048' || { echo "S2 LEGACY summary_thinking->low MISSING"; searchok=0; }
for n in s1 s2; do
  grep -q 'server_tool_use' "$OUT/$n.resp" || { echo "$n RESPONSE NOT A SEARCH ANSWER (server_tool_use missing)"; searchok=0; }
  grep -q 'web_search_tool_result' "$OUT/$n.resp" || { echo "$n RESPONSE MISSING web_search_tool_result"; searchok=0; }
  grep -q 'Result 1: smoke summary' "$OUT/$n.resp" || { echo "$n SUMMARY TEXT NOT PASSED THROUGH"; searchok=0; }
done
scount=$(grep -c 'mode=search' "$OUT/mock.log")
echo "mock mode=search POST count: $scount (want 2: one per request, no step2)"
[ "$scount" = "2" ] || { echo "SEARCH UPSTREAM POST COUNT WRONG (two-step machinery alive?)"; searchok=0; }
slog=$(grep -o 'standalone search sub-agent request' "$OUT/data.json" | wc -l | tr -d ' ')
echo "[searchsum] single-request log lines: $slog (want 2)"
[ "$slog" = "2" ] || { echo "SEARCHSUM LOG LINES WRONG"; searchok=0; }
# --- status-page badge fields: recentflights JSON carries the badge data, the served page carries the badge JS ---
smark=$(grep -o '"searchMarker":true' "$OUT/recent.json" | wc -l | tr -d ' ')
slvl=$(grep -o '"searchLevel":"high"' "$OUT/recent.json" | wc -l | tr -d ' ')
sthk=$(grep -o '"searchThinkTo":"low"' "$OUT/recent.json" | wc -l | tr -d ' ')
echo "badge fields in recent.json: searchMarker=true x$smark, searchLevel=high x$slvl, searchThinkTo=low x$sthk (want 2/2/2)"
[ "$smark" = "2" ] && [ "$slvl" = "2" ] && [ "$sthk" = "2" ] || { echo "SEARCH BADGE FIELDS WRONG in recent.json"; searchok=0; }
grep -qF 'SearchSummary-' "$OUT/page.html" || { echo "SearchSummary BADGE JS MISSING in served page"; searchok=0; }
grep -qF "thinkFrom||'auto'" "$OUT/page.html" || { echo "auto-from BADGE JS MISSING in served page"; searchok=0; }
[ "$searchok" = 1 ] && echo "SEARCH SMOKE PASS" || { echo "SEARCH SMOKE FAIL"; exit 1; }
echo "page classifier(codex) log lines: $(grep -c 'classifier(codex)' "$OUT/data.json")"
[ "$clsok" = 1 ] && echo "CLS SMOKE PASS" || { echo "CLS SMOKE FAIL"; exit 1; }
if [ "$pairs" = "$want" ]; then echo "SMOKE PASS"; else echo "SMOKE FAIL (want [$want])"; exit 1; fi
