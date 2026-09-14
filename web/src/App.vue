<script setup>
import { computed, onMounted, ref } from 'vue'
import { api } from './api'
import { isValidZ, nowZ } from './lib/time'
import { canRevokeEvent, eventTypeLabel, isEventRevoked, projectionConclusion } from './lib/derive'
import BatchCard from './components/BatchCard.vue'
import LocationCard from './components/LocationCard.vue'

const barcode = ref('')
const batch = ref(null)
const events = ref([])
const error = ref('')
const info = ref('')
const busy = ref(false)

// Event timestamp submitted with 取出/归还; prefilled with "now", editable
// so boundary conditions can be demonstrated and tested deterministically.
const eventTime = ref(nowZ())

// Advisory asOf risk evaluation: assessTime is the "评估至" instant and
// projection holds the non-persistent result returned by GET ...?asOf=...
// It is deliberately separate from `batch`: 取出/归还 always act on the
// persisted state, never on a projection.
const assessTime = ref(nowZ())
const projection = ref(null)
const assessError = ref('')
const assessBusy = ref(false)
// True while the operator is editing 评估至: an automatic re-evaluation must
// not reset a time they are typing (an explicit submit still computes it).
const assessEditing = ref(false)

// Inline revocation form for the latest non-revoked event: { id, at, reason }.
const revokeForm = ref(null)

// Independent cabinet-slot occupancy. These are deliberately separate from
// the exposure flow: a failed/conflicting placement keeps the loaded batch and
// the scanned slot draft (held inside LocationCard), only explaining why.
const locationBusy = ref(false)
const locationInfo = ref('')
const locationError = ref('')

function resetLocationMessages() {
  locationInfo.value = ''
  locationError.value = ''
}

function explainLocationError(e) {
  if (e.status === 409 && e.code === 'location_occupied') {
    return '目标格位已被其他批次占用（可能刚刚被并发扫码抢先）；本次放置/移位未生效，原格位占用保持不变。'
  }
  if (e.status === 409 && e.code === 'batch_not_in_cabinet') {
    return '该批次当前在柜外，不能占用格位；请先归还，再扫描格位码放置。'
  }
  if (e.status === 409 && e.code === 'batch_scrapped') {
    return '该批次已报废，不能再放入柜内格位。'
  }
  if (e.status === 409 && e.code === 'location_not_occupied') {
    return '该批次当前没有占用任何格位（可能已被其他工位腾空）；已为你刷新服务端状态。'
  }
  if (e.status === 404) {
    return '批次不存在，格位操作未执行。'
  }
  return `格位操作失败（${e.code || e.status}）：${e.message}`
}

// placeLocation handles both the first placement and a move. On conflict the
// batch stays loaded and the scanned slot draft stays in the input (the card
// keeps it); we only reload the authoritative batch so another station's
// occupancy becomes visible. No event is written and no exposure total moves.
async function placeLocation(code) {
  if (!batch.value) return
  resetLocationMessages()
  error.value = ''
  locationBusy.value = true
  const barcode = batch.value.barcode
  const previous = batch.value.location || null
  try {
    const updated = await api.putLocation(barcode, code)
    batch.value = updated
    locationInfo.value = previous
      ? `已移位：旧格位 ${previous} 已腾空，批次「${barcode}」现位于格位 ${code}`
      : `已放置：批次「${barcode}」现位于格位 ${code}`
  } catch (e) {
    locationError.value = explainLocationError(e)
    try {
      batch.value = await api.getBatch(barcode)
    } catch {
      /* keep the previously loaded batch on screen */
    }
  } finally {
    locationBusy.value = false
  }
}

// vacateLocation is the manual 腾空 operation. A takeout vacates the slot
// implicitly inside its own event transaction; this endpoint only releases
// the occupancy, touching neither events nor accumulated exposure.
async function vacateLocation() {
  if (!batch.value || !batch.value.location) return
  resetLocationMessages()
  error.value = ''
  locationBusy.value = true
  const barcode = batch.value.barcode
  const oldSlot = batch.value.location
  try {
    const updated = await api.clearLocation(barcode)
    batch.value = updated
    locationInfo.value = `已腾空格位 ${oldSlot}；批次仍在柜内，当前为未定位状态。`
  } catch (e) {
    locationError.value = explainLocationError(e)
    try {
      batch.value = await api.getBatch(barcode)
    } catch {
      /* keep the previously loaded batch on screen */
    }
  } finally {
    locationBusy.value = false
  }
}

