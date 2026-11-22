# agy CLI — Antigravity CLI Prompt Enhancer

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Changelog](https://img.shields.io/badge/Keep%20a%20Changelog-1.1.0-orange.svg)](CHANGELOG.md)
[![Author: Kyuzan0](https://img.shields.io/badge/Author-Kyuzan0-181717.svg?logo=github)](https://github.com/Kyuzan0)

Toolkit peningkatan prompt independen untuk **Antigravity CLI (`agy`)** yang mencakup plugin `PreInvocation`, Go ConPTY wrapper interaktif, spesifikasi hook *fail-closed*, core engine ESM/TS, dan skrip daemon legacy.

*(Untuk ekstensi **Antigravity IDE**, lihat folder proyek terpisah di `../agy/`.)*

## Arsitektur & Komponen

| Komponen | Lokasi | Peran |
|---|---|---|
| **CLI Plugin & Skill `/enhance`** | [`plugin/`](plugin/) | Hook `PreInvocation` aditif ([`plugin/hook.mjs`](plugin/hook.mjs)) dengan mode `hybrid`/`llm`/`local`, flag `--version` / `--about`, pembacaan transkrip sesi, dan skill `/enhance` ([`plugin/skills/enhance/SKILL.md`](plugin/skills/enhance/SKILL.md)). |
| **Go ConPTY CLI Wrapper** | [`wrapper/`](wrapper/) | Wrapper Go (`module ctrle-interceptor`) di atas `agy-real.exe` untuk shortcut terminal `Ctrl+E`, native editor bridge `Ctrl+G`, `--wrap-version`, trigger `!enhance`/`/enhance` sebelum Enter, dan proteksi paste-chip. |
| **Fail-Closed Hook Spec** | [`specs/fail-closed-hook/`](specs/fail-closed-hook/) | Spesifikasi hook `PreInvocation` *fail-closed* ([`hook.mjs`](specs/fail-closed-hook/hook.mjs)) yang menghentikan turn jika model kanonik tidak valid atau kontrak replace-draft tidak tersedia. |
| **Core Engine (ESM/TS)** | [`core-engine/`](core-engine/) | Modul engine modular standalone ([`prompt-enhancer.mjs`](core-engine/prompt-enhancer.mjs) / `.ts`) dengan validasi payload, sanitasi anti-injection, dan retry backoff. |
| **Contracts & Drift Verifier** | [`contracts/`](contracts/) | Kontrak [`model-catalog.json`](contracts/model-catalog.json) & [`strict-deny-policy.json`](contracts/strict-deny-policy.json) beserta [`verify-contracts.mjs`](verify-contracts.mjs). |
| **Legacy Daemon *(Deprecated)*** | [`legacy-daemon/`](legacy-daemon/) | Daemon hotkey global WinForms (`daemon.ps1`, `manage-daemon.ps1`, `PromptEnhancerApp.cs`, `enhance.mjs`). |

## Prasyarat

- **Node.js** `>= 18`
- **Antigravity CLI (`agy`)** terpasang di sistem (`~/.gemini/bin/agy*` atau `%LOCALAPPDATA%\agy\bin\agy-real.exe`)
- **PowerShell** `5.1` atau `7+` *(untuk skrip instalasi/build Windows)*
- **Go** `>= 1.26.4` *(hanya jika mengompilasi `wrapper/` dari source)*

## Quick Start

### 1. Antigravity CLI Plugin (`plugin/`)

```powershell
# Dry-run
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -WhatIf

# Instal ke %USERPROFILE%\.gemini\config\plugins\prompt-enhancer
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1

# Atau via agy CLI native
agy plugin validate ./plugin
agy plugin install ./plugin
```

### 2. Go ConPTY CLI Wrapper (`wrapper/`)

```powershell
# Build saja (otomatis menyuntikkan versi dari package.json)
powershell -ExecutionPolicy Bypass -File wrapper/build.ps1

# Build dan deploy aman ke %LOCALAPPDATA%\agy\bin
powershell -ExecutionPolicy Bypass -File wrapper/build.ps1 -Deploy
```

## Alur Versioning, Changelog, & Packaging

Proyek ini menggunakan [`package.json`](package.json) sebagai *single source of truth* SemVer (otomatis disinkronkan ke [`plugin/plugin.json`](plugin/plugin.json) dan [`wrapper/version.go`](wrapper/version.go)) serta [`CHANGELOG.md`](CHANGELOG.md) berformat **Keep a Changelog**.

```bash
# 1. Jalankan syntax check, unit test, dan verifikasi kontrak + sinkronisasi versi
npm run verify

# 2. Bangun arsip plugin siap distribusi (agy-cli-plugin-<version>.zip)
npm run package

# 3. Alur rilis SemVer lengkap (bump versi + update CHANGELOG.md + verify + build zip):
npm run release:patch
npm run release:minor
npm run release:major
```

## Konfigurasi & Environment Variables

| Variable | Default | Deskripsi |
|---|---|---|
| `AGY_CONFIG_DIR` | `%USERPROFILE%\.gemini\config` | Target root config CLI untuk `plugin/install-plugin.ps1`. |
| `AGY_BIN_DIR` | `%LOCALAPPDATA%\agy\bin` | Target direktori binary CLI untuk `wrapper/build.ps1`. |
| `AGY_CLI_BIN` | Auto (`~/.gemini/bin/agy*`) | Override path ke biner native `agy` untuk `legacy-daemon/enhance.mjs`. |
| `PROMPT_ENHANCER_CONFIG` | `config.json` / `enhancer-config.json` | Override path file konfigurasi JSON untuk `plugin/hook.mjs`. |
| `PROMPT_ENHANCER_MODE` | `hybrid` | `hybrid`, `llm`, atau `local`. |
| `PROMPT_ENHANCER_MODEL` | Active session model | Override model eksplisit untuk `plugin/hook.mjs`. |
| `AGY_AUTO_ENHANCE` | `false` | Aktifkan auto-enhance untuk setiap pesan pada `plugin/hook.mjs`. |

## Verifikasi

```powershell
# Cek versi plugin & wrapper
node plugin/hook.mjs --version
agy --wrap-version

# Syntax check skrip Node
node --check plugin/hook.mjs
node --check specs/fail-closed-hook/hook.mjs
node --check legacy-daemon/enhance.mjs

# Unit test & verifikasi kontrak
node specs/fail-closed-hook/hook.test.mjs
node core-engine/prompt-enhancer.test.mjs
node verify-contracts.mjs

# Dry-run installer plugin
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -WhatIf

# Test & build Go wrapper
pushd wrapper
go test -run 'Test(Enhancer|Composer|EditorBridge|Span|Renderer|Guard|Unicode|Slot|Matrix|Wrapfix)' .
go build -o "$env:TEMP\agy-verify.exe" .
Remove-Item "$env:TEMP\agy-verify.exe" -Force -ErrorAction SilentlyContinue
popd
```

Lihat prosedur acceptance live di [docs/RUNBOOK_CLI_ACCEPTANCE.md](docs/RUNBOOK_CLI_ACCEPTANCE.md).

## Uninstall & Rollback

- **CLI Plugin:** `powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -Uninstall`
- **Go ConPTY Wrapper:** Pulihkan `agy-real.exe` ke `%LOCALAPPDATA%\agy\bin\agy.exe` (atau dari folder `backup_build_deploy_*` yang dibuat oleh `wrapper/build.ps1 -Deploy`).

## Author, Credits & Changelog

- **Author & Maintainer:** [**Kyuzan0**](https://github.com/Kyuzan0)
- **Repository:** [Kyuzan0/Enhance-Prompt-Antigravity](https://github.com/Kyuzan0/Enhance-Prompt-Antigravity)
- **Riwayat Rilis:** Lihat [CHANGELOG.md](CHANGELOG.md)
- **Lisensi:** Dilisensikan di bawah [MIT License](LICENSE)
