import { execSync, spawn } from 'child_process';
import path from 'path';
import { fileURLToPath } from 'url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

// Read stdin context from hook (if any)
process.stdin.resume();

try {
  // Check if daemon.ps1 is running
  const output = execSync('powershell -NoProfile -Command "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like \'*daemon.ps1*\' } | Select-Object -ExpandProperty ProcessId"', { encoding: 'utf8' }).trim();
  
  if (!output) {
    const vbsPath = path.join(__dirname, 'start.vbs');
    spawn('wscript.exe', [vbsPath], { detached: true, stdio: 'ignore' }).unref();
  }
} catch (e) {
  // Ignore errors to avoid blocking agent
}

// Hook output contract requires valid JSON
process.stdout.write(JSON.stringify({}));
