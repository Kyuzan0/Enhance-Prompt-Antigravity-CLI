# Changelog

All notable changes to **agy CLI Prompt Enhancer** (`agy-cli-prompt-enhancer`) will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.2...HEAD
[2.1.2]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.1...v2.1.2
[2.1.1]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.1.0...v2.1.1
[2.1.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/Kyuzan0/Enhance-Prompt-Antigravity/releases/tag/v1.0.0
