#!/usr/bin/env node

/**
 * Antigravity CLI PreInvocation Hook Handler: Prompt Enhancer (fail-closed)
 *
 * Source policy after the remote cutover:
 * - No remote endpoint, API key, model env override, settings fallback, or
 *   hard-coded default model.
 * - Only the invoking PreInvocation payload may supply the owning model.
 * - Known model IDs are the exact wire IDs listed by `agy-real models`.
 * - No enhancement is performed when the model is missing/invalid, or while
 *   the PreInvocation contract cannot replace the submitted draft. When the
 *   installed wrapper is active it consumes !/enhance before Enter and uses the
 *   native editor bridge, so this hook path is the no-wrapper fallback.
 *   The print subprocess does have a supported strict-deny contract, so
 *   permissions are no longer the blocker; draft replacement/composer
 *   preservation and a guaranteed owning-model payload are. In every refusal
 *   case the triggered turn is terminated before the original draft task
 *   executes; the original text is echoed in the error output (the composer is
 *   not restored).
 *
 * The installed plugin copy is NOT migrated by this file.
 */

import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

const DEFAULT_CONFIG = {
  enabled: true,
  autoEnhance: false,
  triggerPrefixes: ['! ', '!enhance ', '!enhance:', '/enhance ', '/enhance:'],
};

// Verified via `agy.exe models` (2026-11-19).
const CANONICAL_MODELS = new Map([
  ['gemini-4-argon-low', { wire: 'gemini-4-argon-low', display: 'Gemini 4 Argon (Low)', effort: 'low' }],
  ['gemini-4-argon-medium', { wire: 'gemini-4-argon-medium', display: 'Gemini 4 Argon (Medium)', effort: 'medium' }],
  ['gemini-4-argon-high', { wire: 'gemini-4-argon-high', display: 'Gemini 4 Argon (High)', effort: 'high' }],
  ['gemini-4-hydrogen-low', { wire: 'gemini-4-hydrogen-low', display: 'Gemini 4 Hydrogen (Low)', effort: 'low' }],
  ['gemini-4-hydrogen-medium', { wire: 'gemini-4-hydrogen-medium', display: 'Gemini 4 Hydrogen (Medium)', effort: 'medium' }],
  ['gemini-4-hydrogen-high', { wire: 'gemini-4-hydrogen-high', display: 'Gemini 4 Hydrogen (High)', effort: 'high' }],
  ['gemini-4-hydrogen-xhigh', { wire: 'gemini-4-hydrogen-xhigh', display: 'Gemini 4 Hydrogen (XHigh)', effort: 'xhigh' }],
  ['gemini-pro-orchestrator-g1-pro', { wire: 'gemini-pro-orchestrator-g1-pro', display: 'Orchestrator (Argon)', effort: 'high' }],
  ['gemini-3.8-flash-high', { wire: 'gemini-3.8-flash-high', display: 'Gemini 3.8 Flash (High)', effort: 'high' }],
  ['gemini-3.8-flash-medium', { wire: 'gemini-3.8-flash-medium', display: 'Gemini 3.8 Flash (Medium)', effort: 'medium' }],
  ['gemini-3.8-flash-low', { wire: 'gemini-3.8-flash-low', display: 'Gemini 3.8 Flash (Low)', effort: 'low' }],
  ['claude-opus-5-5-low', { wire: 'claude-opus-5-5-low', display: 'Claude Opus 5.5 (Low)', effort: 'low' }],
  ['claude-opus-5-5-medium', { wire: 'claude-opus-5-5-medium', display: 'Claude Opus 5.5 (Medium)', effort: 'medium' }],
  ['claude-opus-5-5-high', { wire: 'claude-opus-5-5-high', display: 'Claude Opus 5.5 (High)', effort: 'high' }],
  ['claude-sonnet-5-5-low', { wire: 'claude-sonnet-5-5-low', display: 'Claude Sonnet 5.5 (Low)', effort: 'low' }],
  ['claude-sonnet-5-5-medium', { wire: 'claude-sonnet-5-5-medium', display: 'Claude Sonnet 5.5 (Medium)', effort: 'medium' }],
  ['claude-sonnet-5-5-high', { wire: 'claude-sonnet-5-5-high', display: 'Claude Sonnet 5.5 (High)', effort: 'high' }],
  ['claude-sonnet-4-6', { wire: 'claude-sonnet-4-6', display: 'Claude Sonnet 4.6 (Thinking)', effort: 'high' }],
  ['claude-opus-4-6-thinking', { wire: 'claude-opus-4-6-thinking', display: 'Claude Opus 4.6 (Thinking)', effort: 'high' }],
  ['gpt-oss-120b-medium', { wire: 'gpt-oss-120b-medium', display: 'GPT-OSS 120B (Medium)', effort: 'medium' }],
]);
const MODEL_LOOKUP = new Map();
for (const entry of CANONICAL_MODELS.values()) {
  MODEL_LOOKUP.set(entry.wire.toLowerCase(), entry);
  MODEL_LOOKUP.set(entry.display.toLowerCase(), entry);
}

