# daemon.ps1 - Background Hotkey Listener for Antigravity CLI Prompt Enhancer
# Shortcut: Ctrl + E (Toggle: Enhance / Cancel)
# Catatan: Pembatalan 100% menggunakan Ctrl + E lagi (BUKAN Ctrl + C)

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path

# Resilient directory selection for logs and PID
$DataDir = $ScriptDir
try {
    $testFile = Join-Path $ScriptDir ".perm_test_$PID"
    [System.IO.File]::WriteAllText($testFile, "ok")
    Remove-Item $testFile -Force -ErrorAction SilentlyContinue
} catch {
    $DataDir = Join-Path $env:TEMP "prompt-enhancer"
    if (-not (Test-Path $DataDir)) {
        New-Item -ItemType Directory -Path $DataDir -Force -ErrorAction SilentlyContinue | Out-Null
    }
}

$LogFile = Join-Path $DataDir "daemon.log"
$PidFile = Join-Path $DataDir "daemon.pid"
$EnhanceScript = Join-Path $ScriptDir "enhance.mjs"

# Write PID file for reliable tracking
try {
    Set-Content -Path $PidFile -Value $PID -Force -ErrorAction SilentlyContinue
} catch {}

function Log-Msg([string]$msg) {
    $timestamp = (Get-Date).ToString("yyyy-MM-dd HH:mm:ss")
    $line = "[$timestamp] $msg"
    Write-Host $line
    Add-Content -Path $LogFile -Value $line -ErrorAction SilentlyContinue
}

Log-Msg "=== Antigravity Prompt Enhancer v2.1 Starting ==="

Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing

# Node binary resolution (env override -> PATH -> common install paths)
$NodeExe = "node"
if ($env:AGY_NODE_BIN -and (Test-Path -LiteralPath $env:AGY_NODE_BIN -ErrorAction SilentlyContinue)) {
    $NodeExe = $env:AGY_NODE_BIN
} elseif ($cmdNode = Get-Command "node" -ErrorAction SilentlyContinue) {
    $NodeExe = $cmdNode.Source
} else {
    $PossiblePaths = @(
        $(if ($env:NVM_SYMLINK) { Join-Path $env:NVM_SYMLINK "node.exe" }),
        "$env:ProgramFiles\nodejs\node.exe",
        "$env:LOCALAPPDATA\Programs\node\node.exe"
    ) | Where-Object { $_ }
    foreach ($p in $PossiblePaths) {
        if (Test-Path -LiteralPath $p -ErrorAction SilentlyContinue) {
            $NodeExe = $p
            break
        }
    }
}
Log-Msg "Using node binary: $NodeExe"

# Win32 API Definitions with Native Form Handle
Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
using System.Windows.Forms;

public class HotkeyForm : Form {
    [DllImport("user32.dll")]
    public static extern bool RegisterHotKey(IntPtr hWnd, int id, int fsModifiers, int vk);

    [DllImport("user32.dll")]
    public static extern bool UnregisterHotKey(IntPtr hWnd, int id);

    public const int WM_HOTKEY = 0x0312;
    public event Action OnHotkey;

    public HotkeyForm() {
        this.WindowState = FormWindowState.Minimized;
        this.ShowInTaskbar = false;
        this.FormBorderStyle = FormBorderStyle.None;
        this.Size = new System.Drawing.Size(0, 0);
    }

    protected override void OnLoad(EventArgs e) {
        this.Visible = false;
        RegisterHotKey(this.Handle, 9001, 0x0002, 0x45); // Ctrl + E
        base.OnLoad(e);
    }

    protected override void WndProc(ref Message m) {
        if (m.Msg == WM_HOTKEY) {
            if (OnHotkey != null) {
                OnHotkey();
            }
        }
        base.WndProc(ref m);
    }

    protected override void OnFormClosing(FormClosingEventArgs e) {
        UnregisterHotKey(this.Handle, 9001);
        base.OnFormClosing(e);
    }
}
"@ -ReferencedAssemblies System.Windows.Forms, System.Drawing

# Global State
$script:CurrentJob = $null
$script:CurrentTempFile = $null
$script:LastOriginalDraft = $null

# System Tray Notification Helper
$trayIcon = New-Object System.Windows.Forms.NotifyIcon
$trayIcon.Icon = [System.Drawing.SystemIcons]::Application
$trayIcon.Text = "Prompt Enhancer (Ctrl+E untuk Mulai / Batal)"
$trayIcon.Visible = $true

function Show-Toast([string]$title, [string]$msg, [int]$timeout = 2500) {
    $trayIcon.BalloonTipTitle = $title
    $trayIcon.BalloonTipText = $msg
    $trayIcon.BalloonTipIcon = [System.Windows.Forms.ToolTipIcon]::Info
    $trayIcon.ShowBalloonTip($timeout)
}

function Get-ClipboardSafe {
    for ($i = 0; $i -lt 5; $i++) {
        try {
            return [System.Windows.Forms.Clipboard]::GetText()
        } catch {
            Start-Sleep -Milliseconds 25
        }
    }
    return ""
}

function Set-ClipboardSafe([string]$text) {
    for ($i = 0; $i -lt 5; $i++) {
        try {
            [System.Windows.Forms.Clipboard]::SetText($text)
            return $true
        } catch {
            Start-Sleep -Milliseconds 25
        }
    }
    return $false
}

