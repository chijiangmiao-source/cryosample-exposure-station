import { expect, test } from '@playwright/test'

// Backup barcodes (aliases): a supplier relabel or a box with two barcodes is
// bound to an existing batch. Scanning either code reaches the same canonical
// batch, so timing, revocation and location records stay shared. The response
// always carries the canonical primary barcode and, when an alias was hit, the
// optional matchedBarcode the page uses for its "used a backup label" hint.

const uniq = Date.now()
let seq = 0
function nextBarcode(prefix) {
  seq += 1
  return `E2E-ALIAS-${prefix}-${uniq}-${seq}`
}
function aliasOf(b) {
  return `${b}-BACKUP`
}
function slot(name) {
  return `E2EA${uniq % 1_000_000}-${name}`
}

const T0 = '2026-01-01T00:00:00Z'

async function createBatchViaAPI(request, barcode, allowedSeconds = 100, createdAt = T0) {
  const res = await request.post('/api/batches', {
    data: { barcode, allowedSeconds, createdAt }
  })
  expect(res.status()).toBe(201)
  return res.json()
}

async function bindAlias(request, barcode, alias) {
  const res = await request.post(`/api/batches/${barcode}/aliases`, { data: { alias } })
  return { status: res.status(), body: await res.json().catch(() => ({})) }
}

async function unbindAlias(request, barcode, alias) {
  const res = await request.delete(`/api/batches/${barcode}/aliases/${alias}`)
  return { status: res.status(), body: await res.json().catch(() => ({})) }
}

async function createBatch(page, barcode, allowedSeconds = 100) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(barcode)
  await page.getByTestId('lookup-btn').click()
  await expect(page.getByTestId('create-form')).toBeVisible()
  await page.getByTestId('create-allowed').fill(String(allowedSeconds))
  await page.getByTestId('create-created-at').fill(T0)
  await page.getByTestId('create-submit').click()
  await expect(page.getByTestId('batch-card')).toBeVisible()
}

async function scan(page, code) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(code)
  await page.getByTestId('lookup-btn').click()
  await expect(page.getByTestId('batch-card')).toBeVisible()
}

async function takeout(page, at) {
  await page.getByTestId('event-time').fill(at)
  await page.getByTestId('takeout-btn').click()
  await expect(page.getByTestId('assess-result')).toBeVisible()
}

async function doReturn(page, at) {
  await page.getByTestId('event-time').fill(at)
  await page.getByTestId('return-btn').click()
  await expect(page.getByTestId('assess-result')).toBeVisible()
}

test('绑定备用条码后扫描别名进入同一批次，页面提示使用了备用标签', async ({ page }) => {
  const b = nextBarcode('BIND')
  const alias = aliasOf(b)
  await createBatch(page, b)

  // Bind the backup code through the batch page.
  await expect(page.getByTestId('primary-barcode')).toHaveText(b)
  await page.getByTestId('alias-input').fill(alias)
  await page.getByTestId('alias-bind-btn').click()
  await expect(page.getByTestId('alias-result')).toContainText('已绑定备用条码')
  await expect(page.getByTestId(`alias-row-${alias}`)).toBeVisible()

  // Scanning the backup code loads the same canonical batch.
  await scan(page, alias)
  await expect(page.getByTestId('card-barcode')).toHaveText(b)
  await expect(page.getByTestId('alias-hit-banner')).toContainText(alias)
  await expect(page.getByTestId('alias-hit-banner')).toContainText(b)

  // Scanning the primary barcode shows no hit hint.
  await scan(page, b)
  await expect(page.getByTestId('card-barcode')).toHaveText(b)
  await expect(page.getByTestId('alias-hit-banner')).toHaveCount(0)
})

test('别名可完成取出、归还、重新定位；刷新后仍指向同一批次；解除后无法再查询', async ({ page, request }) => {
  const b = nextBarcode('FLOW')
  const alias = aliasOf(b)
  const s1 = slot('F1')
  const s2 = slot('F2')
  await createBatch(page, b)
  await page.getByTestId('alias-input').fill(alias)
  await page.getByTestId('alias-bind-btn').click()
  await expect(page.getByTestId(`alias-row-${alias}`)).toBeVisible()

  // Enter through the backup label and place into a slot.
  await scan(page, alias)
  await expect(page.getByTestId('alias-hit-banner')).toContainText(alias)
  await page.getByTestId('location-input').fill(s1)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('current-location')).toContainText(s1)

  // Takeout via the alias: the shared slot auto-vacates.
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('current-location')).toContainText('未定位')

  // Return via the alias: exposure is counted on the canonical batch.
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('current-location')).toContainText('未定位')

  // Relocate/re-place via the alias.
  await page.getByTestId('location-input').fill(s2)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('current-location')).toContainText(s2)

  // Refresh (the page restores the last scanned code — the alias) and confirm
  // it still resolves to the same canonical batch with shared state/slot.
  await page.reload()
  await expect(page.getByTestId('batch-card')).toBeVisible()
  await expect(page.getByTestId('card-barcode')).toHaveText(b)
  await expect(page.getByTestId('alias-hit-banner')).toContainText(alias)
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('current-location')).toContainText(s2)

  // Server truth via the primary barcode is identical.
  const body = await request.get(`/api/batches/${b}`).then((r) => r.json())
  expect(body.barcode).toBe(b)
  expect(body.accumulatedSeconds).toBe(10)
  expect(body.location).toBe(s2)
  expect(body.matchedBarcode).toBeUndefined()
  const aliasBody = await request.get(`/api/batches/${alias}`).then((r) => r.json())
  expect(aliasBody.barcode).toBe(b)
  expect(aliasBody.matchedBarcode).toBe(alias)

  // Unbind via the batch page: the alias can no longer be queried.
  await page.getByTestId(`alias-unbind-${alias}`).click()
  await expect(page.getByTestId('alias-result')).toContainText('已解除备用条码')
  const gone = await request.get(`/api/batches/${alias}`)
  expect(gone.status()).toBe(404)
  // The primary batch keeps its state and slot.
  const still = await request.get(`/api/batches/${b}`).then((r) => r.json())
  expect(still.location).toBe(s2)
  expect(still.accumulatedSeconds).toBe(10)
})

