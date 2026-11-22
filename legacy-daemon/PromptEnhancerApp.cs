using System;
using System.Diagnostics;
using System.Drawing;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Windows.Forms;

public class PromptEnhancerApp : Form {
    [DllImport("user32.dll")]
    public static extern bool RegisterHotKey(IntPtr hWnd, int id, int fsModifiers, int vk);

    [DllImport("user32.dll")]
    public static extern bool UnregisterHotKey(IntPtr hWnd, int id);

    private const int WM_HOTKEY = 0x0312;
    private const int MOD_CONTROL = 0x0002;
    private const int VK_E = 0x45;
    private const int HOTKEY_ID = 9001;

    private NotifyIcon trayIcon;
    private ContextMenuStrip trayMenu;
    private Process currentAgyProcess = null;
    private string lastOriginalDraft = "";

    public PromptEnhancerApp() {
        this.WindowState = FormWindowState.Minimized;
        this.ShowInTaskbar = false;
        this.FormBorderStyle = FormBorderStyle.None;
        this.Size = new Size(0, 0);

        trayMenu = new ContextMenuStrip();
        trayMenu.Items.Add("Antigravity Prompt Enhancer (Ctrl+E)").Enabled = false;
        trayMenu.Items.Add("-");
        trayMenu.Items.Add("Keluar", null, (s, e) => Application.Exit());

        trayIcon = new NotifyIcon();
        trayIcon.Text = "Antigravity Prompt Enhancer (Tekan Ctrl+E)";
        trayIcon.Icon = SystemIcons.Application;
        trayIcon.ContextMenuStrip = trayMenu;
        trayIcon.Visible = true;
    }

    protected override void OnLoad(EventArgs e) {
        this.Visible = false;
        bool registered = RegisterHotKey(this.Handle, HOTKEY_ID, MOD_CONTROL, VK_E);
        if (!registered) {
            MessageBox.Show("Gagal mendaftarkan shortcut Ctrl+E. Mungkin sedang digunakan oleh aplikasi lain.", 
                "Prompt Enhancer Error", MessageBoxButtons.OK, MessageBoxIcon.Warning);
        }
        base.OnLoad(e);
    }

    protected override void WndProc(ref Message m) {
        if (m.Msg == WM_HOTKEY) {
            HandleHotkey();
            return;
        }
        base.WndProc(ref m);
    }

    private void ShowToast(string title, string msg, ToolTipIcon icon = ToolTipIcon.Info, int timeout = 2500) {
        try {
            trayIcon.BalloonTipTitle = title;
            trayIcon.BalloonTipText = msg;
            trayIcon.BalloonTipIcon = icon;
            trayIcon.ShowBalloonTip(timeout);
        } catch {}
    }

    private string GetClipboardSafe() {
        for (int i = 0; i < 5; i++) {
            try {
                if (Clipboard.ContainsText()) {
                    return Clipboard.GetText();
                }
            } catch {
                Thread.Sleep(30);
            }
        }
        return "";
    }

    private void SetClipboardSafe(string text) {
        for (int i = 0; i < 5; i++) {
            try {
                Clipboard.SetText(text);
                return;
            } catch {
                Thread.Sleep(30);
            }
        }
    }

    private void CancelCurrent() {
        if (currentAgyProcess != null && !currentAgyProcess.HasExited) {
            try {
                currentAgyProcess.Kill();
            } catch {}
            System.Media.SystemSounds.Hand.Play();
            ShowToast("Enhance Dibatalkan", "Proses enhance prompt dibatalkan oleh pengguna (Ctrl+E).", ToolTipIcon.Warning, 1500);
        }
        currentAgyProcess = null;
    }

    private void HandleHotkey() {
        // Toggle abort if already running
        if (currentAgyProcess != null && !currentAgyProcess.HasExited) {
            CancelCurrent();
            return;
        }

        string oldClipboard = GetClipboardSafe();

        // Select all text in prompt and copy
        SendKeys.SendWait("^a");
        Thread.Sleep(50);
        SendKeys.SendWait("^c");
        Thread.Sleep(90);

        string draft = GetClipboardSafe().Trim();

        if (string.IsNullOrEmpty(draft)) {
            System.Media.SystemSounds.Beep.Play();
            if (!string.IsNullOrEmpty(oldClipboard)) {
                SetClipboardSafe(oldClipboard);
            }
            return;
        }

        lastOriginalDraft = draft;
        System.Media.SystemSounds.Asterisk.Play();
        ShowToast("⏳ Enhancing Prompt...", "Sedang memproses dengan model aktif. Tekan Ctrl+E lagi untuk batal.", ToolTipIcon.Info, 3000);

        ThreadPool.QueueUserWorkItem(_ => RunEnhanceThread(draft));
    }

