<#
.SYNOPSIS
    Production-Grade Lifecycle Manager for Antigravity Prompt Enhancer Daemon.
.DESCRIPTION
    Provides idempotent, enterprise-grade process management, artifact cleanup,
    background detachment, status telemetry, and log monitoring.
.PARAMETER Action
    The lifecycle operation to perform: Start, Stop, Restart, Status, Clean, or Logs.
.PARAMETER RepoRoot
    The repository / project root directory path.
.EXAMPLE
    .\manage-daemon.ps1 -Action Restart
    .\manage-daemon.ps1 -Action Status
    .\manage-daemon.ps1 -Action Clean
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Position = 0)]
    [ValidateSet('Start', 'Stop', 'Restart', 'Status', 'Clean', 'Logs')]
    [string]$Action = 'Restart',

    [Parameter()]
    [string]$RepoRoot = '',

    [Parameter()]
    [string]$DaemonScript = '',

    [Parameter()]
    [string]$ArtifactDir = '',

    [Parameter()]
    [string]$LogDir = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$CurrentProcessId = $PID

$scriptParent = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $RepoRoot) {
    $RepoRoot = Split-Path -Parent $scriptParent
}
if (-not $DaemonScript) { $DaemonScript = Join-Path $scriptParent 'daemon.ps1' }
if (-not $ArtifactDir) {
    $ArtifactDir = if ($env:AGY_ARTIFACT_DIR) { $env:AGY_ARTIFACT_DIR } else { Join-Path $RepoRoot '.agents' }
}
if (-not $LogDir) {
    $LogDir = if ($env:AGY_ENHANCER_LOG_DIR) { $env:AGY_ENHANCER_LOG_DIR } else { $scriptParent }
}

# Ensure Log and PID Paths
$OutLog = Join-Path $LogDir 'daemon.out.log'
$ErrLog = Join-Path $LogDir 'daemon.err.log'
$DaemonLog = Join-Path $LogDir 'daemon.log'
$PidFile = Join-Path $LogDir 'daemon.pid'

function Write-TimestampedLog([string]$Message, [string]$Level = 'INFO') {
    $timestamp = (Get-Date).ToString('yyyy-MM-dd HH:mm:ss.fff')
    $color = switch ($Level) {
        'SUCCESS' { 'Green' }
        'WARN'    { 'Yellow' }
        'ERROR'   { 'Red' }
        'DEBUG'   { 'Gray' }
        Default   { 'Cyan' }
    }
    Write-Host "[$timestamp] [$Level] $Message" -ForegroundColor $color
}

function Get-DaemonProcesses {
    <#
    .SYNOPSIS
        Queries active daemon instances using CIM/WMI and PID file tracking.
    #>
    [object[]]$results = @()

    # 1. Check PID file if present
    if (Test-Path $PidFile) {
        try {
            $savedPid = [int]((Get-Content $PidFile -Raw).Trim())
            $proc = Get-Process -Id $savedPid -ErrorAction SilentlyContinue
            if ($proc -and -not $proc.HasExited) {
                return @($proc)
            } else {
                Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
            }
        } catch {}
    }

    # 2. Query via CIM/WMI filtering strictly on CommandLine
    try {
        $processes = Get-CimInstance Win32_Process -Filter "CommandLine LIKE '%daemon.ps1%'" -ErrorAction Stop
        if ($processes) {
            $results = @($processes | Where-Object { $_.ProcessId -ne $CurrentProcessId })
        }
    } catch {
        try {
            $wmi = Get-WmiObject Win32_Process -Filter "CommandLine LIKE '%daemon.ps1%'" -ErrorAction Stop
            if ($wmi) {
                $results = @($wmi | Where-Object { $_.ProcessId -ne $CurrentProcessId })
            }
        } catch {}
    }
    return @($results)
}

