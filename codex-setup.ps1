<#
Codex <-> Proxy429 one-click setup (Windows)

Usage:
  powershell -ExecutionPolicy Bypass -File .\codex-setup.ps1
  (usually you run the command shown on the web console Config tab:
     irm '<proxy-address>/__codexsetup.ps1?...' | iex
   with the address/model baked in for the current config - zero interaction)

Menu:
  1 = use gpt-5-codex (matches the gpt-5* route in the proxy config template)
  2 = use claude-fable-5 (adaptive thinking on by default)
  3 = custom model name
  9 = restore the default config (remove everything this script wrote)

Structure follows DeepSeek's official codex-deepseek-setup script: backup/restore +
surgical config.toml rewrite + model catalog (model_catalog_json) + atomic writes.
Difference: this proxy does not verify the token (the real upstream key is injected
by the matching route's api field), so there is no API-key prompt, only a placeholder.
#>

# This script often runs via irm | iex in the user's current session, where exit
# would close their terminal window. All aborts go through Die -> throw sentinel,
# swallowed by the try/catch at the entry point; ErrorActionPreference / StrictMode
# are only set inside entry functions, to avoid polluting the user session.

$SCRIPT_VERSION   = '1.1.0'
$PROVIDER_ID      = 'proxy429'
$DEFAULT_BASE_URL = 'http://127.0.0.1:8081/v1'
$BACKUP_DIRNAME   = 'backup-proxy429'
$CATALOG_FILENAME = 'proxy429-models.json'
$BEARER_TOKEN     = 'proxy429'   # placeholder; the proxy does not verify it

# The web-console generated variant replaces these empty strings with real
# values (replacement anchors, do not change the format); non-empty values skip the
# corresponding prompts, so paste-and-run works. BAKED_MODEL = __restore__ goes
# straight to restore; BAKED_CATALOG is a comma-separated model list (one
# representative name per routes pattern), all listed in the Codex /model menu.
# BAKED_CONTEXT_WINDOW / BAKED_COMPACT_PERCENT override the catalog's context
# window and auto-compact percent (digits only, validated server-side).
$BAKED_BASE_URL = ''
$BAKED_MODEL    = ''
$BAKED_CATALOG  = ''
$BAKED_CONTEXT_WINDOW  = ''
$BAKED_COMPACT_PERCENT = ''

# catalog context declarations: defaults match the cc-switch template (256k, compact at 95%)
$ContextWindow  = 262144
$CompactPercent = 95
if ($BAKED_CONTEXT_WINDOW  -ne '') { $ContextWindow  = [int]$BAKED_CONTEXT_WINDOW }
if ($BAKED_COMPACT_PERCENT -ne '') { $CompactPercent = [int]$BAKED_COMPACT_PERCENT }

$ABORT_SENTINEL = '__PROXY429_SETUP_ABORT__'

$script:ModelSlug = ''   # chosen from the menu
$script:BaseUrl   = ''   # asked at install time

# ---------------------------------------------------------------- output helpers

function Write-Ok    { param($m) Write-Host "[OK] " -ForegroundColor Green -NoNewline; Write-Host $m }
function Write-Warn2 { param($m) Write-Host "[!]  " -ForegroundColor Yellow -NoNewline; Write-Host $m }
function Write-Head  { param($m) Write-Host ''; Write-Host $m -ForegroundColor White }
function Write-Dim   { param($m) Write-Host $m -ForegroundColor DarkGray }
function Die {
    param($m)
    Write-Host ''
    Write-Host "[X] $m" -ForegroundColor Red
    throw $ABORT_SENTINEL
}

# ---------------------------------------------------------------- paths

$CodexHomeDir = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME '.codex' }
$ConfigPath   = Join-Path $CodexHomeDir 'config.toml'
$ModelsPath   = Join-Path $CodexHomeDir $CATALOG_FILENAME
$BackupDir    = Join-Path $CodexHomeDir $BACKUP_DIRNAME
$BackupConfig = Join-Path $BackupDir 'config.toml'
$Manifest     = Join-Path $BackupDir 'manifest.txt'

# On Windows we cannot rely on Codex expanding ~; unexpanded, it silently falls
# back to metadata. Always write the resolved absolute path with forward slashes
# (backslashes are escape characters in TOML).
$CatalogValue = $ModelsPath -replace '\\', '/'

# ---------------------------------------------------------------- model catalog (models.json)

