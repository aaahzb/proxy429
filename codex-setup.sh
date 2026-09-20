#!/usr/bin/env bash
# Codex <-> Proxy429 one-click setup (macOS / Linux)
#
# Usage:
#   bash ./codex-setup.sh
#   (usually without saving to disk: the web console Config tab gives you
#     bash <(curl -fsSL <proxy-address>/__codexsetup.sh?...)
#   with the address/model baked in for the current config - zero interaction)
#
# Menu (interactive variant without baked parameters):
#   1 = use gpt-5-codex (matches the gpt-5* route in the proxy config template)
#   2 = use claude-fable-5 (adaptive thinking on by default)
#   3 = custom model name
#   9 = restore the default config (remove everything this script wrote)
#
# Same logic as codex-setup.ps1 (Windows). Structure follows DeepSeek's official
# codex-deepseek-setup script: backup/restore + surgical config.toml rewrite +
# model catalog (model_catalog_json) + atomic writes, plus a loopback NO_PROXY
# auto-fix when a system proxy would hijack 127.0.0.1 (macOS: launchd
# environment + login item; Linux: detect and advise only). This proxy does not
# verify the token (the real upstream key is injected by the matching route's
# api field), so there is no API-key prompt, only a placeholder.

set -uo pipefail

# The whole script is wrapped in { }: bash reads the entire block before
# executing, so Ctrl+C under bash <(curl ...) does not make curl report (23).
{

trap 'printf "\nCancelled.\n"; exit 130' INT

SCRIPT_VERSION='1.2.0'
PROVIDER_ID='proxy429'
DEFAULT_BASE_URL='http://127.0.0.1:8081/v1'
BACKUP_DIRNAME='backup-proxy429'
CATALOG_FILENAME='proxy429-models.json'
BEARER_TOKEN='proxy429'   # placeholder; the proxy does not verify it

# The command shown on the web console Config tab fetches a baked variant from
# this proxy: these empty strings get replaced with real values
# (replacement anchors, do not change the format). Non-empty values skip the
# corresponding prompts, so paste-and-run works.
# BAKED_MODEL = __restore__ goes straight to restore;
# BAKED_CATALOG is a comma-separated model list (one representative name per
# routes pattern), all listed in the Codex /model menu.
# BAKED_CONTEXT_WINDOW / BAKED_COMPACT_PERCENT override the catalog's context
# window and auto-compact percent (digits only, validated server-side).
BAKED_BASE_URL=''
BAKED_MODEL=''
BAKED_CATALOG=''
BAKED_CONTEXT_WINDOW=''
BAKED_COMPACT_PERCENT=''

MODEL_SLUG=''   # chosen from the menu or baked in
BASE_URL=''     # asked at install time or baked in

# catalog context declarations: defaults match the cc-switch template (256k, compact at 95%)
CONTEXT_WINDOW=262144
COMPACT_PERCENT=95
[ -n "$BAKED_CONTEXT_WINDOW" ]  && CONTEXT_WINDOW="$BAKED_CONTEXT_WINDOW"
[ -n "$BAKED_COMPACT_PERCENT" ] && COMPACT_PERCENT="$BAKED_COMPACT_PERCENT"

# ---------------------------------------------------------------- output helpers

if [ -t 1 ]; then
  C_RST=$'\033[0m'; C_B=$'\033[1m'; C_RED=$'\033[31m'
  C_GRN=$'\033[32m'; C_YEL=$'\033[33m'; C_DIM=$'\033[2m'
else
  C_RST=''; C_B=''; C_RED=''; C_GRN=''; C_YEL=''; C_DIM=''
fi

info() { printf '%s\n' "$*"; }
ok()   { printf '%s[OK]%s %s\n' "$C_GRN" "$C_RST" "$*"; }
warn() { printf '%s[!]%s  %s\n' "$C_YEL" "$C_RST" "$*"; }
dim()  { printf '%s%s%s\n' "$C_DIM" "$*" "$C_RST"; }
die()  { printf '\n%s[X] %s%s\n' "$C_RED" "$*" "$C_RST" >&2; exit 1; }
head1(){ printf '\n%s%s%s\n' "$C_B" "$*" "$C_RST"; }

# Read user input: when stdin is a pipe (printf ... | script, automated tests)
# read stdin first; when it is a terminal or exhausted, read /dev/tty.
# Compatible with both curl ... | bash and bash <(curl ...).
read_tty() {
  local __var="$1" __prompt="$2" __ans='' __got=1
  if [ ! -t 0 ]; then
    printf '%s' "$__prompt"
    if IFS= read -r __ans; then __got=0; fi
  fi
  if [ "$__got" -ne 0 ] && [ -r /dev/tty ]; then
    printf '%s' "$__prompt" > /dev/tty
    IFS= read -r __ans < /dev/tty || __ans=''
  fi
  eval "$__var=\$__ans"
}

# ---------------------------------------------------------------- paths

CODEX_HOME_DIR="${CODEX_HOME:-$HOME/.codex}"
CONFIG_PATH="$CODEX_HOME_DIR/config.toml"
MODELS_PATH="$CODEX_HOME_DIR/$CATALOG_FILENAME"
BACKUP_DIR="$CODEX_HOME_DIR/$BACKUP_DIRNAME"
BACKUP_CONFIG="$BACKUP_DIR/config.toml"
MANIFEST="$BACKUP_DIR/manifest.txt"

# With the default path, ~ is verified to work (Codex expands it); with a custom
# CODEX_HOME, writing the absolute path is safer
if [ -n "${CODEX_HOME:-}" ]; then
  CATALOG_VALUE="$MODELS_PATH"
else
  CATALOG_VALUE="~/.codex/$CATALOG_FILENAME"
fi
# Same as the ps1: catalog paths always use forward slashes (backslashes are
# escape characters in TOML; under git-bash CODEX_HOME may be a Windows path
# with backslashes)
CATALOG_VALUE="${CATALOG_VALUE//\\//}"

# ---------------------------------------------------------------- model catalog (models.json)

find_python() {
  local cand
  for cand in python3 python; do
    if command -v "$cand" >/dev/null 2>&1; then printf '%s' "$cand"; return 0; fi
  done
  printf ''
}

# Entry fields match New-CatalogEntry in codex-setup.ps1 (cc-switch's native
# responses catalog template minimal set + apply_patch_tool_type=freeform +
# minimal_client_version; freeform apply_patch needs Codex 0.144.0+, and this
# proxy supports freeform custom tools).
# $1=slug $2=priority; the slug is already validated (no quotes/backslashes),
# so direct interpolation is safe.
print_catalog_entry() {
  cat <<ENTRY
    {
      "slug": "$1",
      "display_name": "$1",
      "description": "Served via Proxy429 local proxy.",
      "base_instructions": "You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user's goals.",
      "default_reasoning_level": "high",
      "supported_reasoning_levels": [
        { "effort": "none",   "description": "Disable thinking" },
        { "effort": "low",    "description": "Light reasoning" },
        { "effort": "medium", "description": "Medium reasoning" },
        { "effort": "high",   "description": "Deep reasoning for complex problems" },
        { "effort": "max",    "description": "Maximum reasoning depth" }
      ],
      "shell_type": "shell_command",
      "apply_patch_tool_type": "freeform",
      "minimal_client_version": "0.144.0",
      "visibility": "list",
      "supported_in_api": true,
      "priority": $2,
      "supports_reasoning_summaries": true,
      "default_reasoning_summary": "none",
      "support_verbosity": false,
      "truncation_policy": { "mode": "tokens", "limit": 10000 },
      "supports_parallel_tool_calls": false,
      "supports_image_detail_original": false,
      "context_window": $CONTEXT_WINDOW,
      "max_context_window": $CONTEXT_WINDOW,
      "effective_context_window_percent": $COMPACT_PERCENT,
      "experimental_supported_tools": [],
      "input_modalities": [ "text", "image" ],
      "supports_search_tool": true
    }
ENTRY
}

SLUGS=()
NSLUGS=0
add_slug() {
  # ${arr[@]+...}: expanding an empty array under set -u is safe on bash 3.2;
  # exact one-by-one comparison, unaffected by the caller's IFS
  local x
  for x in ${SLUGS[@]+"${SLUGS[@]}"}; do
    [ "$x" = "$1" ] && return 0
  done
  SLUGS[$NSLUGS]="$1"; NSLUGS=$((NSLUGS+1))
}

# Extract the slug list from an existing models.json (keeps manually added
# entries): prefer python parsing, fall back to sed
extract_slugs() {
  local out='' py
  py=$(find_python)
  if [ -n "$py" ]; then
    out=$("$py" - "$1" <<'PYX' 2>/dev/null
import json,sys
try:
    with open(sys.argv[1],encoding='utf-8') as f:
        d=json.load(f)
    print(' '.join(m['slug'] for m in d.get('models',[]) if isinstance(m,dict) and m.get('slug')))
except Exception:
    pass
PYX
)
  fi
  if [ -z "$out" ]; then
    out=$(sed -n 's/.*"slug"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$1" 2>/dev/null | tr '\n' ' ')
  fi
  printf '%s' "$out"
}

# Catalog content = the baked list (BAKED_CATALOG, one representative name per
# routes pattern) or the two built-in demo models
#           + the currently selected model + other slugs from an existing
#             catalog (custom entries survive a model switch)
collect_slugs() {
  SLUGS=(); NSLUGS=0
  local s
  if [ -n "$BAKED_CATALOG" ]; then
    # read the comma-separated list line by line (no local IFS=',' here: bash
    # dynamic scoping would leak it into word-splitting inside add_slug/extract)
    while IFS= read -r s; do
      s=$(_trim "$s")
      [ -n "$s" ] && add_slug "$s"
    done <<SLUG_LIST
$(printf '%s' "$BAKED_CATALOG" | tr ',' '\n')
SLUG_LIST
  else
    add_slug 'gpt-5-codex'
    add_slug 'claude-fable-5'
  fi
  [ -n "$MODEL_SLUG" ] && add_slug "$MODEL_SLUG"
  if [ -f "$MODELS_PATH" ]; then
    for s in $(extract_slugs "$MODELS_PATH"); do
      [ -n "$s" ] && add_slug "$s"
    done
  fi
}

write_models_json() {
  collect_slugs
  local entries=() ne=0 i=0
  while [ "$i" -lt "$NSLUGS" ]; do
    entries[$ne]=$(print_catalog_entry "${SLUGS[$i]}" $((i+1)))
    ne=$((ne+1)); i=$((i+1))
  done
  {
    printf '{\n  "models": [\n'
    i=0
    while [ "$i" -lt "$ne" ]; do
      printf '%s' "${entries[$i]}"
      if [ "$i" -lt $((ne-1)) ]; then printf ',\n'; else printf '\n'; fi
      i=$((i+1))
    done
    printf '  ]\n}\n'
  } > "$1"
}

# Validate the freshly written models.json: valid JSON containing the selected
# model; falls back to grep without python.
# MODELS_JSON_PARSED=1 means a full parse happened (for the report).
MODELS_JSON_PARSED=0
check_models_json() {
  local py="$1" f="$2"
  MODELS_JSON_PARSED=0
  if [ -n "$py" ]; then
    if "$py" - "$f" "$MODEL_SLUG" <<'PYJSON' 2>/dev/null
import json,sys
with open(sys.argv[1],encoding='utf-8') as f:
    d=json.load(f)
ms=d['models']
assert isinstance(ms,list) and len(ms)>=1, 'models must not be empty'
slugs={m.get('slug') for m in ms}
assert sys.argv[2] in slugs, sys.argv[2]+' missing'
PYJSON
    then
      MODELS_JSON_PARSED=1
      return 0
    fi
    return 1
  fi
  grep -q "\"$MODEL_SLUG\"" "$f"
}

# ---------------------------------------------------------------- restore (menu 9)

do_restore() {
  head1 'Restore default Codex config (remove proxy429 settings)'
  [ -d "$BACKUP_DIR" ] || die "Backup directory not found:
  $BACKUP_DIR
Nothing to restore - this script was probably never installed, or was already restored."

  local had_config=1
  if [ -f "$MANIFEST" ] && grep -q '^original_config_existed=0$' "$MANIFEST" 2>/dev/null; then
    had_config=0
  fi

  local n=1
  info ''
  info 'The following will be performed:'
  if [ "$had_config" -eq 1 ]; then
    [ -f "$BACKUP_CONFIG" ] || die "Backup is corrupt: missing $BACKUP_CONFIG"
    info "  $n. Delete current $CONFIG_PATH"; n=$((n+1))
    info "     ${C_YEL}(changes made to this file after installation will be lost)${C_RST}"
    info "  $n. Restore config.toml from backup"; n=$((n+1))
  else
    info "  $n. Delete $CONFIG_PATH"; n=$((n+1))
    info "     ${C_DIM}(this file did not exist before installation)${C_RST}"
  fi
  info "  $n. Delete $MODELS_PATH"; n=$((n+1))
  info "  $n. Delete backup directory $BACKUP_DIR"

  local agent_plist="$HOME/Library/LaunchAgents/$LAUNCHAGENT_LABEL.plist"
  local rm_agent=0
  if [ -f "$agent_plist" ]; then
    rm_agent=1
    n=$((n+1))
    info "  $n. Remove login item $agent_plist"
    info "     ${C_DIM}(re-applies loopback NO_PROXY at every login; written by this script on macOS)${C_RST}"
  fi
  info ''

  local ans=''
  read_tty ans 'Proceed with restore? Type y to continue, anything else to cancel: '
  case "$ans" in
    y|Y|yes|YES) ;;
    *) info 'Cancelled; no files were modified.'; exit 0 ;;
  esac

  rm -f "$MODELS_PATH"
  if [ "$had_config" -eq 1 ]; then
    cp "$BACKUP_CONFIG" "$CONFIG_PATH" || die 'Failed to restore config.toml'
    ok 'config.toml restored'
  else
    rm -f "$CONFIG_PATH"
    ok 'config.toml deleted (it did not exist before installation)'
  fi
  ok "$CATALOG_FILENAME deleted"

  rm -rf "$BACKUP_DIR"
  ok 'backup directory cleaned up'

  if [ "$rm_agent" -eq 1 ]; then
    launchctl bootout "gui/$(id -u 2>/dev/null)/$LAUNCHAGENT_LABEL" 2>/dev/null
    rm -f "$agent_plist"
    ok 'login item removed'
    dim '  (this login session keeps NO_PROXY until logout; clear now with: launchctl unsetenv NO_PROXY; launchctl unsetenv no_proxy)'
  fi
  info ''
  ok 'Restore complete; Codex config is back to its pre-install state.'
  info ''
  warn 'Restart Codex for this to take effect (config.toml is only read at startup)'
  exit 0
}

