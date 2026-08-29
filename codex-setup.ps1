<#
Codex ↔ Proxy429 一键配置（Windows）

用法:
  powershell -ExecutionPolicy Bypass -File .\codex-setup.ps1
  （也可把本文件放到任意 Web 服务器上，irm <url> | iex）

启动后选菜单:
  1 = 使用 gpt-5-codex（命中代理配置模板的 gpt-5* 路由）
  2 = 使用 claude-fable-5（adaptive thinking 默认开）
  3 = 自定义模型名
  9 = 还原默认配置（移除本脚本写入的内容）

结构参照 DeepSeek 官方 codex-deepseek-setup 脚本：备份/还原 + config.toml
外科手术式改写 + 模型目录（model_catalog_json）+ 原子写盘。
区别：本代理不校验 token（真实上游 key 由代理路由的 api 字段注入），
所以不提示输入 API key，只写占位串。
#>

# 本脚本常以 irm | iex 在用户当前会话里执行，exit 会关掉用户的终端窗口。
# 所有中止都走 Die -> throw 哨兵，由底部入口的 try/catch 吞掉；
# ErrorActionPreference / StrictMode 只在入口函数内设置，避免污染用户会话。

$SCRIPT_VERSION   = '1.0.0'
$PROVIDER_ID      = 'proxy429'
$DEFAULT_BASE_URL = 'http://127.0.0.1:8081/v1'
$BACKUP_DIRNAME   = 'backup-proxy429'
$CATALOG_FILENAME = 'proxy429-models.json'
$BEARER_TOKEN     = 'proxy429'   # 占位，代理不校验

# 网页控制台「配置」页生成版会把这三个空串替换为实际值（替换锚点，勿改格式），
# 非空时跳过对应交互，实现"复制粘贴即跑"。BAKED_MODEL 为 __restore__ 时直接走还原；
# BAKED_CATALOG 为逗号分隔的模型清单（routes 各 pattern 的代表名），全部进 Codex /model 菜单。
$BAKED_BASE_URL = ''
$BAKED_MODEL    = ''
$BAKED_CATALOG  = ''

$ABORT_SENTINEL = '__PROXY429_SETUP_ABORT__'

$script:ModelSlug = ''   # 菜单选择决定
$script:BaseUrl   = ''   # 安装时询问

# ---------------------------------------------------------------- 输出辅助

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

# ---------------------------------------------------------------- 路径

$CodexHomeDir = if ($env:CODEX_HOME) { $env:CODEX_HOME } else { Join-Path $HOME '.codex' }
$ConfigPath   = Join-Path $CodexHomeDir 'config.toml'
$ModelsPath   = Join-Path $CodexHomeDir $CATALOG_FILENAME
$BackupDir    = Join-Path $CodexHomeDir $BACKUP_DIRNAME
$BackupConfig = Join-Path $BackupDir 'config.toml'
$Manifest     = Join-Path $BackupDir 'manifest.txt'

# Windows 上不能确认 Codex 会展开 ~，不展开时静默走 fallback 元数据。
# 一律写解析后的绝对路径并用正斜杠（反斜杠在 TOML 里是转义符）。
$CatalogValue = $ModelsPath -replace '\\', '/'

# ---------------------------------------------------------------- 模型目录（models.json）

# 条目字段以 cc-switch 的 native responses 目录模板为准（真实可用的最小集），
# 另加 apply_patch_tool_type=freeform 与 minimal_client_version（DeepSeek 官方目录同款，
# freeform apply_patch 需要 Codex 0.144.0+；本代理支持 freeform custom 工具回传）。
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
        context_window                = 262144
        max_context_window            = 262144
        effective_context_window_percent = 95
        experimental_supported_tools  = @()
        input_modalities              = @('text', 'image')
        supports_search_tool          = $true   # 本代理把 web_search 映射成 web_search_20250305（cc-switch 是丢弃）
    }
}

# 目录内容 = 网页生成版烤入的清单（BAKED_CATALOG，routes 各 pattern 的代表名，
# 即 Codex /model 菜单可见的全部模型）或交互版的两个内置演示模型
#           + 当前选中模型 + 已有目录里的其它 slug（换模型时保留自定义条目）
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
        } catch { }   # 旧目录解析不了就当不存在，重建
    }
    $entries = @()
    for ($i = 0; $i -lt $slugs.Count; $i++) { $entries += New-CatalogEntry $slugs[$i] ($i + 1) }
    return [ordered]@{ models = $entries }
}

