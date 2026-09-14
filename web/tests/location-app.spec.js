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
    revokeEvent: vi.fn(),
    putLocation: vi.fn(),
    clearLocation: vi.fn()
  }
}))

function apiError(status, code, message) {
  const e = new Error(message)
  e.status = status
  e.code = code
  return e
}

const placedBatch = {
  barcode: 'B-1',
  allowedSeconds: 100,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  usable: true,
  remainingSeconds: 100,
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null,
  location: 'A-01'
}

async function scan(w, code = 'B-1') {
  await w.get('[data-test="barcode-input"]').setValue(code)
  await w.get('[data-test="scan-form"]').trigger('submit')
  await flushPromises()
}

beforeEach(() => {
  vi.clearAllMocks()
  localStorage.clear()
  api.listEvents.mockResolvedValue({ events: [] })
  // In-cabinet batches get an asOf assessment on load (settled projection).
  api.getBatch.mockImplementation((code, asOf) => {
    const b = api.getBatch._current
    if (!asOf) return Promise.resolve(b)
    return Promise.resolve({
      ...b,
      projection: {
        asOf,
        accumulatedSeconds: b.accumulatedSeconds,
        remainingSeconds: b.remainingSeconds,
        usable: b.status === 'usable',
        projectedOverLimit: false,
        settled: true
      }
    })
  })
})

function serve(batch) {
  api.getBatch._current = batch
}

describe('App location occupancy flow', () => {
  it('places an unplaced batch into the scanned slot', async () => {
    serve({ ...placedBatch, location: null })
    const updated = { ...placedBatch, location: 'A-01' }
    api.putLocation.mockResolvedValue(updated)

    const w = mount(App)
    await scan(w)
    expect(w.get('[data-test="current-location"]').text()).toContain('未定位')

    await w.get('[data-test="location-input"]').setValue('A-01')
    await w.get('[data-test="location-form"]').trigger('submit')
    await flushPromises()

    expect(api.putLocation).toHaveBeenCalledWith('B-1', 'A-01')
    expect(w.get('[data-test="current-location"]').text()).toContain('A-01')
    expect(w.get('[data-test="location-result"]').text()).toContain('已放置')
  })

  it('moves a placed batch: reports the old slot released and shows the new one', async () => {
    serve({ ...placedBatch })
    api.putLocation.mockResolvedValue({ ...placedBatch, location: 'B-09' })

    const w = mount(App)
    await scan(w)

    await w.get('[data-test="location-input"]').setValue('B-09')
    await w.get('[data-test="location-form"]').trigger('submit')
    await flushPromises()

    expect(api.putLocation).toHaveBeenCalledWith('B-1', 'B-09')
    expect(w.get('[data-test="location-result"]').text()).toContain('旧格位 A-01 已腾空')
    expect(w.get('[data-test="current-location"]').text()).toContain('B-09')
  })

  it('on an occupied-slot conflict keeps the batch and the scanned slot, and explains why', async () => {
    // Batch already sits at B-09; the move to A-01 is rejected because another
    // batch holds it. The authoritative reload still reports B-09.
    serve({ ...placedBatch, location: 'B-09' })
    api.putLocation.mockRejectedValueOnce(
      apiError(409, 'location_occupied', 'location A-01 is already occupied by batch X')
    )

    const w = mount(App)
    await scan(w)
    await w.get('[data-test="location-input"]').setValue('A-01')
    await w.get('[data-test="location-form"]').trigger('submit')
    await flushPromises()

    expect(w.get('[data-test="location-error"]').text()).toContain('已被其他批次占用')
    // The old occupancy is unchanged...
    expect(w.get('[data-test="current-location"]').text()).toContain('B-09')
    // ...and the rejected draft is preserved in the input for another try.
    expect(w.get('[data-test="location-input"]').element.value).toBe('A-01')
    // Timing state and log are untouched.
    expect(w.get('[data-test="accumulated"]').text()).toBe('0 秒')
    expect(api.postEvent).not.toHaveBeenCalled()
  })

  it('rejects placement for an out-of-cabinet batch with an explanation', async () => {
    serve({
      ...placedBatch,
      state: 'out',
      location: null,
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    })

    const w = mount(App)
    await scan(w)
    // The form is disabled while the batch is out.
    expect(w.get('[data-test="location-put-btn"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="location-out-hint"]').text()).toContain('柜外')

    // Returning keeps the batch unplaced; placement then becomes possible.
    const returned = {
      ...placedBatch, state: 'in', location: null, accumulatedSeconds: 10, remainingSeconds: 90,
      lastEvent: { id: 2, type: 'return', at: '2026-09-13T08:00:15Z', deltaSeconds: 10 }
    }
    serve(returned)
    api.postEvent.mockResolvedValueOnce(returned)
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:15Z')
    await w.get('[data-test="return-btn"]').trigger('click')
    await flushPromises()
    expect(w.get('[data-test="current-location"]').text()).toContain('未定位')
    expect(w.get('[data-test="info-banner"]').text()).toContain('未定位')
    expect(w.get('[data-test="location-put-btn"]').attributes('disabled')).toBeUndefined()
  })

  it('a takeout auto-vacates the slot in the same flow', async () => {
    serve({ ...placedBatch, location: 'A-01' })
    const out = {
      ...placedBatch,
      state: 'out',
      location: null,
      lastEvent: { id: 1, type: 'takeout', at: '2026-09-13T08:00:05Z', deltaSeconds: null }
    }
    api.postEvent.mockResolvedValueOnce(out)

    const w = mount(App)
    await scan(w)
    // The post-takeout auto-assessment reads the now-out authoritative batch.
    serve(out)
    await w.get('[data-test="event-time"]').setValue('2026-09-13T08:00:05Z')
    await w.get('[data-test="takeout-btn"]').trigger('click')
    await flushPromises()

    expect(w.get('[data-test="state-badge"]').text()).toBe('柜外')
    expect(w.get('[data-test="current-location"]').text()).toContain('未定位')
    expect(w.get('[data-test="location-input"]').element.value).toBe('')
    expect(w.get('[data-test="info-banner"]').text()).toContain('自动腾空')
    // The occupancy release happens in the event transaction, so the location
    // API is never called.
    expect(api.clearLocation).not.toHaveBeenCalled()
    expect(api.putLocation).not.toHaveBeenCalled()
  })

  it('manual 腾空 releases the slot and explains the result', async () => {
    serve({ ...placedBatch, location: 'A-01' })
    api.clearLocation.mockResolvedValueOnce({ ...placedBatch, location: null })

    const w = mount(App)
    await scan(w)
    await w.get('[data-test="location-clear-btn"]').trigger('click')
    await flushPromises()

    expect(api.clearLocation).toHaveBeenCalledWith('B-1')
    expect(w.get('[data-test="current-location"]').text()).toContain('未定位')
    expect(w.get('[data-test="location-result"]').text()).toContain('已腾空格位 A-01')
  })
})
