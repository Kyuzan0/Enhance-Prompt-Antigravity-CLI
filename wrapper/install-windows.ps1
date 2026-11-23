# Install or update the standalone Windows launcher for the current user.
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$BinaryPath = (Join-Path $PSScriptRoot "agy-enhance.exe"),
    [string]$InstallDir = (Join-Path $env:LOCALAPPDATA "agy-prompt-enhancer\bin"),
    [switch]$NoPath,
    [switch]$Uninstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$targetDir = [System.IO.Path]::GetFullPath($InstallDir).TrimEnd('\', '/')
$target = Join-Path $targetDir "agy-enhance.exe"
$pathMarker = Join-Path $targetDir ".path-managed"

function Test-PathEntry([string]$Entry) {
    $expanded = [Environment]::ExpandEnvironmentVariables($Entry.Trim().Trim('"'))
    return $expanded.TrimEnd('\', '/') -ieq $targetDir
}

if ($Uninstall) {
    if ($PSCmdlet.ShouldProcess($target, "Uninstall agy-enhance")) {
        if (Test-Path -LiteralPath $target -PathType Leaf) {
            Remove-Item -LiteralPath $target -Force
        }
        if (Test-Path -LiteralPath $pathMarker -PathType Leaf) {
            $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
            $remaining = @($userPath -split ';' | Where-Object { $_ -and -not (Test-PathEntry $_) })
            [Environment]::SetEnvironmentVariable("Path", ($remaining -join ';'), "User")
            $env:Path = (@($env:Path -split ';' | Where-Object { $_ -and -not (Test-PathEntry $_) }) -join ';')
            Remove-Item -LiteralPath $pathMarker -Force
        }
        Write-Host "agy-enhance removed. Settings are in %APPDATA%\agy-prompt-enhancer."
    }
    return
}

if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) {
    throw "agy-enhance.exe not found at $BinaryPath. Extract the Windows package or run build.ps1 -Standalone."
}
$source = (Get-Item -LiteralPath $BinaryPath).FullName
if (-not $PSCmdlet.ShouldProcess($target, "Install agy-enhance for the current user")) {
    return
}

[System.IO.Directory]::CreateDirectory($targetDir) | Out-Null
if ($source -ine $target) {
    $staging = Join-Path $targetDir (".incoming-" + [guid]::NewGuid().ToString("N"))
    try {
        Copy-Item -LiteralPath $source -Destination $staging
        $expected = (Get-FileHash -LiteralPath $source -Algorithm SHA256).Hash
        if ((Get-FileHash -LiteralPath $staging -Algorithm SHA256).Hash -ne $expected) {
            throw "The staged launcher failed SHA-256 verification."
        }
        if (Test-Path -LiteralPath $target -PathType Leaf) {
            [System.IO.File]::Replace($staging, $target, [NullString]::Value)
        } else {
            [System.IO.File]::Move($staging, $target)
        }
    } finally {
        if (Test-Path -LiteralPath $staging -PathType Leaf) {
            Remove-Item -LiteralPath $staging -Force
        }
    }
}

if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not @($userPath -split ';' | Where-Object { $_ -and (Test-PathEntry $_) }).Count) {
        $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $targetDir } else { $userPath.TrimEnd(';') + ';' + $targetDir }
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        [System.IO.File]::WriteAllText($pathMarker, $targetDir)
    }
    if (-not @($env:Path -split ';' | Where-Object { $_ -and (Test-PathEntry $_) }).Count) {
        $env:Path = $env:Path.TrimEnd(';') + ';' + $targetDir
    }
}

Write-Host "Installed: $target"
Write-Host "Run agy-enhance in PowerShell. Settings: agy-enhance settings menu."
