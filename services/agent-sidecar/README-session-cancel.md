# `session/cancel` — interrupting an in-progress dsh turn

This sidecar drives the DeepSeek headless harness (`dsh`) as a JSON-RPC 2.0 /
stdio child process (bin `dsh-jsonrpc-agent`). The stock wire protocol only
answers `initialize`, `session/prompt`, and `shutdown`. We add a fourth method,
**`session/cancel`**, so the Go side can abort the turn that is currently
streaming for one session (e.g. the user hit "stop").

- **Wire request:** `{"jsonrpc":"2.0","id":N,"method":"session/cancel","params":{"sessionId":"<id>"}}`
- **Wire result:** `{"ok":true,"sessionId":"<id>","cancelled":true}` on a live
  session, or `{"ok":false,...,"reason":"<why>"}` when there is nothing to abort.
- **Effect:** the active turn ends as `aborted` (cause `{kind:'user'}`) and the
  agent returns to `idle`; a fresh `session/prompt` for the same `sessionId`
  starts a new turn as usual.

The Go side needs **no change** — same `binPath`, same config. `pnpm install`
replays the patch automatically (see "How it is landed").

---

## Why this is possible — the real SDK code path (verified in node_modules)

All packages are pinned to `0.1.1-rc.2`. Paths below are under
`services/agent-sidecar/node_modules/`.

1. **Dispatch switch** — `@deepseek-ai/dsh-sdk-jsonrpc-server/lib/index.js`,
   `HarnessSdkJsonRpcServer.handleRequest(method, params)` (a `switch` that, in
   the stock build, only knows `initialize | session/prompt | shutdown` and
   throws on `default`). This is the single place a new method must be wired in.

2. **From `sessionId` to the agent** — the server keeps
   `this.sessions: Map<string, { handle }>` keyed by the **raw** `sessionId`
   string. `prompt()` reaches the agent as `rec.handle.agent` and calls
   `rec.handle.agent.followup(message)`. It also guards with
   `this.ctx.agents.get(rec.handle.agent.id) !== rec.handle.agent` (agent
   disposed out-of-band). We reuse exactly this path for cancel.

3. **`Agent.cancel`** — `@deepseek-ai/dsh-agent/lib/types/runtime-types.d.ts`:
   ```ts
   // line 80
   cancel(cause: AgentCancelCause, options?: CancelOptions): void;
   ```
   Its doc (lines 73–76) is the robustness guarantee we rely on:
   > "Clear queued and steering work … and abort the active turn or between-turn
   > task. The first cause wins for that activity. **With no active activity,
   > cancellation is a no-op and does not arm later work.**"

   So calling `cancel()` on an idle agent is a safe no-op — it will not throw and
   will not queue anything.

4. **`AgentCancelCause`** — `@deepseek-ai/dsh-session/lib/types/types.d.ts`
   lines 118–127: `{ kind:'user' } | { kind:'parent' } | { kind:'hook', reason }
   | { kind:'disposed' }`. We pass `{ kind:'user' }`.

5. **`SessionId`** — `@deepseek-ai/dsh-session/lib/index.js:11` is an **identity
   brand** (`function SessionId(id){ return id }`, a compile-time cast, no runtime
   cost). So `this.sessions.get(sessionId)` and
   `ctx.agents.get(SessionId(sessionId))` key on the very same string.

6. **Transport safety** — `@deepseek-ai/dsh-sdk-protocol/lib/index.js`
   `JsonRpcLineTransport.handleIncomingRequest` wraps every handler in
   `try/catch` and turns a throw into a `-32603` error frame; it never crashes
   the process, and `write()` only ever emits JSON-RPC frames (stdout stays
   pure). `objectParams()` also guarantees `params` is always a plain object.

**No drift from the design brief was found** — the rc.2 code matches the
investigation (switch, `this.sessions`, `ctx.agents`, `Agent.cancel`).

---

## The change we add

Into `HarnessSdkJsonRpcServer` (see the committed patch for exact context):

```js
// new switch case, next to session/prompt:
case "session/cancel": return this.cancel(params);

// new method:
cancel(params) {
  const sessionId = params.sessionId;
  if (typeof sessionId !== "string" || sessionId.length === 0)
    return { ok: false, cancelled: false, reason: "session/cancel requires a non-empty string sessionId" };
  const rec = this.sessions.get(sessionId);
  if (rec === void 0)
    return { ok: false, sessionId, cancelled: false, reason: "unknown session" };
  const agent = rec.handle.agent;
  if (this.ctx.agents.get(agent.id) !== agent)
    return { ok: false, sessionId, cancelled: false, reason: "session agent was disposed outside the server" };
  agent.cancel({ kind: "user" });
  return { ok: true, sessionId, cancelled: true };
}
```

