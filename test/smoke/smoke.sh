#!/usr/bin/env bash
# Lineage smoke v3 (cross-platform, CI-ready): chained same-session requests on Anthropic (18083) + Responses (18084) ports.
# Pairs: a1/a2 plain chain (sess A), b1/b2 cache_control breakpoint migration (sess B),
# c1/c2 interior tail-element growth (sess C), r1/r2 Responses chain. a3 = session-less control.
# Portable: curl-based port probes (no netstat/taskkill), cleanup by PID, proxy runs with -no-tray.
# Requires a fresh build first (bash build.sh). CI runs this on ubuntu/windows/macos; locally: bash test/smoke/smoke.sh
# Search: s1 routes via search_fallback (the smoke-sf route's own claudeSearchSummary=forceHigh overrides sf's forceMid
# per field; sf's thinking=low still applies); s2 via enhance_search (legacy summary_*). Both must reach the mock exactly
# once (single-request summary injection, no step1/step2) and stream its search answer back untouched.
# History-quote negatives: s3 quotes the search marker in an earlier user turn (and still carries a web_search tool),
# h1 quotes the compaction prompt in an earlier user turn. Their final user message is ordinary talk, so neither may arm
# search/compaction: no upstream injection, badge fields stay false, and the global searchMarker=true count stays 2.
# Route effort: e1/e3 on a route with effort:max get output_config.effort forced (adaptive shape only), e2 (budget) and
# e4 (route without effort) must reach the mock byte-identical; the API column badge counts prove the [from->to] fields.
# Compact effort: e5 is a genuine compaction request on the same effort:max route and the top-level compactEffort=mid
# must beat that route pin (upstream medium, not max) and show [medium] in the model column's [level] slot; h1
# (a history quote) must stay unflagged with empty compact result fields.
# Token column: completed rows carry "tokens":"18/50"; the /slow mock holds 1.5s after message_start so a live row and
# a downstream-cancelled row both read 18/- (input seen, tail not yet); the served page swaps the finished table's
# bytes column for this cell with the byte count as its hover title.
# Vars: the config resolves $SMOKE_KEY from the top-level vars section (search_fallback uses it; routes keep a
# literal key) — a green run proves variable expansion works end to end on the real startup path.
# Pattern arrays: smoke-sf is written as "pattern": ["smoke-sf", "smoke-sf-alt"] (desugared into two route copies at
# load) — a green run proves array expansion works end to end alongside vars.
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

