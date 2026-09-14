import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import App from '../src/App.vue'
import { api } from '../src/api'

vi.mock('../src/api', () => ({
  api: {
    getBatch: vi.fn(),
    listEvents: vi.fn(),
    createBatch: vi.fn(),
    postEvent: vi.fn(),
    revokeEvent: vi.fn()
  }
}))

const freshBatch = {
  barcode: 'B-1',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  usable: true,
  remainingSeconds: 10,
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null
}

function apiError(status, code, message) {
  const e = new Error(message)
  e.status = status
  e.code = code
  return e
}

// Emulate the server's asOf projection so App's auto-evaluation has realistic
// responses in the component tests.
function withProjection(b, asOf) {
  let accumulated = b.accumulatedSeconds
  let settled = true
  if (b.state === 'out' && b.lastEvent && b.lastEvent.type === 'takeout') {
    settled = false
    accumulated += Math.max(0, (Date.parse(asOf) - Date.parse(b.lastEvent.at)) / 1000)
  }
  return {
    ...b,
    projection: {
      asOf,
      accumulatedSeconds: accumulated,
      remainingSeconds: b.allowedSeconds - accumulated,
      usable: accumulated <= b.allowedSeconds,
      projectedOverLimit: accumulated > b.allowedSeconds,
      settled
    }
  }
}

// Mock-server helper: holds the "persisted" batch; plain getBatch returns it,
// getBatch(code, asOf) returns it with a computed projection.
function mockServer(initial) {
  let current = initial
  api.getBatch.mockImplementation((code, asOf) => {
    if (!asOf) return Promise.resolve(current)
    return Promise.resolve(withProjection(current, asOf))
  })
  return {
    set(b) {
      current = b
    },
    get() {
      return current
    }
  }
}

async function scan(w, code) {
  await w.get('[data-test="barcode-input"]').setValue(code)
  await w.get('[data-test="scan-form"]').trigger('submit')
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  api.listEvents.mockResolvedValue({ events: [] })
})