# Entry fields follow cc-switch's native responses catalog template (the minimal
# set that actually works), plus apply_patch_tool_type=freeform and
# minimal_client_version (same as DeepSeek's official catalog; freeform
# apply_patch needs Codex 0.144.0+; this proxy supports freeform custom tools).
function New-CatalogEntry {
    param([string]$Slug, [int]$Priority)
    [ordered]@{
        slug                          = $Slug
        display_name                  = $Slug
        description                   = "Served via Proxy429 local proxy."
        base_instructions             = 'You are Codex, a coding agent. You and the user share the same workspace and collaborate to achieve the user''s goals.'
        default_reasoning_level       = 'high'
        supported_reasoning_levels    = @(
            [ordered]@{ effort = 'none';   description = 'Disable thinking' },
            [ordered]@{ effort = 'low';    description = 'Light reasoning' },
            [ordered]@{ effort = 'medium'; description = 'Medium reasoning' },
            [ordered]@{ effort = 'high';   description = 'Deep reasoning for complex problems' },
            [ordered]@{ effort = 'max';    description = 'Maximum reasoning depth' }
        )
        shell_type                    = 'shell_command'
        apply_patch_tool_type         = 'freeform'
        minimal_client_version        = '0.144.0'
        visibility                    = 'list'
        supported_in_api              = $true
        priority                      = $Priority
        supports_reasoning_summaries  = $true
        default_reasoning_summary     = 'none'
        support_verbosity             = $false
        truncation_policy             = [ordered]@{ mode = 'tokens'; limit = 10000 }
        supports_parallel_tool_calls  = $false
        supports_image_detail_original = $false
        context_window                = $ContextWindow
        max_context_window            = $ContextWindow
        effective_context_window_percent = $CompactPercent
        experimental_supported_tools  = @()
        input_modalities              = @('text', 'image')
        supports_search_tool          = $true   # this proxy maps web_search to web_search_20250305 (cc-switch drops it)
    }
}

# Catalog content = the list baked into the web-console variant (BAKED_CATALOG,
# one representative name per routes pattern, i.e. everything visible in the
# Codex /model menu) or the two built-in demo models of the interactive variant
#           + the currently selected model + other slugs from an existing
#             catalog (custom entries survive a model switch)
function Build-CatalogObject {
    $slugs = New-Object System.Collections.Generic.List[string]
    if ($BAKED_CATALOG -ne '') {
        foreach ($s in ($BAKED_CATALOG -split ',')) {
            $s = $s.Trim()
            if ($s -and -not $slugs.Contains($s)) { $slugs.Add($s) }
        }
    } else {
        foreach ($s in @('gpt-5-codex', 'claude-fable-5')) { $slugs.Add($s) }
    }
    if ($script:ModelSlug -and -not $slugs.Contains($script:ModelSlug)) { $slugs.Add($script:ModelSlug) }
    if (Test-Path -LiteralPath $ModelsPath) {
        try {
            $old = (Get-Content -LiteralPath $ModelsPath -Raw -Encoding UTF8) | ConvertFrom-Json
            foreach ($m in @($old.models)) {
                if ($m.slug -and -not $slugs.Contains($m.slug)) { $slugs.Add($m.slug) }
            }
        } catch { }   # an unparseable old catalog is treated as absent and rebuilt
    }
    $entries = @()
    for ($i = 0; $i -lt $slugs.Count; $i++) { $entries += New-CatalogEntry $slugs[$i] ($i + 1) }
    return [ordered]@{ models = $entries }
}

function Write-ModelsJson {
    param([string]$Path)
    $json = Build-CatalogObject | ConvertTo-Json -Depth 10
    # Codex reads UTF-8; UTF8Encoding($false) keeps PS 5.1 from writing a BOM
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $json.TrimEnd("`r", "`n") + "`n", $utf8NoBom)
}

# Validate the freshly written models.json: valid JSON containing the selected model (throws otherwise)
function Assert-ModelsJson {
    param([string]$Path)
    $parsed = (Get-Content -LiteralPath $Path -Raw -Encoding UTF8) | ConvertFrom-Json
    $slugs = @($parsed.models | ForEach-Object { $_.slug })
    if ($slugs -notcontains $script:ModelSlug) { throw "models.json is missing model $script:ModelSlug" }
    if ($slugs.Count -lt 1) { throw 'models.json contains no model entries' }
}

# ---------------------------------------------------------------- restore (menu 9)

function Invoke-Proxy429Restore {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head 'Restore default Codex config (remove proxy429 settings)'
    if (-not (Test-Path -LiteralPath $BackupDir)) {
        Die "Backup directory not found:`n  $BackupDir`nNothing to restore - this script was probably never installed, or was already restored."
    }

    $hadConfig = $true
    if (Test-Path -LiteralPath $Manifest) {
        if ((Get-Content -LiteralPath $Manifest -Raw) -match 'original_config_existed=0') {
            $hadConfig = $false
        }
    }

    $n = 1
    Write-Host ''
    Write-Host 'The following will be performed:'
    if ($hadConfig) {
        if (-not (Test-Path -LiteralPath $BackupConfig)) {
            Die "Backup is corrupt: missing $BackupConfig"
        }
        Write-Host "  $n. Delete current $ConfigPath"; $n++
        Write-Host '     (changes made to this file after installation will be lost)' -ForegroundColor Yellow
        Write-Host "  $n. Restore config.toml from backup"; $n++
    } else {
        Write-Host "  $n. Delete $ConfigPath"; $n++
        Write-Dim  '     (this file did not exist before installation)'
    }
    Write-Host "  $n. Delete $ModelsPath"; $n++
    Write-Host "  $n. Delete backup directory $BackupDir"
    Write-Host ''

    $ans = Read-Host 'Proceed with restore? Type y to continue, anything else to cancel'
    if ($ans -notin @('y','Y','yes','YES')) {
        Write-Host 'Cancelled; no files were modified.'
        return
    }

    if (Test-Path -LiteralPath $ModelsPath) { Remove-Item -LiteralPath $ModelsPath -Force }

    if ($hadConfig) {
        Copy-Item -LiteralPath $BackupConfig -Destination $ConfigPath -Force
        Write-Ok 'config.toml restored'
    } else {
        if (Test-Path -LiteralPath $ConfigPath) { Remove-Item -LiteralPath $ConfigPath -Force }
        Write-Ok 'config.toml deleted (it did not exist before installation)'
    }
    Write-Ok "$CATALOG_FILENAME deleted"

    Remove-Item -LiteralPath $BackupDir -Recurse -Force
    Write-Ok 'backup directory cleaned up'
    Write-Host ''
    Write-Ok 'Restore complete; Codex config is back to its pre-install state.'
    Write-Host ''
    Write-Warn2 'Restart Codex for this to take effect (config.toml is only read at startup)'
}

