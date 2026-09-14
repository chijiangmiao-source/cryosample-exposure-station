<script setup>
import { onMounted, ref } from 'vue'
import { api } from './api'
import { isValidZ, nowZ } from './lib/time'
import { canRevokeEvent, eventTypeLabel, isEventRevoked } from './lib/derive'
import BatchCard from './components/BatchCard.vue'

const barcode = ref('')
const batch = ref(null)
const events = ref([])
const error = ref('')
const info = ref('')
const busy = ref(false)

// Event timestamp submitted with 取出/归还; prefilled with "now", editable
// so boundary conditions can be demonstrated and tested deterministically.
const eventTime = ref(nowZ())

// Inline revocation form for the latest non-revoked event: { id, at, reason }.
const revokeForm = ref(null)

// Create form (shown when the scanned barcode is unknown).
const showCreate = ref(false)
const createAllowed = ref(60)
const createCreatedAt = ref('')

async function refreshEvents() {
  if (!batch.value) return
  const res = await api.listEvents(batch.value.barcode)
  events.value = res.events
}

async function lookup() {
  error.value = ''
  info.value = ''
  showCreate.value = false
  revokeForm.value = null
  const code = barcode.value.trim()
  if (!code) {
    error.value = '请输入或扫描批次条码'
    return
  }
  busy.value = true
  try {
    batch.value = await api.getBatch(code)
    localStorage.setItem('lastBarcode', code)
    await refreshEvents()
    eventTime.value = nowZ()
  } catch (e) {
    batch.value = null
    events.value = []
    if (e.status === 404) {
      showCreate.value = true
      createCreatedAt.value = nowZ()
      info.value = `未找到批次「${code}」，可在下方创建`
    } else {
      error.value = `查询失败：${e.message}`
    }
  } finally {
    busy.value = false
  }
}

async function submitCreate() {
  error.value = ''
  info.value = ''
  const code = barcode.value.trim()
  if (!Number.isInteger(createAllowed.value) || createAllowed.value <= 0) {
    error.value = '允许暴露秒数必须是正整数'
    return
  }
  if (!isValidZ(createCreatedAt.value)) {
    error.value = '创建时刻必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    return
  }
  busy.value = true
  try {
    batch.value = await api.createBatch({
      barcode: code,
      allowedSeconds: createAllowed.value,
      createdAt: createCreatedAt.value
    })
    localStorage.setItem('lastBarcode', code)
    showCreate.value = false
    events.value = []
    revokeForm.value = null
    info.value = '批次已创建：柜内，累计 0 秒'
    eventTime.value = nowZ()
  } catch (e) {
    if (e.status === 409) {
      error.value = '该条码已存在，已为你载入现有批次'
      try {
        batch.value = await api.getBatch(code)
        showCreate.value = false
        await refreshEvents()
      } catch {
        /* keep the conflict message */
      }
    } else {
      error.value = `创建失败：${e.message}`
    }
  } finally {
    busy.value = false
  }
}

async function act(type) {
  error.value = ''
  info.value = ''
  if (!batch.value) return
  if (!isValidZ(eventTime.value)) {
    error.value = '事件时间必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    return
  }
  busy.value = true
  const at = eventTime.value
  try {
    batch.value = await api.postEvent(batch.value.barcode, { type, at })
    revokeForm.value = null
    await refreshEvents()
    info.value = type === 'takeout' ? '已取出（柜外计时开始）' : '已归还（本次暴露已计入）'
    // Only reset the field when it still holds the submitted value, so a
    // fast follow-up edit made while the request was in flight survives.
    if (eventTime.value === at) {
      eventTime.value = nowZ()
    }
  } catch (e) {
    // 409 etc.: show the conflict, then reload the authoritative state so a
    // racing/duplicate submission can never display stale numbers.
    error.value = `操作被拒绝（${e.code}）：${e.message}`
    try {
      batch.value = await api.getBatch(batch.value.barcode)
      await refreshEvents()
    } catch {
      /* keep previous state */
    }
  } finally {
    busy.value = false
  }
}

function canRevoke(ev) {
  return canRevokeEvent(ev, events.value)
}

function openRevoke(ev) {
  error.value = ''
  info.value = ''
  revokeForm.value = { id: ev.id, at: nowZ(), reason: '' }
}

function cancelRevoke() {
  revokeForm.value = null
  error.value = ''
}

