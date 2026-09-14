import { expect, test } from '@playwright/test'

// Unique barcodes per run so the suite is repeatable against the same DB.
const uniq = Date.now()
let seq = 0
function nextBarcode(prefix) {
  seq += 1
  return `E2E-${prefix}-${uniq}-${seq}`
}

const T0 = '2026-01-01T00:00:00Z'

async function createBatch(page, barcode, allowedSeconds, createdAt = T0) {
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
  // Wait for the automatic asOf assessment after the state change to settle so
  // it cannot reset the 评估至 field after the test starts editing it.
  await expect(page.getByTestId('assess-result')).toBeVisible()
}

async function doReturn(page, at) {
  await page.getByTestId('event-time').fill(at)
  await page.getByTestId('return-btn').click()
  await expect(page.getByTestId('assess-result')).toBeVisible()
}

// Only the latest non-revoked event renders a revoke button, so the button
// selector is unique on the page.
async function revokeLast(page, at, reason) {
  await page.locator('button[data-test^="revoke-btn-"]').click()
  await expect(page.getByTestId('revoke-form')).toBeVisible()
  await page.getByTestId('revoke-at').fill(at)
  await page.getByTestId('revoke-reason').fill(reason)
  await page.getByTestId('revoke-submit').click()
  await expect(page.getByTestId('revoke-form')).toBeHidden()
}

test('临界归还仍可用，刷新后状态保持一致', async ({ page }) => {
  const barcode = nextBarcode('BOUNDARY')
  await createBatch(page, barcode, 10)

  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('remaining')).toHaveText('10 秒')
  await expect(page.getByTestId('last-event')).toHaveText('尚无事件')

  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()
  await expect(page.getByTestId('return-btn')).toBeEnabled()

  // 恰好等于上限：累计 10 == 允许 10，仍然可用
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
  await expect(page.getByTestId('last-event')).toContainText('归还 @ 2026-01-01T00:00:15Z')
  await expect(page.getByTestId('last-event')).toContainText('本次 +10 秒')

  // 刷新后保持一致（状态持久化在服务端）
  await page.reload()
  await expect(page.getByTestId('batch-card')).toBeVisible()
  await expect(page.getByTestId('card-barcode')).toHaveText(barcode)
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('conclusion')).toHaveText('可用')
  await expect(page.getByTestId('events-table')).toContainText('取出')
  await expect(page.getByTestId('events-table')).toContainText('归还')
})

test('超限归还立即报废，报废批次不得再取出', async ({ page }) => {
  const barcode = nextBarcode('SCRAP')
  await createBatch(page, barcode, 10)

  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // +11 秒 > 上限 10

  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  await expect(page.getByTestId('conclusion')).toContainText('已报废')
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()
  await expect(page.getByTestId('return-btn')).toBeDisabled()

  await page.reload()
  await expect(page.getByTestId('conclusion')).toContainText('已报废')
})

test('重复/竞态归还不会二次计时', async ({ context, page }) => {
  const barcode = nextBarcode('RACE')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  // 第二个工位同时打开同一批次，看到的也是“柜外”
  const page2 = await context.newPage()
  await scan(page2, barcode)
  await expect(page2.getByTestId('state-badge')).toHaveText('柜外')

  // 工位 1 归还成功
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')

  // 工位 2 仍停留在过期的“柜外”视图，重复归还被 409 拒绝
  await doReturn(page2, '2026-01-01T00:00:20Z')
  await expect(page2.getByTestId('error-banner')).toContainText('invalid_transition')
  // 拒绝后页面重新拉取权威状态：累计仍是 10 秒，没有二次计时
  await expect(page2.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page2.getByTestId('state-badge')).toHaveText('柜内')

  await page.reload()
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await page2.reload()
  await expect(page2.getByTestId('accumulated')).toHaveText('10 秒')
  await page2.close()
})

test('事件时间不严格递增时被拒绝并提示', async ({ page }) => {
  const barcode = nextBarcode('ORDER')
  await createBatch(page, barcode, 10)

  // 与创建时刻相同：不是“严格晚于”，409
  await takeout(page, T0)
  await expect(page.getByTestId('error-banner')).toContainText('time_not_monotonic')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
})