function Write-ModelsJson {
    param([string]$Path)
    $json = Build-CatalogObject | ConvertTo-Json -Depth 10
    # Codex 读 UTF-8；UTF8Encoding($false) 避免 PS 5.1 写出 BOM
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $json.TrimEnd("`r", "`n") + "`n", $utf8NoBom)
}

# 校验刚写出的 models.json：是合法 JSON 且包含当前选中的模型（不通过则抛异常）
function Assert-ModelsJson {
    param([string]$Path)
    $parsed = (Get-Content -LiteralPath $Path -Raw -Encoding UTF8) | ConvertFrom-Json
    $slugs = @($parsed.models | ForEach-Object { $_.slug })
    if ($slugs -notcontains $script:ModelSlug) { throw "models.json 缺少模型 $script:ModelSlug" }
    if ($slugs.Count -lt 1) { throw 'models.json 不含任何模型条目' }
}

# ---------------------------------------------------------------- 还原（菜单 9）

function Invoke-Proxy429Restore {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head '还原默认 Codex 配置（移除 proxy429 设置）'
    if (-not (Test-Path -LiteralPath $BackupDir)) {
        Die "找不到备份目录：`n  $BackupDir`n没有可还原的内容——本脚本可能从未安装过，或已经还原过了。"
    }

    $hadConfig = $true
    if (Test-Path -LiteralPath $Manifest) {
        if ((Get-Content -LiteralPath $Manifest -Raw) -match 'original_config_existed=0') {
            $hadConfig = $false
        }
    }

    $n = 1
    Write-Host ''
    Write-Host '将执行以下操作：'
    if ($hadConfig) {
        if (-not (Test-Path -LiteralPath $BackupConfig)) {
            Die "备份损坏：缺少 $BackupConfig"
        }
        Write-Host "  $n. 删除当前 $ConfigPath"; $n++
        Write-Host '     （安装后对该文件做过的改动会丢失）' -ForegroundColor Yellow
        Write-Host "  $n. 从备份还原 config.toml"; $n++
    } else {
        Write-Host "  $n. 删除 $ConfigPath"; $n++
        Write-Dim  '     （安装前该文件不存在）'
    }
    Write-Host "  $n. 删除 $ModelsPath"; $n++
    Write-Host "  $n. 删除备份目录 $BackupDir"
    Write-Host ''

    $ans = Read-Host '确认还原？输入 y 继续，其它任意键取消'
    if ($ans -notin @('y','Y','yes','YES')) {
        Write-Host '已取消，未修改任何文件。'
        return
    }

    if (Test-Path -LiteralPath $ModelsPath) { Remove-Item -LiteralPath $ModelsPath -Force }

    if ($hadConfig) {
        Copy-Item -LiteralPath $BackupConfig -Destination $ConfigPath -Force
        Write-Ok 'config.toml 已还原'
    } else {
        if (Test-Path -LiteralPath $ConfigPath) { Remove-Item -LiteralPath $ConfigPath -Force }
        Write-Ok 'config.toml 已删除（安装前本就不存在）'
    }
    Write-Ok "$CATALOG_FILENAME 已删除"

    Remove-Item -LiteralPath $BackupDir -Recurse -Force
    Write-Ok '备份目录已清理'
    Write-Host ''
    Write-Ok '还原完成，Codex 配置回到安装前状态。'
    Write-Host ''
    Write-Warn2 '重启 Codex 后生效（config.toml 只在启动时读取）'
}

# ---------------------------------------------------------------- TOML 扫描器

# 跟踪方括号深度与多行字符串状态，用于识别真正的节头
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
    'oss_provider'    = '另一个 provider 选择器，会把请求导到别处'
    'openai_base_url' = '全局 base_url 覆盖，会劫持请求'
}