# ---------------------------------------------------------------- TOML scanner

# Character-by-character state machine: track bracket depth and multi-line
# strings to recognize real section headers
_depth=0
_mlstate=''

_scan_line() {
  # local arguments are expanded before assignment (bash 3.2); ${#line} cannot
  # share one statement with line="$1"
  local line="$1"
  local n=${#line} i=0 c c3 instr=''
  while [ "$i" -lt "$n" ]; do
    c="${line:$i:1}"
    if [ -n "$_mlstate" ]; then
      c3="${line:$i:3}"
      if [ "$_mlstate" = basic ] && [ "$c3" = '"""' ]; then
        _mlstate=''; i=$((i+3)); continue
      fi
      if [ "$_mlstate" = literal ] && [ "$c3" = "'''" ]; then
        _mlstate=''; i=$((i+3)); continue
      fi
      if [ "$_mlstate" = basic ] && [ "$c" = '\' ]; then i=$((i+2)); continue; fi
      i=$((i+1)); continue
    fi
    if [ -n "$instr" ]; then
      if [ "$instr" = basic ]; then
        if [ "$c" = '\' ]; then i=$((i+2)); continue; fi
        [ "$c" = '"' ] && instr=''
      else
        [ "$c" = "'" ] && instr=''
      fi
      i=$((i+1)); continue
    fi
    c3="${line:$i:3}"
    if [ "$c3" = '"""' ]; then _mlstate=basic; i=$((i+3)); continue; fi
    if [ "$c3" = "'''" ]; then _mlstate=literal; i=$((i+3)); continue; fi
    case "$c" in
      '#') return 0 ;;
      '"') instr=basic ;;
      "'") instr=literal ;;
      '[') _depth=$((_depth+1)) ;;
      ']') [ "$_depth" -gt 0 ] && _depth=$((_depth-1)) ;;
    esac
    i=$((i+1))
  done
  return 0
}

_trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

_key_of() {
  local l k
  l=$(_trim "$1")
  case "$l" in
    '#'*|'') printf ''; return ;;
    *=*) ;;
    *) printf ''; return ;;
  esac
  k=$(_trim "${l%%=*}")
  k="${k#\"}"; k="${k%\"}"
  k="${k#\'}"; k="${k%\'}"
  printf '%s' "$k"
}

_val_of() {
  local l="$1"
  case "$l" in
    *=*) printf '%s' "$(_trim "${l#*=}")" ;;
    *) printf '' ;;
  esac
}

_in_list() {
  local needle="$1"; shift
  local x
  for x in $*; do
    [ "$x" = "$needle" ] && return 0
  done
  return 1
}

# Top-level keys that must be replaced in place
TARGET_KEYS='model model_provider preferred_auth_method forced_login_method model_reasoning_effort model_catalog_json'

# Level A: would shadow or hijack the target configuration
DEL_A='oss_provider openai_base_url'

# Level B: contradict the models.json declarations -> silent errors or 400
DEL_B='model_context_window model_auto_compact_token_limit model_auto_compact_token_limit_scope base_instructions model_instructions_file compact_prompt experimental_compact_prompt_file service_tier model_verbosity model_reasoning_summary plan_mode_reasoning_effort experimental_use_unified_exec_tool'

# Level C: warn only
WARN_KEYS='review_model experimental_thread_config_endpoint experimental_thread_store_endpoint experimental_thread_store'

target_value_for() {
  case "$1" in
    model)                   printf '%s' "\"$MODEL_SLUG\"" ;;
    model_provider)          printf '%s' "\"$PROVIDER_ID\"" ;;
    preferred_auth_method)   printf '%s' '"apikey"' ;;
    forced_login_method)     printf '%s' '"api"' ;;
    model_reasoning_effort)  printf '%s' '"high"' ;;
    model_catalog_json)      printf '%s' "\"$CATALOG_VALUE\"" ;;
  esac
}

# Deletion reasons (for the report), matching the $DEL_A/$DEL_B comments in codex-setup.ps1
del_why() {
  case "$1" in
    oss_provider)                        printf 'another provider selector that would route requests elsewhere' ;;
    openai_base_url)                     printf 'global base_url override that would hijack requests' ;;
    model_context_window)                printf 'overrides the context window declared by models.json; when too large, auto-compact never triggers and the API errors mid-run' ;;
    model_auto_compact_token_limit)      printf 'overrides the auto-compact timing' ;;
    model_auto_compact_token_limit_scope) printf 'overrides the auto-compact timing' ;;
    base_instructions)                   printf 'overrides the base_instructions of models.json' ;;
    model_instructions_file)             printf 'overrides the base_instructions of models.json' ;;
    compact_prompt)                      printf 'overrides the context-compaction prompt' ;;
    experimental_compact_prompt_file)    printf 'overrides the context-compaction prompt' ;;
    service_tier)                        printf 'stale value gets sent as an API parameter, possibly causing 400' ;;
    model_verbosity)                     printf 'stale value may exceed what the model supports' ;;
    model_reasoning_summary)             printf 'models.json declares default_reasoning_summary=none; a stale value would emit reasoning.summary' ;;
    plan_mode_reasoning_effort)          printf 'may be a level the catalog does not declare' ;;
    experimental_use_unified_exec_tool)  printf 'conflicts with shell_type=shell_command in models.json' ;;
    *)                                   printf 'conflicts with the target configuration' ;;
  esac
}

truncate_val() {
  local v="$1"
  if [ "${#v}" -gt 58 ]; then printf '%s...' "${v:0:58}"; else printf '%s' "$v"; fi
}

