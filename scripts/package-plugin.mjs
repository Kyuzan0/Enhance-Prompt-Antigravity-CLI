#!/usr/bin/env node
/**
 * Zero-dependency cross-platform plugin ZIP packager for agy-cli.
 * Reads package.json & plugin/plugin.json, removes stale agy-cli-plugin-*.zip
 * archives, and builds agy-cli-plugin-<version>.zip.
 */

import fs from 'node:fs';
import path from 'node:path';
import zlib from 'node:zlib';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const rootDir = path.resolve(__dirname, '..');

const CRC_TABLE = new Uint32Array(256);
for (let i = 0; i < 256; i++) {
  let c = i;
  for (let k = 0; k < 8; k++) {
    c = (c & 1) ? (0xedb88320 ^ (c >>> 1)) : (c >>> 1);
  }
  CRC_TABLE[i] = c >>> 0;
}

function crc32(buf) {
  let c = 0xffffffff;
  for (let i = 0; i < buf.length; i++) {
    c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8);
  }
  return (c ^ 0xffffffff) >>> 0;
}

function toDosTimeDate(date) {
  const year = Math.max(1980, date.getFullYear());
  const dosTime =
    ((date.getHours() & 0x1f) << 11) |
    ((date.getMinutes() & 0x3f) << 5) |
    ((date.getSeconds() >> 1) & 0x1f);
  const dosDate =
    (((year - 1980) & 0x7f) << 9) |
    (((date.getMonth() + 1) & 0x0f) << 5) |
    (date.getDate() & 0x1f);
  return { dosTime, dosDate };
}

function buildZipBuffer(entries) {
  const localChunks = [];
  const centralChunks = [];
  let offset = 0;

  for (const item of entries) {
    const nameBuf = Buffer.from(item.name, 'utf8');
    const raw = Buffer.isBuffer(item.data) ? item.data : Buffer.from(item.data, 'utf8');
    const compressed = zlib.deflateRawSync(raw, { level: 9 });
    const useStore = compressed.length >= raw.length;
    const payload = useStore ? raw : compressed;
    const method = useStore ? 0 : 8;
    const crc = crc32(raw);
    const { dosTime, dosDate } = toDosTimeDate(item.mtime || new Date());

    const localHeader = Buffer.alloc(30);
    localHeader.writeUInt32LE(0x04034b50, 0);
    localHeader.writeUInt16LE(20, 4);
    localHeader.writeUInt16LE(0x0800, 6);
    localHeader.writeUInt16LE(method, 8);
    localHeader.writeUInt16LE(dosTime, 10);
    localHeader.writeUInt16LE(dosDate, 12);
    localHeader.writeUInt32LE(crc, 14);
    localHeader.writeUInt32LE(payload.length, 18);
    localHeader.writeUInt32LE(raw.length, 22);
    localHeader.writeUInt16LE(nameBuf.length, 26);
    localHeader.writeUInt16LE(0, 28);

    localChunks.push(localHeader, nameBuf, payload);

    const centralHeader = Buffer.alloc(46);
    centralHeader.writeUInt32LE(0x02014b50, 0);
    centralHeader.writeUInt16LE(20, 4);
    centralHeader.writeUInt16LE(20, 6);
    centralHeader.writeUInt16LE(0x0800, 8);
    centralHeader.writeUInt16LE(method, 10);
    centralHeader.writeUInt16LE(dosTime, 12);
    centralHeader.writeUInt16LE(dosDate, 14);
    centralHeader.writeUInt32LE(crc, 16);
    centralHeader.writeUInt32LE(payload.length, 20);
    centralHeader.writeUInt32LE(raw.length, 24);
    centralHeader.writeUInt16LE(nameBuf.length, 28);
    centralHeader.writeUInt16LE(0, 30);
    centralHeader.writeUInt16LE(0, 32);
    centralHeader.writeUInt16LE(0, 34);
    centralHeader.writeUInt16LE(0, 36);
    centralHeader.writeUInt32LE(0, 38);
    centralHeader.writeUInt32LE(offset, 42);

    centralChunks.push(centralHeader, nameBuf);
    offset += localHeader.length + nameBuf.length + payload.length;
  }

  const centralBuf = Buffer.concat(centralChunks);
  const eocd = Buffer.alloc(22);
  eocd.writeUInt32LE(0x06054b50, 0);
  eocd.writeUInt16LE(0, 4);
  eocd.writeUInt16LE(0, 6);
  eocd.writeUInt16LE(entries.length, 8);
  eocd.writeUInt16LE(entries.length, 10);
  eocd.writeUInt32LE(centralBuf.length, 12);
  eocd.writeUInt32LE(offset, 16);
  eocd.writeUInt16LE(0, 20);

  return Buffer.concat([...localChunks, centralBuf, eocd]);
}