// Create form (shown when the scanned barcode is unknown).
const showCreate = ref(false)
const createAllowed = ref(60)
const createCreatedAt = ref('')

async function refreshEvents() {
  if (!batch.value) return
  const res = await api.listEvents(batch.value.barcode)
  events.value = res.events
}

// runAssess performs the advisory risk evaluation at the given whole-second
// timestamp. The result is a projection only: it writes no events and never
// scraps the batch, and the 取出/归还 buttons keep working off the persistent
// batch state. A 400/409 failure is explained inline while the loaded batch
// stays on screen.
async function runAssess(at) {
  assessError.value = ''
  assessBusy.value = true
  try {
    const res = await api.getBatch(batch.value.barcode, at)
    batch.value = res
    projection.value = res.projection || null
  } catch (e) {
    // Keep the already loaded batch (and its persistent numbers) on screen;
    // drop the stale projection and explain the failure in place.
    projection.value = null
    if (e.status === 409 && e.code === 'time_not_monotonic') {
      assessError.value = '评估时刻早于该批次最后事件，无法评估；批次、事件流水与归还计时均不受影响。'
    } else if (e.status === 400 && e.code === 'invalid_time') {
      assessError.value = '评估时刻格式非法：必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    } else {
      assessError.value = `评估失败（${e.code || e.status}）：${e.message}`
    }
  } finally {
    assessBusy.value = false
  }
}

async function assess() {
  if (!batch.value) return
  if (!isValidZ(assessTime.value)) {
    projection.value = null
    assessError.value = '评估时刻格式非法：必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    return
  }
  await runAssess(assessTime.value)
}

// After (re)loading a batch, evaluate once at the current whole second; an
// out-of-cabinet batch thus shows its projected exposure immediately. While
// the operator is editing 评估至 we leave their draft untouched; the next
// explicit 风险评估 recomputes from it.
async function autoAssess() {
  if (assessEditing.value) return
  assessTime.value = nowZ()
  if (batch.value) await runAssess(assessTime.value)
}

const assessConclusion = computed(() => projectionConclusion(projection.value))
const assessProjected = computed(() => !!projection.value && projection.value.settled === false)