async function submitRevoke() {
  error.value = ''
  info.value = ''
  if (!batch.value || !revokeForm.value) return
  const f = revokeForm.value
  if (!isValidZ(f.at)) {
    error.value = '撤销时刻必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    return
  }
  if (!f.reason.trim()) {
    error.value = '请填写非空撤销原因'
    return
  }
  busy.value = true
  try {
    const id = f.id
    batch.value = await api.revokeEvent(
      batch.value.barcode,
      id,
      { at: f.at, reason: f.reason.trim() }
    )
    revokeForm.value = null
    await refreshEvents()
    info.value = `已撤销事件 #${id}：批次已恢复到撤销前的累计与柜内外状态`
    eventTime.value = nowZ()
  } catch (e) {
    // 409 etc.: show the conflict, then reload the authoritative server state
    // so a rejected/racing undo never changes the displayed totals or state.
    error.value = `撤销被拒绝（${e.code}）：${e.message}`
    revokeForm.value = null
    try {
      batch.value = await api.getBatch(batch.value.barcode)
      await refreshEvents()
    } catch {
      /* keep previous state */
    }
  } finally {
    busy.value = false
  }
}

function resetEventTime() {
  eventTime.value = nowZ()
}

onMounted(async () => {
  const saved = localStorage.getItem('lastBarcode')
  if (saved) {
    barcode.value = saved
    await lookup()
  }
})
</script>

<template>
  <main class="page">
    <h1>冷冻库样本扫码计时站</h1>
    <p class="hint">扫描或输入批次条码，提交「取出 / 归还」。事件时间须为带 Z 的 RFC3339 整秒，且严格晚于该批次上一时刻。</p>

    <form class="scan" data-test="scan-form" @submit.prevent="lookup">
      <input
        v-model="barcode"
        data-test="barcode-input"
        placeholder="扫描 / 输入批次条码，回车查询"
        autofocus
      />
      <button type="submit" data-test="lookup-btn" :disabled="busy">查询</button>
    </form>

    <p v-if="error" class="banner error" data-test="error-banner">{{ error }}</p>
    <p v-if="info" class="banner info" data-test="info-banner">{{ info }}</p>

    <form v-if="showCreate" class="card create" data-test="create-form" @submit.prevent="submitCreate">
      <h2>创建新批次</h2>
      <label>
        条码
        <input :value="barcode.trim()" data-test="create-barcode" readonly />
      </label>
      <label>
        允许暴露秒数（正整数）
        <input v-model.number="createAllowed" data-test="create-allowed" type="number" min="1" step="1" />
      </label>
      <label>
        创建时刻（RFC3339 Z 整秒）
        <input v-model="createCreatedAt" data-test="create-created-at" />
      </label>
      <button type="submit" data-test="create-submit" :disabled="busy">创建批次</button>
    </form>

    <template v-if="batch">
      <div class="time-row">
        <label>
          事件时间
          <input v-model="eventTime" data-test="event-time" placeholder="2026-09-13T08:00:00Z" />
        </label>
        <button type="button" data-test="now-btn" @click="resetEventTime">设为当前时间</button>
      </div>

      <BatchCard :batch="batch" :busy="busy" @takeout="act('takeout')" @return="act('return')" />

      <section class="card" data-test="events-card">
        <h2>事件记录</h2>
        <table v-if="events.length" data-test="events-table">
          <thead>
            <tr><th>#</th><th>类型</th><th>时刻</th><th>本次暴露</th><th>撤销 / 审计</th></tr>
          </thead>
          <tbody>
            <template v-for="ev in events" :key="ev.id">
              <tr :data-test="`event-row-${ev.id}`" :class="{ revoked: isEventRevoked(ev) }">
                <td>{{ ev.id }}</td>
                <td>{{ eventTypeLabel(ev.type) }}</td>
                <td>{{ ev.at }}</td>
                <td>{{ ev.deltaSeconds != null ? `+${ev.deltaSeconds} 秒` : '—' }}</td>
                <td>
                  <template v-if="isEventRevoked(ev)">
                    <span class="revoked-tag" data-test="revoked-tag">
                      已撤销 @ {{ ev.revokedAt }}
                    </span>
                    <div class="reason" data-test="revoked-reason">原因：{{ ev.revokeReason }}</div>
                  </template>
                  <button
                    v-else-if="canRevoke(ev)"
                    type="button"
                    class="revoke"
                    :data-test="`revoke-btn-${ev.id}`"
                    :disabled="busy"
                    @click="openRevoke(ev)"
                  >撤销此记录（误扫）</button>
                  <span v-else class="muted">—</span>
                </td>
              </tr>
              <tr v-if="revokeForm && revokeForm.id === ev.id" :key="`revoke-form-${ev.id}`" class="revoke-row">
                <td colspan="5">
                  <form class="revoke-form" data-test="revoke-form" @submit.prevent="submitRevoke">
                    <label>
                      撤销时刻（晚于批次最后操作，RFC3339 Z 整秒）
                      <input v-model="revokeForm.at" data-test="revoke-at" placeholder="2026-09-13T08:00:00Z" />
                    </label>
                    <label>
                      撤销原因（必填）
                      <input v-model="revokeForm.reason" data-test="revoke-reason" placeholder="如：误扫归还，样本仍在柜外" />
                    </label>
                    <div class="revoke-actions">
                      <button type="submit" class="revoke" data-test="revoke-submit" :disabled="busy">确认撤销</button>
                      <button type="button" class="ghost" data-test="revoke-cancel" :disabled="busy" @click="cancelRevoke">取消</button>
                    </div>
                  </form>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
        <p v-else data-test="events-empty">尚无事件</p>
        <p class="rule-hint">仅允许撤销当前最后一条未撤销记录；撤销取出回到柜内，撤销归还扣回本次暴露并恢复柜外。</p>
      </section>
    </template>
  </main>
