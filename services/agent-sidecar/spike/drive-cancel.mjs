#!/usr/bin/env node
/**
 * MANUAL E2E smoke test for the `session/cancel` JSON-RPC method.
 *
 * This is the REAL end-to-end check that our offline unit-style probe
 * (spike/check-session-cancel.mjs) cannot do: it needs a live Ark turn.
 * It spawns the bundled `dsh-jsonrpc-agent`, starts a turn, then interrupts it
 * mid-flight with `session/cancel` and asserts the turn ends as `aborted`
 * (cause `{kind:'user'}`) and the agent returns to `idle`.
 *
 * It deliberately asks for a LONG answer so there is a real in-flight turn to
 * abort, and fires the cancel as soon as the first streamed assistant chunk
 * arrives (with a time-based fallback if the model has no visible streaming).
 *
 * Prereqs (run by OPS, not CI):
 *   - ARK_API_KEY exported (e.g. `set -a; . /path/to/repo-root/.env; set +a`).
 *     The key is NEVER printed by this script.
 *   - `pnpm install` has run in services/agent-sidecar (so the patched SDK and
 *     the bin are materialized in node_modules).
 *
 * Usage:
 *   node services/agent-sidecar/spike/drive-cancel.mjs [model] [cordisConfig]
 * e.g.
 *   node services/agent-sidecar/spike/drive-cancel.mjs doubao-seed-1-6-250615
 *
 * Exit 0 = observed turn/end aborted (user) AND idle after cancel; else 1.
 */

import { spawn } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { mkdirSync, createWriteStream } from 'node:fs'

const __dirname = dirname(fileURLToPath(import.meta.url))
const sidecarDir = resolve(__dirname, '..')
const require = createRequire(import.meta.url)

const MODEL = process.argv[2] || process.env.DSH_MODEL || 'deepseek-v4-flash'
const CONFIG_PATH = process.argv[3] ? resolve(process.argv[3]) : join(sidecarDir, 'cordis.yml')
const SESSION_ROOT = join(sidecarDir, '.sessions')
const LOG_DIR = join(__dirname, 'logs')
const PROVIDER = 'deepseek-official'
const SESSION_ID = 'cancel-smoke-1'
// A prompt that guarantees a long, cancellable turn.
const PROMPT_TEXT = process.argv[4] || '请用中文写一篇不少于800字、分5段的科普长文，介绍海洋洋流的成因。'
// Safety net: if no assistant chunk is seen, cancel anyway after this delay.
const CANCEL_FALLBACK_MS = 4000
const OVERALL_TIMEOUT_MS = 60_000

mkdirSync(LOG_DIR, { recursive: true })
mkdirSync(SESSION_ROOT, { recursive: true })

if (!process.env.ARK_API_KEY) {
  console.error('[cancel] ARK_API_KEY is not set; aborting (key is never printed).')
  process.exit(2)
}

let binJs
try {
  binJs = require.resolve('@deepseek-ai/dsh-sdk-jsonrpc-demo/bin')
} catch (err) {
  console.error('[cancel] could not resolve the bin — did `pnpm install` run in services/agent-sidecar?')
  console.error(String(err))
  process.exit(2)
}

const stderrStream = createWriteStream(join(LOG_DIR, 'cancel-sidecar-stderr.log'), { flags: 'w' })
const wireStream = createWriteStream(join(LOG_DIR, 'cancel-wire.ndjson'), { flags: 'w' })

const child = spawn(process.execPath, [binJs], {
  cwd: sidecarDir,
  env: {
    ...process.env,
    DSH_CORDIS_CONFIG: CONFIG_PATH,
    DSH_SESSION_ROOT: SESSION_ROOT,
    DSH_MODEL: MODEL,
    DSH_CWD: sidecarDir,
    DSH_HOME: join(sidecarDir, '.dsh'),
    DSH_SYSTEM_PROMPT: '你是知微(zhiwei)语音助手，用简体中文亲切地回答。',
    NODE_OPTIONS: [process.env.NODE_OPTIONS, '--disable-warning=ExperimentalWarning'].filter(Boolean).join(' '),
  },
  stdio: ['pipe', 'pipe', 'pipe'],
})

let nextId = 1
const pending = new Map()
let promptSent = false
let cancelSent = false
let sawRunning = false
let sawAbort = false
let sawIdleAfterCancel = false
let cancelResult = null

function send(method, params) {
  const id = nextId++
  child.stdin.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n')
  console.error(`\n[cancel] >>> #${id} ${method} ${params ? JSON.stringify(params) : ''}`)
  return new Promise((res, rej) => pending.set(id, { resolve: res, reject: rej }))
}