# The suite queries archived upstream bodies by id after all requests have run. The product default keeps only the
# last 10 finished streams, which evicts early flights (id 10 was the first casualty once the suite grew to 20).
# Raise it through the product's own status-page endpoint so every id stays queryable.
curl -s --max-time 10 -X POST "http://127.0.0.1:18083/__finishedcap" -H "Content-Type: application/json" -d '{"n":60}' | grep -q '"ok":true' || { echo "ABORT: could not raise finished-stream cap"; exit 1; }

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
# s1 asks thinking off (disabled), so the mock thinking block must be stripped from its response; s2 (id 18) hits
# route smoke-enh -> enhance_search legacy path (summary_level=high, summary_thinking -> low) and carries adaptive
# high, so its thinking blocks must pass through untouched.
cat > "$OUT/s1.json" <<'EOF'
{"model":"smoke-sf","max_tokens":8192,"stream":true,"thinking":{"type":"disabled"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: smoke weather"}]}
EOF
cat > "$OUT/s2.json" <<'EOF'
{"model":"smoke-enh","max_tokens":8192,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: smoke transit"}]}
EOF
# s3/h1 history-quote negatives: the marker lives in an earlier user turn, the final user turn is ordinary talk.
# Both ride the default upstream (model matches no route): upstream must receive the history bytes untouched and
# no badge may be set. s3 keeps the web_search tool so it also covers a search-capable request that is no sub-agent.
cat > "$OUT/s3.json" <<'EOF'
{"model":"smoke-hist-search","max_tokens":8192,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Pasted detector source for reference: hasSearchSubagentMarker scans for the literal Perform a web search for the query prefix."},{"role":"assistant","content":"Noted."},{"role":"user","content":"Explain that function, no search needed."}]}
EOF
cat > "$OUT/h1.json" <<'EOF'
{"model":"smoke-hist-compact","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"Earlier prompt to review: CRITICAL: Respond with TEXT ONLY. Do not call any tools. Explain it."},{"role":"assistant","content":"It asks for a plain-text summary."},{"role":"user","content":"Good, continue the original task."}]}
EOF
for n in s1 s2 s3 h1; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "anthropic $n -> HTTP $code"
done

# Route effort pin (e1 id 21, e2 id 22, e3 id 23, e4 id 24): e1/e2/e3 hit route smoke-effort (effort:max), e4 hits
# smoke-noeff (no effort). e1 adaptive+high -> forced max; e2 enabled budget -> untouched; e3 adaptive without
# output_config -> max appended; e4 adaptive+high -> byte-identical pass-through (the route did not opt in).
cat > "$OUT/e1.json" <<'EOF'
{"model":"smoke-effort","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"x"}]}
EOF
cat > "$OUT/e2.json" <<'EOF'
{"model":"smoke-effort","max_tokens":64,"stream":true,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"x"}]}
EOF
cat > "$OUT/e3.json" <<'EOF'
{"model":"smoke-effort","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"x"}]}
EOF
cat > "$OUT/e4.json" <<'EOF'
{"model":"smoke-noeff","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"x"}]}
EOF
for n in e1 e2 e3 e4; do
  code=$(curl -s -o "$OUT/$n.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/$n.json")
  echo "anthropic $n -> HTTP $code"
done
# e5 (id 25): a genuine compaction request on smoke-effort (effort:max) — the top-level compactEffort=mid beats the
# route pin: adaptive+max is rewritten to medium, not max, and only this row carries the compaction flag.
cat > "$OUT/e5.json" <<'EOF'
{"model":"smoke-effort","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do not call any tools. Your task is to create a detailed summary of the conversation so far."}]}
EOF
code=$(curl -s -o "$OUT/e5.resp" -w "%{http_code}" --max-time 30 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/e5.json")
echo "anthropic e5 -> HTTP $code"

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
echo "$s1up" | grep -q '"type":"adaptive"' && echo "$s1up" | grep -q '"effort":"low"' || { echo "S1 THINKING-LOW REWRITE MISSING (want adaptive+low)"; searchok=0; }
echo "$s1up" | grep -q '"model":"searchmodel"' || { echo "S1 FALLBACK MODEL SWAP MISSING"; searchok=0; }
s2up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=18&side=up")
echo "$s2up" | grep -q 'Perform a web search for the query: smoke transit' || { echo "S2 MARKER LOST upstream"; searchok=0; }
echo "$s2up" | grep -q 'After performing the web search' || { echo "S2 INJECTION MISSING"; searchok=0; }
echo "$s2up" | grep -q 'detailed comprehensive plaintext summary' || { echo "S2 LEVEL NOT HIGH"; searchok=0; }
echo "$s2up" | grep -q 'data points, direct quotes' || { echo "S2 HIGH WORDING MISSING"; searchok=0; }
echo "$s2up" | grep -q 'verbatim' && { echo "S2 UNEXPECTED VERBATIM (max-only wording leaked)"; searchok=0; }
echo "$s2up" | grep -q '"type":"adaptive"' && echo "$s2up" | grep -q '"effort":"low"' || { echo "S2 LEGACY summary_thinking->low MISSING (want adaptive+low)"; searchok=0; }
for n in s1 s2; do
  grep -q 'server_tool_use' "$OUT/$n.resp" || { echo "$n RESPONSE NOT A SEARCH ANSWER (server_tool_use missing)"; searchok=0; }
  grep -q 'web_search_tool_result' "$OUT/$n.resp" || { echo "$n RESPONSE MISSING web_search_tool_result"; searchok=0; }
  grep -q 'Result 1: smoke summary' "$OUT/$n.resp" || { echo "$n SUMMARY TEXT NOT PASSED THROUGH"; searchok=0; }
done
! grep -q 'thinking_delta' "$OUT/s1.resp" || { echo "S1 THINKING NOT STRIPPED (its client asked thinking off)"; searchok=0; }
grep -q 'thinking_delta' "$OUT/s2.resp" || { echo "S2 THINKING MISSING (its client carries adaptive: must pass through)"; searchok=0; }
scount=$(grep -c 'mode=search' "$OUT/mock.log")
echo "mock mode=search POST count: $scount (want 2: one per request, no step2)"
[ "$scount" = "2" ] || { echo "SEARCH UPSTREAM POST COUNT WRONG (two-step machinery alive?)"; searchok=0; }
slog=$(grep -o 'standalone search sub-agent request' "$OUT/data.json" | wc -l | tr -d ' ')
echo "[searchsum] single-request log lines: $slog (want 2)"
[ "$slog" = "2" ] || { echo "SEARCHSUM LOG LINES WRONG"; searchok=0; }
# --- status-page badge fields: recentflights JSON carries the badge data, the served page carries the badge JS ---
smark=$(grep -o '"searchMarker":true' "$OUT/recent.json" | wc -l | tr -d ' ')
slvl=$(grep -o '"searchLevel":"high"' "$OUT/recent.json" | wc -l | tr -d ' ')
sthk=$(grep -o '"thinkTo":"low"' "$OUT/recent.json" | wc -l | tr -d ' ')
echo "badge fields in recent.json: searchMarker=true x$smark, searchLevel=high x$slvl, thinkTo=low x$sthk (want 2/2/2)"
[ "$smark" = "2" ] && [ "$slvl" = "2" ] && [ "$sthk" = "2" ] || { echo "SEARCH BADGE FIELDS WRONG in recent.json"; searchok=0; }
grep -qF 'SearchSummary-' "$OUT/page.html" || { echo "SearchSummary BADGE JS MISSING in served page"; searchok=0; }
grep -qF "thinkFrom||'auto'" "$OUT/page.html" || { echo "auto-from BADGE JS MISSING in served page"; searchok=0; }
[ "$searchok" = 1 ] && echo "SEARCH SMOKE PASS" || { echo "SEARCH SMOKE FAIL"; exit 1; }

# --- history-quote negative fixtures (s3 id 19, h1 id 20): a marker quoted in conversation history must not arm
# search or compaction. Rows are located by their unique model names, not by fixed ids.
hqok=1
srow=$(sed 's/},{/}\n{/g' "$OUT/recent.json" | grep '"model":"smoke-hist-search"')
hrow=$(sed 's/},{/}\n{/g' "$OUT/recent.json" | grep '"model":"smoke-hist-compact"')
sid=$(echo "$srow" | grep -o '"id":[0-9]*' | head -1 | cut -d: -f2)
[ -n "$sid" ] || { echo "S3 ROW MISSING (model smoke-hist-search)"; hqok=0; }
echo "$srow" | grep -q '"searchMarker":false' || { echo "S3 SEARCH MARKER FALSE POSITIVE"; hqok=0; }
echo "$srow" | grep -q '"searchPrompt":""' || { echo "S3 SEARCH PROMPT MUST STAY EMPTY"; hqok=0; }
echo "$srow" | grep -q '"searchGuard":false' || { echo "S3 SEARCH GUARD FALSE POSITIVE"; hqok=0; }
[ -n "$hrow" ] || { echo "H1 ROW MISSING (model smoke-hist-compact)"; hqok=0; }
echo "$hrow" | grep -q '"compaction":false' || { echo "H1 COMPACTION FALSE POSITIVE"; hqok=0; }
echo "$hrow" | grep -q '"compactSave":""' || { echo "H1 COMPACT SAVE MUST STAY EMPTY"; hqok=0; }
echo "$hrow" | grep -q '"compactSavePct":""' || { echo "H1 COMPACT SAVE PERCENT MUST STAY EMPTY"; hqok=0; }
ccount=$(grep -o '"compaction":true' "$OUT/recent.json" | wc -l | tr -d ' ')
echo "compaction=true rows in recent.json: $ccount (want 1: only the e5 compact request)"
[ "$ccount" = "1" ] || { echo "HISTORY QUOTE FLAGGED AS COMPACTION (or the e5 compact row is missing)"; hqok=0; }
if [ -n "$sid" ]; then
  s3up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=$sid&side=up")
  echo "$s3up" | grep -q 'Perform a web search for the query' || { echo "S3 HISTORY QUOTE LOST upstream"; hqok=0; }
  mcount=$(echo "$s3up" | grep -o 'Perform a web search for the query' | wc -l | tr -d ' ')
  echo "S3 marker occurrences upstream: $mcount (want 1, the history quote only)"
  [ "$mcount" = "1" ] || { echo "S3 MARKER COUNT WRONG upstream"; hqok=0; }
  ! echo "$s3up" | grep -q 'After performing the web search' || { echo "S3 GUARD INJECTED AT HISTORY QUOTE"; hqok=0; }
  ! echo "$s3up" | grep -q 'detailed comprehensive plaintext summary' || { echo "S3 SUMMARY INJECTED AT HISTORY QUOTE"; hqok=0; }
  echo "$s3up" | grep -q 'Explain that function, no search needed.' || { echo "S3 FINAL MESSAGE LOST upstream"; hqok=0; }
fi
[ "$hqok" = 1 ] && echo "HISTORY-QUOTE NEGATIVE SMOKE PASS" || { echo "HISTORY-QUOTE NEGATIVE SMOKE FAIL"; exit 1; }

# --- route effort pin: e1 (id 21) / e2 (id 22) / e3 (id 23) hit smoke-effort (effort:max); e4 (id 24) hits smoke-noeff.
# Only adaptive bodies are rewritten (e1 in place, e3 appended); a budget body and a route without effort pass untouched.
efok=1
e1up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=21&side=up")
echo "$e1up" | grep -q '"effort":"max"' && ! echo "$e1up" | grep -q '"effort":"high"' || { echo "E1 EFFORT NOT FORCED TO MAX"; efok=0; }
e2up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=22&side=up")
echo "$e2up" | grep -q '"budget_tokens":2048' && ! echo "$e2up" | grep -q 'output_config' || { echo "E2 BUDGET BODY TOUCHED"; efok=0; }
e3up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=23&side=up")
echo "$e3up" | grep -q '"effort":"max"' || { echo "E3 OUTPUT_CONFIG NOT APPENDED"; efok=0; }
e4up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=24&side=up")
echo "$e4up" | grep -q '"effort":"high"' && ! echo "$e4up" | grep -q '"effort":"max"' || { echo "E4 NO-EFFORT ROUTE REWROTE THE BODY"; efok=0; }
eto=$(grep -o '"thinkTo":"max"' "$OUT/recent.json" | wc -l | tr -d ' ')
efrom=$(grep -o '"thinkFrom":"high"' "$OUT/recent.json" | wc -l | tr -d ' ')
efromad=$(grep -o '"thinkFrom":"adaptive"' "$OUT/recent.json" | wc -l | tr -d ' ')
eflog=$(grep -o 'adaptive thinking effort forced' "$OUT/data.json" | wc -l | tr -d ' ')
# thinkFrom=high counts e1 plus the earlier s2 search rewrite (its client carried adaptive+high); adaptive is e3's "from".
# The compact log line contains the same substring, so the count is 3 (e1, e3, e5).
echo "effort badges: thinkTo=max x$eto, thinkFrom=high x$efrom, thinkFrom=adaptive x$efromad, log lines x$eflog (want 2/2/1/3)"
[ "$eto" = "2" ] && [ "$efrom" = "2" ] && [ "$efromad" = "1" ] && [ "$eflog" = "3" ] || { echo "EFFORT BADGE/LOG COUNTS WRONG"; efok=0; }
# Model column: the smoke-effort flights whose route pin still governs carry the configured [max] (e2's budget
# body included — the tag is the configured pin, the badge is the actual rewrite). e5 is the exception: its
# compactEffort pin takes the [level] slot, so its row carries [medium] and no [max] (want 3). smoke-noeff carries no tag.
emodel=$(grep -o '"model":"smoke-effort \[max\]' "$OUT/recent.json" | wc -l | tr -d ' ')
enoeff=$(grep -o '"model":"smoke-noeff"' "$OUT/recent.json" | wc -l | tr -d ' ')
echo "effort model column: smoke-effort [max] x$emodel, smoke-noeff plain x$enoeff (want 3/1)"
[ "$emodel" = "3" ] && [ "$enoeff" = "1" ] || { echo "EFFORT MODEL COLUMN WRONG"; efok=0; }
# e5 (id 25): compactEffort=mid beats the route's effort:max on a genuine compaction request. The upstream body must
# carry medium (neither max nor high), the row must carry compaction with thinkFrom=max thinkTo=medium and the
# compactEffort level [medium] in the model column's [level] slot (no route [max]), and the compact [effort] log
# line appears exactly once.
e5up=$(curl -s --max-time 10 "http://127.0.0.1:18083/__flightreq?id=25&side=up")
echo "$e5up" | grep -q '"effort":"medium"' && ! echo "$e5up" | grep -q '"effort":"max"' || { echo "E5 COMPACTEFFORT DID NOT BEAT ROUTE MAX"; efok=0; }
e5row=$(sed 's/},{/}\n{/g' "$OUT/recent.json" | grep '"id":25,')
echo "$e5row" | grep -q '"compaction":true' || { echo "E5 COMPACTION FLAG MISSING"; efok=0; }
echo "$e5row" | grep -q '"model":"smoke-effort \[medium\]"' || { echo "E5 COMPACTEFFORT LEVEL MISSING FROM MODEL COLUMN"; efok=0; }
echo "$e5row" | grep -q '"thinkFrom":"max"' && echo "$e5row" | grep -q '"thinkTo":"medium"' || { echo "E5 API BADGE WRONG (want max->medium)"; efok=0; }
echo "$e5row" | grep -q '\[max\]' && { echo "E5 ROUTE MAX TAG NOT SUPPRESSED"; efok=0; }
clog=$(grep -o 'compact adaptive thinking effort forced' "$OUT/data.json" | wc -l | tr -d ' ')
echo "compact effort log lines: $clog (want 1)"
[ "$clog" = "1" ] || { echo "E5 COMPACT EFFORT LOG LINE WRONG"; efok=0; }
grep -qF "f.compactSavePct?' '+esc(f.compactSavePct):''" "$OUT/page.html" || { echo "COMPACT-SAVE BADGE JS MISSING in served page"; efok=0; }
[ "$efok" = 1 ] && echo "EFFORT SMOKE PASS" || { echo "EFFORT SMOKE FAIL"; exit 1; }

# --- token in/out column: a completed stream shows 18/50 (mock message_start 10+5+3=18, message_delta ends at 50);
# the /slow mock holds 1.5s after message_start so an in-flight row shows the head's 18 with the tail still "-",
# and a downstream cancel in that window archives the same 18/- (the display keeps it while the aggregates roll
# back). The served page swaps the bytes column for this cell on finished rows, with bytes as the hover title.
tokok=1
grep -q '"tokens":"18/50"' "$OUT/recent.json" || { echo "TOKENS COMPLETE ROW MISSING (want 18/50)"; tokok=0; }
trows=$(grep -o '"id":' "$OUT/recent.json" | wc -l | tr -d ' ')
tcells=$(grep -o '"tokens":"' "$OUT/recent.json" | wc -l | tr -d ' ')
echo "recent token cells: $tcells / $trows rows (want equal, every row carries one)"
[ "$trows" -gt 0 ] && [ "$trows" = "$tcells" ] || { echo "TOKENS FIELD MISSING ON SOME ROWS"; tokok=0; }
grep -q '"tokens":""' "$OUT/recent.json" && { echo "TOKENS EMPTY VALUE MUST NOT RENDER (unseen is \"-\")"; tokok=0; }
# The console language follows the environment (zh on a zh Windows profile, en elsewhere, e.g. WSL CI),
# so the header check accepts either language's label.
grep -qF '<th>token 入/出' "$OUT/page.html" || grep -qF '<th>token in/out' "$OUT/page.html" || { echo "TOKENS HEADER MISSING in served page"; tokok=0; }
grep -qF 'function tokensCell(' "$OUT/page.html" || { echo "TOKENS CELL JS MISSING in served page"; tokok=0; }
grep -qF 'tokensCell(f.tokens, f.bytes)' "$OUT/page.html" || { echo "TOKENS BYTES-REPLACEMENT JS MISSING in served page"; tokok=0; }
# The status input card is the uncached-only counter: its label and hover title must explain the
# total-input formula, whichever language the environment serves.
grep -qF "card('输入（未命中）'" "$OUT/page.html" || grep -qF "card('Input (uncached)'" "$OUT/page.html" || { echo "UNCACHED INPUT CARD MISSING in served page"; tokok=0; }
grep -qF '未命中缓存的新鲜输入 token' "$OUT/page.html" || grep -qF 'Fresh input tokens that missed the cache' "$OUT/page.html" || { echo "UNCACHED INPUT CARD TITLE MISSING in served page"; tokok=0; }
cat > "$OUT/t1.json" <<'EOF'
{"model":"smoke-slow","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"T"}]}
EOF
curl -s -o "$OUT/t1.resp" --max-time 20 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/t1.json" &
t1pid=$!
# Poll instead of a fixed sleep: the /slow mock holds 1.5s after message_start, so finding the value
# early finishes early and a loaded machine still has the whole window before the first delta.
ltok=""
for i in 1 2 3 4 5 6 7 8 9 10; do
  ltok=$(curl -s --max-time 5 "http://127.0.0.1:18083/__logs/data" | grep -o '"tokens":"[^"]*"' | head -1)
  [ "$ltok" = '"tokens":"18/-"' ] && break
  sleep 0.1
done
echo "live in-flight token cell: ${ltok:-<none>} (want \"tokens\":\"18/-\")"
[ "$ltok" = '"tokens":"18/-"' ] || { echo "IN-FLIGHT TOKENS LIVE VALUE WRONG"; tokok=0; }
wait "$t1pid" 2>/dev/null || true
grep -q 'message_stop' "$OUT/t1.resp" || { echo "T1 SLOW STREAM DID NOT COMPLETE"; tokok=0; }
cat > "$OUT/t2.json" <<'EOF'
{"model":"smoke-slow","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"T"}]}
EOF
curl -s -o /dev/null --max-time 20 -X POST "http://127.0.0.1:18083/v1/messages" -H "Content-Type: application/json" --data-binary "@$OUT/t2.json" &
t2pid=$!
# Cancel only once the proxy has parsed message_start (the in-flight row reads 18/-), so the archived
# row is deterministic; a wall-clock cutoff would race connect/parse on a loaded machine.
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  [ "$(curl -s --max-time 5 "http://127.0.0.1:18083/__logs/data" | grep -o '"tokens":"18/-"' | head -1)" = '"tokens":"18/-"' ] && break
  sleep 0.05
done
kill "$t2pid" 2>/dev/null || true
wait "$t2pid" 2>/dev/null || true
# The 499 row lands after the cancel; poll for the newest smoke-slow 499 row instead of a fixed sleep.
cancelled=""
for i in 1 2 3 4 5 6 7 8 9 10; do
  curl -s --max-time 5 "http://127.0.0.1:18083/__recentflights" > "$OUT/recent2.json"
  cancelled=$(sed 's/},{/}\n{/g' "$OUT/recent2.json" | grep '"model":"smoke-slow"' | grep '"status":499' | head -1)
  [ -n "$cancelled" ] && break
  sleep 0.2
done
echo "$cancelled" | grep -q '"tokens":"18/-"' || { echo "INTERRUPTED ROW MUST KEEP 18/- (got: ${cancelled:-no smoke-slow 499 row})"; tokok=0; }
[ "$tokok" = 1 ] && echo "TOKENS SMOKE PASS" || { echo "TOKENS SMOKE FAIL"; exit 1; }

echo "page classifier(codex) log lines: $(grep -c 'classifier(codex)' "$OUT/data.json")"
[ "$clsok" = 1 ] && echo "CLS SMOKE PASS" || { echo "CLS SMOKE FAIL"; exit 1; }
if [ "$pairs" = "$want" ]; then echo "SMOKE PASS"; else echo "SMOKE FAIL (want [$want])"; exit 1; fi
