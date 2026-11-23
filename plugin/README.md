# Antigravity CLI Prompt Enhancer Plugin

Portable `PreInvocation` plugin and `/enhance` skill for **Antigravity CLI (`agy`)**.

## Prerequisites

- **Node.js** `>= 18` on the `PATH` used to launch `agy`.
- **Antigravity CLI (`agy`)** installed on your system
- **PowerShell** `5.1` or `7+` if using the Windows installer.

## Installation (After `git clone`)

### Windows (PowerShell)

```powershell
# Preview changes (dry-run)
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -WhatIf

# Install into %USERPROFILE%\.gemini\config\plugins\prompt-enhancer
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1
```

### Kubuntu / Linux and other platforms

From the repository root:

```bash
node --version
agy plugin validate ./plugin --strict
agy plugin install ./plugin
```

## Configuration

Copy [`config.example.json`](config.example.json) to `config.json` (or `enhancer-config.json`, which is gitignored) or set environment variables:

| Key / Env Var | Default | Purpose |
|---|---|---|
| `enabled` | `true` | Enable/disable the hook globally. |
| `autoEnhance` / `AGY_AUTO_ENHANCE` | `false` | Auto-enhance every message unless prefixed with `raw:` or `\`. |
| `triggerPrefixes` | `["! ", "!enhance ", "!enhance:", "/enhance ", "/enhance:"]` | Explicit trigger prefixes. |
| `mode` / `PROMPT_ENHANCER_MODE` | `hybrid` | `hybrid`, `llm` (requires `apiUrl`), or `local` deterministic synthesis. |
| `model` / `PROMPT_ENHANCER_MODEL` | Active payload model | Explicit model override (`gemini-4-hydrogen-medium`, etc.). |
| `PROMPT_ENHANCER_CONFIG` | `enhancer-config.json` / `config.json` | Override path to JSON config file. |
| `AGY_CONFIG_DIR` | `%USERPROFILE%\.gemini\config` | Custom Antigravity config root for `install-plugin.ps1`. |

> Never commit API keys or private tokens into `config.json`. Keep private overrides in `enhancer-config.json` (gitignored) or environment variables.

## Usage & Verification

- Prefix a prompt in Antigravity CLI with `!enhance <draft>` or `/enhance <draft>`.
- Attachment markers (`📄 #1`, `[Image #1]`, `@file`, `file://...`) are preserved verbatim.
- Verify syntax & behavior:
  ```bash
  node --check plugin/hook.mjs
  ```

## Relation to Wrapper & IDE Extension

- `plugin/hook.mjs` runs as an additive `PreInvocation` CLI hook (appending an enhanced system guidance block via `injectSteps`).
- The Go ConPTY wrapper in `../wrapper/` provides editable draft replacement in the Windows terminal with `Ctrl+E`.
- For the **Antigravity IDE** **✦** button next to Send, see the separate Antigravity IDE extension project (`../agy/`).
- [`../specs/fail-closed-hook/hook.mjs`](../specs/fail-closed-hook/hook.mjs) is the strict *fail-closed* specification used by `hook.test.mjs`.

## Uninstall

```bash
agy plugin uninstall prompt-enhancer
```

For an installation managed by the Windows PowerShell script:

```powershell
powershell -ExecutionPolicy Bypass -File plugin/install-plugin.ps1 -Uninstall
```