test('绑定与主条码重名或重复时给出明确提示并保留批次与输入', async ({ page, request }) => {
  const b1 = nextBarcode('CONF')
  const b2 = nextBarcode('CONF')
  await createBatchViaAPI(request, b2)
  await createBatch(page, b1)

  // Alias equal to another batch's primary barcode -> rejected, input kept.
  await page.getByTestId('alias-input').fill(b2)
  await page.getByTestId('alias-bind-btn').click()
  await expect(page.getByTestId('alias-error')).toContainText('主条码')
  await expect(page.getByTestId('alias-input')).toHaveValue(b2)
  await expect(page.getByTestId('card-barcode')).toHaveText(b1)

  // Bind a fresh alias, then repeat it -> duplicate, still on the same batch.
  const dup = `${b1}-DUP`
  const first = await bindAlias(request, b1, dup)
  expect(first.status).toBe(200)
  await page.getByTestId('alias-input').fill(dup)
  await page.getByTestId('alias-bind-btn').click()
  await expect(page.getByTestId('alias-error')).toContainText('已经绑定在当前批次')
  await expect(page.getByTestId('alias-input')).toHaveValue(dup)

  // Trying to create a new batch whose barcode is an existing alias conflicts.
  const clash = await request.post('/api/batches', {
    data: { barcode: dup, allowedSeconds: 10, createdAt: T0 }
  })
  expect(clash.status()).toBe(409)
  expect((await clash.json()).error.code).toBe('duplicate_barcode')

  // Unbinding a code that does not belong to this batch changes nothing.
  const r = await unbindAlias(request, b2, dup)
  expect(r.status).toBe(409)
  expect(r.body.error.code).toBe('alias_not_bound')
  // dup still resolves to b1.
  const resolved = await request.get(`/api/batches/${dup}`).then((x) => x.json())
  expect(resolved.barcode).toBe(b1)
})

test('并发绑定同一备用条码仅一个批次成功', async ({ request }) => {
  const barcodes = []
  for (let i = 0; i < 4; i++) {
    barcodes.push(nextBarcode('RACE'))
  }
  await Promise.all(barcodes.map((bc) => createBatchViaAPI(request, bc)))
  const raced = `${nextBarcode('RACED')}-X`

  const responses = await Promise.all(
    barcodes.map((bc) => request.post(`/api/batches/${bc}/aliases`, { data: { alias: raced } }))
  )
  const statuses = responses.map((r) => r.status())
  expect(statuses.filter((s) => s === 200)).toHaveLength(1)
  expect(statuses.filter((s) => s === 409)).toHaveLength(3)

  for (const r of responses) {
    if (r.status() !== 200) {
      const body = await r.json()
      expect(['duplicate_alias', 'alias_bound_elsewhere']).toContain(body.error.code)
    }
  }
  // The raced code resolves to exactly one canonical batch.
  const winner = await request.get(`/api/batches/${raced}`).then((x) => x.json())
  expect(winner.matchedBarcode).toBe(raced)
  expect(barcodes).toContain(winner.barcode)
  expect(winner.aliases).toEqual([raced])
})

test('旧客户端忽略新增字段后主条码原流程结果不变', async ({ request }) => {
  const b = nextBarcode('LEGACY')
  await createBatchViaAPI(request, b, 100)
  await bindAlias(request, b, aliasOf(b))

  // An old client only reads the fields it already knows; the additive
  // matchedBarcode/aliases keys do not change any timing result.
  const res = await request.post(`/api/batches/${b}/events`, {
    data: { type: 'takeout', at: '2026-01-01T00:00:05Z' }
  })
  expect(res.status()).toBe(201)
  const out = await res.json()
  expect(out.barcode).toBe(b)
  expect(out.state).toBe('out')
  expect(out.accumulatedSeconds).toBe(0)

  const back = await request.post(`/api/batches/${b}/events`, {
    data: { type: 'return', at: '2026-01-01T00:00:15Z' }
  })
  expect(back.status()).toBe(201)
  const inCabinet = await back.json()
  expect(inCabinet.state).toBe('in')
  expect(inCabinet.accumulatedSeconds).toBe(10)
  expect(inCabinet.status).toBe('usable')
})
