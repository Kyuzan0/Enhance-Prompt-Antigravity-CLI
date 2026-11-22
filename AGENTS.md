# AGENTS.md — agy CLI Prompt Enhancer (`agy-cli-prompt-enhancer`)

Instruksi kerja wajib bagi AI coding agent saat memodifikasi proyek ini.

## 1. Batas Workspace & Single Source of Truth
- Proyek ini berdiri independen khusus untuk **Antigravity CLI (`agy`)**. Jangan memodifikasi proyek sibling `../agy` kecuali diminta secara eksplisit oleh pengguna.
- **`package.json`** adalah *single source of truth* SemVer yang wajib selalu sinkron dengan `plugin/plugin.json` dan `wrapper/version.go` (`WrapperVersion`). Jangan membiarkan versi antar-komponen berbeda.
- Pertahankan atribusi author **`Kyuzan0`** (`https://github.com/Kyuzan0`).

## 2. Alur Wajib Setiap Ada Perubahan Kode
Setiap perubahan fungsional pada `plugin/`, `wrapper/`, `specs/`, `core-engine/`, atau `legacy-daemon/` **wajib** diikuti oleh:
1. **Update `CHANGELOG.md` & Bump Versi (SemVer):**
   - Catat perubahan di bawah `## [Unreleased]` pada `CHANGELOG.md`, lalu jalankan `node scripts/release.mjs patch|minor|major`.
   - *(Jika `git commit` dijalankan langsung tanpa `release.mjs`, `.githooks/commit-msg` akan otomatis menyinkronkan versi di `package.json`, `plugin/plugin.json`, dan `wrapper/version.go`, memperbarui `CHANGELOG.md`, serta membangun ulang `agy-cli-plugin-<version>.zip`.)*
2. **Verifikasi Penuh Sebelum Selesai:**
   - Jalankan `npm run verify` (`--check`, `specs/fail-closed-hook/hook.test.mjs`, `core-engine/prompt-enhancer.test.mjs`, dan `verify-contracts.mjs`).
   - Jika file Go di `wrapper/` berubah, ingatkan atau jalankan `go test` & `powershell -ExecutionPolicy Bypass -File wrapper/build.ps1` di Windows.
3. **Rebuild Plugin ZIP:**
   - Pastikan arsip `agy-cli-plugin-<version>.zip` terbaru tersedia dan versi lama dibersihkan (`scripts/package-plugin.mjs`).

## 3. Aturan Git Hook (`.githooks/commit-msg`)
- Aktifkan di clone baru dengan `npm run hooks:install` (`git config core.hooksPath .githooks`).
- Pemetaan otomatis saat commit:
  - `feat:` → bump `minor` (`### Added`)
  - `fix:` → bump `patch` (`### Fixed`)
  - `perf:` / `refactor:` → bump `patch` (`### Changed`)
  - `!:` atau `BREAKING CHANGE` → bump `major` (`### Changed`)
  - `docs:` / `chore:` / `test:` / `ci:` / `chore(release):` → tidak menaikkan versi.
- Memiliki guard anti *double-bump*: jika `package.json` `"version"` sudah di-bump oleh `scripts/release.mjs`, hook otomatis dilewati. Gunakan `SKIP_AUTO_RELEASE=1` bila perlu melewati hook.

## 4. Invarian Keamanan & Containment
- Setiap pemanggilan `agy -p` harus memakai `toolPermission: "strict"` + seluruh 7 aturan `permissions.deny` sesuai `contracts/strict-deny-policy.json` (verifikasi via `node verify-contracts.mjs`).
- Pertahankan aturan hemat token pada `plugin/hook.mjs` dan `legacy-daemon/enhance.mjs` (tanpa template berlebih atau glosarium bilingual dalam kurung).
