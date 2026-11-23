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
| **Windows ConPTY Launcher** | [`wrapper/`](wrapper/) | `agy-enhance.exe` menjalankan `agy.exe` resmi dengan shortcut rewrite sebelum Enter, pengaturan, dan proteksi paste chip. |
| **Linux CLI Launcher** | [`wrapper/linux/`](wrapper/linux/) | `Ctrl+E` untuk rewrite draft teks di Kubuntu dengan editor native, pengaturan model, dan mode offline. |
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

Pilih paket di GitHub Releases sesuai fitur yang ingin dipakai.

| Paket | Fitur |
|---|---|
| `agy-enhance-windows-amd64-VERSION.zip` | Launcher Windows untuk **Ctrl+E** di composer. Jalankan `install-windows.ps1` lewat PowerShell, lalu buka `agy-enhance`. |
| `agy-enhance-linux-VERSION.tar.gz` | Launcher Ubuntu/Kubuntu untuk **Ctrl+E** di composer. |
| `agy-cli-plugin-VERSION.zip` | Plugin opsional untuk **`/enhance`** di Antigravity CLI. |

Untuk memakai **Ctrl+E di Windows**, cukup instal launcher Windows AMD64.

### 1. Ubuntu / Kubuntu (`Ctrl+E`)

Siapkan **Python 3.11+**, **Node.js 18+**, dan CLI resmi `agy` di `PATH`. Unduh `agy-enhance-linux-VERSION.tar.gz` dari GitHub Releases, lalu ganti `VERSION` sesuai nama arsip.

```bash
version=VERSION
mkdir -p ~/.local/share/agy-prompt-enhancer ~/.local/bin
tar -xzf "agy-enhance-linux-$version.tar.gz" -C ~/.local/share/agy-prompt-enhancer
ln -s "$HOME/.local/share/agy-prompt-enhancer/agy-enhance-linux-$version/wrapper/linux/agy-enhance" ~/.local/bin/agy-enhance
~/.local/bin/agy-enhance
```

Jika `~/.local/bin/agy-enhance` sudah berupa symlink dari instalasi lama, hapus symlink tersebut sebelum menjalankan `ln -s` lagi. Anda juga dapat menjalankan launcher dari clone.

```bash
./wrapper/linux/agy-enhance
```

Ketik draft, tekan **Ctrl+E** atau shortcut yang tampil di samping model, lalu tinjau hasil di composer. Tekan **Enter** untuk mengirim. Selama rewrite berlangsung, tekan shortcut yang sama sekali lagi untuk membatalkan. Draft dengan paste panjang atau lampiran yang terdeteksi dipertahankan apa adanya. **Ctrl+G** tetap membuka editor biasa dari `VISUAL` atau `EDITOR`.

Launcher memakai layar penuh native. Saat rewrite berjalan, draft tetap terlihat dan baris terbawah menampilkan indikator bergerak, waktu berjalan, dan shortcut untuk membatalkan. Input biasa ditahan sampai composer siap agar draft tidak teredit atau terkirim tanpa sengaja. Setelah itu baris terbawah menampilkan hasilnya selama 7 detik, termasuk jika rewrite dibatalkan, gagal, atau melewati timeout.

Opsi `altScreenMode` atau `statusLine` yang diberikan lewat `--settings` didahulukan. Baris progress dan status hasil memerlukan layar penuh.

Untuk membuka launcher dari direktori mana pun, buat symlink di direktori pengguna yang ada di `PATH`.

```bash
mkdir -p ~/.local/bin
ln -s "$(pwd)/wrapper/linux/agy-enhance" ~/.local/bin/agy-enhance
agy-enhance
```

```bash
agy-enhance settings menu
agy-enhance settings
agy-enhance settings detail detailed
agy-enhance settings language id
agy-enhance settings shortcut Alt+K
agy-enhance settings mode native
agy-enhance settings mode local
agy-enhance settings model MODEL_ID
agy-enhance settings model ""
agy-enhance settings timeoutSeconds 60
agy-enhance settings reset
```

| Pengaturan | Pilihan |
|---|---|
| Tingkat detail | `concise`, `balanced` (default), atau `detailed`. Mengatur panjang dan struktur rewrite dengan cakupan tugas yang tetap sama. |
| Bahasa hasil | `auto` untuk mengikuti draft, `id` untuk Bahasa Indonesia, atau `en` untuk bahasa Inggris. |
| Shortcut | `Ctrl+E` (default) atau kombinasi `Ctrl`/`Alt` dengan huruf A–Z. `Ctrl+C/G/H/I/J/M` dicadangkan untuk kontrol CLI, editor, atau input terminal. |

Mode `native` menerapkan pilihan detail dan bahasa. Model dipilih dari pengaturan launcher, lalu `--model` jika diberikan, lalu default CLI. Mode `local` merapikan teks sederhana secara offline dalam bahasa draft. Pilih `off` untuk meneruskan sesi ke CLI native.

Pengaturan disimpan di `${XDG_CONFIG_HOME:-~/.config}/agy-prompt-enhancer/settings.json` dan dibaca saat sesi baru dibuka. Gunakan `agy models` untuk melihat ID model dan `npm run test:linux` untuk tes launcher.

### 2. Antigravity CLI Plugin (`plugin/`)

```powershell
# Dry-run
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -WhatIf

# Instal ke %USERPROFILE%\.gemini\config\plugins\prompt-enhancer
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1

# Atau via agy CLI native
agy plugin validate ./plugin
agy plugin install ./plugin
```

