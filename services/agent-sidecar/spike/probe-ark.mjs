#!/usr/bin/env node
/**
 * Probe which model id the current ARK_API_KEY can actually call on Ark's
 * OpenAI-compatible endpoint, and whether the DeepSeek-specific request fields
 * (`thinking`, `reasoning_effort`) are accepted. Never prints the key.
 */
const BASE = 'https://ark.cn-beijing.volces.com/api/v3'
const KEY = process.env.ARK_API_KEY
if (!KEY) { console.error('ARK_API_KEY missing'); process.exit(2) }
const AUTH = { authorization: `Bearer ${KEY}`, 'content-type': 'application/json' }

async function listModels() {
  try {
    const r = await fetch(`${BASE}/models`, { headers: AUTH })
    const text = await r.text()
    console.log(`GET /models -> HTTP ${r.status}`)
    if (r.ok) {
      try {
        const j = JSON.parse(text)
        const ids = (j.data ?? []).map(m => m.id)
        console.log(`  models (${ids.length}): ${ids.slice(0, 60).join(', ')}`)
      } catch { console.log('  (non-JSON body)') }
    } else {
      console.log(`  body: ${text.slice(0, 300)}`)
    }
  } catch (e) { console.log(`GET /models threw: ${String(e)}`) }
}

async function tryModel(model, extra = {}) {
  const body = {
    model,
    messages: [{ role: 'user', content: '用三个字打个招呼' }],
    max_tokens: 64,
    stream: false,
    ...extra,
  }
  try {
    const r = await fetch(`${BASE}/chat/completions`, { method: 'POST', headers: AUTH, body: JSON.stringify(body) })
    const text = await r.text()
    let note = ''
    if (r.ok) {
      try {
        const j = JSON.parse(text)
        const msg = j.choices?.[0]?.message
        note = `OK reply=${JSON.stringify(msg?.content)?.slice(0, 60)} reasoning=${msg?.reasoning_content ? 'yes' : 'no'} usage=${JSON.stringify(j.usage)}`
      } catch { note = 'OK (non-JSON)' }
    } else {
      try {
        const j = JSON.parse(text)
        note = `code=${j.error?.code} type=${j.error?.type} msg=${(j.error?.message ?? '').slice(0, 160)}`
      } catch { note = text.slice(0, 200) }
    }
    console.log(`POST chat model=${model}${extra.thinking ? ' +thinking' : ''}${extra.reasoning_effort ? ' +reasoning_effort' : ''} -> HTTP ${r.status} | ${note}`)
    return r.ok
  } catch (e) {
    console.log(`POST chat model=${model} threw: ${String(e)}`)
    return false
  }
}

const CANDIDATES = [
  'deepseek-v4-flash',
  'deepseek-v3-1-250821',
  'deepseek-v3-250324',
  'deepseek-v3',
  'deepseek-v3-1',
  'deepseek-r1-250528',
  'deepseek-r1',
  'deepseek-chat',
  'doubao-seed-1-6-250615',
  'doubao-1-5-pro-32k-250115',
]

console.log('=== Ark model discovery ===')
await listModels()
console.log('\n=== plain chat (no thinking / no reasoning_effort) ===')
let firstWorking
for (const m of CANDIDATES) {
  const ok = await tryModel(m)
  if (ok && !firstWorking) firstWorking = m
}
if (firstWorking) {
  console.log(`\n=== param compatibility on working model "${firstWorking}" ===`)
  await tryModel(firstWorking, { thinking: { type: 'enabled' }, reasoning_effort: 'max' })
  await tryModel(firstWorking, { reasoning_effort: 'high' })
  await tryModel(firstWorking, { thinking: { type: 'enabled' } })
} else {
  console.log('\nNo candidate model worked with this key.')
}