function loadConfig() {
  const config = { ...DEFAULT_CONFIG };
  const configFiles = [
    process.env.PROMPT_ENHANCER_CONFIG || '',
    path.join(__dirname, 'enhancer-config.json'),
    path.join(__dirname, 'config.json'),
  ].filter(Boolean);

  for (const cfgPath of configFiles) {
    if (!fs.existsSync(cfgPath)) continue;
    try {
      Object.assign(config, JSON.parse(fs.readFileSync(cfgPath, 'utf8')));
      break;
    } catch (err) {
      logStructured('WARN', 'config_parse_failed', { file: cfgPath, error: err.message });
    }
  }

  if (process.env.AGY_AUTO_ENHANCE === 'true'
    || process.env.AGY_AUTO_ENHANCE === '1'
    || process.env.PROMPT_ENHANCER_AUTO === '1') {
    config.autoEnhance = true;
  }

  return config;
}

function logStructured(level, event, meta = {}) {
  const sanitizedMeta = {};
  for (const [k, v] of Object.entries(meta)) {
    sanitizedMeta[k] = /key|token|auth|secret|credential|password/i.test(k) ? '[REDACTED]' : v;
  }
  process.stderr.write(JSON.stringify({
    timestamp: new Date().toISOString(),
    level,
    event,
    component: 'prompt-enhancer-hook',
    ...sanitizedMeta,
  }) + '\n');
}

/**
 * Strict, payload-only model resolution. No settings/env/global read and no
 * guessed fallback: an unknown value returns null.
 */
function resolveSessionModel(payload) {
  const raw = typeof payload?.modelName === 'string' && payload.modelName.trim()
    ? payload.modelName
    : (typeof payload?.model === 'string' ? payload.model : '');
  const key = raw.trim().toLowerCase();
  if (!key) return null;
  return MODEL_LOOKUP.get(key) || null;
}

function checkTrigger(userInput, config) {
  if (!userInput || typeof userInput !== 'string') {
    return { triggered: false, draft: '', prefix: null };
  }

  const trimmed = userInput.trim();
  if (!trimmed) {
    return { triggered: false, draft: '', prefix: null };
  }

  const prefixes = config.triggerPrefixes || DEFAULT_CONFIG.triggerPrefixes;
  for (const prefix of prefixes) {
    if (trimmed.toLowerCase().startsWith(prefix.toLowerCase())) {
      return { triggered: true, draft: trimmed.slice(prefix.length).trim(), prefix };
    }
  }

  if (trimmed === '!enhance' || trimmed === '/enhance') {
    return { triggered: true, draft: '', prefix: trimmed };
  }

  if (config.autoEnhance) {
    if (trimmed.startsWith('raw:') || trimmed.startsWith('\\')) {
      return { triggered: false, draft: trimmed.replace(/^(raw:|\\)\s*/, ''), prefix: null };
    }
    return { triggered: true, draft: trimmed, prefix: '[auto]' };
  }

  return { triggered: false, draft: userInput, prefix: null };
}

function blockSubmission(reason, payload, meta = {}) {
  logStructured('INFO', 'enhance_blocked', { reason, ...meta });
  const original = typeof payload?.lastUserInput === 'string' ? payload.lastUserInput : '';
  const message = [
    `**Prompt Enhancer unavailable (${reason})**`,
    '',
    'The original message was NOT executed. No enhancement was applied, and the composer was not restored. Copy the original text below and resend it manually if you want it to run unchanged.',
    '',
    '--- original text ---',
    original,
    '--- end original text ---',
  ].join('\n');

  process.stdout.write(JSON.stringify({
    injectSteps: [{ errorMessage: { userMessage: message } }],
    terminationBehavior: 'terminate',
  }) + '\n');
  process.exit(0);
}

function noOp(event, meta = {}) {
  logStructured('INFO', event, meta);
  process.stdout.write(JSON.stringify({ injectSteps: [] }) + '\n');
  process.exit(0);
}

async function main() {
  const traceId = crypto.randomUUID();

  let inputBuffer = '';
  try {
    for await (const chunk of process.stdin) inputBuffer += chunk;
  } catch (err) {
    logStructured('ERROR', 'stdin_read_failed', { traceId, error: err.message });
  }

  inputBuffer = inputBuffer.replace(/^\uFEFF/, '').trim();
  if (!inputBuffer) noOp('empty_payload', { traceId });

  let payload;
  try {
    payload = JSON.parse(inputBuffer);
  } catch (err) {
    noOp('invalid_payload_json', { traceId, error: err.message });
  }

  const config = loadConfig();
  if (!config.enabled) noOp('disabled', { traceId });

  const { triggered, draft, prefix } = checkTrigger(payload.lastUserInput || '', config);
  if (!triggered) noOp('enhance_not_triggered', { traceId });

  if (!draft.trim()) blockSubmission('empty_draft', payload, { traceId, trigger: prefix });

  const selected = resolveSessionModel(payload);
  if (!selected) {
    blockSubmission('missing_or_invalid_owning_model', payload, {
      traceId,
      trigger: prefix,
      payloadModel: payload.modelName || payload.model || null,
    });
  }

  // The native print subprocess now has a supported strict-deny contract
  // (toolPermission + permissions.deny). The remaining blockers are the
  // PreInvocation contract itself: it cannot replace the submitted draft or
  // preserve the composer, and the owning-model payload is not guaranteed.
  // Do not spawn a model call here while the returned draft cannot be applied.
  blockSubmission('draft_replacement_contract_unsupported', payload, {
    traceId,
    trigger: prefix,
    selectedModel: selected.wire,
    selectedEffort: selected.effort,
  });
}

main().catch((err) => {
  logStructured('ERROR', 'fatal_uncaught_error', { error: err.message });
  process.stdout.write(JSON.stringify({ injectSteps: [] }) + '\n');
  process.exit(0);
});
