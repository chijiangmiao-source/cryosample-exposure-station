import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import LocationCard from '../src/components/LocationCard.vue'

const fresh = {
  barcode: 'B-100',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null,
  location: null
}

function setProps(w, props) {
  w.setProps({ batch: { ...w.props('batch'), ...props } })
}

describe('LocationCard', () => {
  it('shows an unplaced in-cabinet batch and emits place with the scanned slot', async () => {
    const w = mount(LocationCard, { props: { batch: fresh } })
    expect(w.get('[data-test="current-location"]').text()).toContain('未定位')
    expect(w.get('[data-test="location-put-btn"]').text()).toBe('放置到此格位')
    expect(w.get('[data-test="location-clear-btn"]').attributes('disabled')).toBeDefined()

    await w.get('[data-test="location-input"]').setValue('A-01')
    await w.get('[data-test="location-form"]').trigger('submit')
    expect(w.emitted('place')).toEqual([['A-01']])
  })

  it('shows the current slot, offers 移位 and emits clear on 腾空', async () => {
    const placed = { ...fresh, location: 'A-01' }
    const w = mount(LocationCard, { props: { batch: placed } })
    expect(w.get('[data-test="current-location"]').text()).toContain('A-01')
    expect(w.get('[data-test="location-put-btn"]').text()).toBe('移位到此格位')
    expect(w.get('[data-test="location-clear-btn"]').attributes('disabled')).toBeUndefined()

    await w.get('[data-test="location-input"]').setValue('B-02')
    await w.get('[data-test="location-form"]').trigger('submit')
    expect(w.emitted('place')).toEqual([['B-02']])

    await w.get('[data-test="location-clear-btn"]').trigger('click')
    expect(w.emitted('clear')).toHaveLength(1)
  })

  it('keeps the scanned draft and shows the error when the placement is rejected', async () => {
    // 1) type a slot and submit (pending confirmation for C-09)
    const w = mount(LocationCard, { props: { batch: fresh } })
    await w.get('[data-test="location-input"]').setValue('C-09')
    await w.get('[data-test="location-form"]').trigger('submit')

    // 2) Server rejects it: the authoritative batch stays unplaced (location
    //    unchanged) and App surfaces the conflict explanation.
    await w.setProps({ resultKind: 'error', resultText: '目标格位已被其他批次占用' })
    expect(w.get('[data-test="location-input"]').element.value).toBe('C-09', 'draft preserved')
    expect(w.get('[data-test="location-error"]').text()).toContain('已被其他批次占用')

    // 3) Correcting to a free slot that succeeds adopts the server slot.
    await w.get('[data-test="location-input"]').setValue('C-10')
    await w.get('[data-test="location-form"]').trigger('submit')
    await w.setProps({ resultKind: 'info', resultText: '已放置' })
    await w.setProps({ batch: { ...fresh, location: 'C-10' } })
    expect(w.get('[data-test="location-input"]').element.value).toBe('C-10')
    expect(w.get('[data-test="current-location"]').text()).toContain('C-10')
  })

  it('disables the form for out-of-cabinet and scrapped batches', async () => {
    const w = mount(LocationCard, {
      props: { batch: { ...fresh, state: 'out' } }
    })
    expect(w.get('[data-test="location-input"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="location-put-btn"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="location-out-hint"]').text()).toContain('柜外')

    await w.setProps({ batch: { ...fresh, state: 'in', status: 'scrapped' } })
    expect(w.get('[data-test="location-put-btn"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="location-scrapped-hint"]').text()).toContain('报废')
  })

  it('restores the real slot from the server on batch switch / refresh', async () => {
    const w = mount(LocationCard, { props: { batch: fresh } })
    await w.get('[data-test="location-input"]').setValue('typed-but-not-submitted')

    // Simulated page refresh: same barcode, server now reports an authoritative slot.
    await w.setProps({ batch: { ...fresh, location: 'D-04' } })
    expect(w.get('[data-test="location-input"]').element.value).toBe('D-04')

    // Switching to a different batch resyncs to that batch's slot.
    await w.setProps({ batch: { ...fresh, barcode: 'B-200', location: null } })
    expect(w.get('[data-test="location-input"]').element.value).toBe('')
  })

  it('clears the input when the slot is auto-released by a takeout', async () => {
    const w = mount(LocationCard, { props: { batch: { ...fresh, location: 'A-01' } } })
    expect(w.get('[data-test="location-input"]').element.value).toBe('A-01')
    await w.setProps({ batch: { ...fresh, location: null, state: 'out' } })
    expect(w.get('[data-test="location-input"]').element.value).toBe('')
    expect(w.get('[data-test="location-out-hint"]').isVisible()).toBe(true)
  })
})
