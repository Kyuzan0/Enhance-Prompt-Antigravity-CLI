import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import fs from 'node:fs/promises';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { TextDecoder, promisify } from 'node:util';
import {
  buildSystemPrompt,
  MAX_DRAFT_CHARS,
  sanitizeAndDisarmDraft,
} from '../../core-engine/prompt-enhancer.mjs';

const run = promisify(execFile);
const policy = JSON.stringify(JSON.parse(await fs.readFile(
  new URL('../../contracts/strict-deny-policy.json', import.meta.url), 'utf8',
)));
const nativeAttachment = /\[(?:Pasted text|Image)\s*#\d+|📄\s*#\d+/i;
const control = /[\x00-\x08\x0b\x0c\x0e-\x1f\x7f-\x9f\u202a-\u202e\u2066-\u2069]/u;
const boundary = /<\/?user_untrusted_draft>/i;
const utf8 = new TextDecoder('utf-8', { fatal: true });

function report(message) {
  process.stderr.write(`\n[agy-enhance] ${message}\n`);
}

function conservativeRewrite(draft) {
  if (draft.includes('\n') || !/^\p{Ll}[\p{L} ]+$/u.test(draft)) return draft;
  return draft[0].toUpperCase() + draft.slice(1) + '.';
}

async function nativeRewrite(draft, settings, signal, session) {
  const { disarmedText } = sanitizeAndDisarmDraft(draft);
  const instruction = buildSystemPrompt([]) + `\n\n<user_untrusted_draft>\n${disarmedText}\n</user_untrusted_draft>`;
  const args = ['--settings', policy, '-p', instruction, '--output-format', 'text', '--disable-slash-commands'];
  if (settings.model) args.push('--model', settings.model);

  const env = { ...process.env, PROMPT_ENHANCER_CONFIG: path.join(session, 'disabled-hook.json') };
  for (const name of Object.keys(env)) {
    if (name.startsWith('ANTIGRAVITY_') || name.startsWith('AGY_ENHANCE_')) delete env[name];
  }
  try {
    const { stdout } = await run(process.env.AGY_NATIVE_BIN, args, {
      env, signal, timeout: settings.timeoutSeconds * 1000, maxBuffer: 512 * 1024, killSignal: 'SIGKILL',
    });
    return stdout.trim().replace(/^```[^\r\n]*\r?\n([\s\S]+)\r?\n```$/, '$1').trim();
  } catch (error) {
    if (signal.aborted) throw new Error('Dibatalkan. Draft dipertahankan.');
    if (error.killed) throw new Error('Model melewati batas waktu. Draft dipertahankan.');
    throw new Error('Model native gagal. Cek folder trust, autentikasi, atau model di agy-enhance settings.');
  }
}

async function readDraft(file) {
  const info = await fs.lstat(file);
  if (!info.isFile() || info.size > 256 * 1024) throw new Error('Draft terlalu besar atau bukan berkas biasa.');
  const bytes = await fs.readFile(file);
  const raw = utf8.decode(bytes);
  if (control.test(raw)) throw new Error('Draft berisi karakter kontrol. Draft dipertahankan.');
  const draft = raw.replace(/\r\n/g, '\n').trim();
  if (nativeAttachment.test(draft) || (draft.match(/\n/g)?.length ?? 0) >= 4 && draft.length >= 1000) {
    throw new Error('Draft berisi lampiran atau paste panjang. Draft dipertahankan.');
  }
  if (Array.from(draft).length > MAX_DRAFT_CHARS) throw new Error('Draft terlalu panjang. Draft dipertahankan.');
  return { bytes, draft, raw, mode: info.mode & 0o777 };
}

async function commitDraft(file, original, reply, cancelled) {
  const newline = original.raw.includes('\r\n') ? '\r\n' : '\n';
  let content = reply.replace(/\r\n?/g, '\n') + (original.raw.match(/\r?\n$/) ? '\n' : '');
  if (newline === '\r\n') content = content.replaceAll('\n', newline);
  const temporary = path.join(path.dirname(file), `.agy-enhance-${randomUUID()}.tmp`);
  try {
    await fs.writeFile(temporary, content, { flag: 'wx', mode: original.mode });
    if (!(await fs.readFile(file)).equals(original.bytes)) throw new Error('Draft berubah selama rewrite.');
    if (cancelled()) throw new Error('Dibatalkan. Draft dipertahankan.');
    await fs.rename(temporary, file);
  } finally {
    await fs.rm(temporary, { force: true });
  }
}

async function main(file, session) {
  const cancelFile = path.join(session, 'cancel');
  const controller = new AbortController();
  let cancelled = false;
  const checkCancel = async () => {
    if (await fs.stat(cancelFile, { throwIfNoEntry: false }).catch(() => null)) {
      cancelled = true;
      controller.abort();
    }
  };
  const poll = setInterval(() => void checkCancel(), 75);
  try {
    const settings = JSON.parse(await fs.readFile(path.join(session, 'settings.json'), 'utf8'));
    const original = await readDraft(file);
    if (!original.draft || settings.mode === 'off') return;
    await checkCancel();
    if (cancelled) return;
    report('Menulis ulang draft… Ctrl+E atau Ctrl+C untuk membatalkan.');
    const { markers } = sanitizeAndDisarmDraft(original.draft);
    const reply = settings.mode === 'native'
      ? await nativeRewrite(original.draft, settings, controller.signal, session)
      : conservativeRewrite(original.draft);
    if (reply === original.draft) return;
    if (!reply || control.test(reply) || boundary.test(reply)
        || nativeAttachment.test(reply) || Array.from(reply).length > MAX_DRAFT_CHARS
        || markers.some(marker => !reply.includes(marker))) {
      throw new Error('Hasil rewrite tidak valid atau menghilangkan referensi. Draft dipertahankan.');
    }
    await checkCancel();
    await commitDraft(file, original, reply, () => cancelled || existsSync(cancelFile));
  } finally {
    clearInterval(poll);
  }
}

// Exit successfully so agy can reopen the untouched draft after any refusal.
try {
  await main(process.argv[2], process.env.AGY_ENHANCE_SESSION);
} catch (error) {
  report(error instanceof Error && !error.code ? error.message : 'Draft gagal diproses dan dipertahankan.');
}