function Stop-DaemonInstance([int]$TimeoutSeconds = 5) {
    Write-TimestampedLog "Phase 1: Terminating existing daemon instances..." 'INFO'
    [object[]]$targets = @(Get-DaemonProcesses)

    if (-not $targets -or ($targets | Measure-Object).Count -eq 0) {
        Write-TimestampedLog "No active daemon instances found." 'DEBUG'
        return
    }

    foreach ($proc in $targets) {
        $procId = if ($proc.PSObject.Properties.Match('ProcessId').Count -gt 0) {
            $proc.ProcessId
        } elseif ($proc.PSObject.Properties.Match('Id').Count -gt 0) {
            $proc.Id
        } else {
            continue
        }
        Write-TimestampedLog "Found running daemon (PID: $procId). Initiating termination..." 'WARN'
        if (-not $PSCmdlet.ShouldProcess("PID $procId", 'Stop prompt-enhancer daemon process')) { continue }
        try {
            Stop-Process -Id $procId -Force -ErrorAction Stop
        } catch {
            Write-TimestampedLog "Failed to gracefully stop PID $procId : $($_.Exception.Message)" 'ERROR'
        }
    }

    # Polling wait for lock release
    $stopwatch = [System.Diagnostics.Stopwatch]::StartNew()
    while ($stopwatch.Elapsed.TotalSeconds -lt $TimeoutSeconds) {
        [object[]]$remaining = @(Get-DaemonProcesses)
        if (-not $remaining -or ($remaining | Measure-Object).Count -eq 0) {
            Write-TimestampedLog "All daemon processes successfully terminated." 'SUCCESS'
            return
        }
        Start-Sleep -Milliseconds 250
    }

    # Force kill any lingering instances
    [object[]]$lingering = @(Get-DaemonProcesses)
    if ($lingering -and ($lingering | Measure-Object).Count -gt 0) {
        foreach ($proc in $lingering) {
            $targetProcId = if ($proc.PSObject.Properties['ProcessId']) { $proc.ProcessId } else { $proc.Id }
            Write-TimestampedLog "Force-killing stubborn PID $targetProcId..." 'WARN'
            try {
                Stop-Process -Id $targetProcId -Force -ErrorAction SilentlyContinue
            } catch {}
        }
    }
}

function Remove-ArtifactDirectorySafe {
    Write-TimestampedLog "Phase 2: Cleaning prompt-enhancer daemon artifacts (preserving non-enhancer .agents files)..." 'INFO'
    $targets = @(
        $PidFile,
        $DaemonLog,
        $OutLog,
        $ErrLog
    )

    if (Test-Path -LiteralPath $ArtifactDir) {
        $enhancerItems = Get-ChildItem -LiteralPath $ArtifactDir -Recurse -Force -ErrorAction SilentlyContinue |
            Where-Object { $_.Name -like '*prompt-enhancer*' }
        foreach ($item in $enhancerItems) {
            $targets += $item.FullName
        }
    }

    $removed = 0
    foreach ($path in ($targets | Select-Object -Unique)) {
        if (-not (Test-Path -LiteralPath $path)) { continue }
        if (-not $PSCmdlet.ShouldProcess($path, 'Remove prompt-enhancer daemon artifact')) { continue }
        try {
            $item = Get-Item -LiteralPath $path -Force -ErrorAction Stop
            $item.Attributes = [System.IO.FileAttributes]::Normal
            Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction Stop
            $removed++
        } catch {
            Write-TimestampedLog "Failed to remove $path : $($_.Exception.Message)" 'WARN'
        }
    }
    Write-TimestampedLog "Artifact cleanup complete ($removed prompt-enhancer item(s) removed)." 'SUCCESS'
}

function Start-DaemonDetached {
    Write-TimestampedLog "Phase 3: Launching daemon in detached background state..." 'INFO'

    if (-not (Test-Path -LiteralPath $DaemonScript)) {
        throw "Target daemon script does not exist at: $DaemonScript"
    }

    if (-not $PSCmdlet.ShouldProcess($DaemonScript, 'Start detached prompt-enhancer daemon')) {
        return
    }

    $workDir = Split-Path -Parent $DaemonScript

    # Initialize log files if absent
    if (-not (Test-Path $OutLog)) { New-Item -Path $OutLog -ItemType File -Force | Out-Null }
    if (-not (Test-Path $ErrLog)) { New-Item -Path $ErrLog -ItemType File -Force | Out-Null }

    # Launch process using .NET ProcessStartInfo for robust background detachment
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = 'powershell.exe'
    $psi.Arguments = "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$DaemonScript`""
    $psi.WorkingDirectory = $workDir
    $psi.WindowStyle = [System.Diagnostics.ProcessWindowStyle]::Hidden
    $psi.CreateNoWindow = $true
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true

    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $psi

    # Hook asynchronous stream redirection
    $outWriter = [System.IO.StreamWriter]::new($OutLog, $true, [System.Text.Encoding]::UTF8)
    $errWriter = [System.IO.StreamWriter]::new($ErrLog, $true, [System.Text.Encoding]::UTF8)

    $process.add_OutputDataReceived({
        param($s, $e)
        if ($e.Data) {
            $script:outWriter.WriteLine("[$((Get-Date).ToString('yyyy-MM-dd HH:mm:ss.fff'))] $($e.Data)")
            $script:outWriter.Flush()
        }
    })

    $process.add_ErrorDataReceived({
        param($s, $e)
        if ($e.Data) {
            $script:errWriter.WriteLine("[$((Get-Date).ToString('yyyy-MM-dd HH:mm:ss.fff'))] [STDERR] $($e.Data)")
            $script:errWriter.Flush()
        }
    })

    $started = $process.Start()
    if (-not $started) {
        throw "ProcessStartInfo.Start() returned false."
    }

    $process.BeginOutputReadLine()
    $process.BeginErrorReadLine()

    $spawnedPid = $process.Id
    Write-TimestampedLog "Daemon spawned with Process ID: $spawnedPid" 'INFO'

    # Post-spawn health verification (wait 1.2s to detect early startup crash)
    Start-Sleep -Milliseconds 1200
    if ($process.HasExited) {
        $exitCode = $process.ExitCode
        $errContent = Get-Content -Path $ErrLog -Raw -ErrorAction SilentlyContinue
        throw "Daemon process PID $spawnedPid terminated immediately with exit code $exitCode. Error Details: $errContent"
    }

    Write-TimestampedLog "Daemon post-spawn health check passed. Running stably." 'SUCCESS'
    return $spawnedPid
}