# ---------------------------------------------------------------- fast path: switch model only

# When the backup and models.json look as expected, only the model key in the
# top-level area of config.toml is rewritten; everything else (base_url, the
# result of the previous surgery, ...) stays untouched.
switch_model_only() {
  head1 "Switching default model -> $MODEL_SLUG"
  dim 'Backup from this script detected; refreshing the model catalog, switching the model field,'
  dim 'and syncing the script-owned provider block (name/wire_api; base_url only when baked in).'

  local nlines=0 i=0 l trimmed k is_header hdr nv
  local lines=() out=() nout=0 replaced=0 in_leading=1
  local cur_sec='' prov_synced=''
  # only the web-console variant carries a base URL; interactive re-runs keep the existing line
  local sync_base_url=''
  [ -n "$BAKED_BASE_URL" ] && sync_base_url="${BAKED_BASE_URL%/}"
  while IFS= read -r l || [ -n "$l" ]; do
    l="${l%$'\r'}"   # normalize CRLF configs to LF before processing
    lines[$nlines]="$l"; nlines=$((nlines+1))
  done < "$CONFIG_PATH"

  _depth=0; _mlstate=''
  while [ "$i" -lt "$nlines" ]; do
    l="${lines[$i]-}"
    if [ "$in_leading" -eq 0 ]; then
      # inside sections: keep the scan state accurate so real headers are recognized
      if [ -n "$_mlstate" ] || [ "$_depth" -ne 0 ]; then
        _scan_line "$l"
        out[$nout]="$l"; nout=$((nout+1)); i=$((i+1)); continue
      fi
      trimmed=$(_trim "$l")
      case "$trimmed" in
        '['*)
          hdr="$trimmed"
          hdr="${hdr%%]*}"
          hdr="${hdr#[}"
          hdr=$(_trim "$hdr")
          hdr=$(printf '%s' "$hdr" | tr -d '"'"'")
          cur_sec="$hdr"
          _scan_line "$l"
          out[$nout]="$l"; nout=$((nout+1)); i=$((i+1)); continue
          ;;
      esac
      # the provider block is script-owned (same rule as the catalog file): refresh
      # name/wire_api constants, and base_url when this run has one baked in - this
      # is what upgrades a block written by an older script version (e.g. Chinese name)
      if [ "$cur_sec" = "model_providers.$PROVIDER_ID" ]; then
        k=$(_key_of "$l")
        nv=''
        case "$k" in
          name)     nv='"Proxy429 Local Proxy"' ;;
          base_url) [ -n "$sync_base_url" ] && nv="\"$sync_base_url\"" ;;
          wire_api) nv='"responses"' ;;
        esac
        if [ -n "$nv" ]; then
          # swallow a theoretical multi-line value before writing the replacement
          _scan_line "$l"; i=$((i+1))
          while { [ -n "$_mlstate" ] || [ "$_depth" -ne 0 ]; } && [ "$i" -lt "$nlines" ]; do
            _scan_line "${lines[$i]-}"; i=$((i+1))
          done
          [ "$trimmed" != "$k = $nv" ] && prov_synced="$prov_synced $k"
          out[$nout]="$k = $nv"; nout=$((nout+1))
          continue
        fi
      fi
      _scan_line "$l"
      out[$nout]="$l"; nout=$((nout+1)); i=$((i+1)); continue
    fi
    # continuation lines of multi-line strings/arrays pass through without key parsing
    if [ -n "$_mlstate" ] || [ "$_depth" -ne 0 ]; then
      _scan_line "$l"
      out[$nout]="$l"; nout=$((nout+1)); i=$((i+1)); continue
    fi
    trimmed=$(_trim "$l")
    is_header=0
    case "$trimmed" in '['*) is_header=1 ;; esac
    if [ "$is_header" -eq 1 ]; then
      # top-level area ended without seeing model -> insert before the first section header
      if [ "$replaced" -eq 0 ]; then
        out[$nout]="model = \"$MODEL_SLUG\""; nout=$((nout+1))
        out[$nout]=''; nout=$((nout+1))
        replaced=1
      fi
      in_leading=0
      out[$nout]="$l"; nout=$((nout+1)); i=$((i+1)); continue
    fi
    k=$(_key_of "$l")
    if [ "$k" = model ]; then
      # swallow the complete assignment (could theoretically be a multi-line string)
      _scan_line "$l"; i=$((i+1))
      while { [ -n "$_mlstate" ] || [ "$_depth" -ne 0 ]; } && [ "$i" -lt "$nlines" ]; do
        _scan_line "${lines[$i]-}"; i=$((i+1))
      done
      out[$nout]="model = \"$MODEL_SLUG\""; nout=$((nout+1))
      replaced=1
      continue
    fi
    _scan_line "$l"
    out[$nout]="$l"; nout=$((nout+1)); i=$((i+1))
  done
  if [ "$replaced" -eq 0 ]; then
    out[$nout]="model = \"$MODEL_SLUG\""; nout=$((nout+1))
  fi

  local tmp="$CONFIG_PATH.proxy429-tmp.$$"
  : > "$tmp" || die "Cannot write temporary file: $tmp"
  i=0
  while [ "$i" -lt "$nout" ]; do
    printf '%s\n' "${out[$i]}" >> "$tmp"
    i=$((i+1))
  done

  local tmp_models="$MODELS_PATH.proxy429-tmp.$$"
  write_models_json "$tmp_models" || {
    rm -f "$tmp" "$tmp_models"
    die "Cannot write temporary file: $tmp_models"
  }

  local py
  py=$(find_python)
  if [ -n "$py" ]; then
    # tomllib needs Python 3.11+; rc=3 means no tomllib, skip the parse check
    if "$py" - "$tmp" "$MODEL_SLUG" <<'PYTOML' 2>/dev/null
import sys
try:
    import tomllib
except ImportError:
    sys.exit(3)
with open(sys.argv[1],'rb') as f:
    c=tomllib.load(f)