### 3. Windows / PowerShell (`Ctrl+E`)

Gunakan **Windows Terminal dengan profil PowerShell**. Installer mendukung Windows PowerShell 5.1 dan PowerShell 7. Paket Windows AMD64 memuat launcher yang sudah dikompilasi. Pastikan CLI resmi bisa dijalankan dengan `agy --version`, lalu ganti `VERSION` di bawah dengan versi ZIP yang Anda unduh.

```powershell
Expand-Archive .\agy-enhance-windows-amd64-VERSION.zip -DestinationPath .\agy-enhance-windows
powershell -NoProfile -ExecutionPolicy Bypass -File .\agy-enhance-windows\install-windows.ps1
```

Buka terminal baru setelah instalasi dan jalankan:

```powershell
agy-enhance --wrap-version
agy-enhance settings menu
agy-enhance
```

Ketik draft, tekan **Ctrl+E**, lalu tinjau hasil sebelum Enter. Tekan shortcut yang sama lagi untuk membatalkan. Selama model bekerja, status menampilkan waktu berjalan. Status hasil hilang setelah sekitar tujuh detik. Pilihan pengaturan sama dengan Linux, dengan file di `%APPDATA%\agy-prompt-enhancer\settings.json`. Mode `native` memakai model konfigurasi, lalu `--model`, lalu default CLI. Mode `local` merapikan teks biasa secara offline.

Installer menempatkan launcher di `%LOCALAPPDATA%\agy-prompt-enhancer\bin` dan menambahkannya ke PATH pengguna. Gunakan `agy` resmi untuk penggunaan biasa dan pembaruan. Jika biner berada di lokasi khusus, set `$env:AGY_REAL_PATH = 'C:\path\to\agy.exe'` sebelum menjalankan launcher.

Untuk build dari source, jalankan perintah berikut di PowerShell dengan Go terpasang.

```powershell
powershell -ExecutionPolicy Bypass -File .\wrapper\build.ps1 -Standalone
powershell -ExecutionPolicy Bypass -File .\wrapper\install-windows.ps1
```

Untuk membuat paket distribusi dari checkout commit rilis, jalankan `wrapper/build.ps1` dengan `-Standalone -Package`. ZIP berisi launcher, installer, README, lisensi proyek, dan `THIRD-PARTY-NOTICES.txt`. Untuk mencopot launcher, jalankan installer dengan `-Uninstall`.

## Alur Versioning, Changelog, & Packaging

Proyek ini menggunakan [`package.json`](package.json) sebagai *single source of truth* SemVer (otomatis disinkronkan ke [`plugin/plugin.json`](plugin/plugin.json) dan [`wrapper/version.go`](wrapper/version.go)) serta [`CHANGELOG.md`](CHANGELOG.md) berformat **Keep a Changelog**.

```bash
# 1. Jalankan syntax check, unit test, dan verifikasi kontrak + sinkronisasi versi
npm run verify

# 2. Bangun arsip plugin siap distribusi (agy-cli-plugin-<version>.zip)
npm run package

# Bangun paket launcher untuk Ubuntu dan Kubuntu
npm run package:linux

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

`plugin/config.json` berisi konfigurasi bawaan dan ikut dikemas ke ZIP plugin. Simpan pengaturan pribadi serta kredensial di `enhancer-config.json` lokal atau gunakan `PROMPT_ENHANCER_CONFIG` untuk menunjuk ke berkas di luar repositori.

## Publikasi ke GitHub

Commit kode sumber, konfigurasi bawaan, dan tes. Unggah ZIP plugin, ZIP launcher Windows AMD64, serta tarball launcher Ubuntu/Kubuntu sebagai aset GitHub Release. Simpan salinan CLI resmi, konfigurasi pribadi, tangkapan terminal, transkrip, dan hasil debug di luar commit. Gunakan `temp/` untuk hasil investigasi lokal dan periksa daftar calon commit dengan `git diff --cached --name-only`.

## Verifikasi

```powershell
# Cek versi plugin & wrapper
node plugin/hook.mjs --version
agy-enhance --wrap-version

# Verifikasi plugin, engine, dan kontrak
npm run verify

# Dry-run installer plugin
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -WhatIf

# Tes launcher di Windows
pushd wrapper
go test -run '^TestStandalone' .
popd
```

Untuk uji manual di Windows Terminal, instal paket, jalankan `agy-enhance`, lalu gunakan `Ctrl+E` pada draft contoh. Ulangi dan tekan shortcut yang sama saat proses masih berjalan untuk memastikan draft asli tetap tersedia setelah pembatalan. Jalankan `npm run test:linux` untuk tes launcher Linux.

## Uninstall & Rollback

- **CLI Plugin:** `powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -Uninstall`
- **Windows standalone:** Jalankan `install-windows.ps1 -Uninstall`.
- **Wrapper lama yang dipasang dengan `-Deploy`:** Pulihkan `agy-real.exe` ke `%LOCALAPPDATA%\agy\bin\agy.exe` (atau dari folder `backup_build_deploy_*` yang dibuat oleh `wrapper/build.ps1 -Deploy`).

## Author, Credits & Changelog

- **Author & Maintainer:** [**Kyuzan0**](https://github.com/Kyuzan0)
- **Repository:** [Kyuzan0/Enhance-Prompt-Antigravity](https://github.com/Kyuzan0/Enhance-Prompt-Antigravity)
- **Riwayat Rilis:** Lihat [CHANGELOG.md](CHANGELOG.md)
- **Lisensi:** Dilisensikan di bawah [MIT License](LICENSE)