**Robustness:** unknown / not-yet-created session → safe `{ok:false}`; missing
or non-string `sessionId` → safe `{ok:false}`; agent disposed out-of-band →
safe `{ok:false}` (mirrors the `prompt()` guard); idle agent → runtime no-op,
returns `{ok:true, cancelled:true}` harmlessly; any unexpected throw becomes a
`-32603` error frame (never a process crash). Adds **no** stdout logging.

---

## How it is landed — pnpm `patchedDependencies` (NOT patch-package)

> **Deviation from the brief, on purpose.** The brief suggested `patch-package`
> + `npm ci` + a `postinstall`. This project is a **pnpm workspace** (`pnpm-lock.yaml`,
> `pnpm-workspace.yaml`, `.npmrc`) and `patch-package` is **not installed**.
> pnpm's own first-class patching (`pnpm patch`) is the mature, native
> equivalent here: it survives `pnpm install` / `pnpm install --frozen-lockfile`
> (the `npm ci` analog) with no extra dependency, no `postinstall` hook, and it
> handles pnpm's content-addressable store correctly (a naive in-place edit of a
> store-linked file can corrupt other projects). It also does **not** run git on
> this repo.

Committed artifacts (all tracked; `node_modules/` is gitignored):

- `patches/@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch` — the diff.
- `pnpm-workspace.yaml` — gains:
  ```yaml
  patchedDependencies:
    '@deepseek-ai/dsh-sdk-jsonrpc-server@0.1.1-rc.2': patches/@deepseek-ai__dsh-sdk-jsonrpc-server@0.1.1-rc.2.patch
  ```
- `pnpm-lock.yaml` — records the patch (`patchedDependencies:` + the resolved
  version now carries `(patch_hash=…)`), which is what makes a fresh install on
  another machine materialize the patched copy.

On any `pnpm install`, pnpm re-materializes the package from the store and
re-applies this patch. **The Go `binPath` is unchanged.**

### Re-generating / editing the patch later

```bash
cd services/agent-sidecar
pnpm patch '@deepseek-ai/dsh-sdk-jsonrpc-server@0.1.1-rc.2'   # prints an editable dir
#   …edit lib/index.js in that dir…
pnpm patch-commit '<the printed dir>'                          # rewrites the .patch + wires config
```

(If the store is warm you can prefix with `npm_config_offline=true` to avoid the
network, as was done here.)

---

## Verification

### Done here (offline — no Ark, no network)

- `node --check` on the patched `lib/index.js` → **SYNTAX_OK**.
- `node spike/check-session-cancel.mjs` → **ALL CHECKS PASSED**. It imports the
  **real patched** `HarnessSdkJsonRpcServer` and, against tiny stubs, asserts:
  unknown session → safe `ok:false`; missing `sessionId` → safe `ok:false`;
  known session → `ok:true` **and** `agent.cancel({kind:'user'})` was invoked;
  disposed-out-of-band agent → safe `ok:false`; unknown **method** still throws
  (original `default` contract preserved).
- `pnpm install --frozen-lockfile` (offline) → clean, "Already up to date"; the
  live `node_modules` copy still contains the new case afterward (patch replays
  and is lockfile-consistent).

### NOT verified here (needs a real Ark turn — ops must run it)

A true end-to-end interrupt (start a turn, cancel mid-stream, observe the
`aborted` turn end + `idle`) needs a live `ARK_API_KEY` and a real model turn,
which CI cannot provide.

**Manual E2E smoke (ops, from the repo root):**

```bash
# 1. Load the Ark key from the repo-root .env (key is never printed by the driver).
set -a; . ./.env; set +a          # provides ARK_API_KEY
# 2. Ensure deps are installed (replays the patch):
cd services/agent-sidecar && pnpm install --frozen-lockfile
# 3. Run the cancel smoke driver. Pass an Ark model this account has activated
#    (deepseek-v4-flash is NOT on this account; doubao-seed-1-6-* is — see
#    spike/probe-ark.mjs / project memory). Example:
node spike/drive-cancel.mjs doubao-seed-1-6-250615
```

Expected: the driver logs `session.status = running`, fires `session/cancel`
on the first streamed chunk, prints `session/cancel result: {"ok":true,…}`,
then logs `session.status = idle` and a `PASS` verdict. The raw frames are
saved to `spike/logs/cancel-wire.ndjson` — inspect the turn-end event there to
confirm the `aborted` cause (`{kind:'user'}`); the exact envelope shape is
build-dependent, so the driver scans for it best-effort rather than asserting a
fixed schema.

To sanity-check the wire without cancel, `spike/drive.mjs` runs a normal turn.