assert c.get('model')==sys.argv[2], 'model was not written correctly'
PYTOML
    then
      :
    else
      rc=$?
      if [ "$rc" -ne 3 ]; then
        rm -f "$tmp" "$tmp_models"
        die 'The updated config.toml failed TOML validation; aborted (the original file was not modified).'
      fi
    fi
  fi

  check_models_json "$py" "$tmp_models" || {
    rm -f "$tmp" "$tmp_models"
    die "The generated models.json failed validation (missing model $MODEL_SLUG); aborted (the original files were not modified)."
  }

  # the new catalog is a superset of the current one, so write it first: if the
  # config.toml write then fails, previously chosen models still resolve
  mv "$tmp_models" "$MODELS_PATH" || { rm -f "$tmp"; die 'Failed to write models.json'; }
  mv "$tmp" "$CONFIG_PATH" || die 'Failed to write config.toml'
  ok "Refreshed $CATALOG_FILENAME (this file is rewritten by the script; manual edits are overwritten)"
  ok "config.toml updated: model = \"$MODEL_SLUG\""
  [ -n "$prov_synced" ] && ok "Synced [model_providers.$PROVIDER_ID]:$prov_synced"

  # ---- proxy check (same 503 trap as install; the base_url was recorded in the install manifest)
  BASE_URL="$(sed -n 's/^base_url=//p' "$MANIFEST" 2>/dev/null | head -n 1)"
  check_proxy_for_loopback

  info ''
  warn 'Restart Codex for this to take effect (config.toml is only read at startup)'
  info ''
  info 'How to verify:'
  info "  - the Codex CLI startup banner shows model: $MODEL_SLUG"
  info '  - the proxy web console Status tab shows an in-flight stream whose API column reads [translate]'
  info ''
  dim 'Run this script again to switch models (1/2/3) or restore the default config (9).'
  exit 0
}

# ---------------------------------------------------------------- proxy check / auto-fix

# Codex's HTTP stack honors the system/environment proxy but ignores its
# exception list (verified on Windows and macOS alike: with the system proxy
# on, a base_url pointing at 127.0.0.1 gets sent to the proxy server -> 503,
# and this proxy sees nothing). The only bypass Codex (reqwest) honors is the
# NO_PROXY/no_proxy environment variable.
# Windows (ps1) writes a user-level variable. On macOS the equivalent that
# reaches GUI apps (the ChatGPT desktop app's bundled codex) is the launchd
# environment: launchctl setenv covers apps and terminal windows launched
# afterwards, and a tiny per-user LaunchAgent re-applies it at every login.
# Linux has no user-scope mechanism covering every session type, so there we
# only detect and advise.

LOOPBACK_NO_PROXY='localhost,127.0.0.1,::1'
LAUNCHAGENT_LABEL='com.proxy429.noproxy'

# rc 0 when $1 (a NO_PROXY-style comma list) already bypasses loopback
_np_covers_loopback() {
  local np="$1" e oldifs="$IFS" rc=1
  IFS=','
  for e in $np; do
    e=$(_trim "$e")
    case "$e" in
      '127.0.0.1'|'localhost'|'::1'|'*') rc=0 ;;
    esac
  done
  IFS="$oldifs"
  return "$rc"
}

# $1 = existing comma list (may be empty); print it with the missing loopback
# entries appended (never clobbers what the user already has)
_np_merge_loopback() {
  local new="$1" e oldifs="$IFS"
  new="${new%,}"
  IFS=','
  for e in $LOOPBACK_NO_PROXY; do
    case ",$new," in
      *",$e,"*) ;;
      *) new="${new:+$new,}$e" ;;
    esac
  done
  IFS="$oldifs"
  printf '%s' "$new"
}

# Content of the per-user LaunchAgent re-applying the loopback merge at every
# login (the fix then survives logout/reboot; deleting the file undoes it).
_print_launchagent_plist() {
  cat <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.proxy429.noproxy</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>for v in NO_PROXY no_proxy; do cur="$(launchctl getenv "$v")"; for h in localhost 127.0.0.1 ::1; do case ",$cur," in *",$h,"*) ;; *) cur="${cur:+$cur,}$h" ;; esac; done; launchctl setenv "$v" "$cur"; done</string>
  </array>
  <key>RunAtLoad</key><true/>
</dict>
</plist>
PLIST
}

# macOS: write NO_PROXY/no_proxy into the launchd environment (immediate, for
# apps and terminal windows launched afterwards) + install the login item.
fix_noproxy_macos() {
  local cur new plist domain
  cur="$(launchctl getenv NO_PROXY 2>/dev/null)"
  new="$(_np_merge_loopback "$cur")"
  if launchctl setenv NO_PROXY "$new" 2>/dev/null && launchctl setenv no_proxy "$new" 2>/dev/null; then
    ok "launchd environment now has NO_PROXY = no_proxy = $new"
    dim '  (covers Codex launched afterwards: GUI apps like the ChatGPT desktop app, and new terminal windows)'
  else
    warn 'launchctl setenv failed; set NO_PROXY manually instead:'
    info '    export NO_PROXY="localhost,127.0.0.1,::1"'
    info '  (add it to ~/.zshrc / ~/.bashrc to persist; restart Codex afterwards)'
    return 1
  fi

  [ -d "$HOME/Library/LaunchAgents" ] || mkdir -p "$HOME/Library/LaunchAgents" 2>/dev/null
  plist="$HOME/Library/LaunchAgents/$LAUNCHAGENT_LABEL.plist"
  if ! _print_launchagent_plist > "$plist" 2>/dev/null; then
    warn "Could not write $plist; the setenv above still holds until logout"
    return 0
  fi
  domain="gui/$(id -u 2>/dev/null)"
  launchctl bootout "$domain/$LAUNCHAGENT_LABEL" 2>/dev/null
  if launchctl bootstrap "$domain" "$plist" 2>/dev/null || launchctl load -w "$plist" 2>/dev/null; then
    ok "Login item installed ($plist): re-applies NO_PROXY at every login"
  else
    warn "Wrote $plist but could not load it; the setenv above still holds until logout"
    dim "  (load it later by hand: launchctl load -w '$plist')"
  fi
  dim "  (undo: launchctl bootout $domain/$LAUNCHAGENT_LABEL; rm '$plist'; launchctl unsetenv NO_PROXY; launchctl unsetenv no_proxy)"
}

