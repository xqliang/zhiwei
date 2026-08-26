#!/usr/bin/env node
/**
 * OFFLINE dispatch check for the `session/cancel` JSON-RPC method that our
 * pnpm patch adds to `@deepseek-ai/dsh-sdk-jsonrpc-server`
 * (patches/@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch).
 *
 * WHY this exists / what it can and cannot prove
 * ----------------------------------------------
 * A true end-to-end test (send `session/prompt`, then `session/cancel`
 * mid-turn, and observe a `session.event` turn/end `aborted` + status `idle`)
 * needs a real Ark API key and a live model turn — we cannot do that in CI.
 *
 * What we CAN verify with zero network and zero Ark:
 *   1. the patched module still loads (import succeeds);
 *   2. `handleRequest("session/cancel", ...)` is wired into the switch and
 *      routes to the new `cancel()` method;
 *   3. an UNKNOWN / not-yet-created session returns a safe `{ ok:false }` and
 *      never throws (so it can never crash the sidecar process);
 *   4. a KNOWN session drives the real runtime call `agent.cancel({kind:'user'})`
 *      and returns `{ ok:true, cancelled:true }`;
 *   5. a disposed-out-of-band agent returns a safe `{ ok:false }`;
 *   6. the pre-existing `default` branch still throws on an unknown METHOD
 *      (we did not break the original dispatch contract).
 *
 * We instantiate the REAL exported `HarnessSdkJsonRpcServer` class against tiny
 * stubs for the two collaborators the constructor and `cancel()` touch:
 *   - ctx:       needs `on(event, handler) -> disposer` (constructor subscribes
 *                to 4 lifecycle events) and `agents.get(id)` (cancel() re-checks
 *                the agent is still the registered one, mirroring prompt()).
 *   - transport: needs `notify()` (only called from the lifecycle subscriptions,
 *                which never fire here).
 *
 * Usage:  node services/agent-sidecar/spike/check-session-cancel.mjs
 * Exit code 0 = all assertions passed; 1 = a check failed.
 */

import { HarnessSdkJsonRpcServer } from '@deepseek-ai/dsh-sdk-jsonrpc-server'

let failures = 0
/** Tiny assert helper: records + prints PASS/FAIL, never throws. */
function check(label, cond, detail) {
  const ok = Boolean(cond)
  if (!ok) failures++
  const tag = ok ? 'PASS' : 'FAIL'
  console.log(`[${tag}] ${label}${detail === undefined ? '' : `  -> ${JSON.stringify(detail)}`}`)
}

// --- stub collaborators ------------------------------------------------------
// The ctx.agents registry: maps SessionId (a branded string) -> agent handle.
const agentsRegistry = new Map()
const ctx = {
  // Constructor calls ctx.on(...) four times and stores the returned disposers.
  on: () => () => {},
  agents: { get: (id) => agentsRegistry.get(String(id)) },
}
// transport.notify is only invoked from lifecycle-event subscriptions (which do
// not fire in this offline check); a no-op is enough.
const transport = { notify: () => {} }

const server = new HarnessSdkJsonRpcServer(ctx, transport, {})

// --- 1) unknown session: safe ok:false, no throw -----------------------------
const rUnknown = await server.handleRequest('session/cancel', { sessionId: 'does-not-exist' })
check('unknown session -> ok:false, cancelled:false', rUnknown && rUnknown.ok === false && rUnknown.cancelled === false, rUnknown)

// --- 2) missing / invalid sessionId param: safe ok:false ---------------------
const rNoParam = await server.handleRequest('session/cancel', {})
check('missing sessionId -> ok:false', rNoParam && rNoParam.ok === false, rNoParam)

// --- 3) known session: drives agent.cancel({kind:'user'}), returns ok:true ---
let cancelledCause = null
const SID = 'session-under-test'
const fakeAgent = {
  id: SID, // SessionId is an identity brand (a plain string at runtime).
  cancel(cause) { cancelledCause = cause },
}
agentsRegistry.set(SID, fakeAgent)
// The server keys `this.sessions` by the RAW sessionId string (see prompt()).
server.sessions.set(SID, { handle: { agent: fakeAgent } })
const rKnown = await server.handleRequest('session/cancel', { sessionId: SID })
check('known session -> ok:true, cancelled:true', rKnown && rKnown.ok === true && rKnown.cancelled === true, rKnown)
check('agent.cancel called with {kind:"user"}', cancelledCause && cancelledCause.kind === 'user', cancelledCause)

// --- 4) agent disposed out-of-band: safe ok:false ----------------------------
// Session record still present, but the agent is no longer the registered one.
agentsRegistry.delete(SID)
const rDisposed = await server.handleRequest('session/cancel', { sessionId: SID })
check('disposed-out-of-band agent -> ok:false', rDisposed && rDisposed.ok === false, rDisposed)

// --- 5) unknown METHOD still throws (original contract preserved) -------------
let unknownMethodThrew = false
try {
  await server.handleRequest('bogus/method', {})
} catch {
  unknownMethodThrew = true
}
check('unknown METHOD still throws (default branch intact)', unknownMethodThrew)

// --- summary -----------------------------------------------------------------
console.log(failures === 0 ? '\nALL CHECKS PASSED' : `\n${failures} CHECK(S) FAILED`)
process.exit(failures === 0 ? 0 : 1)
