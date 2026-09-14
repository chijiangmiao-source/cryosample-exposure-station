import { expect, test } from '@playwright/test'

// Cabinet location occupancy flows. Occupancy is independent of the
// takeout/return timing: in-cabinet usable batches are shelved by scanning a
// location code, a takeout vacates the slot in the same transaction, and a
// returned batch stays unplaced until it is shelved again.

const uniq = Date.now()
let seq = 0
function nextBarcode(prefix) {
  seq += 1
  return `E2E-LOC-${prefix}-${uniq}-${seq}`
}

const T0 = '2026-01-01T00:00:00Z'

// Slot codes are namespaced per run too: the deployed database persists
// between suite runs, so a fixed "A-01" could still be held by an earlier run.
function slot(name) {
  return `E2E${uniq % 1_000_000}-${name}`
}

async function createBatch(page, barcode, allowedSeconds = 100, createdAt = T0) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(barcode)
  await page.getByTestId('lookup-btn').click()
  await expect(page.getByTestId('create-form')).toBeVisible()
  await page.getByTestId('create-allowed').fill(String(allowedSeconds))
  await page.getByTestId('create-created-at').fill(createdAt)
  await page.getByTestId('create-submit').click()
  await expect(page.getByTestId('batch-card')).toBeVisible()
}

async function scan(page, barcode) {
  await page.goto('/')
  await page.getByTestId('barcode-input').fill(barcode)
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

async function putLocation(request, barcode, location) {
  const res = await request.put(`/api/batches/${barcode}/location`, {
    data: { location }
  })
  const body = await res.json().catch(() => ({}))
  return { status: res.status(), body }
}

async function createBatchViaAPI(request, barcode, allowedSeconds = 100, createdAt = T0) {
  const res = await request.post('/api/batches', {
    data: { barcode, allowedSeconds, createdAt }
  })
  expect(res.status()).toBe(201)
  return res.json()
}

test('放置后刷新可见真实格位，移位释放旧格位，腾空后格位可再用', async ({ page, request }) => {
  const b1 = nextBarcode('SHELF')
  const b2 = nextBarcode('SHELF')
  const s1 = slot('S1')
  const s2 = slot('S2')
  await createBatch(page, b1)
  // Secondary batch is created through the API so the page keeps showing b1.
  await createBatchViaAPI(request, b2)

  // Place b1 into the first slot through the scan form.
  await expect(page.getByTestId('current-location')).toContainText('未定位')
  await page.getByTestId('location-input').fill(s1)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('location-result')).toContainText('已放置')
  await expect(page.getByTestId('current-location')).toContainText(s1)

  // Refresh: the real slot is restored from the server.
  await page.reload()
  await expect(page.getByTestId('batch-card')).toBeVisible()
  await expect(page.getByTestId('current-location')).toContainText(s1)
  await expect(page.getByTestId('location-input')).toHaveValue(s1)

  // Move b1 to the second slot: the old slot is released in the same transaction.
  await page.getByTestId('location-input').fill(s2)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('location-result')).toContainText(`旧格位 ${s1} 已腾空`)
  await expect(page.getByTestId('current-location')).toContainText(s2)

  // b2 can now take the freed first slot.
  const r = await putLocation(request, b2, s1)
  expect(r.status).toBe(200)
  expect(r.body.location).toBe(s1)

  // 腾空 b1: the slot becomes free and b1 stays in the cabinet, unplaced.
  await page.getByTestId('location-clear-btn').click()
  await expect(page.getByTestId('location-result')).toContainText(`已腾空格位 ${s2}`)
  await expect(page.getByTestId('current-location')).toContainText('未定位')
  const again = await putLocation(request, b2, s2)
  // b2 moves off the first slot onto the just-vacated second one.
  expect(again.status).toBe(200)
  expect(again.body.location).toBe(s2)
})

test('取出在原事件事务内自动腾空，归还后保持未定位并可重新放置', async ({ page, request }) => {
  const b = nextBarcode('TAKEOUT')
  const other = nextBarcode('TAKEOUT')
  const s1 = slot('T1')
  const s2 = slot('T2')
  const s3 = slot('T3')
  await createBatch(page, b)
  // The "other" batch exists only for the slot-reuse API checks; the page
  // must keep displaying b throughout this test.
  await createBatchViaAPI(request, other)

  await page.getByTestId('location-input').fill(s1)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('current-location')).toContainText(s1)

  // Takeout: the slot is released by the original event transaction — no
  // separate location call, and the info banner says so.
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('current-location')).toContainText('未定位')
  await expect(page.getByTestId('location-input')).toHaveValue('')
  await expect(page.getByTestId('info-banner')).toContainText('自动腾空')

  // The released slot can immediately hold another in-cabinet batch.
  const reused = await putLocation(request, other, s1)
  expect(reused.status).toBe(200)
  expect(reused.body.location).toBe(s1)

  // Placing an out-of-cabinet batch is a clear conflict.
  const rejected = await putLocation(request, b, s3)
  expect(rejected.status).toBe(409)
  expect(rejected.body.error.code).toBe('batch_not_in_cabinet')

  // Return: back in the cabinet but still unplaced.
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('current-location')).toContainText('未定位')
  await expect(page.getByTestId('info-banner')).toContainText('未定位')
  expect((await request.get(`/api/batches/${b}`).then((r) => r.json())).location).toBeNull()

  // Re-place after return into a new free slot.
  await page.getByTestId('location-input').fill(s2)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('current-location')).toContainText(s2)
  // Refresh shows the re-placed slot.
  await page.reload()
  await expect(page.getByTestId('current-location')).toContainText(s2)
})