    private void RunEnhanceThread(string draft) {
        try {
            string rules = BuildRules(draft);
            string tmpDir = Path.GetTempPath();

            ProcessStartInfo psi = new ProcessStartInfo {
                FileName = "agy",
                Arguments = "-p \"" + EscapeCli(rules) + "\" --dangerously-skip-permissions",
                WorkingDirectory = tmpDir,
                UseShellExecute = false,
                RedirectStandardOutput = true,
                RedirectStandardError = true,
                CreateNoWindow = true,
                StandardOutputEncoding = Encoding.UTF8
            };

            Process proc = new Process { StartInfo = psi };
            currentAgyProcess = proc;

            proc.Start();
            string output = proc.StandardOutput.ReadToEnd();
            proc.WaitForExit();

            if (proc.ExitCode != 0 && string.IsNullOrWhiteSpace(output)) {
                string err = proc.StandardError.ReadToEnd();
                this.BeginInvoke(new Action(() => {
                    ShowToast("Gagal Enhance", string.IsNullOrWhiteSpace(err) ? "Proses agy keluar dengan kode " + proc.ExitCode : err, ToolTipIcon.Error);
                }));
                return;
            }

            string cleaned = CleanReply(output);

            this.BeginInvoke(new Action(() => {
                SetClipboardSafe(cleaned);
                Thread.Sleep(60);
                SendKeys.SendWait("^v");
                System.Media.SystemSounds.Question.Play();
                ShowToast("✦ Prompt Selesai!", "Prompt berhasil diperjelas dan ditempelkan ke terminal.", ToolTipIcon.Info, 2000);
            }));
        } catch (Exception ex) {
            this.BeginInvoke(new Action(() => {
                ShowToast("Error", ex.Message, ToolTipIcon.Error);
            }));
        } finally {
            currentAgyProcess = null;
        }
    }

    private string BuildRules(string draft) {
        StringBuilder sb = new StringBuilder();
        sb.AppendLine("You are an expert prompt engineer and senior software architect.");
        sb.AppendLine("Your task is to rewrite and enhance the user's prompt draft into an optimal, production-grade instruction for an AI coding agent.");
        sb.AppendLine();
        sb.AppendLine("Rules:");
        sb.AppendLine("1. Enhance ONLY the prompt instructions. Do NOT fulfill or answer the request yourself.");
        sb.AppendLine("2. Keep the original language (e.g. Bahasa Indonesia or English), intent, facts, and core constraints completely intact.");
        sb.AppendLine("3. Structure the enhanced prompt clearly with objective, key requirements, edge cases, and verification criteria.");
        sb.AppendLine("4. Output ONLY the rewritten prompt text. Do not wrap in markdown code blocks or add introductory/concluding commentary.");
        sb.AppendLine();
        sb.AppendLine("Draft to enhance:");
        sb.AppendLine(draft);
        return sb.ToString();
    }

    private string EscapeCli(string text) {
        return text.Replace("\"", "\\\"");
    }

    private string CleanReply(string raw) {
        string text = raw.Trim();
        if (text.StartsWith("```") && text.EndsWith("```")) {
            text = Regex.Replace(text, @"^```[a-zA-Z]*\r?\n?", "");
            text = Regex.Replace(text, @"\r?\n?```$", "").Trim();
        }
        if ((text.StartsWith("\"") && text.EndsWith("\"")) || (text.StartsWith("'") && text.EndsWith("'"))) {
            text = text.Substring(1, text.Length - 2).Trim();
        }
        return text;
    }

    protected override void OnFormClosing(FormClosingEventArgs e) {
        UnregisterHotKey(this.Handle, HOTKEY_ID);
        CancelCurrent();
        if (trayIcon != null) {
            trayIcon.Visible = false;
            trayIcon.Dispose();
        }
        base.OnFormClosing(e);
    }

    [STAThread]
    public static void Main() {
        Application.EnableVisualStyles();
        Application.SetCompatibleTextRenderingDefault(false);
        Application.Run(new PromptEnhancerApp());
    }
}