test('误归还导致报废后可撤销：恢复柜外与原累计，再正确归还', async ({ page }) => {
  const barcode = nextBarcode('UNDO-RETURN')
  await createBatch(page, barcode, 10)

  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:16Z') // 误归还：+11 > 10，报废
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')

  // 撤销这条误归还
  await revokeLast(page, '2026-01-01T00:00:30Z', '误扫归还，样本实际仍在柜外')

  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('remaining')).toHaveText('10 秒')
  await expect(page.getByTestId('last-event')).toContainText('取出 @ 2026-01-01T00:00:05Z')
  await expect(page.getByTestId('info-banner')).toContainText('已撤销事件')

  // 审计结果：撤销时刻与原因显示在该记录旁
  await expect(page.getByTestId('revoked-tag')).toContainText('2026-01-01T00:00:30Z')
  await expect(page.getByTestId('revoked-reason')).toContainText('误扫归还，样本实际仍在柜外')

  // 撤销后取出事件重新成为“最近事件”，归还按钮可用
  await expect(page.getByTestId('return-btn')).toBeEnabled()
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()

  // 继续按正确状态操作：正确归还 +10 恰好等于上限，仍可用
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')

  // 刷新后一致
  await page.reload()
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('revoked-reason')).toContainText('误扫归还，样本实际仍在柜外')
})

test('误取出可撤销：批次回到柜内、无活动事件', async ({ page }) => {
  const barcode = nextBarcode('UNDO-TAKEOUT')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:10Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  await revokeLast(page, '2026-01-01T00:00:20Z', '误扫取出')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('last-event')).toHaveText('尚无事件')
  await expect(page.getByTestId('revoked-reason')).toContainText('误扫取出')
  // 已无未撤销事件，页面不提供撤销按钮
  await expect(page.locator('button[data-test^="revoke-btn-"]')).toHaveCount(0)
})

test('非最近事件不提供撤销入口，直接撤销被服务端 409 拒绝', async ({ page, request }) => {
  const barcode = nextBarcode('UNDO-NONLATEST')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:15Z')

  // 页面上只有最近一条（归还）可撤销
  await expect(page.locator('button[data-test^="revoke-btn-"]')).toHaveCount(1)

  // 直接对非最近的取出事件发起撤销 -> 409 event_not_latest，状态不变
  const evsRes = await request.get(`/api/batches/${barcode}/events`)
  const { events } = await evsRes.json()
  const takeoutId = events[0].id
  const r = await request.post(`/api/batches/${barcode}/events/${takeoutId}/revoke`, {
    data: { at: '2026-01-01T00:00:20Z', reason: '试图撤销旧取出' }
  })
  expect(r.status()).toBe(409)
  expect((await r.json()).error.code).toBe('event_not_latest')

  await page.reload()
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')
  await expect(page.locator('button[data-test^="revoke-btn-"]')).toHaveCount(1)
})

test('柜外风险评估随评估时刻增长并跨过上限，但不落库；原接口归还是否才真正累计报废', async ({ page, request }) => {
  const barcode = nextBarcode('ASSESS')
  await createBatch(page, barcode, 10)
  await takeout(page, '2026-01-01T00:00:05Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  // 查到柜外批次后自动出现评估区（评估至默认当前整秒）
  await expect(page.getByTestId('assess-card')).toBeVisible()
  await expect(page.getByTestId('assess-result')).toBeVisible()

  // 改评估时刻重算：+1 秒 -> 预计累计 1，预计可用
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:06Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-asof')).toHaveText('2026-01-01T00:00:06Z')
  await expect(page.getByTestId('assess-accumulated')).toHaveText('1 秒')
  await expect(page.getByTestId('assess-remaining')).toHaveText('9 秒')
  await expect(page.getByTestId('assess-conclusion')).toHaveText('预计可用')
  expect(await page.getByTestId('assess-over-limit').count()).toBe(0)

  // 恰好上限：累计 10 == 允许 10，仍预计可用
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:15Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('assess-remaining')).toHaveText('0 秒')
  await expect(page.getByTestId('assess-conclusion')).toHaveText('预计可用')

  // 再走一秒跨过上限：明确提示“已预计超限”
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:16Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('assess-remaining')).toHaveText('-1 秒')
  await expect(page.getByTestId('assess-over-limit')).toHaveText('已预计超限')

  // 评估只是提示：持久状态仍是柜外·可用，取出/归还按钮按持久状态工作
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('status-badge')).toHaveText('可用')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('return-btn')).toBeEnabled()
  await expect(page.getByTestId('takeout-btn')).toBeDisabled()

  // 多次评估没有写入任何事件
  const evsRes = await request.get(`/api/batches/${barcode}/events`)
  expect((await evsRes.json()).events).toHaveLength(1)

  // 通过原有归还接口在 +11 秒归还，才真正累计 11 秒并报废
  await doReturn(page, '2026-01-01T00:00:16Z')
  await expect(page.getByTestId('state-badge')).toHaveText('柜内')
  await expect(page.getByTestId('accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('status-badge')).toHaveText('已报废')
  // 柜内评估返回已结算数值
  await expect(page.getByTestId('assess-accumulated')).toHaveText('11 秒')
  await expect(page.getByTestId('assess-conclusion')).toContainText('已结算·已报废')

  const evsRes2 = await request.get(`/api/batches/${barcode}/events`)
  expect((await evsRes2.json()).events).toHaveLength(2)
})

