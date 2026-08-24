#!/usr/bin/env node
/**
 * Spike driver: spawn the bundled deepseek-harness headless JSON-RPC agent
 * (`dsh-jsonrpc-agent`) as a child process, drive it over newline-delimited
 * JSON-RPC 2.0 on stdio, and capture the EXACT wire behavior of one text turn
 * against 火山方舟 Ark's deepseek-v4-flash.
 *
 * Protocol (from @deepseek-ai/dsh-sdk-protocol):
 *   - requests  (client->server): initialize, session/prompt, shutdown
 *   - responses (server->client): match by `id`
 *   - notifications (server->client, `method` only, no `id`):
 *       session.event      { sessionId, event: <SessionEvent envelope> }
 *       session.status     { sessionId, status: 'running' | 'idle' }
 *       subagent.started / subagent.finished
 *   Framing: one JSON object per line, `JSON.stringify(msg) + "\n"`.
 *
 * NOTHING here prints ARK_API_KEY. Child stderr is teed to a log file.
 *
 * Usage:  node services/agent-sidecar/spike/drive.mjs
 */

import { spawn } from 'node:child_process'
import { createRequire } from 'node:module'
import { fileURLToPath } from 'node:url'
import { dirname, join, resolve } from 'node:path'
import { mkdirSync, createWriteStream, writeFileSync } from 'node:fs'

const __dirname = dirname(fileURLToPath(import.meta.url))
const sidecarDir = resolve(__dirname, '..')            // services/agent-sidecar
const require = createRequire(import.meta.url)

// ---- config knobs -----------------------------------------------------------
// argv[2] = model, argv[3] = cordis config path, argv[4] = prompt text.
const CONFIG_PATH = process.argv[3] ? resolve(process.argv[3]) : join(sidecarDir, 'cordis.yml')
const SESSION_ROOT = join(sidecarDir, '.sessions')
const LOG_DIR = join(__dirname, 'logs')
const STDERR_LOG = join(LOG_DIR, 'sidecar-stderr.log')
const WIRE_LOG = join(LOG_DIR, 'wire.ndjson')          // every parsed frame, raw
const PROVIDER = 'deepseek-official'                   // the route llm-deepseek owns
// Task intent is 'deepseek-v4-flash'; override via argv[2] or DSH_MODEL to a
// model this Ark account has actually activated (e.g. doubao-seed-1-6-250615).
const MODEL = process.argv[2] || process.env.DSH_MODEL || 'deepseek-v4-flash'
const SESSION_ID = 'spike-1'
const PROMPT_TEXT = process.argv[4] || '用三个字打个招呼'
const TURN_TIMEOUT_MS = 90_000                         // Ark reasoning turns can be slow

mkdirSync(LOG_DIR, { recursive: true })
mkdirSync(SESSION_ROOT, { recursive: true })

// ---- preflight ---------------------------------------------------------------
if (!process.env.ARK_API_KEY) {
  console.error('[drive] ARK_API_KEY is not set in the environment; aborting (key is never printed).')
  process.exit(2)
}

// Resolve the published bin entry: package.json "exports"["./bin"] -> lib/bin.js
let binJs
try {
  binJs = require.resolve('@deepseek-ai/dsh-sdk-jsonrpc-demo/bin')
} catch (err) {
  console.error('[drive] could not resolve @deepseek-ai/dsh-sdk-jsonrpc-demo/bin — did `pnpm install` run in services/agent-sidecar?')
  console.error(String(err))
  process.exit(2)
}

const stderrStream = createWriteStream(STDERR_LOG, { flags: 'w' })
const wireStream = createWriteStream(WIRE_LOG, { flags: 'w' })

console.error(`[drive] bin:        ${binJs}`)
console.error(`[drive] config:     ${CONFIG_PATH}`)
console.error(`[drive] provider:   ${PROVIDER}`)
console.error(`[drive] model:      ${MODEL}`)
console.error(`[drive] stderr log: ${STDERR_LOG}`)
console.error(`[drive] wire  log:  ${WIRE_LOG}`)
console.error('[drive] spawning child...')

