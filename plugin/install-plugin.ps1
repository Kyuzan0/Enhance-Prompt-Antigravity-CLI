<#
.SYNOPSIS
    Installs, updates, or uninstalls the prompt-enhancer CLI plugin to Antigravity's global plugin directory.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter()]
    [string]$ConfigRoot = $(if ($env:AGY_CONFIG_DIR) { $env:AGY_CONFIG_DIR } else { Join-Path $env:USERPROFILE ".gemini\config" }),

    [Parameter()]
    [switch]$Uninstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$srcDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$repoRoot = Split-Path -Parent $srcDir
$pkgMeta = Get-Content -LiteralPath (Join-Path $srcDir "plugin.json") -Raw -Encoding UTF8 | ConvertFrom-Json
$pluginVersion = if ($pkgMeta.version) { [string]$pkgMeta.version } else { "unknown" }
$dstDir = Join-Path (Join-Path $ConfigRoot "plugins") "prompt-enhancer"

if ($Uninstall) {
    if ((Test-Path -LiteralPath $dstDir) -and $PSCmdlet.ShouldProcess($dstDir, "Remove prompt-enhancer CLI plugin")) {
        Remove-Item -LiteralPath $dstDir -Recurse -Force
        Write-Host "[PLUGIN UNINSTALLED] $dstDir" -ForegroundColor Green
    } else {
        Write-Host "Plugin directory not found at $dstDir" -ForegroundColor DarkGray
    }
    return
}

if (-not (Test-Path -LiteralPath $dstDir)) {
    if ($PSCmdlet.ShouldProcess($dstDir, "Create plugin directory")) {
        New-Item -ItemType Directory -Path $dstDir -Force | Out-Null
    }
}

foreach ($rel in @("plugin.json", "hooks.json", "config.json", "hook.mjs")) {
    $src = Join-Path $srcDir $rel
    $dst = Join-Path $dstDir $rel
    if ((Test-Path -LiteralPath $src) -and $PSCmdlet.ShouldProcess($dst, "Install $rel")) {
        Copy-Item -LiteralPath $src -Destination $dst -Force
    }
}

$skillSrc = Join-Path $srcDir "skills\enhance\SKILL.md"
$skillDstDir = Join-Path $dstDir "skills\enhance"
if (Test-Path -LiteralPath $skillSrc) {
    if (-not (Test-Path -LiteralPath $skillDstDir) -and $PSCmdlet.ShouldProcess($skillDstDir, "Create skill directory")) {
        New-Item -ItemType Directory -Path $skillDstDir -Force | Out-Null
    }
    if ($PSCmdlet.ShouldProcess((Join-Path $skillDstDir "SKILL.md"), "Install enhance SKILL.md")) {
        Copy-Item -LiteralPath $skillSrc -Destination (Join-Path $skillDstDir "SKILL.md") -Force
    }
}

foreach ($rootDoc in @("LICENSE", "CHANGELOG.md")) {
    $srcDoc = Join-Path $repoRoot $rootDoc
    $dstDoc = Join-Path $dstDir $rootDoc
    if ((Test-Path -LiteralPath $srcDoc) -and $PSCmdlet.ShouldProcess($dstDoc, "Install $rootDoc")) {
        Copy-Item -LiteralPath $srcDoc -Destination $dstDoc -Force
    }
}

Write-Host "[PLUGIN INSTALLED] $dstDir (v$pluginVersion)" -ForegroundColor Green