check_proxy_for_loopback() {
  case "$BASE_URL" in
    http://127.*|http://localhost*|http://\[::1\]*|https://127.*|https://localhost*|https://\[::1\]*) ;;
    *) return 0 ;;
  esac
  [ "${PROXY429_SKIP_PROXY_FIX:-}" = '1' ] && return 0   # escape hatch for automated tests

  local v val src=''
  for v in https_proxy HTTPS_PROXY http_proxy HTTP_PROXY all_proxy ALL_PROXY; do
    val="${!v:-}"
    if [ -n "$val" ]; then src="environment variable $v=$val"; break; fi
  done
  if [ -z "$src" ] && command -v scutil >/dev/null 2>&1; then
    if scutil --proxy 2>/dev/null | grep -qE 'HTTPEnable : 1|HTTPSEnable : 1'; then
      src='macOS system proxy (enabled in Network settings)'
    fi
  fi
  [ -z "$src" ] && return 0

  # macOS: GUI apps (and new terminals) inherit the launchd environment, so it
  # - not this script's own env - decides whether Codex can be hijacked.
  if [ "$(uname 2>/dev/null)" = 'Darwin' ] && command -v launchctl >/dev/null 2>&1; then
    local ld_np=''
    ld_np="$(launchctl getenv NO_PROXY 2>/dev/null)"
    if _np_covers_loopback "$ld_np"; then
      ok "launchd environment NO_PROXY ($ld_np) already bypasses the proxy for loopback"
      if ! _np_covers_loopback "${NO_PROXY:-${no_proxy:-}}"; then
        dim '  (this terminal session lacks NO_PROXY - open a new terminal window, or export it by hand)'
      fi
      return 0
    fi
    warn "A proxy is enabled ($src), but the launchd environment does not exclude loopback addresses"
    info '  Codex follows the proxy and ignores its exception list, so requests for 127.0.0.1'
    info '  would be handed to the proxy server -> 503, and this proxy sees nothing. Fixing it now:'
    fix_noproxy_macos
    return 0
  fi

  # Linux & others: no user-scope env mechanism covers every session type -
  # detect and advise only, never touch the user's environment.
  if _np_covers_loopback "${NO_PROXY:-${no_proxy:-}}"; then
    ok 'NO_PROXY already covers loopback addresses; proxies will not intercept this proxy'
    return 0
  fi
  warn "A proxy is enabled ($src), but NO_PROXY does not exclude loopback addresses"
  info '  Codex would hand requests for 127.0.0.1 to the proxy server -> 503, and this proxy sees nothing.'
  info '  Fix (applies to Codex started afterwards):'
  info '    export NO_PROXY="localhost,127.0.0.1,::1"'
  info '  Add it to your shell profile (~/.zshrc / ~/.bashrc) to persist; restart Codex afterwards.'
}

# ---------------------------------------------------------------- install (first run)

do_install() {
  head1 "First-time installation (target model: $MODEL_SLUG)"

  # ---- proxy address (the real upstream key lives in the proxy config; no API key needed here)

  if [ -n "$BAKED_BASE_URL" ]; then
    # web-console variant: the address is baked in for the current config; skip the prompt
    BASE_URL="$BAKED_BASE_URL"
  else
    info ''
    dim 'Address of the proxy Responses listener (responses_listen in the proxy config; the template defaults to 127.0.0.1:8081)'
    BASE_URL=''
    read_tty BASE_URL "base_url [default $DEFAULT_BASE_URL]: "
    [ -z "$BASE_URL" ] && BASE_URL="$DEFAULT_BASE_URL"
  fi
  while [ "${BASE_URL%/}" != "$BASE_URL" ]; do BASE_URL="${BASE_URL%/}"; done
  case "$BASE_URL" in
    http://*|https://*) ;;
    *) die 'base_url must start with http:// or https://; exiting (no files were modified).' ;;
  esac
  case "$BASE_URL" in
    *'"'*) die 'base_url must not contain double quotes.' ;;
  esac

  # ---- backup

  mkdir -p "$BACKUP_DIR" || die "Cannot create backup directory: $BACKUP_DIR"

  ORIG_EXISTED=1
  if [ -f "$CONFIG_PATH" ]; then
    cp "$CONFIG_PATH" "$BACKUP_CONFIG" || die 'Failed to back up config.toml'
    ok "Backed up config.toml -> $BACKUP_CONFIG"
  else
    ORIG_EXISTED=0
    warn 'config.toml does not exist; a new one will be created'
  fi

  # ---- read the original file

  NLINES=0
  LINES=()
  if [ "$ORIG_EXISTED" -eq 1 ]; then
    while IFS= read -r __l || [ -n "$__l" ]; do
      __l="${__l%$'\r'}"   # normalize CRLF configs to LF before processing
      LINES[$NLINES]="$__l"; NLINES=$((NLINES+1))
    done < "$CONFIG_PATH"
  fi

  OUT=()
  NOUT=0
  INS_AT=0
  REPORT=()
  NREPORT=0
  SEEN=' '

  out_add() { OUT[$NOUT]="$1"; NOUT=$((NOUT+1)); }
  rep_add() { REPORT[$NREPORT]="$1"; NREPORT=$((NREPORT+1)); }

  IDX=0
  consume_block() {
    # swallow one complete assignment (including multi-line arrays/strings) starting at IDX, advancing IDX
    while [ "$IDX" -lt "$NLINES" ]; do
      _scan_line "${LINES[$IDX]-}"
      IDX=$((IDX+1))
      if [ -z "$_mlstate" ] && [ "$_depth" -eq 0 ]; then break; fi
    done
  }

  _depth=0
  _mlstate=''
  CUR_SECTION=''
  SKIP_SECTION=0

  while [ "$IDX" -lt "$NLINES" ]; do
    line="${LINES[$IDX]-}"
    trimmed=$(_trim "$line")

    # a leading [ is a section header only at bracket depth 0 outside multi-line strings
    is_header=0
    if [ -z "$_mlstate" ] && [ "$_depth" -eq 0 ]; then
      case "$trimmed" in '['*) is_header=1 ;; esac
    fi

    if [ "$is_header" -eq 1 ]; then
      hdr="$trimmed"
      hdr="${hdr%%]*}"
      hdr="${hdr#[}"
      hdr=$(_trim "$hdr")
      hdr=$(printf '%s' "$hdr" | tr -d '"'"'")
      CUR_SECTION="$hdr"
      SKIP_SECTION=0

      case "$hdr" in
        "model_providers.$PROVIDER_ID"|"model_providers.$PROVIDER_ID".*)
          SKIP_SECTION=1
          rep_add "Removed old [$hdr] (will be rewritten with the new settings)"
          ;;
        profiles|profiles.*)
          rep_add "Kept [$hdr] (note: when activated via --profile it overrides the top-level model/model_provider)"
          ;;
      esac

      _scan_line "$line"
      IDX=$((IDX+1))
      [ "$SKIP_SECTION" -eq 0 ] && out_add "$line"
      continue
    fi

    if [ -n "$CUR_SECTION" ]; then
      # ---- inside a section
      if [ "$SKIP_SECTION" -eq 1 ]; then
        _scan_line "$line"; IDX=$((IDX+1)); continue
      fi
      k=$(_key_of "$line")
      if [ "$k" = 'wire_api' ]; then
        v=$(_val_of "$trimmed")
        case "$v" in
          '"chat"'*|"'chat'"*)
            indent="${line%%[![:space:]]*}"
            out_add "${indent}wire_api = \"responses\""
            rep_add "Fixed wire_api of [$CUR_SECTION]: \"chat\" -> \"responses\" (chat would prevent Codex from starting)"
            _scan_line "$line"; IDX=$((IDX+1)); continue
            ;;
        esac
      fi
      out_add "$line"
      _scan_line "$line"
      IDX=$((IDX+1))
      continue
    fi

    # ---- top-level area

    k=$(_key_of "$line")

    if [ -n "$k" ] && _in_list "$k" $TARGET_KEYS; then
      oldv=$(_val_of "$trimmed")
      newv=$(target_value_for "$k")
      consume_block
      out_add "$k = $newv"
      INS_AT=$NOUT
      SEEN="$SEEN$k "
      if [ "$oldv" != "$newv" ]; then
        rep_add "Rewrote $k: $(truncate_val "$oldv") -> $newv"
      fi
      continue
    fi

    if [ -n "$k" ] && _in_list "$k" $DEL_A; then
      oldv=$(_val_of "$trimmed")
      consume_block
      rep_add "Removed $k = $(truncate_val "$oldv")  <- $(del_why "$k")"
      continue
    fi

    if [ -n "$k" ] && _in_list "$k" $DEL_B; then
      oldv=$(_val_of "$trimmed")
      consume_block
      rep_add "Removed $k = $(truncate_val "$oldv")  <- $(del_why "$k")"
      continue
    fi

    if [ -n "$k" ] && _in_list "$k" $WARN_KEYS; then
      rep_add "Kept $k (note: may cause fallback metadata or be overridden by remote config)"
    fi

    out_add "$line"
    _scan_line "$line"
    # do not record the insertion point on a line that opens a multi-line
    # string/array: inserting in the middle of a half-written assignment
    # produces invalid TOML
    if [ -n "$k" ] && [ -z "$_mlstate" ] && [ "$_depth" -eq 0 ]; then INS_AT=$NOUT; fi
    IDX=$((IDX+1))
  done

  # missing target keys are inserted after the last top-level key (before the first section header)
  MISSING=''
  for k in $TARGET_KEYS; do
    case "$SEEN" in
      *" $k "*) ;;
      *) MISSING="$MISSING$k " ;;
    esac
  done

  # ---- write back (tmp first, validate, then mv: atomic)

  TMP_CONFIG="$CONFIG_PATH.proxy429-tmp.$$"
  : > "$TMP_CONFIG" || die "Cannot write temporary file: $TMP_CONFIG"

  i=0
  while [ "$i" -lt "$NOUT" ]; do
    if [ "$i" -eq "$INS_AT" ] && [ -n "$MISSING" ]; then
      for k in $MISSING; do
        printf '%s = %s\n' "$k" "$(target_value_for "$k")" >> "$TMP_CONFIG"
      done
      MISSING=''
      # add a blank line when a section header follows, so these keys do not look like they belong to that section
      case "$(_trim "${OUT[$i]}")" in
        '['*) printf '\n' >> "$TMP_CONFIG" ;;
      esac
    fi
    printf '%s\n' "${OUT[$i]}" >> "$TMP_CONFIG"
    i=$((i+1))
  done
  if [ -n "$MISSING" ]; then
    for k in $MISSING; do
      printf '%s = %s\n' "$k" "$(target_value_for "$k")" >> "$TMP_CONFIG"
    done
  fi

  {
    printf '\n[model_providers.%s]\n' "$PROVIDER_ID"
    printf 'name = "Proxy429 Local Proxy"\n'
    printf 'base_url = "%s"\n' "$BASE_URL"
    printf 'wire_api = "responses"\n'
    printf 'experimental_bearer_token = "%s"\n' "$BEARER_TOKEN"
  } >> "$TMP_CONFIG"

  # ---- write the model catalog

  TMP_MODELS="$MODELS_PATH.proxy429-tmp.$$"
  write_models_json "$TMP_MODELS" || {
    rm -f "$TMP_MODELS" "$TMP_CONFIG"
    die "Cannot write temporary file: $TMP_MODELS"
  }

  # ---- validate

  PY=$(find_python)

  check_models_json "$PY" "$TMP_MODELS" || {
    rm -f "$TMP_MODELS" "$TMP_CONFIG"
    die "The generated models.json failed validation (missing model $MODEL_SLUG); aborted (the original files were not modified)."
  }
  VALIDATED_JSON=$MODELS_JSON_PARSED

  # duplicate-key self-check: duplicate top-level TOML keys would prevent Codex from starting at all
  _depth=0; _mlstate=''; IN_LEADING=1; DUPSEEN=' '
  while IFS= read -r l || [ -n "$l" ]; do
    t=$(_trim "$l")
    if [ -z "$_mlstate" ] && [ "$_depth" -eq 0 ]; then
      case "$t" in '['*) IN_LEADING=0 ;; esac
    fi
    if [ "$IN_LEADING" -eq 1 ]; then
      kk=$(_key_of "$l")
      if [ -n "$kk" ]; then
        case "$DUPSEEN" in
          *" $kk "*)
            rm -f "$TMP_MODELS" "$TMP_CONFIG"
            die "The generated config.toml contains a duplicate top-level key: $kk