# 这些顶层键会与模型目录打架（DeepSeek 脚本同款清理）：context window/压缩阈值覆盖
# 会让目录声明失效；service_tier 等陈旧值会被发给 API 导致 400。
$DEL_B = [ordered]@{
    'model_context_window'               = '覆盖 models.json 声明的上下文窗口；过大时自动压缩不触发，跑到一半 API 报错'
    'model_auto_compact_token_limit'      = '覆盖自动压缩时机'
    'model_auto_compact_token_limit_scope'= '覆盖自动压缩时机'
    'base_instructions'                  = '覆盖 models.json 的 base_instructions'
    'model_instructions_file'            = '覆盖 models.json 的 base_instructions'
    'compact_prompt'                     = '覆盖上下文压缩提示词'
    'experimental_compact_prompt_file'   = '覆盖上下文压缩提示词'
    'service_tier'                       = '陈旧值会作为参数发给 API，可能 400'
    'model_verbosity'                    = '陈旧值可能超出模型支持范围'
    'model_reasoning_summary'            = 'models.json 声明 default_reasoning_summary=none，陈旧值会发出 reasoning.summary'
    'plan_mode_reasoning_effort'         = '可能是目录未声明的档位'
    'experimental_use_unified_exec_tool' = '与 models.json 的 shell_type=shell_command 冲突'
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

# ---------------------------------------------------------------- 快速路径：只换模型

# 备份与 models.json 都符合预期时，只改 config.toml 顶层区的 model 键，
# 其余（base_url、上次手术结果等）原样不动。
function Invoke-Proxy429SwitchModel {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "切换默认模型 -> $($script:ModelSlug)"
    Write-Dim  '检测到本脚本的备份；将刷新模型目录并只改 config.toml 的 model 字段。'

    $raw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
    if ($null -eq $raw) { $raw = '' }
    $raw = $raw -replace "`r`n", "`n"
    $raw = $raw.TrimEnd("`n")
    if ($raw -eq '') { $Lines = @() } else { $Lines = $raw -split "`n" }

    $Out = New-Object System.Collections.Generic.List[string]
    $script:Depth = 0
    $script:MlState = ''
    $i = 0
    $replaced = $false
    $inLeading = $true

    while ($i -lt $Lines.Count) {
        $line = $Lines[$i]
        if (-not $inLeading) {
            $Out.Add($line); $i++; continue
        }
        # 多行字符串/数组的续行不做键解析，原样通过
        if ($script:MlState -or $script:Depth -ne 0) {
            Update-ScanState $line
            $Out.Add($line); $i++; continue
        }
        $trimmed = $line.Trim()
        $isHeader = $trimmed.StartsWith('[')
        if ($isHeader) {
            # 顶层区结束还没见到 model -> 在第一个节头前插入
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
            # 吞掉完整赋值（理论上可能是多行字符串）
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
        Die "生成的 models.json 未通过校验，已中止（原文件未改动）。`n$($_.Exception.Message)"
    }

    # 目录是当前目录的超集，先落目录：若随后 config.toml 写失败，之前选的模型仍可解析
    Move-Item -LiteralPath $TmpModels -Destination $ModelsPath -Force
    Move-Item -LiteralPath $TmpConfig -Destination $ConfigPath -Force

    Write-Ok "已刷新 $CATALOG_FILENAME（本文件由脚本重写，手工改动会被覆盖）"
    Write-Ok "config.toml 已更新: model = `"$($script:ModelSlug)`""
    Write-Host ''
    Write-Warn2 '重启 Codex 后生效（config.toml 只在启动时读取）'
    Write-Host ''
    Write-Host '验证方式：'
    Write-Host "  - Codex CLI 启动横幅显示 model: $($script:ModelSlug)"
    Write-Host '  - 代理网页控制台「状态」页出现带 [translate] 前缀的在途流'
    Write-Host ''
    Write-Dim '再次运行本脚本可切换模型（选 1/2/3）或还原默认配置（选 9）。'
}

# ---------------------------------------------------------------- 安装（首次）

function Invoke-Proxy429Install {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "首次安装（目标模型：$($script:ModelSlug)）"

    # ---- 交互输入：代理地址（真实上游 key 在代理配置里，这里不需要）

    if ($BAKED_BASE_URL -ne '') {
        # 网页生成版：地址已按当前配置烤进脚本，跳过询问
        $script:BaseUrl = $BAKED_BASE_URL
    } else {
        Write-Host ''
        Write-Dim "代理 Responses 监听口地址（代理配置里的 responses_listen，模板默认 127.0.0.1:8081）"
        $script:BaseUrl = ''
        try {
            $script:BaseUrl = (Read-Host "base_url [默认 $DEFAULT_BASE_URL]").Trim()
        } catch {
            Die '无法读取输入（非交互环境），退出（未修改任何文件）。'
        }
        if ($script:BaseUrl -eq '') { $script:BaseUrl = $DEFAULT_BASE_URL }
    }
    $script:BaseUrl = $script:BaseUrl.TrimEnd('/')
    if ($script:BaseUrl -notmatch '^https?://') {
        Die "base_url 必须以 http:// 或 https:// 开头，退出（未修改任何文件）。"
    }
    if ($script:BaseUrl -match '"') { Die 'base_url 不能含双引号。' }

    # ---- 备份

    New-Item -ItemType Directory -Path $BackupDir -Force | Out-Null

    $OrigExisted = Test-Path -LiteralPath $ConfigPath
    if ($OrigExisted) {
        Copy-Item -LiteralPath $ConfigPath -Destination $BackupConfig -Force
        Write-Ok "已备份 config.toml -> $BackupConfig"
    } else {
        Write-Warn2 'config.toml 不存在，将新建'
    }

    # ---- 处理

    $Lines = @()
    if ($OrigExisted) {
        $raw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
        if ($null -eq $raw) { $raw = '' }
        # -split 空字符串会产生一个空元素，所以先判断 ''
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

    # 吞掉一条完整赋值（含多行数组/字符串）；$Lines 经调用者作用域可见
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
                $Report.Add("移除旧 [$hdr]（将按新设置重写）")
            }
            elseif ($hdr -eq 'profiles' -or $hdr -like 'profiles.*') {
                $Report.Add("保留 [$hdr]（注意：--profile 激活时会覆盖顶层 model/model_provider）")
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
                    $Report.Add("修正 [$curSection] 的 wire_api: `"chat`" -> `"responses`"（chat 会导致 Codex 无法启动）")
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

        # ---- 顶层区

        $k = Get-TomlKey $line

        if ($k -and $TARGET_KEYS -contains $k) {
            $oldv = Get-TomlValue $trimmed
            $newv = Get-TargetValue $k
            Consume-Block
            $Out.Add("$k = $newv")
            $InsAt = $Out.Count
            [void]$Seen.Add($k)
            if ($oldv -ne $newv) {
                $Report.Add("改写 $k`: $(Format-Val $oldv) -> $newv")
            }
            continue
        }

        if ($k -and $DEL_A.Contains($k)) {
            $oldv = Get-TomlValue $trimmed
            Consume-Block
            $Report.Add("移除 $k = $(Format-Val $oldv)  <- $($DEL_A[$k])")
            continue
        }

        if ($k -and $DEL_B.Contains($k)) {
            $oldv = Get-TomlValue $trimmed
            Consume-Block
            $Report.Add("移除 $k = $(Format-Val $oldv)  <- $($DEL_B[$k])")
            continue
        }

        if ($k -and $WARN_KEYS -contains $k) {
            $Report.Add("保留 $k（注意：可能导致 fallback 元数据或被远端配置覆盖）")
        }

        $Out.Add($line)
        if ($k) { $InsAt = $Out.Count }
        Update-ScanState $line
        $script:idx++
    }

    $missing = @($TARGET_KEYS | Where-Object { -not $Seen.Contains($_) })

    # ---- 组装

    $final = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $Out.Count; $i++) {
        if ($i -eq $InsAt -and $missing.Count -gt 0) {
            foreach ($k in $missing) { $final.Add("$k = $(Get-TargetValue $k)") }
            $missing = @()
            # 紧邻节头时补空行，避免这些键看起来属于那个节
            if ($Out[$i].Trim().StartsWith('[')) { $final.Add('') }
        }
        $final.Add($Out[$i])
    }
    foreach ($k in $missing) { $final.Add("$k = $(Get-TargetValue $k)") }

    $final.Add('')
    $final.Add("[model_providers.$PROVIDER_ID]")
    $final.Add('name = "Proxy429 本地代理"')
    $final.Add("base_url = `"$($script:BaseUrl)`"")
    $final.Add('wire_api = "responses"')
    $final.Add("experimental_bearer_token = `"$BEARER_TOKEN`"")

    $TmpConfig = "$ConfigPath.proxy429-tmp"
    $TmpModels = "$ModelsPath.proxy429-tmp"

    # Codex 读 UTF-8；UTF8Encoding($false) 避免 PS 5.1 写出 BOM
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($TmpConfig, (($final -join "`n") + "`n"), $utf8NoBom)

    Write-ModelsJson $TmpModels

    # ---- 校验

    $ValidatedJson = $false
    try {
        Assert-ModelsJson $TmpModels
        $ValidatedJson = $true
    } catch {
        Remove-Item -LiteralPath $TmpModels, $TmpConfig -Force -ErrorAction SilentlyContinue
        Die "生成的 models.json 未通过 JSON 校验，已中止（原文件未改动）。`n$($_.Exception.Message)"
    }

    # 重复键自检：顶层 TOML 键重复会让 Codex 根本无法启动
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
                    Die "生成的 config.toml 存在重复的顶层键: $kk`n已中止，原文件未改动。备份在: $BackupDir"
                }
                $dupCheck[$kk] = $true
            }
        }
        $script:MlState = $ml2; $script:Depth = $depth2
        Update-ScanState $l
        $ml2 = $script:MlState; $depth2 = $script:Depth
    }

    # ---- 落盘

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
        '--- config.toml 变更 ---'
    ) + $Report
    [System.IO.File]::WriteAllText($Manifest, (($manifestLines -join "`n") + "`n"), $utf8NoBom)

    # ---- 报告

    Write-Ok "已写入 $ModelsPath（模型目录，供 /model 菜单列出）"
    Write-Ok "已更新 $ConfigPath"

    if ($Report.Count -gt 0) {
        Write-Head "对既有配置的改动（共 $($Report.Count) 项）"
        foreach ($r in $Report) { Write-Host "  - $r" }
    }

    Write-Head '写入的配置'
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

    Write-Head '校验'
    if ($ValidatedJson) { Write-Ok 'models.json 是合法 JSON' }
    Write-Ok 'config.toml 无重复顶层键'

    Write-Host ''
    Write-Ok '安装完成。'
    Write-Host ''
    Write-Warn2 '重启 Codex 后生效（config.toml 与模型目录只在启动时读取）'
    Write-Host ''
    Write-Host '验证方式：'
    Write-Host "  - Codex CLI 启动横幅显示 model: $($script:ModelSlug)"
    Write-Host '  - 代理网页控制台「状态」页出现带 [translate] 前缀的在途流'
    Write-Host ''
    Write-Dim '如果 Codex 日志出现 "fallback model metadata" 或 "Unknown model"，'
    Write-Dim '说明模型目录没加载——请重跑本脚本。'
    Write-Host ''
    Write-Dim "真实上游 key 在代理配置的 routes.api 里，Codex 侧无需填写；切换思考档位用 /model。"
    Write-Dim '再次运行本脚本可切换模型（选 1/2/3）或还原默认配置（选 9）。'
}

