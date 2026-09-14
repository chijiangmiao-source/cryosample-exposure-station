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
    clearLocation: vi.fn(),
    bindAlias: vi.fn(),
    unbindAlias: vi.fn()
  }
}))

function apiError(status, code, message) {
  const e = new Error(message)
  e.status = status
  e.code = code
  return e
}

const canonical = {
  barcode: 'B-1',
  allowedSeconds: 100,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  usable: true,
  remainingSeconds: 100,
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null,
  location: null,
  aliases: []
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
  api.getBatch.mockImplementation((code, asOf) => {
    const b = api.getBatch._current
    if (!asOf) return Promise.resolve(b)
    return Promise.resolve({
      ...b,
      projection: {
        asOf, accumulatedSeconds: b.accumulatedSeconds, remainingSeconds: b.remainingSeconds,
        usable: b.status === 'usable', projectedOverLimit: false, settled: true
      }
    })
  })
})

function serve(batch) {
  api.getBatch._current = batch
}

describe('App backup-barcode (alias) flow', () => {
  it('scanning a bound alias loads the canonical batch and shows the hit hint', async () => {
    serve({ ...canonical, matchedBarcode: 'B-1-OLD', aliases: ['B-1-OLD'] })
    const w = mount(App)
    await scan(w, 'B-1-OLD')

    // The card always shows the canonical primary barcode.
    expect(w.get('[data-test="card-barcode"]').text()).toBe('B-1')
    expect(w.get('[data-test="primary-barcode"]').text()).toBe('B-1')
    // The optional hit code drives the "used a backup label" hint.
    expect(w.get('[data-test="alias-hit-banner"]').text()).toContain('B-1-OLD')
    expect(w.get('[data-test="alias-hit-banner"]').text()).toContain('B-1')
    expect(api.getBatch).toHaveBeenCalledWith('B-1-OLD')
  })

  it('binds a typed backup code and lists it; later operations use the canonical barcode', async () => {
    serve({ ...canonical })
    api.bindAlias.mockImplementation(async (barcode, alias) => {
      const next = { ...api.getBatch._current, aliases: [alias] }
      api.getBatch._current = next
      return next
    })

    const w = mount(App)
    await scan(w, 'B-1')

    await w.get('[data-test="alias-input"]').setValue('B-1-NEW')
    await w.get('[data-test="alias-form"]').trigger('submit')
    await flushPromises()

    expect(api.bindAlias).toHaveBeenCalledWith('B-1', 'B-1-NEW')
    expect(w.get('[data-test="alias-code"]').text()).toBe('B-1-NEW')
    expect(w.get('[data-test="alias-result"]').text()).toContain('已绑定备用条码')
    expect(w.get('[data-test="alias-input"]').element.value).toBe('')
  })

  it('a name/duplicate conflict keeps the batch and the typed input and explains why', async () => {
    serve({ ...canonical })
    api.bindAlias.mockRejectedValueOnce(
      apiError(409, 'alias_conflicts_barcode', 'code is a primary barcode')
    )

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="alias-input"]').setValue('B-2')
    await w.get('[data-test="alias-form"]').trigger('submit')
    await flushPromises()

    expect(api.bindAlias).toHaveBeenCalledTimes(1)
    expect(w.get('[data-test="alias-error"]').text()).toContain('主条码')
    expect(w.get('[data-test="alias-input"]').element.value).toBe('B-2', 'draft preserved')
    expect(w.get('[data-test="card-barcode"]').text()).toBe('B-1', 'batch unchanged')
  })

  it('unbinds an alias through its row button', async () => {
    serve({ ...canonical, aliases: ['B-1-OLD'] })
    api.unbindAlias.mockImplementation(async () => {
      const next = { ...api.getBatch._current, aliases: [] }
      api.getBatch._current = next
      return next
    })

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="alias-unbind-B-1-OLD"]').trigger('click')
    await flushPromises()

    expect(api.unbindAlias).toHaveBeenCalledWith('B-1', 'B-1-OLD')
    expect(w.get('[data-test="alias-empty"]').text()).toContain('尚无备用条码')
    expect(w.get('[data-test="alias-result"]').text()).toContain('已解除备用条码')
  })

  it('a failed unbind (foreign/missing alias) changes neither events nor the slot', async () => {
    serve({ ...canonical, location: 'A-01', aliases: ['B-1-OLD'] })
    api.unbindAlias.mockRejectedValueOnce(
      apiError(409, 'alias_not_bound', 'not a backup barcode of this batch')
    )

    const w = mount(App)
    await scan(w, 'B-1')
    await w.get('[data-test="alias-unbind-B-1-OLD"]').trigger('click')
    await flushPromises()

    expect(w.get('[data-test="alias-error"]').text()).toContain('不属于当前批次')
    // Authoritative batch still occupies the slot and keeps the alias.
    expect(w.get('[data-test="current-location"]').text()).toContain('A-01')
    expect(w.get('[data-test="alias-code"]').text()).toBe('B-1-OLD')
    expect(api.postEvent).not.toHaveBeenCalled()
  })
})
