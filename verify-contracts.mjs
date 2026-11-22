import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));

const modelCatalog = JSON.parse(
  fs.readFileSync(path.join(__dirname, 'contracts', 'model-catalog.json'), 'utf8')
);
const strictPolicy = JSON.parse(
  fs.readFileSync(path.join(__dirname, 'contracts', 'strict-deny-policy.json'), 'utf8')
);

const strictTargets = [
  path.join(__dirname, 'legacy-daemon', 'enhance.mjs'),
  path.join(__dirname, 'wrapper', 'enhancer.go'),
];

for (const filePath of strictTargets) {
  const src = fs.readFileSync(filePath, 'utf8');
  assert.ok(
    src.includes(strictPolicy.toolPermission),
    `Missing toolPermission "${strictPolicy.toolPermission}" in ${filePath}`
  );
  for (const rule of strictPolicy.permissions.deny) {
    assert.ok(src.includes(rule), `Missing deny rule "${rule}" in ${filePath}`);
  }
}

const failClosedHook = fs.readFileSync(
  path.join(__dirname, 'specs', 'fail-closed-hook', 'hook.mjs'),
  'utf8'
);
for (const entry of modelCatalog.canonicalModels) {
  assert.ok(
    failClosedHook.includes(entry.wire),
    `Missing canonical model "${entry.wire}" in specs/fail-closed-hook/hook.mjs`
  );
}

// Single-source SemVer & Kyuzan0 credit checks
const rootPkg = JSON.parse(fs.readFileSync(path.join(__dirname, 'package.json'), 'utf8'));
const pluginPkg = JSON.parse(fs.readFileSync(path.join(__dirname, 'plugin', 'plugin.json'), 'utf8'));
const wrapperVerSrc = fs.readFileSync(path.join(__dirname, 'wrapper', 'version.go'), 'utf8');
const changelogSrc = fs.readFileSync(path.join(__dirname, 'CHANGELOG.md'), 'utf8');

assert.equal(rootPkg.version, pluginPkg.version, 'package.json and plugin/plugin.json versions must match');
assert.ok(
  wrapperVerSrc.includes(`var WrapperVersion = "${rootPkg.version}"`),
  `wrapper/version.go must match version ${rootPkg.version}`
);
assert.ok(
  changelogSrc.includes(`## [${rootPkg.version}]`) || changelogSrc.includes('## [Unreleased]'),
  'CHANGELOG.md must exist with release headings'
);
assert.equal(rootPkg.author?.name, 'Kyuzan0', 'package.json author.name must be Kyuzan0');
assert.ok(String(pluginPkg.author || '').includes('Kyuzan0'), 'plugin/plugin.json author must include Kyuzan0');

console.log('CLI_CONTRACTS_VERIFIED_OK');
