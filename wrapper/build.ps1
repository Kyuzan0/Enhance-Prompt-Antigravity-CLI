# Build and deploy script for agy ConPTY wrapper
[CmdletBinding()]
param(
    [switch]$Deploy = $false,

    [switch]$Standalone,

    [switch]$Package,

    [string]$BinDir = $(if ($env:AGY_BIN_DIR) { $env:AGY_BIN_DIR } else { Join-Path $env:LOCALAPPDATA "agy\bin" }),

    [string]$ExpectedSHA256 = ""
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $scriptDir

if ($Standalone -and $Deploy) {
    throw "Use install-windows.ps1 to install the standalone agy-enhance.exe."
}
if ($Package -and -not $Standalone) {
    throw "-Package requires -Standalone."
}

function Get-Sha256 {
    param([string]$LiteralPath)
    (Get-FileHash -LiteralPath $LiteralPath -Algorithm SHA256).Hash.ToUpperInvariant()
}

function Test-PeAmd64 {
    param([string]$LiteralPath)
    $stream = [System.IO.File]::Open($LiteralPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    try {
        $reader = New-Object System.IO.BinaryReader($stream)
        if ($reader.ReadUInt16() -ne 0x5A4D) { return $false }
        $stream.Position = 0x3C
        $peOffset = $reader.ReadInt32()
        if ($peOffset -lt 0 -or $peOffset -gt ($stream.Length - 6)) { return $false }
        $stream.Position = $peOffset
        if ($reader.ReadUInt32() -ne 0x00004550) { return $false }
        return ($reader.ReadUInt16() -eq 0x8664)
    } finally {
        $stream.Dispose()
    }
}

function Assert-TargetUnlocked {
    param([string]$LiteralPath)
    if (-not (Test-Path -LiteralPath $LiteralPath -PathType Leaf)) { return }
    $stream = [System.IO.File]::Open($LiteralPath, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
    $stream.Dispose()
}

function Write-DeployManifest {
    param([string]$LiteralPath, [hashtable]$Data)
    ($Data | ConvertTo-Json -Depth 6) | Set-Content -LiteralPath $LiteralPath -Encoding ASCII
}

function Write-ThirdPartyNotices {
    param([string]$LiteralPath)

    $goRoot = go env GOROOT
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($goRoot)) {
        throw "Unable to locate the Go toolchain license."
    }

    $dependencies = go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' .
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to list Go dependencies for the Windows package."
    }

    $notices = @(
        'Go standard library'
        (Get-Content -LiteralPath (Join-Path $goRoot 'LICENSE') -Raw -Encoding UTF8).TrimEnd()
        'Go patent grant'
        (Get-Content -LiteralPath (Join-Path $goRoot 'PATENTS') -Raw -Encoding UTF8).TrimEnd()
        foreach ($dependency in ($dependencies | Where-Object { $_ } | Sort-Object -Unique)) {
            $name, $directory = $dependency -split ' ', 2
            if ([string]::IsNullOrWhiteSpace($directory)) {
                throw "Unable to locate the license for $name."
            }
            $name
            (Get-Content -LiteralPath (Join-Path $directory 'LICENSE') -Raw -Encoding UTF8).TrimEnd()
        }
    )
    ($notices -join "`r`n`r`n") | Set-Content -LiteralPath $LiteralPath -Encoding UTF8
}

# 1. Compile agy.exe
if (-not (Get-Command "go" -ErrorAction SilentlyContinue)) {
    throw "Go compiler ('go') not found in PATH."
}

$rootPkgPath = Join-Path (Split-Path -Parent $scriptDir) "package.json"
$wrapVersion = "2.0.0"
if (Test-Path -LiteralPath $rootPkgPath -PathType Leaf) {
    $rootPkg = Get-Content -LiteralPath $rootPkgPath -Raw -Encoding UTF8 | ConvertFrom-Json
    if ($rootPkg.version) { $wrapVersion = [string]$rootPkg.version }
}

$outputName = if ($Standalone) { "agy-enhance.exe" } else { "agy.exe" }
Write-Host "Compiling $outputName (v$wrapVersion)..." -ForegroundColor DarkGray
$buildOut = Join-Path $scriptDir "agy-built.exe"
if (Test-Path -LiteralPath $buildOut) {
    Remove-Item -LiteralPath $buildOut -Force
}
go build -trimpath -ldflags="-s -w -X main.WrapperVersion=$wrapVersion" -o $buildOut .
if ($LASTEXITCODE -ne 0) {
    Remove-Item -LiteralPath $buildOut -Force -ErrorAction SilentlyContinue
    throw "go build failed with exit code $LASTEXITCODE"
}

$candidate = Join-Path $scriptDir $outputName
Move-Item -LiteralPath $buildOut -Destination $candidate -Force
if (-not $Standalone) {
    Copy-Item -LiteralPath $candidate -Destination (Join-Path $scriptDir "agy-wrap.exe") -Force
}

if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
    throw "Build failed: candidate not found at $candidate"
}
if (-not (Test-PeAmd64 -LiteralPath $candidate)) {
    throw "Build failed: candidate is not an AMD64 PE image"
}
$candidateHash = Get-Sha256 -LiteralPath $candidate
if ($ExpectedSHA256 -and ($candidateHash -ne $ExpectedSHA256.ToUpperInvariant())) {
    throw "Candidate hash mismatch: expected $($ExpectedSHA256.ToUpperInvariant()), got $candidateHash"
}
$item = Get-Item -LiteralPath $candidate
$sizeMb = [math]::Round($item.Length / 1MB, 2)
Write-Host "[BUILD OK] $outputName ($sizeMb MB) SHA-256 $candidateHash" -ForegroundColor Green