function main() {
  const args = new Set(process.argv.slice(2));
  const keepOld = args.has('--keep-old');

  const rootPkg = JSON.parse(fs.readFileSync(path.join(rootDir, 'package.json'), 'utf8'));
  const pluginPkg = JSON.parse(fs.readFileSync(path.join(rootDir, 'plugin', 'plugin.json'), 'utf8'));
  if (rootPkg.version !== pluginPkg.version) {
    throw new Error(`Version mismatch: package.json (${rootPkg.version}) != plugin/plugin.json (${pluginPkg.version})`);
  }
  const version = rootPkg.version;
  const outName = `agy-cli-plugin-${version}.zip`;
  const outPath = path.join(rootDir, outName);

  if (!keepOld) {
    for (const f of fs.readdirSync(rootDir)) {
      if (f.startsWith('agy-cli-plugin-') && f.endsWith('.zip') && f !== outName) {
        fs.rmSync(path.join(rootDir, f), { force: true });
        console.log(`[package-plugin] Removed stale artifact: ${f}`);
      }
    }
  }

  const filesToPack = [
    { src: path.join(rootDir, 'plugin', 'plugin.json'), dest: 'prompt-enhancer/plugin.json' },
    { src: path.join(rootDir, 'plugin', 'hooks.json'), dest: 'prompt-enhancer/hooks.json' },
    { src: path.join(rootDir, 'plugin', 'config.json'), dest: 'prompt-enhancer/config.json' },
    { src: path.join(rootDir, 'plugin', 'config.example.json'), dest: 'prompt-enhancer/config.example.json' },
    { src: path.join(rootDir, 'plugin', 'hook.mjs'), dest: 'prompt-enhancer/hook.mjs' },
    { src: path.join(rootDir, 'plugin', 'install-plugin.ps1'), dest: 'prompt-enhancer/install-plugin.ps1' },
    { src: path.join(rootDir, 'plugin', 'skills', 'enhance', 'SKILL.md'), dest: 'prompt-enhancer/skills/enhance/SKILL.md' },
    { src: path.join(rootDir, 'plugin', 'README.md'), dest: 'prompt-enhancer/README.md' },
    { src: path.join(rootDir, 'LICENSE'), dest: 'prompt-enhancer/LICENSE' },
    { src: path.join(rootDir, 'CHANGELOG.md'), dest: 'prompt-enhancer/CHANGELOG.md' },
  ];

  const entries = [];
  for (const item of filesToPack) {
    if (!fs.existsSync(item.src)) {
      throw new Error(`Required plugin file missing: ${item.src}`);
    }
    const stat = fs.statSync(item.src);
    entries.push({
      name: item.dest,
      data: fs.readFileSync(item.src),
      mtime: stat.mtime,
    });
  }

  const zipBuf = buildZipBuffer(entries);
  fs.writeFileSync(outPath, zipBuf);
  const sha256 = crypto.createHash('sha256').update(zipBuf).digest('hex');
  console.log(`[package-plugin] Built ${outName} (${zipBuf.length} bytes, ${entries.length} entries)`);
  console.log(`[package-plugin] SHA-256: ${sha256}`);
  for (const e of entries) {
    console.log(`  - ${e.name}`);
  }
}

main();