// ---- spawn -------------------------------------------------------------------
const child = spawn(process.execPath, [binJs], {
  cwd: sidecarDir,
  env: {
    ...process.env,                                    // ARK_API_KEY passthrough
    DSH_CORDIS_CONFIG: CONFIG_PATH,
    DSH_SESSION_ROOT: SESSION_ROOT,
    DSH_MODEL: MODEL,                                  // read by cordis.yml catalog
    DSH_CWD: sidecarDir,
    DSH_HOME: join(sidecarDir, '.dsh'),                // hermetic harness home
    DSH_SYSTEM_PROMPT: '你是知微(zhiwei)语音助手，用简体中文亲切地回答。',
    NODE_OPTIONS: [process.env.NODE_OPTIONS, '--disable-warning=ExperimentalWarning'].filter(Boolean).join(' '),
  },
  stdio: ['pipe', 'pipe', 'pipe'],
})

// ---- JSON-RPC plumbing -------------------------------------------------------
let nextId = 1
const pending = new Map()                              // id -> {resolve, reject}
const received = []                                    // every parsed frame (in order)
const assistantMessages = []                           // captured assistant/message text
const statusHistory = []                               // session.status transitions
const toolCalls = []                                   // captured tool/call events
const toolResults = []                                 // captured tool/result events
const eventTypeCounts = new Map()
let promptSent = false
let finished = false

function send(method, params) {
  const id = nextId++
  const frame = { jsonrpc: '2.0', id, method, params }
  child.stdin.write(JSON.stringify(frame) + '\n')
  console.error(`\n[drive] >>> request #${id} ${method}`)
  console.error(JSON.stringify(params, null, 2))
  return new Promise((res, rej) => pending.set(id, { resolve: res, reject: rej }))
}

function notify(method, params) {
  const frame = params === undefined ? { jsonrpc: '2.0', method } : { jsonrpc: '2.0', method, params }
  child.stdin.write(JSON.stringify(frame) + '\n')
  console.error(`\n[drive] >>> notify ${method}`)
}

function pretty(obj) {
  return JSON.stringify(obj, null, 2)
}

// ---- stdout: newline-delimited JSON frames ----------------------------------
let buf = ''
child.stdout.setEncoding('utf8')
child.stdout.on('data', (chunk) => {
  buf += chunk
  for (;;) {
    const nl = buf.indexOf('\n')
    if (nl < 0) break
    const line = buf.slice(0, nl).trim()
    buf = buf.slice(nl + 1)
    if (!line) continue
    handleLine(line)
  }
})

function handleLine(line) {
  let msg
  try {
    msg = JSON.parse(line)
  } catch {
    // Not a JSON-RPC frame (stray log line). Record it and move on.
    console.error(`[drive] <<< (non-JSON stdout, ignored): ${line}`)
    wireStream.write(`# NON-JSON: ${line}\n`)
    return
  }
  wireStream.write(JSON.stringify(msg) + '\n')
  received.push(msg)

  // Response (has id, no method)
  if (msg.id !== undefined && msg.method === undefined) {
    const p = pending.get(msg.id)
    pending.delete(msg.id)
    if (msg.error) {
      console.error(`\n[drive] <<< ERROR response #${msg.id}:`)
      console.error(pretty(msg.error))
      p?.reject(new Error(`${msg.error.message} (code ${msg.error.code})`))
    } else {
      console.error(`\n[drive] <<< response #${msg.id}:`)
      console.error(pretty(msg.result))
      p?.resolve(msg.result)
    }
    return
  }

  // Notification (method, no id)
  if (msg.method !== undefined) {
    onNotification(msg.method, msg.params)
  }
}

