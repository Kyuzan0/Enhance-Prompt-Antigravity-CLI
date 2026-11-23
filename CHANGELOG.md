# Changelog

All notable changes to **agy CLI Prompt Enhancer** (`agy-cli-prompt-enhancer`) will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [2.7.0] - 2026-11-23

### Added

- Add a portable Ubuntu and Kubuntu launcher archive with its runtime dependencies from this repository.

## [2.6.1] - 2026-11-23

### Changed

- Exclude local CLI configuration, terminal captures, reverse-engineering databases, and Windows build files from Git.

### Fixed

- Keep raw drafts, parser excerpts, and API error bodies out of plugin diagnostics.
- Remove local build paths from the Windows launcher and include third-party license notices in the Windows ZIP.

## [2.6.0] - 2026-11-23

### Added
- Provide a standalone Windows ConPTY launcher with configurable shortcut, language, detail, model, timeout, progress, and expiring rewrite status. Add a per-user PowerShell installer and Windows ZIP packaging without replacing the official CLI.

## [2.5.0] - 2026-11-23

### Added
- add output language support to prompt enhancer and document Linux CLI launcher

## [2.4.0] - 2026-11-23

### Added
- add Linux CLI launcher and prompt enhancement settings

## [2.3.4] - 2026-11-23

### Fixed
- Clear Linux enhancement results after seven seconds even when the native CLI is idle.

## [2.3.3] - 2026-11-23

### Fixed
- Show a live progress row and cancellation hint while the Linux composer is paused. Report the rewrite outcome and keep input gated until the composer redraws.

## [2.3.2] - 2026-11-23

### Fixed
- Keep the Linux composer visible during prompt enhancement by retaining the native alternate-screen frame through the editor hand-off.

## [2.3.1] - 2026-11-23

### Fixed
- Keep Linux rewrite diagnostics out of the terminal redraw, use the configured shortcut alone to cancel, and show that shortcut beside the model in the native status line.

## [2.3.0] - 2026-11-23

### Added
- Add an interactive Linux settings menu and configurable rewrite detail, output language, and keyboard shortcut for the launcher.

## [2.2.0] - 2026-11-23

### Added
- Add a Linux launcher for Kubuntu with Ctrl+E draft rewriting through the native agy editor, cancellation, and configurable mode, model, and timeout.

## [2.1.3] - 2026-11-23

### Changed
- improve reliability of plugin packaging and release script error handling

## [2.1.2] - 2026-11-23

### Changed
- improve testing, release scripts, and plugin configuration for prompt enhancer

## [2.1.1] - 2026-11-23

### Fixed
- Include the Node test suites and the plugin README in Git so a clean checkout can verify and build the plugin ZIP.
- Enable the commit hook on Unix and restore release metadata when verification or packaging fails. Retain previous ZIPs until the replacement is ready.

## [2.1.0] - 2026-11-22

### Added
- Root `package.json` as SemVer single source of truth, `CHANGELOG.md`, and automated release/packaging pipeline (`scripts/release.mjs`, `scripts/package-plugin.mjs`).
- CLI version & credit info: `node plugin/hook.mjs --version` / `--about`, Go wrapper `--wrap-version` flag (`wrapper/version.go`), and `Kyuzan0` (`https://github.com/Kyuzan0`) credits across manifests, docs, and `LICENSE`.
- Standalone `contracts/` (`model-catalog.json`, `strict-deny-policy.json`) and `verify-contracts.mjs` (including cross-component version sync checks).

### Changed
- Separated `agy-cli` into its own independent repository (`plugin/`, `wrapper/`, `specs/fail-closed-hook/`, `core-engine/`, `legacy-daemon/`).
- Tightened prompt-enhancement rules in `plugin/hook.mjs` and `legacy-daemon/enhance.mjs` for token efficiency (anti-repetition and no redundant bilingual parenthetical glosses).
- Updated `plugin/install-plugin.ps1` and `wrapper/build.ps1` to read version metadata dynamically and bundle `LICENSE`/`CHANGELOG.md`.

### Fixed
- Decoupled hardcoded repo depth paths in `legacy-daemon/manage-daemon.ps1`.
- Aligned default `plugin/config.json` `autoEnhance` to `false`.

## [2.0.0] - 2026-11-19

### Added
- Go ConPTY wrapper with native `Ctrl+G` editor bridge, chip-preserving span guard, strict `toolPermission: "strict"` containment, fail-closed `PreInvocation` spec, and `prompt-enhancer` CLI plugin v2.0.0.

## [1.0.0] - 2026-11-14

### Added
- Initial Antigravity CLI prompt enhancer plugin and hotkey daemon.

[Unreleased]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.7.0...HEAD
[2.7.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.6.1...v2.7.0
[2.6.1]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.6.0...v2.6.1
[2.6.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.5.0...v2.6.0
[2.5.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.4.0...v2.5.0
[2.4.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.3.4...v2.4.0
[2.3.4]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.3.3...v2.3.4
[2.3.3]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.3.2...v2.3.3
[2.3.2]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.3.1...v2.3.2
[2.3.1]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.3.0...v2.3.1
[2.3.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.2.0...v2.3.0
[2.2.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.3...v2.2.0
[2.1.3]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.2...v2.1.3
[2.1.2]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.1...v2.1.2
[2.1.1]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.0...v2.1.1
[2.1.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/releases/tag/v1.0.0
