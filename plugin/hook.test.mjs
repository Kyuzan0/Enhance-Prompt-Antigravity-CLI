import assert from 'node:assert/strict';
import { execFile } from 'node:child_process';
import { once } from 'node:events';
import { createServer } from 'node:http';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { promisify } from 'node:util';

const exec = promisify(execFile);
const hook = fileURLToPath(new URL('./hook.mjs', import.meta.url));
const defaultConfig = fileURLToPath(new URL('./config.json', import.meta.url));
const env = Object.fromEntries(Object.entries(process.env).filter(([name]) =>
  !/^(?:PROMPT_ENHANCER_|OPENAI_|ANTIGRAVITY_|AGY_AUTO_ENHANCE$)/i.test(name)
));

function runHook(input, overrides = {}) {
  const result = exec(process.execPath, [hook], {
    env: { ...env, PROMPT_ENHANCER_CONFIG: defaultConfig, ...overrides },
    timeout: 5000
  });
  result.child.stdin.end(input);
  return result;
}

test('the fallback keeps the original draft out of diagnostics', async () => {
  const draft = 'lanjutkan catatan AUDIT_SYNTHETIC_DRAFT';
  const result = await runHook(JSON.stringify({ lastUserInput: `! ${draft}` }));

  assert.match(result.stdout, /AUDIT_SYNTHETIC_DRAFT/);
  assert.match(result.stderr, /unresolved_reference_identity_fallback/);
  assert.doesNotMatch(result.stderr, /AUDIT_SYNTHETIC_DRAFT/);
});

test('malformed hook input is not echoed in diagnostics', async () => {
  const result = await runHook('{"lastUserInput":AUDIT_SYNTHETIC_INPUT}');

  assert.deepEqual(JSON.parse(result.stdout), { injectSteps: [] });
  assert.match(result.stderr, /stdin_json_invalid/);
  assert.doesNotMatch(result.stderr, /AUDIT_SYNTHETIC_INPUT/);
});

test('failed model requests report HTTP status without logging the response body', async () => {
  const server = createServer((request, response) => {
    request.resume();
    response.writeHead(429);
    response.end('AUDIT_SYNTHETIC_RESPONSE');
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');

  try {
    const result = await runHook(JSON.stringify({
      lastUserInput: '! rapikan judul laporan',
      modelName: 'gemini-3.8-flash-medium'
    }), {
      PROMPT_ENHANCER_API_URL: `http://127.0.0.1:${server.address().port}`,
      PROMPT_ENHANCER_MODE: 'llm'
    });

    assert.equal(JSON.parse(result.stdout).injectSteps.length, 1);
    assert.match(result.stderr, /fast_llm_failed_fallback_local/);
    assert.match(result.stderr, /"httpStatus":429/);
    assert.doesNotMatch(result.stderr, /AUDIT_SYNTHETIC_RESPONSE/);
  } finally {
    await new Promise(resolve => server.close(resolve));
  }
});