function Cancel-CurrentJob {
    if ($script:CurrentJob -and -not $script:CurrentJob.HasExited) {
        try {
            Stop-Process -Id $script:CurrentJob.Id -Force -ErrorAction SilentlyContinue
        } catch {}
        [System.Media.SystemSounds]::Hand.Play()
        Show-Toast "Enhance Dibatalkan (Ctrl+E)" "Proses enhancement dihentikan oleh pengguna." 1500
        Log-Msg "Enhance dibatalkan seketika oleh pengguna (Ctrl+E toggle)."
    }
    if ($script:CurrentTempFile -and (Test-Path $script:CurrentTempFile)) {
        Remove-Item $script:CurrentTempFile -Force -ErrorAction SilentlyContinue
    }
    $script:CurrentJob = $null
    $script:CurrentTempFile = $null
}

function Handle-Hotkey {
    # TOGGLE ABORT: Jika sedang running, penekanan Ctrl+E kedua LANGSUNG membatalkan proses!
    if ($script:CurrentJob -and -not $script:CurrentJob.HasExited) {
        Cancel-CurrentJob
        return
    }

    $oldClipboard = Get-ClipboardSafe

    # Kosongkan clipboard sementara untuk mendeteksi apakah penyalinan baru berhasil
    Set-ClipboardSafe ""
    Start-Sleep -Milliseconds 25

    # Strategi 1: Cek apakah user sudah memblok/menyeleksi teks di editor/terminal
    [System.Windows.Forms.SendKeys]::SendWait("^{INSERT}")
    Start-Sleep -Milliseconds 60
    $draft = Get-ClipboardSafe

    # Strategi 2: Jika belum ada seleksi, coba Shift+Home + Ctrl+Insert (baris saat ini)
    if ([string]::IsNullOrWhiteSpace($draft)) {
        [System.Windows.Forms.SendKeys]::SendWait("+{HOME}")
        Start-Sleep -Milliseconds 40
        [System.Windows.Forms.SendKeys]::SendWait("^{INSERT}")
        Start-Sleep -Milliseconds 60
        $draft = Get-ClipboardSafe
    }

    # Strategi 3: Fallback Ctrl+A + Ctrl+Insert (seluruh prompt)
    if ([string]::IsNullOrWhiteSpace($draft)) {
        [System.Windows.Forms.SendKeys]::SendWait("^a")
        Start-Sleep -Milliseconds 40
        [System.Windows.Forms.SendKeys]::SendWait("^{INSERT}")
        Start-Sleep -Milliseconds 60
        $draft = Get-ClipboardSafe
    }

    if ([string]::IsNullOrWhiteSpace($draft)) {
        [System.Media.SystemSounds]::Beep.Play()
        if ($oldClipboard) {
            Set-ClipboardSafe $oldClipboard
        }
        return
    }

    $script:LastOriginalDraft = $draft
    Log-Msg "Draft captured ($($draft.Length) chars): $draft"
    [System.Media.SystemSounds]::Asterisk.Play()
    Show-Toast "⏳ Enhancing Prompt..." "Sedang memproses... Tekan Ctrl+E lagi untuk batal." 3000

    # Create temporary file for IPC
    $tempFile = Join-Path $env:TEMP ("enhance_" + [Guid]::NewGuid().ToString() + ".txt")
    [System.IO.File]::WriteAllText($tempFile, $draft, [System.Text.Encoding]::UTF8)

    $script:CurrentTempFile = $tempFile

    # Spawn enhance runner
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $NodeExe
    $psi.Arguments = "`"$EnhanceScript`" --file `"$tempFile`""
    $psi.WindowStyle = [System.Diagnostics.ProcessWindowStyle]::Hidden
    $psi.CreateNoWindow = $true
    $psi.UseShellExecute = $false

    $script:CurrentJob = [System.Diagnostics.Process]::Start($psi)
    Log-Msg "Enhance process started (PID: $($script:CurrentJob.Id))"
}

# WinForms Message Loop
$form = New-Object HotkeyForm
$form.add_OnHotkey({ Handle-Hotkey })

# Timer to check job completion
$timer = New-Object System.Windows.Forms.Timer
$timer.Interval = 100
$timer.add_Tick({
    if ($script:CurrentJob -and $script:CurrentJob.HasExited) {
        $exitCode = $script:CurrentJob.ExitCode
        $tempFile = $script:CurrentTempFile
        $script:CurrentJob = $null
        $script:CurrentTempFile = $null

        Log-Msg "Enhance process exited with code $exitCode"

        if ($exitCode -eq 0 -and (Test-Path $tempFile)) {
            $enhancedText = [System.IO.File]::ReadAllText($tempFile, [System.Text.Encoding]::UTF8)
            Remove-Item $tempFile -Force -ErrorAction SilentlyContinue

            if (-not [string]::IsNullOrWhiteSpace($enhancedText)) {
                Set-ClipboardSafe $enhancedText
                Start-Sleep -Milliseconds 60
                # Paste hasil menggunakan Shift+Insert atau Ctrl+V
                [System.Windows.Forms.SendKeys]::SendWait("+{INSERT}")
                [System.Media.SystemSounds]::Question.Play()
                Show-Toast "✦ Prompt Selesai!" "Prompt berhasil diperjelas dan ditempelkan." 2000
                Log-Msg "Enhanced text pasted to active window!"
            }
        } else {
            if (Test-Path $tempFile) {
                Remove-Item $tempFile -Force -ErrorAction SilentlyContinue
            }
        }
    }
})
$timer.Start()

Log-Msg "Entering Application.Run loop with HotkeyForm handle..."

try {
    [System.Windows.Forms.Application]::Run($form)
} finally {
    if (Test-Path $PidFile) {
        Remove-Item $PidFile -Force -ErrorAction SilentlyContinue
    }
    $trayIcon.Visible = $false
    $trayIcon.Dispose()
    Cancel-CurrentJob
    Log-Msg "Daemon stopped."
}