function onNotification(method, params) {
  if (method === 'session.event') {
    const ev = params?.event ?? {}
    const type = ev.type ?? '(unknown)'
    eventTypeCounts.set(type, (eventTypeCounts.get(type) ?? 0) + 1)

    // Print full JSON for the "interesting" structural events, and a compact
    // one-liner for the high-frequency streaming deltas.
    if (type === 'assistant/chunk') {
      const c = ev.data?.chunk ?? {}
      const bits = [c.type, c.blockType, c.index !== undefined ? `idx=${c.index}` : '', c.text !== undefined ? JSON.stringify(c.text) : '']
        .filter(Boolean).join(' ')
      console.error(`[drive] <<< session.event assistant/chunk  ${bits}`)
    } else {
      console.error(`\n[drive] <<< session.event  type=${type}`)
      console.error(pretty(params))
    }

    if (type === 'assistant/message') {
      const content = ev.data?.message?.content ?? []
      const text = content.filter(b => b.type === 'text').map(b => b.text).join('')
      const reasoning = content.filter(b => b.type === 'reasoning').map(b => b.text).join('')
      assistantMessages.push({ text, reasoning })
    }
    if (type === 'tool/call') {
      toolCalls.push({ callId: ev.data?.callId, name: ev.data?.name, arguments: ev.data?.arguments })
    }
    if (type === 'tool/result') {
      const rc = ev.data?.message?.content?.[0]
      toolResults.push({
        toolCallId: rc?.toolCallId,
        isError: rc?.isError,
        text: (rc?.content ?? []).filter(b => b.type === 'text').map(b => b.text).join(''),
      })
    }
    return
  }

  if (method === 'session.status') {
    statusHistory.push(params?.status)
    console.error(`\n[drive] <<< session.status  status=${params?.status}`)
    // A turn is "done" when the live agent returns to idle AFTER we prompted.
    if (promptSent && params?.status === 'idle' && !finished) {
      finished = true
      void finishTurn()
    }
    return
  }

  console.error(`\n[drive] <<< notification ${method}`)
  console.error(pretty(params))
}

// ---- child stderr -> log file (+ echo, since it is diagnostics not wire) ----
child.stderr.setEncoding('utf8')
child.stderr.on('data', (chunk) => {
  stderrStream.write(chunk)
  process.stderr.write(`[child-stderr] ${chunk}`)
})

child.on('exit', (code, signal) => {
  console.error(`\n[drive] child exited code=${code} signal=${signal}`)
  if (!finished) {
    printSummary('child-exited-before-idle')
    process.exit(code === 0 ? 1 : (code ?? 1))
  }
})

// ---- turn lifecycle ----------------------------------------------------------
const timeout = setTimeout(() => {
  console.error(`\n[drive] TIMEOUT after ${TURN_TIMEOUT_MS}ms without reaching idle.`)
  printSummary('timeout')
  try { child.kill('SIGKILL') } catch {}
  process.exit(1)
}, TURN_TIMEOUT_MS)

async function main() {
  try {
    const initResult = await send('initialize', { cwd: sidecarDir, provider: PROVIDER, model: MODEL })
    console.error('[drive] initialize OK.')
    void initResult
  } catch (err) {
    console.error(`[drive] initialize FAILED: ${err.message}`)
    printSummary('initialize-failed')
    clearTimeout(timeout)
    try { child.kill('SIGKILL') } catch {}
    process.exit(1)
  }

  try {
    promptSent = true
    const promptResult = await send('session/prompt', {
      sessionId: SESSION_ID,
      contentBlocks: [{ type: 'text', text: PROMPT_TEXT }],
    })
    console.error(`[drive] session/prompt enqueued: messageId=${promptResult?.messageId}`)
  } catch (err) {
    console.error(`[drive] session/prompt FAILED: ${err.message}`)
    printSummary('prompt-failed')
    clearTimeout(timeout)
    try { child.kill('SIGKILL') } catch {}
    process.exit(1)
  }
  // Now we wait for streamed session.event notifications and the idle status.
}

async function finishTurn() {
  clearTimeout(timeout)
  console.error('\n[drive] turn reached idle — sending shutdown.')
  try {
    await send('shutdown', undefined)
    console.error('[drive] shutdown acknowledged.')
  } catch (err) {
    console.error(`[drive] shutdown error (non-fatal): ${err.message}`)
  }
  printSummary('idle')
  // Close stdin; the bin also exits on stdin end / after shutdown.
  try { child.stdin.end() } catch {}
  setTimeout(() => { try { child.kill('SIGTERM') } catch {} ; process.exit(0) }, 1500)
}

function printSummary(reason) {
  const finalText = assistantMessages.map(m => m.text).join('').trim()
  const summary = {
    reason,
    reachedIdle: statusHistory.includes('idle') && promptSent,
    statusHistory,
    eventTypeCounts: Object.fromEntries(eventTypeCounts),
    assistantFinalText: finalText,
    assistantReasoningChars: assistantMessages.reduce((n, m) => n + (m.reasoning?.length ?? 0), 0),
    toolCalls,
    toolResults,
    totalFramesReceived: received.length,
  }
  console.error('\n==================== SPIKE SUMMARY ====================')
  console.error(pretty(summary))
  console.error('=======================================================')
  writeFileSync(join(LOG_DIR, 'summary.json'), pretty(summary) + '\n')
}

main()