function Show-DaemonStatus {
    Write-Host "`n====================== DAEMON STATUS ======================" -ForegroundColor Cyan
    [object[]]$processes = @(Get-DaemonProcesses)
    $count = ($processes | Measure-Object).Count

    if ($count -eq 0) {
        Write-Host "Status        : STOPPED (No active process detected)" -ForegroundColor Red
        Write-Host "Daemon Script : $DaemonScript" -ForegroundColor Gray
        Write-Host "Log Output    : $DaemonLog" -ForegroundColor Gray
        return
    }

    Write-Host "Status        : RUNNING" -ForegroundColor Green
    Write-Host "Active Instances: $count`n" -ForegroundColor Green

    $data = foreach ($p in $processes) {
        $wsMb = if ($p.PSObject.Properties.Match('WorkingSetSize').Count -gt 0) {
            [math]::Round($p.WorkingSetSize / 1MB, 2)
        } elseif ($p.PSObject.Properties.Match('WorkingSet64').Count -gt 0) {
            [math]::Round($p.WorkingSet64 / 1MB, 2)
        } else {
            0
        }

        $targetProcessId = if ($p.PSObject.Properties.Match('ProcessId').Count -gt 0) {
            $p.ProcessId
        } elseif ($p.PSObject.Properties.Match('Id').Count -gt 0) {
            $p.Id
        } else {
            'N/A'
        }

        $parentPid = if ($p.PSObject.Properties.Match('ParentProcessId').Count -gt 0) {
            $p.ParentProcessId
        } else {
            'N/A'
        }

        $created = if ($p.PSObject.Properties.Match('CreationDate').Count -gt 0) {
            $p.CreationDate
        } elseif ($p.PSObject.Properties.Match('StartTime').Count -gt 0) {
            $p.StartTime
        } else {
            'N/A'
        }

        $cmdLine = if ($p.PSObject.Properties.Match('CommandLine').Count -gt 0) {
            $p.CommandLine
        } else {
            'powershell.exe (Hidden)'
        }

        [PSCustomObject]@{
            ProcessId   = $targetProcessId
            ParentPID   = $parentPid
            WorkingSet  = "$wsMb MB"
            Started     = $created
            CommandLine = $cmdLine
        }
    }

    $data | Format-Table -AutoSize
    Write-Host "Stdout Log: $OutLog" -ForegroundColor Gray
    Write-Host "Stderr Log: $ErrLog" -ForegroundColor Gray
    Write-Host "Event Log : $DaemonLog" -ForegroundColor Gray
}

function Monitor-DaemonLogs {
    $targetLog = if (Test-Path $DaemonLog) { $DaemonLog } else { $OutLog }
    Write-TimestampedLog "Streaming live logs from $targetLog (Press Ctrl+C to exit)..." 'INFO'
    if (-not (Test-Path $targetLog)) {
        Write-TimestampedLog "Log file does not exist yet." 'WARN'
        return
    }
    Get-Content -Path $targetLog -Tail 25 -Wait
}

# Lifecycle Execution Dispatcher
switch ($Action) {
    'Start' {
        Write-TimestampedLog "[DEPRECATED] Global-hotkey daemon is superseded by wrapper/ (CLI) and the separate Antigravity IDE extension." 'WARN'
        [object[]]$existing = @(Get-DaemonProcesses)
        if ($existing -and ($existing | Measure-Object).Count -gt 0) {
            $existingPid = if ($existing[0].PSObject.Properties['ProcessId']) { $existing[0].ProcessId } else { $existing[0].Id }
            Write-TimestampedLog "Daemon is already running (PID: $existingPid)." 'WARN'
            Show-DaemonStatus
            return
        }
        Start-DaemonDetached
        Show-DaemonStatus
    }
    'Stop' {
        Stop-DaemonInstance
        Show-DaemonStatus
    }
    'Clean' {
        Stop-DaemonInstance
        Remove-ArtifactDirectorySafe
        Write-TimestampedLog "Cleanup phase complete." 'SUCCESS'
    }
    'Restart' {
        Stop-DaemonInstance
        Remove-ArtifactDirectorySafe
        Start-DaemonDetached
        Show-DaemonStatus
    }
    'Status' {
        Show-DaemonStatus
    }
    'Logs' {
        Monitor-DaemonLogs
    }
}