# ---------------------------------------------------------------- 菜单 + 前置检查

function Invoke-Proxy429Main {
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version Latest

    Write-Head "Codex ↔ Proxy429 一键配置  v$SCRIPT_VERSION"
    Write-Dim  "Codex 目录: $CodexHomeDir"

    # .codex 目录是唯一硬性前提（要改的文件都在里面）；不做客户端启发式检测——
    # 任何客户端首次运行都会建这个目录
    if (-not (Test-Path -LiteralPath $CodexHomeDir)) {
        Die @"
找不到 Codex 配置目录：
  $CodexHomeDir
请先安装并运行一次 Codex CLI / ChatGPT 桌面版 / VS Code Codex 插件
（首次运行会创建该目录），或设置 CODEX_HOME 环境变量后重试：
  - Codex CLI:      npm install -g @openai/codex
  - ChatGPT 桌面版: https://chatgpt.com/download
"@
    }

    if ($BAKED_MODEL -eq '__restore__') {
        # 网页生成版（还原）：跳过菜单直接还原
        Invoke-Proxy429Restore
        return
    }
    if ($BAKED_MODEL -ne '') {
        # 网页生成版：模型已在网页上选好烤进脚本，跳过菜单
        $script:ModelSlug = $BAKED_MODEL
        Write-Ok "目标模型（网页生成版指定）: $($script:ModelSlug)"
    } else {
        Write-Host ''
        Write-Host '选择操作：'
        Write-Host '  1. 使用 gpt-5-codex（命中代理配置模板的 gpt-5* 路由）'
        Write-Host '  2. 使用 claude-fable-5（adaptive thinking 默认开）'
        Write-Host '  3. 自定义模型名（参与代理路由匹配与 thinking 查表）'
        Write-Host '  9. 还原默认 Codex 配置（移除本脚本写入的内容）'
        Write-Host ''

        $choice = ''
        $attempt = 0
        while ($true) {
            try {
                $choice = Read-Host '输入 1 / 2 / 3 / 9'
            } catch {
                Die '无法读取输入（非交互环境），退出（未修改任何文件）。'
            }
            if ($choice -in @('1','2','3','9')) { break }
            $attempt++
            if ($attempt -ge 3) { Die '无效选择，退出（未修改任何文件）。' }
            Write-Warn2 '无效输入，请输入 1、2、3 或 9。'
        }

        switch ($choice) {
            '1' { $script:ModelSlug = 'gpt-5-codex' }
            '2' { $script:ModelSlug = 'claude-fable-5' }
            '3' {
                try {
                    $script:ModelSlug = (Read-Host '输入模型名（与代理 routes 的 pattern 匹配）').Trim()
                } catch {
                    Die '无法读取输入（非交互环境），退出（未修改任何文件）。'
                }
                if ($script:ModelSlug -eq '' -or $script:ModelSlug -match '[\"''\\]') {
                    Die '模型名不能为空，也不能含引号或反斜杠。'
                }
            }
            '9' { Invoke-Proxy429Restore; return }
        }
    }

    if ($script:ModelSlug -eq '' -or $script:ModelSlug -match '[\"''\\]') {
        Die '模型名不合法（空或含引号/反斜杠）。'
    }

    if (Test-Path -LiteralPath $BackupDir) {
        # 有备份 -> 本脚本装过。先核对文件符合预期，再走快速路径；
        # 有任何不符则中止，什么都不动。
        $problems = @()
        if (-not (Test-Path -LiteralPath $ModelsPath)) {
            $problems += "  - 缺少 $ModelsPath"
        }
        if (-not (Test-Path -LiteralPath $ConfigPath)) {
            $problems += "  - 缺少 $ConfigPath"
        } else {
            $configRaw = Get-Content -LiteralPath $ConfigPath -Raw -Encoding UTF8
            if ($configRaw -notmatch "(?m)^\[model_providers\.$PROVIDER_ID\]") {
                $problems += "  - $ConfigPath 缺少 [model_providers.$PROVIDER_ID]"
            }
        }

        if ($problems.Count -gt 0) {
            Die @"
备份目录 $BackupDir 存在，
但当前配置与本脚本预期不符：
$($problems -join "`n")

为避免破坏你的文件和备份，本次运行已中止，未修改任何内容。

建议（二选一）：
  a) 重跑本脚本选 9 先还原默认配置，再重跑选 1/2/3 安装；
  b) 自行检查并删除上面列出的异常文件（若确认备份目录已无用，
     一并删除 $BackupDir），然后重跑本脚本。
"@
        }

        Invoke-Proxy429SwitchModel
        return
    }

    if (Test-Path -LiteralPath $ModelsPath) {
        Die @"
检测到已存在的文件：
  $ModelsPath

该文件不是本脚本写的（找不到本脚本的备份目录 $BackupDir）。
本脚本需要创建这个文件。请先自行删除（或移走）它再重跑：
  Remove-Item '$ModelsPath'
"@
    }

    Invoke-Proxy429Install
}

# ---------------------------------------------------------------- 入口

try {
    Invoke-Proxy429Main
} catch {
    if ("$_" -ne $ABORT_SENTINEL) { throw }
    # Die 已经打印过错误；作为文件运行时保持非零退出码（iex 场景绝不能 exit）
    if ($PSCommandPath) { exit 1 }
}