test('目标格位已被占用时页面保留批次与输入并解释原因，原占用不变', async ({ page, request }) => {
  const b1 = nextBarcode('BUSY')
  const b2 = nextBarcode('BUSY')
  const sHeld = slot('B-held')
  const sMine = slot('B-mine')
  const sFree = slot('B-free')
  await createBatchViaAPI(request, b1)
  await createBatch(page, b2)

  // b1 holds the target slot; b2 holds its own and tries to move onto b1's
  // slot in the UI.
  let r = await putLocation(request, b1, sHeld)
  expect(r.status).toBe(200)
  r = await putLocation(request, b2, sMine)
  expect(r.status).toBe(200)

  // Reload b2 in the UI: its API-recorded slot must now be visible.
  await scan(page, b2)
  await expect(page.getByTestId('current-location')).toContainText(sMine)
  await page.getByTestId('location-input').fill(sHeld)
  await page.getByTestId('location-put-btn').click()

  // Inline explanation; the batch stays loaded, the scanned draft is kept and
  // the old occupancy is unchanged.
  await expect(page.getByTestId('location-error')).toContainText('目标格位已被其他批次占用')
  await expect(page.getByTestId('current-location')).toContainText(sMine)
  await expect(page.getByTestId('location-input')).toHaveValue(sHeld)
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')

  // Server truth: b1 still owns the target slot, b2 keeps its own, no events.
  const b1body = await request.get(`/api/batches/${b1}`).then((x) => x.json())
  const b2body = await request.get(`/api/batches/${b2}`).then((x) => x.json())
  expect(b1body.location).toBe(sHeld)
  expect(b2body.location).toBe(sMine)
  const evs = await request.get(`/api/batches/${b2}/events`).then((x) => x.json())
  expect(evs.events).toHaveLength(0)

  // The operator corrects the slot and succeeds without retyping the barcode.
  await page.getByTestId('location-input').fill(sFree)
  await page.getByTestId('location-put-btn').click()
  await expect(page.getByTestId('location-result')).toContainText('已移位')
  await expect(page.getByTestId('current-location')).toContainText(sFree)
})

test('两个批次并发争抢同一格位仅一个成功，失败方占用不变', async ({ request }) => {
  const barcodes = []
  for (let i = 0; i < 4; i++) {
    const bc = nextBarcode('RACE')
    await request.post('/api/batches', {
      data: { barcode: bc, allowedSeconds: 100, createdAt: T0 }
    })
    barcodes.push(bc)
  }
  const racedSlot = slot('RACE-Z')

  const responses = await Promise.all(
    barcodes.map((bc) =>
      request.put(`/api/batches/${bc}/location`, { data: { location: racedSlot } })
    )
  )
  const statuses = responses.map((r) => r.status())
  expect(statuses.filter((s) => s === 200)).toHaveLength(1)
  expect(statuses.filter((s) => s === 409)).toHaveLength(3)

  const bodies = await Promise.all(responses.map((r) => r.json()))
  for (const body of bodies) {
    if (body.error) expect(body.error.code).toBe('location_occupied')
  }

  // Exactly one batch ends on the raced slot; every other is unplaced.
  const finals = await Promise.all(
    barcodes.map((bc) => request.get(`/api/batches/${bc}`).then((r) => r.json()))
  )
  expect(finals.filter((b) => b.location === racedSlot)).toHaveLength(1)
  expect(finals.filter((b) => b.location === null)).toHaveLength(3)
})

test('报废批次不能放置格位', async ({ page, request }) => {
  const b = nextBarcode('SCRAP')
  await createBatch(page, b, 10)

  // +11 seconds scraps the batch on return.
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z')
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')

  const r = await putLocation(request, b, slot('SCRAP-1'))
  expect(r.status).toBe(409)
  expect(r.body.error.code).toBe('batch_scrapped')
  const body = await request.get(`/api/batches/${b}`).then((x) => x.json())
  expect(body.location).toBeNull()
  expect(body.accumulatedSeconds).toBe(11)
})
