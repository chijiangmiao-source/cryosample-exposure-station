<script setup>
import { computed, ref, watch } from 'vue'

// Independent cabinet-slot shelving. Occupancy is tracked separately from the
// takeout/return timing state: only a batch physically inside the cabinet can
// occupy a slot, a takeout releases it automatically, and a returned batch
// stays unplaced until it is scanned into a slot again.
const props = defineProps({
  batch: { type: Object, required: true },
  busy: { type: Boolean, default: false },
  // Result/error text owned by App so a failed attempt survives the
  // authoritative batch reload; the input itself stays in this component so a
  // conflict never wipes what the operator scanned.
  resultText: { type: String, default: '' },
  resultKind: { type: String, default: '' } // "info" | "error" | ""
})

const emit = defineEmits(['place', 'clear'])

// The scanned slot code. It is re-synced from the server only when the batch
// changes, our own operation is confirmed, or the batch leaves/returns — never
// after a conflict, so the rejected draft is preserved for another attempt.
const slotInput = ref(props.batch.location || '')
// Slot we are waiting for the server to confirm ('' after 腾空); null when no
// operation is in flight.
const expected = ref(null)

watch(
  () => props.batch.barcode,
  () => {
    expected.value = null
    slotInput.value = props.batch.location || ''
  }
)

watch(
  () => props.batch.state,
  (state) => {
    // Leaving the cabinet vacates the slot server-side; a return comes back
    // unplaced. Either way the draft no longer applies.
    expected.value = null
    slotInput.value = state === 'in' ? props.batch.location || '' : ''
  }
)

watch(
  () => props.batch.location,
  (loc) => {
    if (expected.value === null) {
      // No operation in flight: adopt the authoritative slot (initial load,
      // page refresh, automatic release on takeout, another tab's move).
      slotInput.value = loc || ''
      return
    }
    if (loc || '' === expected.value) {
      // Our place/move/腾空 was accepted (null and '' both mean "no slot").
      slotInput.value = loc || ''
      expected.value = null
    }
  }
)

// A rejected attempt surfaces through resultKind; release the pending
// expectation so later authoritative updates flow through normally while the
// scanned draft itself is preserved for another try.
watch(
  () => props.resultKind,
  (kind) => {
    if (kind === 'error') expected.value = null
  }
)

function submit() {
  const code = slotInput.value.trim()
  if (!code) return
  expected.value = code
  emit('place', code)
}

function vacate() {
  expected.value = '' // expect the server to confirm "no slot"
  emit('clear')
}

const inCabinet = computed(() => props.batch.state === 'in')
const scrapped = computed(() => props.batch.status === 'scrapped')
// Only an in-cabinet usable batch can be placed, moved or vacated through this
// form. A takeout releases the slot itself and a return comes back unplaced.
const interactive = computed(() => inCabinet.value && !scrapped.value)
</script>

<template>
  <section class="card location" data-test="location-card">
    <header class="card-head">
      <h2>柜内格位</h2>
    </header>

    <dl class="grid loc-grid">
      <div class="wide">
        <dt>当前格位</dt>
        <dd data-test="current-location" :class="{ unplaced: !batch.location }">
          {{ batch.location ? `格位 ${batch.location}` : '未定位（柜内可用批次尚未放入格位）' }}
        </dd>
      </div>
      <p v-if="!inCabinet" class="rule-hint loc-hint" data-test="location-out-hint">
        批次当前在柜外，不占用任何格位；请归还后再扫描格位码放置。
      </p>
      <p v-else-if="scrapped" class="rule-hint loc-hint" data-test="location-scrapped-hint">
        批次已报废，不能再占用柜内格位。
      </p>
    </dl>

    <form class="location-form" data-test="location-form" @submit.prevent="submit">
      <input
        v-model="slotInput"
        data-test="location-input"
        :disabled="!interactive || busy"
        :placeholder="inCabinet ? '扫描 / 输入格位码，如 A-01' : '批次在柜外，归还后才能放置'"
      />
      <button
        type="submit"
        data-test="location-put-btn"
        :disabled="busy || !interactive"
      >{{ batch.location ? '移位到此格位' : '放置到此格位' }}</button>
      <button
        type="button"
        class="ghost"
        data-test="location-clear-btn"
        :disabled="busy || !batch.location"
        @click="vacate"
      >腾空</button>
    </form>
    <p v-if="resultKind === 'info'" class="banner info loc-banner" data-test="location-result">
      {{ resultText }}
    </p>
    <p v-else-if="resultKind === 'error'" class="banner error loc-banner" data-test="location-error">
      {{ resultText }}
    </p>
    <p class="rule-hint">放置 / 移位 / 腾空在单个事务内完成；格位被占、并发争抢或柜外放置都会被拒绝，原占用与暴露累计不变。取出时格位由系统自动释放。</p>
  </section>
</template>

<style scoped>
.location-form {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 4px;
}
.location-form input {
  flex: 1 1 180px;
}
.loc-grid .wide dd.unplaced {
  color: #8a97a5;
  font-weight: 500;
}
.loc-hint {
  margin: 6px 0 0;
}
.loc-banner {
  margin: 10px 0 0;
}
button.ghost {
  background: #fff;
  color: #5b6b7c;
  border-color: #b9c4d0;
}
</style>