Aborted; the original file is untouched. Backup is at: $BACKUP_DIR"
            ;;
        esac
        DUPSEEN="$DUPSEEN$kk "
      fi
    fi
    _scan_line "$l"
  done < "$TMP_CONFIG"

  VALIDATED_TOML=0
  if [ -n "$PY" ]; then
    # tomllib needs Python 3.11+; it also catches duplicate keys
    if "$PY" - "$TMP_CONFIG" <<'PYTOML' 2>/dev/null
import sys
try:
    import tomllib
except ImportError:
    sys.exit(3)
with open(sys.argv[1],'rb') as f:
    c=tomllib.load(f)
assert c.get('model'), 'model missing'
assert c.get('model_provider'), 'model_provider missing'
assert c.get('model_catalog_json'), 'model_catalog_json missing'
assert 'proxy429' in c.get('model_providers',{}), 'model_providers.proxy429 missing'
PYTOML
    then
      VALIDATED_TOML=1
    else
      rc=$?
      if [ "$rc" -ne 3 ]; then
        rm -f "$TMP_MODELS" "$TMP_CONFIG"
        die "The generated config.toml failed TOML validation (possibly duplicate keys); aborted.
The original file was not modified; backup is at: $BACKUP_DIR"
      fi
    fi
  fi

  # ---- write to disk

  # the new catalog is a superset of the current one, so write it first: if the
  # config.toml write then fails, previously chosen models still resolve
  mv "$TMP_MODELS" "$MODELS_PATH" || die 'Failed to write models.json'
  mv "$TMP_CONFIG" "$CONFIG_PATH" || die 'Failed to write config.toml'

  {
    printf 'script_version=%s\n' "$SCRIPT_VERSION"
    printf 'installed_at=%s\n' "$(date '+%Y-%m-%d %H:%M:%S')"
    printf 'original_config_existed=%s\n' "$ORIG_EXISTED"
    printf 'model_slug=%s\n' "$MODEL_SLUG"
    printf 'base_url=%s\n' "$BASE_URL"
    printf 'catalog_value=%s\n' "$CATALOG_VALUE"
    printf 'codex_home=%s\n' "$CODEX_HOME_DIR"
    printf -- '--- config.toml changes ---\n'
    i=0
    while [ "$i" -lt "$NREPORT" ]; do
      printf '%s\n' "${REPORT[$i]}"
      i=$((i+1))
    done
  } > "$MANIFEST" 2>/dev/null

  # ---- report

  ok "Wrote $MODELS_PATH (model catalog for the /model menu)"
  ok "Updated $CONFIG_PATH"

  if [ "$NREPORT" -gt 0 ]; then
    head1 "Changes to the existing config ($NREPORT total)"
    i=0
    while [ "$i" -lt "$NREPORT" ]; do
      printf '  - %s\n' "${REPORT[$i]}"
      i=$((i+1))
    done
  fi

  head1 'Configuration written'
  cat <<EOF
  model                  = "$MODEL_SLUG"
  model_provider         = "$PROVIDER_ID"
  preferred_auth_method  = "apikey"
  forced_login_method    = "api"
  model_reasoning_effort = "high"
  model_catalog_json     = "$CATALOG_VALUE"

  [model_providers.$PROVIDER_ID]
  base_url  = "$BASE_URL"
  wire_api  = "responses"