describe('App', () => {
  it('scanning an unknown barcode offers the create form; creating shows the batch', async () => {
    // First GET (the scan) is a 404 so the create form opens; later GETs
    // (the automatic asOf assessment after creation) respond normally.
    api.getBatch
      .mockRejectedValueOnce(apiError(404, 'not_found', 'no batch with this barcode'))
      .mockImplementation((code, asOf) =>
        Promise.resolve(asOf ? withProjection(freshBatch, asOf) : freshBatch))
    api.createBatch.mockResolvedValue(freshBatch)

    const w = mount(App)
    await scan(w, 'B-1')
    expect(w.find('[data-test="create-form"]').exists()).toBe(true)

    await w.get('[data-test="create-allowed"]').setValue(10)
    await w.get('[data-test="create-created-at"]').setValue('2026-09-13T08:00:00Z')
    await w.get('[data-test="create-form"]').trigger('submit')
    await flushPromises()

    expect(api.createBatch).toHaveBeenCalledWith({
      barcode: 'B-1',
      allowedSeconds: 10,
      createdAt: '2026-09-13T08:00:00Z'
    })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(localStorage.getItem('lastBarcode')).toBe('B-1')
    // In-cabinet batches are evaluated too: the result is a settled projection.
    expect(w.get('[data-test="assess-result"]').exists()).toBe(true)
    expect(w.get('[data-test="assess-conclusion"]').text()).toContain('已结算·可用')
  })

  it('rejects invalid create input locally', async () => {
    api.getBatch.mockRejectedValue(apiError(404, 'not_found', 'nope'))
    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="create-created-at"]').setValue('2026-09-13 08:00:00')
    await w.get('[data-test="create-form"]').trigger('submit')
    await flushPromises()
    expect(api.createBatch).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')
  })

  it('takeout then return updates the displayed state', async () => {
    const srv = mockServer(freshBatch)
    const outBatch = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    const returnedBatch = {
      ...freshBatch,
      accumulatedSeconds: 10,
      remainingSeconds: 0,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    }
    api.postEvent.mockImplementation(async () => {
      const next = srv.get().state === 'in' ? outBatch : returnedBatch
      srv.set(next)
      return next
    })

    const w = mount(App)
    await scan(w, 'B-1')
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')

    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:05Z')
    await w.get('[data-test="takeout-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).toHaveBeenNthCalledWith(1, 'B-1', { type: 'takeout', at: '2026-09-13T08:00:05Z' })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')

    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:15Z')
    await w.get('[data-test="return-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).toHaveBeenNthCalledWith(2, 'B-1', { type: 'return', at: '2026-09-13T08:00:15Z' })
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="remaining"]').text()).toBe('0 秒')
    expect(w.get('[data-test="conclusion"]').text()).toBe('可用')
    // Back in the cabinet, the evaluation reports settled values.
    expect(w.get('[data-test="assess-conclusion"]').text()).toContain('已结算·可用')
    expect(w.get('[data-test="assess-accumulated"]').text()).toBe('10 秒')
  })

  it('auto-evaluates an out batch and can recompute at a chosen asOf time', async () => {
    const outBatch = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    mockServer(outBatch)

    const w = mount(App)
    await scan(w, 'B-1')

    // An out-of-cabinet scan auto-runs one assessment with an asOf argument.
    const assessCalls = api.getBatch.mock.calls.filter((c) => c[1])
    expect(assessCalls.length).toBeGreaterThanOrEqual(1)
    expect(w.get('[data-test="assess-result"]').isVisible()).toBe(true)

    // Recompute at a time still within the limit: projected 10/10, usable.
    await w.get('[data-test="assess-time"]').setValue('2026-09-13T08:00:15Z')
    await w.get('[data-test="assess-form"]').trigger('submit')
    await flushPromises()
    expect(api.getBatch).toHaveBeenLastCalledWith('B-1', '2026-09-13T08:00:15Z')
    expect(w.get('[data-test="assess-accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="assess-remaining"]').text()).toBe('0 秒')
    expect(w.get('[data-test="assess-conclusion"]').text()).toBe('预计可用')
    expect(w.find('[data-test="assess-over-limit"]').exists()).toBe(false)

    // One second later the projection crosses the limit...
    await w.get('[data-test="assess-time"]').setValue('2026-09-13T08:00:16Z')
    await w.get('[data-test="assess-form"]').trigger('submit')
    await flushPromises()
    expect(w.get('[data-test="assess-accumulated"]').text()).toBe('11 秒')
    expect(w.get('[data-test="assess-conclusion"]').text()).toContain('已预计超限')
    expect(w.get('[data-test="assess-over-limit"]').text()).toBe('已预计超限')

    // ...but the persistent state is untouched and 归还 stays enabled.
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="status-badge"]').text()).toBe('可用')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeUndefined()
    expect(w.get('[data-test="takeout-btn"]').attributes('disabled')).toBeDefined()
  })

  it('rejects a malformed assess time locally without touching the API', async () => {
    mockServer({
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    })
    const w = mount(App)
    await scan(w, 'B-1')
    const callsBefore = api.getBatch.mock.calls.length

    await w.get('[data-test="assess-time"]').setValue('2026-09-13T08:00:16+08:00')
    await w.get('[data-test="assess-form"]').trigger('submit')
    await flushPromises()

    expect(api.getBatch).toHaveBeenCalledTimes(callsBefore)
    expect(w.get('[data-test="assess-error"]').text()).toContain('RFC3339')
    // Loaded batch stays on screen and 归还 still works off persisted state.
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeUndefined()
  })

  it('keeps the loaded batch when an assessment 409s, and explains it in place', async () => {
    const outBatch = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:10Z', deltaSeconds: null }
    }
    mockServer(outBatch)
    const w = mount(App)
    await scan(w, 'B-1')

    // A later explicit assessment at a pre-last-event time conflicts.
    api.getBatch.mockRejectedValueOnce(
      apiError(409, 'time_not_monotonic', 'earlier than the last event')
    )
    await w.get('[data-test="assess-time"]').setValue('2026-09-13T08:00:05Z')
    await w.get('[data-test="assess-form"]').trigger('submit')
    await flushPromises()

    expect(w.get('[data-test="assess-error"]').text()).toContain('早于该批次最后事件')
    expect(w.find('[data-test="assess-result"]').exists()).toBe(false)
    // Persistent card is unchanged.
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(w.get('[data-test="return-btn"]').attributes('disabled')).toBeUndefined()

    // Fixing the time recomputes successfully.
    await w.get('[data-test="assess-time"]').setValue('2026-09-13T08:00:11Z')
    await w.get('[data-test="assess-form"]').trigger('submit')
    await flushPromises()
    expect(w.find('[data-test="assess-error"]').exists()).toBe(false)
    expect(w.get('[data-test="assess-accumulated"]').text()).toBe('1 秒')
  })

  it('409 on a stale/duplicate action shows the error and reloads truth', async () => {
    // Second tab scenario: our view says "out", but the batch was already returned.
    const outBatch = { ...freshBatch, state: 'out' }
    const alreadyReturned = { ...freshBatch, accumulatedSeconds: 10, remainingSeconds: 0 }
    const srv = mockServer(outBatch)
    api.postEvent.mockRejectedValueOnce(apiError(409, 'invalid_transition', 'batch is not out of the cabinet'))

    const w = mount(App)
    await scan(w, 'B-1')
    srv.set(alreadyReturned) // authoritative state by the time the conflict arrives
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:20Z')
    await w.get('[data-test="return-btn"]').trigger('click')
    await flushPromises()

    expect(w.get('[data-test="error-banner"]').text()).toContain('invalid_transition')
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
  })

  it('rejects a malformed event time without calling the API', async () => {
    mockServer(freshBatch)
    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:05+08:00')
    await w.get('[data-test="takeout-btn"]').trigger('click')
    await flushPromises()
    expect(api.postEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')
  })

  it('restores the last scanned batch after a page refresh', async () => {
    localStorage.setItem('lastBarcode', 'B-9')
    mockServer({ ...freshBatch, barcode: 'B-9' })
    const w = mount(App)
    await flushPromises()
    expect(api.getBatch.mock.calls[0]).toEqual(['B-9'])
    expect(w.get('[data-test="card-barcode"]').text()).toBe('B-9')
  })

  const takeoutEvent = { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
  const mistakenReturn = { id: 2, type: 'return', at: '2026-09-13T08:00:16Z', deltaSeconds: 11 }

  it('revokes the latest mistaken return and restores the out/usable state with audit info', async () => {
    const srv = mockServer({
      ...freshBatch,
      state: 'in',
      status: 'scrapped',
      usable: false,
      accumulatedSeconds: 11,
      remainingSeconds: -1,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:16Z', deltaSeconds: 11 }
    })
    api.listEvents.mockResolvedValueOnce({ events: [takeoutEvent, mistakenReturn] })

    const w = mount(App)
    await scan(w, 'B-1')

    // Only the latest non-revoked event offers a revoke button.
    expect(w.find('[data-test="revoke-btn-1"]').exists()).toBe(false)
    await w.get('[data-test="revoke-btn-2"]').trigger('click')
    expect(w.find('[data-test="revoke-form"]').exists()).toBe(true)

    await w.get('[data-test="revoke-at"]').setValue('2026-09-13T08:00:30Z')
    await w.get('[data-test="revoke-reason"]').setValue('误扫归还，样本仍在柜外')

    const restored = {
      ...freshBatch,
      state: 'out',
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    srv.set(restored)
    api.revokeEvent.mockResolvedValueOnce(restored)
    api.listEvents.mockResolvedValueOnce({
      events: [
        takeoutEvent,
        {
          ...mistakenReturn,
          revokedAt: '2026-09-13T08:00:30Z',
          revokeReason: '误扫归还，样本仍在柜外'
        }
      ]
    })

    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()

    expect(api.revokeEvent).toHaveBeenCalledWith('B-1', 2, {
      at: '2026-09-13T08:00:30Z',
      reason: '误扫归还，样本仍在柜外'
    })
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="status-badge"]').text()).toBe('可用')
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(w.find('[data-test="revoke-form"]').exists()).toBe(false)
    expect(w.get('[data-test="info-banner"]').text()).toContain('已撤销事件 #2')

    // The revoked row shows the audit result, and the takeout is revocable now.
    expect(w.get('[data-test="event-row-2"]').text()).toContain('已撤销 @ 2026-09-13T08:00:30Z')
    expect(w.get('[data-test="revoked-reason"]').text()).toContain('误扫归还，样本仍在柜外')
    expect(w.find('[data-test="revoke-btn-1"]').exists()).toBe(true)
    expect(w.find('[data-test="revoke-btn-2"]').exists()).toBe(false)
  })

  it('blocks a revoke locally for a blank reason or malformed time', async () => {
    mockServer({ ...freshBatch, state: 'out', lastEvent: takeoutEvent })
    api.listEvents.mockResolvedValue({ events: [takeoutEvent] })
    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="revoke-btn-1"]').trigger('click')

    // Blank reason.
    await w.get('[data-test="revoke-at"]').setValue('2026-09-13T08:00:20Z')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()
    expect(api.revokeEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('非空撤销原因')

    // Malformed time.
    await w.get('[data-test="revoke-reason"]').setValue('误扫取出')
    await w.get('[data-test="revoke-at"]').setValue('2026-09-13T08:00:20+08:00')
    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()
    expect(api.revokeEvent).not.toHaveBeenCalled()
    expect(w.get('[data-test="error-banner"]').text()).toContain('RFC3339')

    // Cancel hides the form without calling the API.
    await w.get('[data-test="revoke-cancel"]').trigger('click')
    expect(w.find('[data-test="revoke-form"]').exists()).toBe(false)
    expect(api.revokeEvent).not.toHaveBeenCalled()
  })

  it('409 on a racing/duplicate revoke shows the error and reloads server truth', async () => {
    const returned = {
      ...freshBatch,
      state: 'in',
      accumulatedSeconds: 10,
      remainingSeconds: 0,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    }
    const srv = mockServer(returned)
    api.listEvents.mockResolvedValue({ events: [takeoutEvent, { ...mistakenReturn, id: 2, deltaSeconds: 10 }] })

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="revoke-btn-2"]').trigger('click')
    await w.get('[data-test="revoke-at"]').setValue('2026-09-13T08:00:30Z')
    await w.get('[data-test="revoke-reason"]').setValue('racing undo')

    api.revokeEvent.mockRejectedValueOnce(apiError(409, 'event_already_revoked', 'already undone'))
    api.listEvents.mockResolvedValueOnce({
      events: [
        takeoutEvent,
        {
          id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10,
          revokedAt: '2026-09-13T08:00:25Z', revokeReason: 'other station'
        }
      ]
    })

    await w.get('[data-test="revoke-form"]').trigger('submit')
    await flushPromises()

    expect(api.revokeEvent).toHaveBeenCalledTimes(1)
    expect(w.get('[data-test="error-banner"]').text()).toContain('event_already_revoked')
    expect(w.find('[data-test="revoke-form"]').exists()).toBe(false)
    // Totals and in/out state come from the reloaded server state.
    expect(w.get('[data-test="accumulated"]').text()).toBe('10 秒')
    expect(w.get('[data-test="state-badge"]').text()).toBe('柜内')
    expect(w.get('[data-test="revoked-reason"]').text()).toContain('other station')
  })
})
