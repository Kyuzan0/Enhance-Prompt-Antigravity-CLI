import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const hook = path.join(__dirname, 'hook.mjs');

function run(payload) {
  return spawnSync(process.execPath, [hook], {
    input: JSON.stringify(payload),
    encoding: 'utf8',
    timeout: 5000,
    env: { ...process.env, AGY_AUTO_ENHANCE: '0', PROMPT_ENHANCER_AUTO: '0' },
  });
}

function parse(stdout) {
  const line = stdout.trim();
  assert.ok(line, 'hook must emit one JSON line');
  return JSON.parse(line);
}

function assertBlocked(result, reason, original) {
  assert.equal(result.status, 0);
  const out = parse(result.stdout);
  assert.equal(out.terminationBehavior, 'terminate');
  assert.equal(out.injectSteps.length, 1);
  assert.equal(out.injectSteps[0].errorMessage.userMessage.includes(original), true);
  assert.match(out.injectSteps[0].errorMessage.userMessage, new RegExp(reason));
  assert.match(out.injectSteps[0].errorMessage.userMessage, /original message was NOT executed/i);
}

function assertAllowed(result) {
  assert.equal(result.status, 0);
  const out = parse(result.stdout);
  assert.deepEqual(out, { injectSteps: [] });
}

let passed = 0;
function test(name, fn) {
  fn();
  passed++;
  console.log(`  PASS ${name}`);
}

test('triggered missing owning model terminates before task execution', () => {
  const original = '!enhance hapus folder build lama';
  assertBlocked(run({ lastUserInput: original }), 'missing_or_invalid_owning_model', original);
});

test('triggered invalid owning model terminates before task execution', () => {
  const original = '!enhance jalankan migrasi database';
  assertBlocked(run({ lastUserInput: original, modelName: 'Gemini 3.5 Flash (Low)' }), 'missing_or_invalid_owning_model', original);
});

test('triggered valid model refuses because draft replacement is unsupported', () => {
  const original = '!enhance eksekusi perintah ini';
  const result = run({ lastUserInput: original, modelName: 'Gemini 3.8 Flash (Medium)' });
  assertBlocked(result, 'draft_replacement_contract_unsupported', original);
  assert.match(result.stderr, /"selectedModel":"gemini-3\.8-flash-medium"/);
});

test('triggered current Gemini 4 and Claude 5.5 models resolve against canonical catalog', () => {
  const g4 = run({ lastUserInput: '!enhance analisa codebase', modelName: 'Gemini 4 Hydrogen (Medium)' });
  assertBlocked(g4, 'draft_replacement_contract_unsupported', '!enhance analisa codebase');
  assert.match(g4.stderr, /"selectedModel":"gemini-4-hydrogen-medium"/);

  const c55 = run({ lastUserInput: '!enhance perbaiki test', modelName: 'Claude Opus 5.5 (High)' });
  assertBlocked(c55, 'draft_replacement_contract_unsupported', '!enhance perbaiki test');
  assert.match(c55.stderr, /"selectedModel":"claude-opus-5-5-high"/);
});

test('untriggered normal turn passes unchanged', () => {
  assertAllowed(run({ lastUserInput: 'halo dunia' }));
});

console.log(`\n${passed} hook tests passed`);