EOF

  head1 'Validation'
  if [ "$VALIDATED_JSON" -eq 1 ]; then ok 'models.json is valid JSON'
  else warn 'Python not found; models.json only got a basic check (grep for the model name)'; fi
  ok 'config.toml has no duplicate top-level keys'
  if [ "$VALIDATED_TOML" -eq 1 ]; then ok 'config.toml parses with Python tomllib'
  else dim '(TOML parse validation skipped: needs Python 3.11+; the duplicate-key self-check passed)'; fi

  # ---- proxy check (a hijacked loopback address means 503 with zero logs on this proxy)
  check_proxy_for_loopback

  info ''
  ok 'Installation complete.'
  info ''
  warn 'Restart Codex for this to take effect (config.toml and the model catalog are only read at startup)'
  info ''
  info 'How to verify:'
  info "  - the Codex CLI startup banner shows model: $MODEL_SLUG"
  info '  - the proxy web console Status tab shows an in-flight stream whose API column reads [translate]'
  info ''
  dim 'If the Codex log shows "fallback model metadata" or "Unknown model",'
  dim 'the model catalog was not loaded - please re-run this script.'
  dim 'If Codex reports 503 and the proxy console shows no request at all, a system/environment proxy hijacked 127.0.0.1:'
  dim 'make sure the NO_PROXY environment variable contains 127.0.0.1 (see the check output above), then restart Codex.'
  info ''
  dim 'The real upstream key lives in the proxy config routes.api; nothing to fill in on the Codex side. Use /model to switch reasoning effort.'
  dim 'Run this script again to switch models (1/2/3) or restore the default config (9).'
}

# ---------------------------------------------------------------- menu + preflight

main() {
  head1 "Codex <-> Proxy429 one-click setup  v$SCRIPT_VERSION"
  dim "Codex directory: $CODEX_HOME_DIR"

  # The .codex directory is the only hard prerequisite (every file we touch
  # lives in it); no client heuristics - any client creates this directory on first run
  [ -d "$CODEX_HOME_DIR" ] || die "Codex config directory not found:
  $CODEX_HOME_DIR
Please install and run Codex CLI / the ChatGPT desktop app / the VS Code Codex
extension once (the first run creates this directory), or set the CODEX_HOME
environment variable and retry:
  - Codex CLI:           npm install -g @openai/codex
  - ChatGPT desktop app: https://chatgpt.com/download"

  if [ "$BAKED_MODEL" = '__restore__' ]; then
    # web-console variant (restore): skip the menu and go straight to restore
    do_restore
  fi
  if [ -n "$BAKED_MODEL" ]; then
    # web-console variant: the model was chosen on the page and baked in; skip the menu
    MODEL_SLUG="$BAKED_MODEL"
    ok "Target model (set by the web-console variant): $MODEL_SLUG"
  else
    info ''
    info 'Choose an action:'
    info '  1. Use gpt-5-codex (matches the gpt-5* route in the proxy config template)'
    info '  2. Use claude-fable-5 (adaptive thinking on by default)'
    info '  3. Custom model name (takes part in proxy route matching and the thinking lookup)'
    info '  9. Restore the default Codex config (remove everything this script wrote)'
    info ''

    CHOICE=''
    ATTEMPT=0
    while :; do
      read_tty CHOICE 'Enter 1 / 2 / 3 / 9: '
      case "$CHOICE" in
        1|2|3|9) break ;;
      esac
      ATTEMPT=$((ATTEMPT+1))
      [ "$ATTEMPT" -ge 3 ] && die 'Invalid choice; exiting (no files were modified).'
      warn 'Invalid input; please enter 1, 2, 3 or 9.'
    done

    case "$CHOICE" in
      1) MODEL_SLUG='gpt-5-codex' ;;
      2) MODEL_SLUG='claude-fable-5' ;;
      3) read_tty MODEL_SLUG 'Enter a model name (matched against the proxy routes patterns): ' ;;
      9) do_restore ;;
    esac
  fi

  case "$MODEL_SLUG" in
    ''|*'"'*|*"'"*|*'\\'*) die 'Invalid model name (empty, or contains quotes/backslashes).' ;;
  esac

  if [ -d "$BACKUP_DIR" ]; then
    # backup exists -> this script installed before. Verify the files look as
    # expected first, then take the fast path; on any mismatch, abort and touch nothing.
    PROBLEMS=''
    if [ ! -f "$MODELS_PATH" ]; then
      PROBLEMS="$PROBLEMS
  - missing $MODELS_PATH"
    fi
    if [ ! -f "$CONFIG_PATH" ]; then
      PROBLEMS="$PROBLEMS
  - missing $CONFIG_PATH"
    else
      grep -q "^\[model_providers\.$PROVIDER_ID\]" "$CONFIG_PATH" 2>/dev/null || PROBLEMS="$PROBLEMS
  - $CONFIG_PATH is missing [model_providers.$PROVIDER_ID]"
    fi

    if [ -n "$PROBLEMS" ]; then
      die "The backup directory $BACKUP_DIR exists,
but the current configuration does not match what this script expects: $PROBLEMS

To avoid damaging your files and the backup, this run was aborted; nothing was modified.

Suggested fixes (either one):
  a) re-run this script and choose 9 to restore the default config, then re-run and choose 1/2/3 to install;
  b) inspect and delete the abnormal files listed above yourself (and delete
     $BACKUP_DIR as well if you are sure the backup is useless), then re-run this script."
    fi

    switch_model_only
  fi

  if [ -f "$MODELS_PATH" ]; then
    die "Found an existing file:
  $MODELS_PATH

It was not written by this script (this script's backup directory $BACKUP_DIR was not found).
This script needs to create that file. Please delete (or move) it yourself and re-run:
  rm '$MODELS_PATH'"
  fi

  do_install
}

main
exit 0
}