async function lookup() {
  error.value = ''
  info.value = ''
  showCreate.value = false
  revokeForm.value = null
  resetLocationMessages()
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
    await autoAssess()
  } catch (e) {
    batch.value = null
    events.value = []
    projection.value = null
    assessError.value = ''
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
  resetLocationMessages()
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
    await autoAssess()
  } catch (e) {
    if (e.status === 409) {
      error.value = '该条码已存在，已为你载入现有批次'
      try {
        batch.value = await api.getBatch(code)
        showCreate.value = false
        await refreshEvents()
        await autoAssess()
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
  resetLocationMessages()
  if (!batch.value) return
  if (!isValidZ(eventTime.value)) {
    error.value = '事件时间必须是带 Z 的 RFC3339 整秒，如 2026-09-13T08:00:00Z'
    return
  }
  busy.value = true
  const at = eventTime.value
  const hadSlot = !!batch.value.location
  try {
    batch.value = await api.postEvent(batch.value.barcode, { type, at })
    revokeForm.value = null
    await refreshEvents()
    if (type === 'takeout') {
      info.value = hadSlot
        ? '已取出（柜外计时开始）；批次原占格位已在同一事务中自动腾空'
        : '已取出（柜外计时开始）'
    } else {
      info.value = '已归还（本次暴露已计入）；批次当前未定位，请扫描格位码放置'
    }
    // Only reset the field when it still holds the submitted value, so a
    // fast follow-up edit made while the request was in flight survives.
    if (eventTime.value === at) {
      eventTime.value = nowZ()
    }
    // Re-evaluate at the new current whole second so the projection tracks
    // the new persistent state (projected right after takeout, settled in).
    await autoAssess()
  } catch (e) {
    // 409 etc.: show the conflict, then reload the authoritative state so a
    // racing/duplicate submission can never display stale numbers.
    error.value = `操作被拒绝（${e.code}）：${e.message}`
    try {
      batch.value = await api.getBatch(batch.value.barcode)
      await refreshEvents()
      await autoAssess()
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
  resetLocationMessages()
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
    await autoAssess()
  } catch (e) {
    // 409 etc.: show the conflict, then reload the authoritative server state
    // so a rejected/racing undo never changes the displayed totals or state.
    error.value = `撤销被拒绝（${e.code}）：${e.message}`
    revokeForm.value = null
    try {
      batch.value = await api.getBatch(batch.value.barcode)
      await refreshEvents()
      await autoAssess()
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

function resetAssessTime() {
  assessTime.value = nowZ()
  if (batch.value) assess()
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

      <LocationCard
        :batch="batch"
        :busy="busy || locationBusy"
        :result-text="locationError || locationInfo"
        :result-kind="locationError ? 'error' : (locationInfo ? 'info' : '')"
        @place="placeLocation"
        @clear="vacateLocation"
      />

      <section class="card assess" data-test="assess-card">
        <h2>暴露风险评估（仅提示）</h2>
        <p class="rule-hint" style="margin-top:0">
          评估不写事件、不报废批次；取出/归还仍按下方已持久化的柜内外状态工作。
        </p>
        <form class="assess-form" data-test="assess-form" @submit.prevent="assess">
          <label>
            评估至
            <input
              v-model="assessTime"
              data-test="assess-time"
              placeholder="2026-09-13T08:00:00Z"
              @focus="assessEditing = true"
              @blur="assessEditing = false"
            />
          </label>
          <div class="assess-actions">
            <button type="submit" data-test="assess-btn" :disabled="assessBusy || busy">风险评估</button>
            <button type="button" class="ghost" data-test="assess-now" :disabled="assessBusy" @click="resetAssessTime">设为当前时间</button>
          </div>
        </form>
        <p v-if="assessError" class="banner error assess-banner" data-test="assess-error">{{ assessError }}</p>
        <dl v-if="projection" class="grid assess-result" data-test="assess-result">
          <div>
            <dt>评估时刻</dt>
            <dd data-test="assess-asof">{{ projection.asOf }}</dd>
          </div>
          <div>
            <dt>{{ assessProjected ? '预计累计暴露' : '累计暴露（已结算）' }}</dt>
            <dd data-test="assess-accumulated" :class="{ over: !projection.usable }">
              {{ projection.accumulatedSeconds }} 秒
            </dd>
          </div>
          <div>
            <dt>{{ assessProjected ? '预计剩余额度' : '剩余额度（已结算）' }}</dt>
            <dd data-test="assess-remaining" :class="{ over: !projection.usable }">
              {{ projection.remainingSeconds }} 秒
            </dd>
          </div>
          <div class="wide">
            <dt>预计可用结论</dt>
            <dd data-test="assess-conclusion" :class="projection.usable ? 'usable' : 'scrapped'">
              <template v-if="assessProjected && !projection.usable">
                <span class="over-tag" data-test="assess-over-limit">已预计超限</span>
                （样本仍在柜外，请尽快归还；归还后以实际结算为准）
              </template>
              <template v-else>{{ assessConclusion }}</template>
            </dd>
          </div>
        </dl>
      </section>

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
.assess-form { display: flex; flex-wrap: wrap; gap: 10px; align-items: flex-end; }
.assess-form label { flex: 1 1 220px; font-size: 13px; color: #5b6b7c; }
.assess-form input { display: block; width: 100%; box-sizing: border-box; margin-top: 4px; color: #1c2733; }
.assess-actions { display: flex; gap: 8px; }
.assess-result { margin-top: 14px; }
.assess-banner { margin: 12px 0 0; }
dd.over { color: #b3261e; }
.over-tag {
  display: inline-block; background: #fdecea; color: #b3261e;
  border: 1px solid #f5c6c0; border-radius: 999px;
  padding: 2px 10px; font-size: 13px; font-weight: 700;
}
</style>