test('评估时刻非法或早于最后事件时就地解释失败，批次与流水不受影响', async ({ page }) => {
  const barcode = nextBarcode('ASSESS-ERR')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:10Z')

  // 格式非法：前端直接拦截，不发请求
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:20+08:00')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-error')).toContainText('RFC3339')
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')

  // 改为合法但早于最后事件：服务端 409，就地解释，已加载批次保留
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:09Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-error')).toContainText('早于该批次最后事件')
  // 旧的评估结果已清空，但批次卡片保持不变
  await expect(page.getByTestId('assess-result')).toBeHidden()
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page.getByTestId('return-btn')).toBeEnabled()

  // 修正为合法时刻后重算成功，失败提示消失
  await page.getByTestId('assess-time').fill('2026-01-01T00:00:20Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-accumulated')).toHaveText('10 秒')
  await expect(page.getByTestId('assess-error')).toBeHidden()
})

test('柜内批次评估返回已结算数值，不带 asOf 的旧查询保持兼容', async ({ page, request }) => {
  const barcode = nextBarcode('ASSESS-IN')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:10Z')
  await doReturn(page, '2026-01-01T00:00:40Z') // +30，柜内

  // 柜内：评估不随时刻增长，显示已结算
  await page.getByTestId('assess-time').fill('2026-01-02T00:00:00Z')
  await page.getByTestId('assess-btn').click()
  await expect(page.getByTestId('assess-accumulated')).toHaveText('30 秒')
  await expect(page.getByTestId('assess-remaining')).toHaveText('70 秒')
  await expect(page.getByTestId('assess-conclusion')).toHaveText('已结算·可用')

  // 旧查询（无 asOf）响应中没有 projection 字段
  const res = await request.get(`/api/batches/${barcode}`)
  expect(res.status()).toBe(200)
  const body = await res.json()
  expect(body.projection).toBeUndefined()
  expect(body.accumulatedSeconds).toBe(30)

  // 参数存在但为空属于时刻格式非法 -> 400（只有完全不传才保持旧语义）
  const empty = await request.get(`/api/batches/${barcode}?asOf=`)
  expect(empty.status()).toBe(400)
  expect((await empty.json()).error.code).toBe('invalid_time')

  // 非法 asOf 返回 400 invalid_time
  const bad = await request.get(`/api/batches/${barcode}?asOf=2026-01-02T00:00:00.5Z`)
  expect(bad.status()).toBe(400)
  expect((await bad.json()).error.code).toBe('invalid_time')

  // 早于最后事件返回 409 time_not_monotonic
  const early = await request.get(`/api/batches/${barcode}?asOf=2026-01-01T00:00:39Z`)
  expect(early.status()).toBe(409)
  expect((await early.json()).error.code).toBe('time_not_monotonic')
})

test('两个工位并发撤销同一最近事件，仅一次成功且不改变失败方累计', async ({ context, page }) => {
  const barcode = nextBarcode('UNDO-RACE')
  await createBatch(page, barcode, 100)
  await takeout(page, '2026-01-01T00:00:05Z')
  await doReturn(page, '2026-01-01T00:00:15Z')
  await expect(page.getByTestId('accumulated')).toHaveText('10 秒')

  const page2 = await context.newPage()
  await scan(page2, barcode)
  await expect(page2.getByTestId('state-badge')).toHaveText('柜内')

  // 两个工位都打开撤销表单，填同一撤销时刻
  await page.locator('button[data-test^="revoke-btn-"]').click()
  await page.getByTestId('revoke-at').fill('2026-01-01T00:00:30Z')
  await page.getByTestId('revoke-reason').fill('工位一撤销')
  await page2.locator('button[data-test^="revoke-btn-"]').click()
  await page2.getByTestId('revoke-at').fill('2026-01-01T00:00:30Z')
  await page2.getByTestId('revoke-reason').fill('工位二撤销')

  await page.getByTestId('revoke-submit').click()
  await expect(page.getByTestId('revoke-form')).toBeHidden()
  await expect(page.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page.getByTestId('accumulated')).toHaveText('0 秒')

  await page2.getByTestId('revoke-submit').click()
  // 失败方收到明确冲突，随后重新载入服务端状态
  await expect(page2.getByTestId('error-banner')).toContainText('撤销被拒绝')
  await expect(page2.getByTestId('revoke-form')).toBeHidden()
  await expect(page2.getByTestId('state-badge')).toHaveText('柜外')
  await expect(page2.getByTestId('accumulated')).toHaveText('0 秒')
  await expect(page2.getByTestId('revoked-reason')).toContainText('工位一撤销')

  await page2.close()
})
