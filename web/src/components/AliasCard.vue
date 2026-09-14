<script setup>
import { ref, watch } from 'vue'

// Backup barcodes (aliases): an alternate label bound to the same canonical
// batch. Scanning either code reaches the same batch, so timing, revocation
// and location records are shared. An alias is globally unique and can never
// collide with any primary barcode; those conflicts are rejected server-side.
const props = defineProps({
  batch: { type: Object, required: true },
  busy: { type: Boolean, default: false },
  // The code actually scanned when this page was reached through a bound
  // alias ('' when the primary barcode was scanned). App keeps it across
  // canonical-barcode reloads so the hint lasts the whole session.
  matchedBarcode: { type: String, default: '' },
  // Result/error text owned by App so a rejected bind keeps both the loaded
  // batch and the typed draft; only the explanation is shown here.
  resultText: { type: String, default: '' },
  resultKind: { type: String, default: '' } // "info" | "error" | ""
})

const emit = defineEmits(['bind', 'unbind'])

// The scanned backup code draft. It is cleared only after a confirmed bind,
// never after a conflict, so the operator can fix and retry what they typed.
const aliasInput = ref('')
const pending = ref(null) // code awaiting server confirmation; null when idle

watch(
  () => props.batch.barcode,
  () => {
    pending.value = null
    aliasInput.value = ''
  }
)

// A successful bind/unbind arrives as a new aliases list: release the pending
// expectation and (for a bind) clear the draft.
watch(
  () => props.batch.aliases,
  (aliases) => {
    if (pending.value === null) return
    if (pending.value === '' || (Array.isArray(aliases) && aliases.includes(pending.value))) {
      if (pending.value !== '') aliasInput.value = ''
      pending.value = null
    }
  }
)

// A rejected attempt frees the pending expectation while the draft survives.
watch(
  () => props.resultKind,
  (kind) => {
    if (kind === 'error') pending.value = null
  }
)

function submit() {
  const code = aliasInput.value.trim()
  if (!code) return
  pending.value = code
  emit('bind', code)
}

function remove(alias) {
  pending.value = ''
  emit('unbind', alias)
}
</script>

<template>
  <section class="card alias" data-test="alias-card">
    <header class="card-head">
      <h2>备用条码（别名）</h2>
    </header>

    <p v-if="matchedBarcode" class="banner info alias-hit" data-test="alias-hit-banner">
      本次扫描使用了备用标签「{{ matchedBarcode }}」，已解析到主条码「{{ batch.barcode }}」；计时、撤销与库位记录均归属该批次。
    </p>

    <dl class="grid">
      <div class="wide">
        <dt>主条码</dt>
        <dd data-test="primary-barcode">{{ batch.barcode }}</dd>
      </div>
      <div class="wide">
        <dt>已绑定备用条码</dt>
        <dd>
          <ul v-if="batch.aliases && batch.aliases.length" class="alias-list" data-test="alias-list">
            <li v-for="alias in batch.aliases" :key="alias" :data-test="`alias-row-${alias}`">
              <span class="alias-code" data-test="alias-code">{{ alias }}</span>
              <button
                type="button"
                class="ghost alias-unbind"
                :data-test="`alias-unbind-${alias}`"
                :disabled="busy"
                @click="remove(alias)"
              >解除绑定</button>
            </li>
          </ul>
          <span v-else class="muted" data-test="alias-empty">尚无备用条码</span>
        </dd>
      </div>
    </dl>

    <form class="alias-form" data-test="alias-form" @submit.prevent="submit">
      <input
        v-model="aliasInput"
        data-test="alias-input"
        :disabled="busy"
        placeholder="扫描 / 输入备用条码（须全局唯一，不能与任何主条码重名）"
      />
      <button type="submit" data-test="alias-bind-btn" :disabled="busy">绑定备用条码</button>
    </form>
    <p v-if="resultKind === 'info'" class="banner info alias-banner" data-test="alias-result">
      {{ resultText }}
    </p>
    <p v-else-if="resultKind === 'error'" class="banner error alias-banner" data-test="alias-error">
      {{ resultText }}
    </p>
    <p class="rule-hint">绑定后扫描任一条码都进入同一份计时、撤销与库位记录；解除后该备用条码立即无法查询。绑定冲突或名称重名时不会改变当前批次。</p>
  </section>
</template>

<style scoped>
.alias-form {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 4px;
}
.alias-form input {
  flex: 1 1 220px;
}
.alias-list {
  list-style: none;
  margin: 4px 0 0;
  padding: 0;
}
.alias-list li {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 4px 0;
}
.alias-code {
  font-weight: 600;
  word-break: break-all;
}
.alias-unbind {
  font-size: 12px;
  padding: 4px 10px;
  background: #fff;
  color: #b06000;
  border-color: #e0b887;
}
.alias-hit {
  margin: 0 0 12px;
}
.alias-banner {
  margin: 10px 0 0;
}
</style>