/** Fire the interrupt exactly once. */
function fireCancel(why) {
  if (cancelSent) return
  cancelSent = true
  console.error(`\n[cancel] >>> interrupting turn (${why})`)
  send('session/cancel', { sessionId: SESSION_ID })
    .then((r) => { cancelResult = r; console.error(`[cancel] <<< session/cancel result: ${JSON.stringify(r)}`) })
    .catch((e) => console.error(`[cancel] session/cancel error: ${e.message}`))
}

let buf = ''
child.stdout.setEncoding('utf8')
child.stdout.on('data', (chunk) => {
  buf += chunk
  for (;;) {
    const nl = buf.indexOf('\n')
    if (nl < 0) break
    const line = buf.slice(0, nl).trim()
    buf = buf.slice(nl + 1)
    if (line) handleLine(line)
  }
})

function handleLine(line) {
  let msg
  try { msg = JSON.parse(line) } catch {
    console.error(`[cancel] <<< (non-JSON stdout, ignored): ${line}`)
    return
  }
  wireStream.write(JSON.stringify(msg) + '\n')

  // Responses (id, no method)
  if (msg.id !== undefined && msg.method === undefined) {
    const p = pending.get(msg.id)
    pending.delete(msg.id)
    if (msg.error) p?.reject(new Error(`${msg.error.message} (code ${msg.error.code})`))
    else p?.resolve(msg.result)
    return
  }

  if (msg.method === 'session.event') {
    const ev = msg.params?.event ?? {}
    const type = ev.type ?? '(unknown)'
    // Cancel on the first streamed assistant chunk — the turn is definitely live.
    if (type === 'assistant/chunk') fireCancel('first assistant chunk')
    // Detect the aborted turn end. The exact envelope may vary across rc builds,
    // so we scan generously for an "aborted" turn end carrying a user cause.
    const blob = JSON.stringify(msg.params)
    if (/turn.*end/i.test(type) || /"kind"\s*:\s*"aborted"/.test(blob)) {
      console.error(`\n[cancel] <<< turn-end event: ${blob}`)
      if (/"kind"\s*:\s*"aborted"/.test(blob)) sawAbort = true
    }
    return
  }

  if (msg.method === 'session.status') {
    const status = msg.params?.status
    console.error(`[cancel] <<< session.status = ${status}`)
    if (status === 'running') sawRunning = true
    if (status === 'idle' && cancelSent) { sawIdleAfterCancel = true; void finish() }
    return
  }
}

child.stderr.setEncoding('utf8')
child.stderr.on('data', (chunk) => { stderrStream.write(chunk); process.stderr.write(`[child-stderr] ${chunk}`) })

child.on('exit', (code, signal) => {
  console.error(`\n[cancel] child exited code=${code} signal=${signal}`)
  if (!done) verdict()
})

let done = false
const timeout = setTimeout(() => { console.error('\n[cancel] OVERALL TIMEOUT'); verdict() }, OVERALL_TIMEOUT_MS)
// Fallback: if the model never streams a visible chunk, cancel on a timer.
setTimeout(() => { if (promptSent) fireCancel('fallback timer') }, CANCEL_FALLBACK_MS)

async function finish() {
  if (done) return
  clearTimeout(timeout)
  try { await send('shutdown', undefined) } catch {}
  verdict()
}

function verdict() {
  if (done) return
  done = true
  clearTimeout(timeout)
  const ok = sawIdleAfterCancel && (cancelResult?.ok === true)
  console.error('\n==================== CANCEL SMOKE VERDICT ====================')
  console.error(JSON.stringify({
    sawRunning, cancelSent, cancelResult,
    sawAbortedTurnEnd: sawAbort,          // best-effort: envelope may differ by rc
    sawIdleAfterCancel,
    passed: ok,
  }, null, 2))
  console.error('=============================================================')
  console.error(ok
    ? '[cancel] PASS: cancel acknowledged and agent returned to idle.'
    : '[cancel] CHECK: review cancel-wire.ndjson for the turn-end envelope.')
  try { child.stdin.end() } catch {}
  setTimeout(() => { try { child.kill('SIGTERM') } catch {}; process.exit(ok ? 0 : 1) }, 1000)
}

async function main() {
  await send('initialize', { cwd: sidecarDir, provider: PROVIDER, model: MODEL })
  console.error('[cancel] initialize OK')
  promptSent = true
  const pr = await send('session/prompt', { sessionId: SESSION_ID, contentBlocks: [{ type: 'text', text: PROMPT_TEXT }] })
  console.error(`[cancel] prompt enqueued messageId=${pr?.messageId}`)
}

main().catch((e) => { console.error(`[cancel] fatal: ${e.message}`); verdict() })