# ---------------------------------------------------------------- TOML scanner

# Track bracket depth and multi-line string state to recognize real section headers
$script:Depth = 0
$script:MlState = ''

function Update-ScanState {
    param([string]$Line)
    $n = $Line.Length
    $i = 0
    $instr = ''
    while ($i -lt $n) {
        $c = $Line[$i]
        $c3 = if ($i + 3 -le $n) { $Line.Substring($i, 3) } else { '' }

        if ($script:MlState) {
            if ($script:MlState -eq 'basic' -and $c3 -eq '"""') { $script:MlState = ''; $i += 3; continue }
            if ($script:MlState -eq 'literal' -and $c3 -eq "'''") { $script:MlState = ''; $i += 3; continue }
            if ($script:MlState -eq 'basic' -and $c -eq '\') { $i += 2; continue }
            $i++; continue
        }
        if ($instr) {
            if ($instr -eq 'basic') {
                if ($c -eq '\') { $i += 2; continue }
                if ($c -eq '"') { $instr = '' }
            } else {
                if ($c -eq "'") { $instr = '' }
            }
            $i++; continue
        }
        if ($c3 -eq '"""') { $script:MlState = 'basic';   $i += 3; continue }
        if ($c3 -eq "'''") { $script:MlState = 'literal'; $i += 3; continue }

        switch ($c) {
            '#' { return }
            '"' { $instr = 'basic' }
            "'" { $instr = 'literal' }
            '[' { $script:Depth++ }
            ']' { if ($script:Depth -gt 0) { $script:Depth-- } }
        }
        $i++
    }
}

function Get-TomlKey {
    param([string]$Line)
    $l = $Line.Trim()
    if ($l -eq '' -or $l.StartsWith('#')) { return '' }
    $eq = $l.IndexOf('=')
    if ($eq -lt 1) { return '' }
    $k = $l.Substring(0, $eq).Trim()
    return $k.Trim('"').Trim("'")
}

function Get-TomlValue {
    param([string]$Line)
    $l = $Line.Trim()
    $eq = $l.IndexOf('=')
    if ($eq -lt 0) { return '' }
    return $l.Substring($eq + 1).Trim()
}

$TARGET_KEYS = @('model','model_provider','preferred_auth_method','forced_login_method',
                 'model_reasoning_effort','model_catalog_json')

$DEL_A = @{
    'oss_provider'    = 'another provider selector that would route requests elsewhere'
    'openai_base_url' = 'global base_url override that would hijack requests'
}

# These top-level keys fight the model catalog (same cleanup as the DeepSeek
# script): context-window / compact-threshold overrides defeat the catalog
# declarations; stale values like service_tier get sent to the API and cause 400.
$DEL_B = [ordered]@{
    'model_context_window'               = 'overrides the context window declared by models.json; when too large, auto-compact never triggers and the API errors mid-run'
    'model_auto_compact_token_limit'      = 'overrides the auto-compact timing'
    'model_auto_compact_token_limit_scope'= 'overrides the auto-compact timing'
    'base_instructions'                  = 'overrides the base_instructions of models.json'
    'model_instructions_file'            = 'overrides the base_instructions of models.json'
    'compact_prompt'                     = 'overrides the context-compaction prompt'
    'experimental_compact_prompt_file'   = 'overrides the context-compaction prompt'
    'service_tier'                       = 'stale value gets sent as an API parameter, possibly causing 400'
    'model_verbosity'                    = 'stale value may exceed what the model supports'
    'model_reasoning_summary'            = 'models.json declares default_reasoning_summary=none; a stale value would emit reasoning.summary'
    'plan_mode_reasoning_effort'         = 'may be a level the catalog does not declare'
    'experimental_use_unified_exec_tool' = 'conflicts with shell_type=shell_command in models.json'
}

$WARN_KEYS = @('review_model','experimental_thread_config_endpoint',
               'experimental_thread_store_endpoint','experimental_thread_store')

function Get-TargetValue {
    param([string]$Key)
    switch ($Key) {
        'model'                  { return "`"$($script:ModelSlug)`"" }
        'model_provider'         { return "`"$PROVIDER_ID`"" }
        'preferred_auth_method'  { return '"apikey"' }
        'forced_login_method'    { return '"api"' }
        'model_reasoning_effort' { return '"high"' }
        'model_catalog_json'     { return "`"$CatalogValue`"" }
    }
    return '""'
}

function Format-Val {
    param([string]$v)
    if ($v.Length -gt 58) { return $v.Substring(0, 58) + '...' }
    return $v
}

# ---------------------------------------------------------------- fast path: switch model only

# When the backup and models.json look as expected, only the model key in the
# top-level area of config.toml is rewritten; everything else (base_url, the
# result of the previous surgery, ...) stays untouched.
function Invoke-Proxy429SwitchModel {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "Switching default model -> $($script:ModelSlug)"
    Write-Dim  'Backup from this script detected; refreshing the model catalog, switching the model field,'
    Write-Dim  'and syncing the script-owned provider block (name/wire_api; base_url only when baked in).'

    $raw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
    if ($null -eq $raw) { $raw = '' }
    $raw = $raw -replace "`r`n", "`n"
    $raw = $raw.TrimEnd("`n")
    if ($raw -eq '') { $Lines = @() } else { $Lines = $raw -split "`n" }

    $Out = New-Object System.Collections.Generic.List[string]
    $provSynced = New-Object System.Collections.Generic.List[string]
    $script:Depth = 0
    $script:MlState = ''
    $i = 0
    $replaced = $false
    $inLeading = $true
    $curSec = ''
    # only the web-console variant carries a base URL; interactive re-runs keep the existing line
    $syncBaseUrl = $BAKED_BASE_URL.TrimEnd('/')

    while ($i -lt $Lines.Count) {
        $line = $Lines[$i]
        if (-not $inLeading) {
            # inside sections: keep the scan state accurate so real headers are recognized
            if ($script:MlState -or $script:Depth -ne 0) {
                Update-ScanState $line
                $Out.Add($line); $i++; continue
            }
            $trimmed = $line.Trim()
            if ($trimmed.StartsWith('[')) {
                $h = $trimmed
                $c2 = $h.IndexOf(']')
                if ($c2 -gt 0) { $h = $h.Substring(0, $c2 + 1) }
                $curSec = $h.TrimStart('[').TrimEnd(']').Trim().Replace('"','').Replace("'",'')
                Update-ScanState $line
                $Out.Add($line); $i++; continue
            }
            # the provider block is script-owned (same rule as the catalog file): refresh
            # name/wire_api constants, and base_url when this run has one baked in - this
            # is what upgrades a block written by an older script version (e.g. Chinese name)
            if ($curSec -eq "model_providers.$PROVIDER_ID") {
                $k2 = Get-TomlKey $line
                $nv = $null
                switch ($k2) {
                    'name'     { $nv = '"Proxy429 Local Proxy"' }
                    'base_url' { if ($syncBaseUrl -ne '') { $nv = "`"$syncBaseUrl`"" } }
                    'wire_api' { $nv = '"responses"' }
                }
                if ($null -ne $nv) {
                    # swallow a theoretical multi-line value before writing the replacement
                    Update-ScanState $line
                    $i++
                    while (($script:MlState -or $script:Depth -ne 0) -and $i -lt $Lines.Count) {
                        Update-ScanState $Lines[$i]
                        $i++
                    }
                    if ($trimmed -ne "$k2 = $nv") { $provSynced.Add("$k2 = $nv") }
                    $Out.Add("$k2 = $nv")
                    continue
                }
            }
            Update-ScanState $line
            $Out.Add($line); $i++; continue
        }
        # continuation lines of multi-line strings/arrays pass through without key parsing
        if ($script:MlState -or $script:Depth -ne 0) {
            Update-ScanState $line
            $Out.Add($line); $i++; continue
        }
        $trimmed = $line.Trim()
        $isHeader = $trimmed.StartsWith('[')
        if ($isHeader) {
            # top-level area ended without seeing model -> insert before the first section header
            if (-not $replaced) {
                $Out.Add("model = `"$($script:ModelSlug)`"")
                $Out.Add('')
                $replaced = $true
            }
            $inLeading = $false
            $Out.Add($line); $i++; continue
        }
        $k = Get-TomlKey $line
        if ($k -eq 'model') {
            # swallow the complete assignment (could theoretically be a multi-line string)
            Update-ScanState $line
            $i++
            while (($script:MlState -or $script:Depth -ne 0) -and $i -lt $Lines.Count) {
                Update-ScanState $Lines[$i]
                $i++
            }
            $Out.Add("model = `"$($script:ModelSlug)`"")
            $replaced = $true
            continue
        }
        Update-ScanState $line
        $Out.Add($line); $i++
    }
    if (-not $replaced) {
        $Out.Add("model = `"$($script:ModelSlug)`"")
    }

    $TmpConfig = "$ConfigPath.proxy429-tmp"
    $TmpModels = "$ModelsPath.proxy429-tmp"
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($TmpConfig, (($Out -join "`n") + "`n"), $utf8NoBom)
    Write-ModelsJson $TmpModels

    try {
        Assert-ModelsJson $TmpModels
    } catch {
        Remove-Item -LiteralPath $TmpModels, $TmpConfig -Force -ErrorAction SilentlyContinue
        Die "The generated models.json failed validation; aborted (original files untouched).`n$($_.Exception.Message)"
    }

    # the new catalog is a superset of the current one, so write it first: if the
    # config.toml write then fails, previously chosen models still resolve
    Move-Item -LiteralPath $TmpModels -Destination $ModelsPath -Force
    Move-Item -LiteralPath $TmpConfig -Destination $ConfigPath -Force

    Write-Ok "Refreshed $CATALOG_FILENAME (this file is rewritten by the script; manual edits are overwritten)"
    Write-Ok "config.toml updated: model = `"$($script:ModelSlug)`""
    if ($provSynced.Count -gt 0) {
        Write-Ok "Synced [model_providers.$PROVIDER_ID]: $($provSynced -join '; ')"
    }
    Write-Host ''
    Write-Warn2 'Restart Codex for this to take effect (config.toml is only read at startup)'
    Write-Host ''
    Write-Host 'How to verify:'
    Write-Host "  - the Codex CLI startup banner shows model: $($script:ModelSlug)"
    Write-Host '  - the proxy web console Status tab shows an in-flight stream whose API column reads [translate] (or [Response] when the route has url_response_api)'
    Write-Host ''
    Write-Dim 'Run this script again to switch models (1/2/3) or restore the default config (9).'
}

# ---------------------------------------------------------------- system proxy check

# Codex's HTTP stack honors the Windows "Internet Options" system proxy but
# ignores its ProxyOverride exception list (verified on desktop 0.150 / CLI
# 0.151): with the system proxy on, a base_url pointing at 127.0.0.1 gets sent
# to the proxy server, where nothing listens on that machine's loopback
# -> unexpected status 503 Service Unavailable (and the proxy sees no request).
# The only bypass that actually works is the NO_PROXY environment variable
# (same for codex.exe and the desktop app).
function Ensure-NoProxyForLoopback {
    param([string]$Url)

    if ($env:PROXY429_SKIP_PROXY_FIX -eq '1') { return }   # escape hatch for automated tests
    if ($Url -notmatch '^https?://(127\.|localhost|\[::1\])') { return }

    $proxyEnable = 0
    $proxyServer = ''
    $ie = Get-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -ErrorAction SilentlyContinue
    if ($ie) {
        if ($ie.PSObject.Properties['ProxyEnable']) { $proxyEnable = [int]$ie.ProxyEnable }
        if ($ie.PSObject.Properties['ProxyServer']) { $proxyServer = [string]$ie.ProxyServer }
    }
    if ($proxyEnable -ne 1 -or -not $proxyServer) { return }

    foreach ($scope in 'Process', 'User', 'Machine') {
        $v = [Environment]::GetEnvironmentVariable('NO_PROXY', $scope)
        if (-not $v) { continue }
        $entries = @($v -split ',' | ForEach-Object { $_.Trim() })
        if ($entries -contains '127.0.0.1' -or $entries -contains '*') {
            Write-Ok "NO_PROXY ($scope scope) already contains 127.0.0.1; the system proxy will not intercept this proxy"
            return
        }
    }

    Write-Warn2 "System proxy is enabled ($proxyServer)"
    Write-Dim  'Codex follows the system proxy and ignores its exception list, so requests to 127.0.0.1 would be sent to the proxy server and fail with 503.'
    $cur = [Environment]::GetEnvironmentVariable('NO_PROXY', 'User')
    $add = 'localhost,127.0.0.1,::1'
    $new = if ($cur) { $cur.TrimEnd(',') + ',' + $add } else { $add }
    [Environment]::SetEnvironmentVariable('NO_PROXY', $new, 'User')
    Write-Ok "Appended user-level NO_PROXY = $new"
    Write-Dim  '(only loopback bypasses the system proxy; other traffic is unaffected. To undo: System Properties -> Environment Variables, delete NO_PROXY)'

    # broadcast WM_SETTINGCHANGE (same effect as setx) so Codex started afterwards picks up the new variable
    try {
        if (-not ('Win32.P429Native' -as [type])) {
            Add-Type -Namespace Win32 -Name P429Native -MemberDefinition '[DllImport("user32.dll", CharSet=CharSet.Auto)] public static extern System.IntPtr SendMessageTimeout(System.IntPtr hWnd, uint Msg, System.UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);'
        }
        $r = [System.UIntPtr]::Zero
        [void][Win32.P429Native]::SendMessageTimeout([System.IntPtr]0xffff, 0x1A, [System.UIntPtr]::Zero, 'Environment', 2, 5000, [ref]$r)
    } catch {
        Write-Dim 'Failed to broadcast the environment change (the setting itself is already in effect; signing out and back in will definitely apply it)'
    }
    Write-Warn2 'If Codex still returns 503 after a restart, sign out of Windows and back in, then try again'
}

# ---------------------------------------------------------------- install (first run)

function Invoke-Proxy429Install {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "First-time installation (target model: $($script:ModelSlug))"

    # ---- interactive input: proxy address (the real upstream key lives in the proxy config; not needed here)

    if ($BAKED_BASE_URL -ne '') {
        # web-console variant: the address is baked in for the current config; skip the prompt
        $script:BaseUrl = $BAKED_BASE_URL
    } else {
        Write-Host ''
        Write-Dim "Address of the proxy Responses listener (responses_listen in the proxy config; the template defaults to 127.0.0.1:8081)"
        $script:BaseUrl = ''
        try {
            $script:BaseUrl = (Read-Host "base_url [default $DEFAULT_BASE_URL]").Trim()
        } catch {
            Die 'Cannot read input (non-interactive environment); exiting (no files were modified).'
        }
        if ($script:BaseUrl -eq '') { $script:BaseUrl = $DEFAULT_BASE_URL }
    }
    $script:BaseUrl = $script:BaseUrl.TrimEnd('/')
    if ($script:BaseUrl -notmatch '^https?://') {
        Die "base_url must start with http:// or https://; exiting (no files were modified)."
    }
    if ($script:BaseUrl -match '"') { Die 'base_url must not contain double quotes.' }

    # ---- backup

    New-Item -ItemType Directory -Path $BackupDir -Force | Out-Null

    $OrigExisted = Test-Path -LiteralPath $ConfigPath
    if ($OrigExisted) {
        Copy-Item -LiteralPath $ConfigPath -Destination $BackupConfig -Force
        Write-Ok "Backed up config.toml -> $BackupConfig"
    } else {
        Write-Warn2 'config.toml does not exist; a new one will be created'
    }

    # ---- process

    $Lines = @()
    if ($OrigExisted) {
        $raw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
        if ($null -eq $raw) { $raw = '' }
        # -split on an empty string yields one empty element, so check for '' first
        $raw = $raw -replace "`r`n", "`n"
        $raw = $raw.TrimEnd("`n")
        if ($raw -eq '') { $Lines = @() } else { $Lines = $raw -split "`n" }
    }

    $Out    = New-Object System.Collections.Generic.List[string]
    $Report = New-Object System.Collections.Generic.List[string]
    $Seen   = New-Object System.Collections.Generic.HashSet[string]
    $InsAt  = 0

    $script:Depth = 0
    $script:MlState = ''
    $script:idx = 0
    $curSection = ''
    $skipSection = $false

    # swallow one complete assignment (including multi-line arrays/strings); $Lines is visible through the caller scope
    function Consume-Block {
        while ($script:idx -lt $Lines.Count) {
            Update-ScanState $Lines[$script:idx]
            $script:idx++
            if (-not $script:MlState -and $script:Depth -eq 0) { break }
        }
    }

    while ($script:idx -lt $Lines.Count) {
        $line = $Lines[$script:idx]
        $trimmed = $line.Trim()

        $isHeader = (-not $script:MlState) -and ($script:Depth -eq 0) -and $trimmed.StartsWith('[')

        if ($isHeader) {
            $hdr = $trimmed
            $close = $hdr.IndexOf(']')
            if ($close -gt 0) { $hdr = $hdr.Substring(0, $close + 1) }
            $hdr = $hdr.TrimStart('[').TrimEnd(']').Trim().Replace('"','').Replace("'",'')
            $curSection = $hdr
            $skipSection = $false

            if ($hdr -eq "model_providers.$PROVIDER_ID" -or $hdr -like "model_providers.$PROVIDER_ID.*") {
                $skipSection = $true
                $Report.Add("Removed old [$hdr] (will be rewritten with the new settings)")
            }
            elseif ($hdr -eq 'profiles' -or $hdr -like 'profiles.*') {
                $Report.Add("Kept [$hdr] (note: when activated via --profile it overrides the top-level model/model_provider)")
            }

            Update-ScanState $line
            $script:idx++
            if (-not $skipSection) { $Out.Add($line) }
            continue
        }

        if ($curSection) {
            if ($skipSection) { Update-ScanState $line; $script:idx++; continue }

            if ((Get-TomlKey $line) -eq 'wire_api') {
                $v = Get-TomlValue $trimmed
                if ($v -match '^["'']chat["'']') {
                    $indent = $line.Substring(0, $line.Length - $line.TrimStart().Length)
                    $Out.Add("${indent}wire_api = `"responses`"")
                    $Report.Add("Fixed wire_api of [$curSection]: `"chat`" -> `"responses`" (chat would prevent Codex from starting)")
                    Update-ScanState $line
                    $script:idx++
                    continue
                }
            }
            $Out.Add($line)
            Update-ScanState $line
            $script:idx++
            continue
        }

        # ---- top-level area

        $k = Get-TomlKey $line

        if ($k -and $TARGET_KEYS -contains $k) {
            $oldv = Get-TomlValue $trimmed
            $newv = Get-TargetValue $k
            Consume-Block
            $Out.Add("$k = $newv")
            $InsAt = $Out.Count
            [void]$Seen.Add($k)
            if ($oldv -ne $newv) {
                $Report.Add("Rewrote $k`: $(Format-Val $oldv) -> $newv")
            }
            continue
        }

        if ($k -and $DEL_A.Contains($k)) {
            $oldv = Get-TomlValue $trimmed
            Consume-Block
            $Report.Add("Removed $k = $(Format-Val $oldv)  <- $($DEL_A[$k])")
            continue
        }

        if ($k -and $DEL_B.Contains($k)) {
            $oldv = Get-TomlValue $trimmed
            Consume-Block
            $Report.Add("Removed $k = $(Format-Val $oldv)  <- $($DEL_B[$k])")
            continue
        }

        if ($k -and $WARN_KEYS -contains $k) {
            $Report.Add("Kept $k (note: may cause fallback metadata or be overridden by remote config)")
        }

        $Out.Add($line)
        Update-ScanState $line
        # do not record the insertion point on a line that opens a multi-line
        # string/array: inserting in the middle of a half-written assignment
        # produces invalid TOML
        if ($k -and -not $script:MlState -and $script:Depth -eq 0) { $InsAt = $Out.Count }
        $script:idx++
    }

    $missing = @($TARGET_KEYS | Where-Object { -not $Seen.Contains($_) })

    # ---- assemble

    $final = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $Out.Count; $i++) {
        if ($i -eq $InsAt -and $missing.Count -gt 0) {
            foreach ($k in $missing) { $final.Add("$k = $(Get-TargetValue $k)") }
            $missing = @()
            # add a blank line when a section header follows, so these keys do not look like they belong to that section
            if ($Out[$i].Trim().StartsWith('[')) { $final.Add('') }
        }
        $final.Add($Out[$i])
    }
    foreach ($k in $missing) { $final.Add("$k = $(Get-TargetValue $k)") }

    $final.Add('')
    $final.Add("[model_providers.$PROVIDER_ID]")
    $final.Add('name = "Proxy429 Local Proxy"')
    $final.Add("base_url = `"$($script:BaseUrl)`"")
    $final.Add('wire_api = "responses"')
    $final.Add("experimental_bearer_token = `"$BEARER_TOKEN`"")

    $TmpConfig = "$ConfigPath.proxy429-tmp"
    $TmpModels = "$ModelsPath.proxy429-tmp"

    # Codex reads UTF-8; UTF8Encoding($false) keeps PS 5.1 from writing a BOM
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($TmpConfig, (($final -join "`n") + "`n"), $utf8NoBom)

    Write-ModelsJson $TmpModels

    # ---- validate

    $ValidatedJson = $false
    try {
        Assert-ModelsJson $TmpModels
        $ValidatedJson = $true
    } catch {
        Remove-Item -LiteralPath $TmpModels, $TmpConfig -Force -ErrorAction SilentlyContinue
        Die "The generated models.json failed JSON validation; aborted (original files untouched).`n$($_.Exception.Message)"
    }

    # duplicate-key self-check: duplicate top-level TOML keys would prevent Codex from starting at all
    $dupCheck = @{}
    $depth2 = 0; $ml2 = ''
    $inLeading = $true
    foreach ($l in $final) {
        $t = $l.Trim()
        if (-not $ml2 -and $depth2 -eq 0 -and $t.StartsWith('[')) { $inLeading = $false }
        if ($inLeading) {
            $kk = Get-TomlKey $l
            if ($kk) {
                if ($dupCheck.Contains($kk)) {
                    Remove-Item -LiteralPath $TmpModels, $TmpConfig -Force -ErrorAction SilentlyContinue
                    Die "The generated config.toml contains a duplicate top-level key: $kk`nAborted; the original file is untouched. Backup is at: $BackupDir"
                }
                $dupCheck[$kk] = $true
            }
        }
        $script:MlState = $ml2; $script:Depth = $depth2
        Update-ScanState $l
        $ml2 = $script:MlState; $depth2 = $script:Depth
    }

    # ---- write to disk

    Move-Item -LiteralPath $TmpModels -Destination $ModelsPath -Force
    Move-Item -LiteralPath $TmpConfig -Destination $ConfigPath -Force

    $manifestLines = @(
        "script_version=$SCRIPT_VERSION"
        "installed_at=$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')"
        "original_config_existed=$(if ($OrigExisted) { 1 } else { 0 })"
        "model_slug=$($script:ModelSlug)"
        "base_url=$($script:BaseUrl)"
        "catalog_value=$CatalogValue"
        "codex_home=$CodexHomeDir"
        '--- config.toml changes ---'
    ) + $Report
    [System.IO.File]::WriteAllText($Manifest, (($manifestLines -join "`n") + "`n"), $utf8NoBom)

    # ---- report

    Write-Ok "Wrote $ModelsPath (model catalog for the /model menu)"
    Write-Ok "Updated $ConfigPath"

    if ($Report.Count -gt 0) {
        Write-Head "Changes to the existing config ($($Report.Count) total)"
        foreach ($r in $Report) { Write-Host "  - $r" }
    }

    Write-Head 'Configuration written'
    Write-Host @"
  model                  = "$($script:ModelSlug)"
  model_provider         = "$PROVIDER_ID"
  preferred_auth_method  = "apikey"
  forced_login_method    = "api"
  model_reasoning_effort = "high"
  model_catalog_json     = "$CatalogValue"

  [model_providers.$PROVIDER_ID]
  base_url  = "$($script:BaseUrl)"
  wire_api  = "responses"
"@

    Write-Head 'Validation'
    if ($ValidatedJson) { Write-Ok 'models.json is valid JSON' }
    Write-Ok 'config.toml has no duplicate top-level keys'

    # ---- system proxy check (the invisible 503 trap: with base_url at 127.0.0.1, requests can be hijacked by the system proxy)
    Ensure-NoProxyForLoopback $script:BaseUrl

    Write-Host ''
    Write-Ok 'Installation complete.'
    Write-Host ''
    Write-Warn2 'Restart Codex for this to take effect (config.toml and the model catalog are only read at startup)'
    Write-Host ''
    Write-Host 'How to verify:'
    Write-Host "  - the Codex CLI startup banner shows model: $($script:ModelSlug)"
    Write-Host '  - the proxy web console Status tab shows an in-flight stream whose API column reads [translate] (or [Response] when the route has url_response_api)'
    Write-Host ''
    Write-Dim 'If the Codex log shows "fallback model metadata" or "Unknown model",'
    Write-Dim 'the model catalog was not loaded - please re-run this script.'
    Write-Dim 'If Codex reports 503 and the proxy console shows no request at all, the system proxy hijacked 127.0.0.1:'
    Write-Dim 'make sure the user environment variable NO_PROXY contains 127.0.0.1 (this script writes it at install), then restart Codex.'
    Write-Host ''
    Write-Dim "The real upstream key lives in the proxy config routes.api; nothing to fill in on the Codex side. Use /model to switch reasoning effort."
    Write-Dim 'Run this script again to switch models (1/2/3) or restore the default config (9).'
}

# ---------------------------------------------------------------- menu + preflight

function Invoke-Proxy429Main {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "Codex <-> Proxy429 one-click setup  v$SCRIPT_VERSION"
    Write-Dim  "Codex directory: $CodexHomeDir"

    # The .codex directory is the only hard prerequisite (every file we touch
    # lives in it); no client heuristics - any client creates this directory on first run
    if (-not (Test-Path -LiteralPath $CodexHomeDir)) {
        Die @"
Codex config directory not found:
  $CodexHomeDir
Please install and run Codex CLI / the ChatGPT desktop app / the VS Code Codex
extension once (the first run creates this directory), or set the CODEX_HOME
environment variable and retry:
  - Codex CLI:           npm install -g @openai/codex
  - ChatGPT desktop app: https://chatgpt.com/download
"@
    }

    if ($BAKED_MODEL -eq '__restore__') {
        # web-console variant (restore): skip the menu and go straight to restore
        Invoke-Proxy429Restore
        return
    }
    if ($BAKED_MODEL -ne '') {
        # web-console variant: the model was chosen on the page and baked in; skip the menu
        $script:ModelSlug = $BAKED_MODEL
        Write-Ok "Target model (set by the web-console variant): $($script:ModelSlug)"
    } else {
        Write-Host ''
        Write-Host 'Choose an action:'
        Write-Host '  1. Use gpt-5-codex (matches the gpt-5* route in the proxy config template)'
        Write-Host '  2. Use claude-fable-5 (adaptive thinking on by default)'
        Write-Host '  3. Custom model name (takes part in proxy route matching and the thinking lookup)'
        Write-Host '  9. Restore the default Codex config (remove everything this script wrote)'
        Write-Host ''

        $choice = ''
        $attempt = 0
        while ($true) {
            try {
                $choice = Read-Host 'Enter 1 / 2 / 3 / 9'
            } catch {
                Die 'Cannot read input (non-interactive environment); exiting (no files were modified).'
            }
            if ($choice -in @('1','2','3','9')) { break }
            $attempt++
            if ($attempt -ge 3) { Die 'Invalid choice; exiting (no files were modified).' }
            Write-Warn2 'Invalid input; please enter 1, 2, 3 or 9.'
        }

        switch ($choice) {
            '1' { $script:ModelSlug = 'gpt-5-codex' }
            '2' { $script:ModelSlug = 'claude-fable-5' }
            '3' {
                try {
                    $script:ModelSlug = (Read-Host 'Enter a model name (matched against the proxy routes patterns)').Trim()
                } catch {
                    Die 'Cannot read input (non-interactive environment); exiting (no files were modified).'
                }
                if ($script:ModelSlug -eq '' -or $script:ModelSlug -match '[\"''\\]') {
                    Die 'Model name must not be empty or contain quotes or backslashes.'
                }
            }
            '9' { Invoke-Proxy429Restore; return }
        }
    }

    if ($script:ModelSlug -eq '' -or $script:ModelSlug -match '[\"''\\]') {
        Die 'Invalid model name (empty, or contains quotes/backslashes).'
    }

    if (Test-Path -LiteralPath $BackupDir) {
        # backup exists -> this script installed before. Verify the files look as
        # expected first, then take the fast path; on any mismatch, abort and touch nothing.
        $problems = @()
        if (-not (Test-Path -LiteralPath $ModelsPath)) {
            $problems += "  - missing $ModelsPath"
        }
        if (-not (Test-Path -LiteralPath $ConfigPath)) {
            $problems += "  - missing $ConfigPath"
        } else {
            $configRaw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
            if ($configRaw -notmatch "(?m)^\[model_providers\.$PROVIDER_ID\]") {
                $problems += "  - $ConfigPath is missing [model_providers.$PROVIDER_ID]"
            }
        }

        if ($problems.Count -gt 0) {
            Die @"
The backup directory $BackupDir exists,
but the current configuration does not match what this script expects:
$($problems -join "`n")

To avoid damaging your files and the backup, this run was aborted; nothing was modified.

Suggested fixes (either one):
  a) re-run this script and choose 9 to restore the default config, then re-run and choose 1/2/3 to install;
  b) inspect and delete the abnormal files listed above yourself (and delete
     $BackupDir as well if you are sure the backup is useless), then re-run this script.
"@
        }

        Invoke-Proxy429SwitchModel
        return
    }

    if (Test-Path -LiteralPath $ModelsPath) {
        Die @"
Found an existing file:
  $ModelsPath

It was not written by this script (this script's backup directory $BackupDir was not found).
This script needs to create that file. Please delete (or move) it yourself and re-run:
  Remove-Item '$ModelsPath'
"@
    }

    Invoke-Proxy429Install
}

# ---------------------------------------------------------------- entry point

try {
    Invoke-Proxy429Main
} catch {
    if ("$_" -ne $ABORT_SENTINEL) { throw }
    # Die already printed the error; keep a non-zero exit code when run as a file (never exit in the iex case)
    if ($PSCommandPath) { exit 1 }
}