if ($Standalone) {
    if ($Package) {
        $rootDir = Split-Path -Parent $scriptDir
        $archive = Join-Path $rootDir "agy-enhance-windows-amd64-$wrapVersion.zip"
        $noticeDir = Join-Path ([System.IO.Path]::GetTempPath()) ("agy-notices-" + [guid]::NewGuid().ToString("N"))
        [System.IO.Directory]::CreateDirectory($noticeDir) | Out-Null
        try {
            $noticePath = Join-Path $noticeDir "THIRD-PARTY-NOTICES.txt"
            Write-ThirdPartyNotices -LiteralPath $noticePath
            $releaseFiles = @(
                $candidate
                (Join-Path $scriptDir "install-windows.ps1")
                (Join-Path $rootDir "README.md")
                (Join-Path $rootDir "LICENSE")
                $noticePath
            )
            Compress-Archive -LiteralPath $releaseFiles -DestinationPath $archive -Force
        } finally {
            Remove-Item -LiteralPath $noticeDir -Recurse -Force
        }
        Write-Host "Windows package: $archive" -ForegroundColor Green
    }
    Write-Host "Install with install-windows.ps1." -ForegroundColor Cyan
    exit 0
}

if (-not $Deploy) {
    Write-Host "Build only. Pass -Deploy to install; no active binary was touched." -ForegroundColor Yellow
    exit 0
}

# 2. Deploy preflight: validate everything before any mutation.
if (-not (Test-Path -LiteralPath $BinDir -PathType Container)) {
    throw "Destination directory not found: $BinDir"
}

$nativePath = Join-Path $BinDir "agy-real.exe"
$installedAgyPath = Join-Path $BinDir "agy.exe"
$minNativeBytes = 50MB

# Auto-migrate if install.ps1 overwrote agy.exe with a native >50MB binary.
if (Test-Path -LiteralPath $installedAgyPath -PathType Leaf) {
    $installedSize = (Get-Item -LiteralPath $installedAgyPath).Length
    if ($installedSize -ge $minNativeBytes) {
        Write-Host "Detected native agy.exe ($([math]::Round($installedSize / 1MB, 2)) MB); promoting to agy-real.exe..." -ForegroundColor Cyan
        Assert-TargetUnlocked -LiteralPath $installedAgyPath
        Assert-TargetUnlocked -LiteralPath $nativePath
        Move-Item -LiteralPath $installedAgyPath -Destination $nativePath -Force
    }
}

# Fallback: recover native binary from Antigravity IDE's managed CLI if agy-real.exe is missing.
if (-not (Test-Path -LiteralPath $nativePath -PathType Leaf)) {
    $ideManagedAgy = Join-Path $env:USERPROFILE ".gemini\bin\agy.exe"
    if ((Test-Path -LiteralPath $ideManagedAgy -PathType Leaf) -and ((Get-Item -LiteralPath $ideManagedAgy).Length -ge $minNativeBytes)) {
        Write-Host "Recovering native agy-real.exe from $ideManagedAgy..." -ForegroundColor Cyan
        Copy-Item -LiteralPath $ideManagedAgy -Destination $nativePath -Force
    }
}

if (-not (Test-Path -LiteralPath $nativePath -PathType Leaf)) {
    throw "Native agy-real.exe is missing in $BinDir. Refusing deploy; the wrapper is never renamed as the native binary."
}
$nativeHashBefore = Get-Sha256 -LiteralPath $nativePath

$targetNames = @("agy.exe", "agy-wrap.exe")
foreach ($name in $targetNames) {
    Assert-TargetUnlocked -LiteralPath (Join-Path $BinDir $name)
}

$stamp = Get-Date -Format "yyyyMMdd-HHmmss-fff"
$backupDir = Join-Path $BinDir "backup_build_deploy_$stamp"
New-Item -ItemType Directory -Path $backupDir | Out-Null

$preHashes = @{}
foreach ($name in $targetNames) {
    $target = Join-Path $BinDir $name
    $preHashes[$name] = $null
    if (Test-Path -LiteralPath $target -PathType Leaf) {
        $backup = Join-Path $backupDir $name
        Copy-Item -LiteralPath $target -Destination $backup -Force
        $beforeHash = Get-Sha256 -LiteralPath $target
        $backupHash = Get-Sha256 -LiteralPath $backup
        if ($beforeHash -ne $backupHash) {
            throw "Backup verification failed for $name"
        }
        $preHashes[$name] = $beforeHash
    }
}