</template>

<style>
:root {
  color-scheme: light;
  font-family: "PingFang SC", "Microsoft YaHei", system-ui, sans-serif;
}
body { margin: 0; background: #f2f5f9; color: #1c2733; }
.page { max-width: 720px; margin: 0 auto; padding: 24px 16px 64px; }
h1 { font-size: 22px; }
h2 { font-size: 16px; margin: 0 0 12px; }
.hint { color: #5b6b7c; font-size: 13px; }
.scan { display: flex; gap: 8px; margin: 16px 0; }
.scan input { flex: 1; }
input, button {
  font: inherit; padding: 8px 10px; border: 1px solid #b9c4d0;
  border-radius: 6px; background: #fff;
}
button { cursor: pointer; background: #1f6feb; border-color: #1f6feb; color: #fff; }
button:disabled { opacity: 0.45; cursor: not-allowed; }
.banner { padding: 10px 12px; border-radius: 6px; font-size: 14px; }
.banner.error { background: #fdecea; color: #b3261e; border: 1px solid #f5c6c0; }
.banner.info { background: #e8f2ff; color: #0b5394; border: 1px solid #c4dfff; }
.card {
  background: #fff; border: 1px solid #dde4ec; border-radius: 10px;
  padding: 16px; margin-top: 16px;
}
.create label, .time-row label { display: block; margin-bottom: 10px; font-size: 13px; color: #5b6b7c; }
.create input, .time-row input { display: block; width: 100%; box-sizing: border-box; margin-top: 4px; color: #1c2733; }
.time-row { display: flex; gap: 8px; align-items: flex-end; margin-top: 16px; }
.time-row label { flex: 1; margin-bottom: 0; }
.card-head { display: flex; align-items: center; gap: 8px; margin-bottom: 12px; }
.card-head h2 { margin: 0; flex: 1; word-break: break-all; }
.badge { font-size: 12px; padding: 3px 10px; border-radius: 999px; font-weight: 600; }
.badge.in { background: #e6f4ea; color: #137333; }
.badge.out { background: #fef3e0; color: #b06000; }
.badge.usable { background: #e6f4ea; color: #137333; }
.badge.scrapped { background: #fdecea; color: #b3261e; }
.grid { display: grid; grid-template-columns: 1fr 1fr; gap: 10px 16px; margin: 0; }
.grid .wide { grid-column: 1 / -1; }
dt { font-size: 12px; color: #5b6b7c; }
dd { margin: 2px 0 0; font-size: 15px; font-weight: 600; }
dd.scrapped { color: #b3261e; }
dd.usable { color: #137333; }
.actions { display: flex; gap: 10px; margin-top: 16px; }
.actions .takeout { background: #b06000; border-color: #b06000; }
.actions .return { background: #137333; border-color: #137333; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 6px 8px; border-bottom: 1px solid #edf1f6; }
th { color: #5b6b7c; font-weight: 600; }
tr.revoked td { color: #8a97a5; text-decoration: line-through; }
tr.revoked td:last-child { text-decoration: none; }
.revoked-tag { color: #b06000; font-weight: 600; text-decoration: none; }
.revoked-tag + .reason { font-weight: 400; }
.reason { color: #5b6b7c; font-size: 12px; margin-top: 2px; }
.muted { color: #9aa6b2; }
button.revoke { background: #b06000; border-color: #b06000; padding: 5px 10px; font-size: 12px; }
button.ghost { background: #fff; color: #5b6b7c; border-color: #b9c4d0; padding: 6px 12px; }
tr.revoke-row td { background: #faf6ef; }
.revoke-form { display: flex; flex-wrap: wrap; gap: 10px; align-items: flex-end; }
.revoke-form label { flex: 1 1 220px; font-size: 12px; color: #5b6b7c; }
.revoke-form input { display: block; width: 100%; box-sizing: border-box; margin-top: 4px; color: #1c2733; }
.revoke-actions { display: flex; gap: 8px; }
.rule-hint { color: #8a97a5; font-size: 12px; margin: 10px 0 0; }
</style>