$oldFilesBefore = @(Get-ChildItem -LiteralPath $BinDir -Filter "*.old" -ErrorAction SilentlyContinue | ForEach-Object { $_.Name })
$manifestPath = Join-Path $backupDir "deploy-manifest.json"
$manifest = @{
    status = "prepared"
    timestampUtc = (Get-Date).ToUniversalTime().ToString("o")
    binDir = $BinDir
    candidate = $candidate
    candidateSha256 = $candidateHash
    native = $nativePath
    nativeSha256Before = $nativeHashBefore
    backups = $preHashes
    preExistingOldFiles = $oldFilesBefore
}

$stagedFiles = @{}
$installed = New-Object System.Collections.ArrayList

try {
    # Stage on the destination volume so replacement is a same-volume rename.
    foreach ($name in $targetNames) {
        $stage = Join-Path $BinDir (".$name.incoming.$stamp")
        Copy-Item -LiteralPath $candidate -Destination $stage -Force
        $stageHash = Get-Sha256 -LiteralPath $stage
        if ($stageHash -ne $candidateHash) {
            throw "Staged copy verification failed for $name"
        }
        $stagedFiles[$name] = $stage
    }

    # Replace each target atomically; roll back every completed replacement on failure.
    foreach ($name in $targetNames) {
        $target = Join-Path $BinDir $name
        $stage = $stagedFiles[$name]
        if (Test-Path -LiteralPath $target -PathType Leaf) {
            $replaceBackup = Join-Path $backupDir ("replace-$name-$stamp.bak")
            [System.IO.File]::Replace($stage, $target, $replaceBackup, $true)
        } else {
            [System.IO.File]::Move($stage, $target)
        }
        [void]$installed.Add($name)
        if ((Get-Sha256 -LiteralPath $target) -ne $candidateHash) {
            throw "Installed hash mismatch for $name"
        }
    }

    $nativeHashAfter = Get-Sha256 -LiteralPath $nativePath
    if ($nativeHashAfter -ne $nativeHashBefore) {
        throw "Native agy-real.exe changed during deploy"
    }

    $installedHashes = @{}
    foreach ($name in $targetNames) {
        $installedHashes[$name] = Get-Sha256 -LiteralPath (Join-Path $BinDir $name)
    }
    $oldFilesAfter = @(Get-ChildItem -LiteralPath $BinDir -Filter "*.old" -ErrorAction SilentlyContinue | ForEach-Object { $_.Name })
    foreach ($oldName in $oldFilesBefore) {
        if ($oldFilesAfter -notcontains $oldName) {
            throw "Pre-existing old file disappeared: $oldName"
        }
    }

    $manifest.status = "success"
    $manifest.installed = $installedHashes
    $manifest.nativeSha256After = $nativeHashAfter
    $manifest.oldFilesAfter = $oldFilesAfter
    Write-DeployManifest -LiteralPath $manifestPath -Data $manifest
    Write-Host "[DEPLOY OK] $BinDir" -ForegroundColor Green
    Write-Host "Installed SHA-256: $candidateHash"
    Write-Host "Native agy-real.exe unchanged: $nativeHashAfter"
    Write-Host "Backups and manifest: $backupDir"
    exit 0
} catch {
    $failure = $_.Exception.Message
    $rollbackErrors = @()
    foreach ($name in $installed) {
        $target = Join-Path $BinDir $name
        $backup = Join-Path $backupDir $name
        try {
            if ($preHashes[$name]) {
                Copy-Item -LiteralPath $backup -Destination $target -Force
                if ((Get-Sha256 -LiteralPath $target) -ne $preHashes[$name]) {
                    throw "rollback hash mismatch"
                }
            } else {
                Remove-Item -LiteralPath $target -Force
            }
        } catch {
            $rollbackErrors += "$name`: $($_.Exception.Message)"
        }
    }

    foreach ($stage in $stagedFiles.Values) {
        Remove-Item -LiteralPath $stage -Force -ErrorAction SilentlyContinue
    }

    $nativeHashAfter = if (Test-Path -LiteralPath $nativePath -PathType Leaf) { Get-Sha256 -LiteralPath $nativePath } else { "MISSING" }
    $manifest.status = "failed"
    $manifest.failure = $failure
    $manifest.rollbackErrors = $rollbackErrors
    $manifest.nativeSha256After = $nativeHashAfter
    Write-DeployManifest -LiteralPath $manifestPath -Data $manifest

    if ($rollbackErrors.Count -gt 0) {
        throw "Deploy failed: $failure. Rollback incomplete: $($rollbackErrors -join '; '). Manual recovery required from $backupDir"
    }
    throw "Deploy failed: $failure. Rollback verified against pre-deploy hashes. Backups retained at $backupDir"
}
