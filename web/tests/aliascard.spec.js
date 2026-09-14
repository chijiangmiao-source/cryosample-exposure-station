import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import AliasCard from '../src/components/AliasCard.vue'

const fresh = {
  barcode: 'B-100',
  allowedSeconds: 10,
  accumulatedSeconds: 0,
  state: 'in',
  status: 'usable',
  createdAt: '2026-09-13T08:00:00Z',
  lastEvent: null,
  location: null,
  aliases: []
}

describe('AliasCard', () => {
  it('shows the primary barcode and an empty alias list', () => {
    const w = mount(AliasCard, { props: { batch: fresh } })
    expect(w.get('[data-test="primary-barcode"]').text()).toBe('B-100')
    expect(w.get('[data-test="alias-empty"]').text()).toContain('尚无备用条码')
    expect(w.find('[data-test="alias-hit-banner"]').exists()).toBe(false)
  })

  it('emits bind with the scanned backup code and clears the draft on success', async () => {
    const w = mount(AliasCard, { props: { batch: fresh } })
    await w.get('[data-test="alias-input"]').setValue('B-100-OLD')
    await w.get('[data-test="alias-form"]').trigger('submit')
    expect(w.emitted('bind')).toEqual([['B-100-OLD']])

    // Server confirms: the batch now carries the alias and App passes it down.
    await w.setProps({
      resultKind: 'info',
      resultText: '已绑定',
      batch: { ...fresh, aliases: ['B-100-OLD'] }
    })
    expect(w.get('[data-test="alias-code"]').text()).toBe('B-100-OLD')
    expect(w.get('[data-test="alias-input"]').element.value).toBe('')
  })

  it('keeps the typed draft and shows the error when the bind is rejected', async () => {
    const w = mount(AliasCard, { props: { batch: fresh } })
    await w.get('[data-test="alias-input"]').setValue('OTHER-MAIN')
    await w.get('[data-test="alias-form"]').trigger('submit')

    await w.setProps({ resultKind: 'error', resultText: '不能与主条码重名' })
    expect(w.get('[data-test="alias-input"]').element.value).toBe('OTHER-MAIN', 'draft preserved')
    expect(w.get('[data-test="alias-error"]').text()).toContain('不能与主条码重名')
    // Nothing bound yet.
    expect(w.find('[data-test="alias-code"]').exists()).toBe(false)
  })

  it('lists bound aliases and emits unbind with the removed code', async () => {
    const w = mount(AliasCard, {
      props: { batch: { ...fresh, aliases: ['ALT-1', 'ALT-2'] } }
    })
    const rows = w.findAll('[data-test^="alias-row-"]')
    expect(rows).toHaveLength(2)
    await w.get('[data-test="alias-unbind-ALT-1"]').trigger('click')
    expect(w.emitted('unbind')).toEqual([['ALT-1']])

    // After the server confirms removal only ALT-2 remains.
    await w.setProps({
      resultKind: 'info',
      resultText: '已解除',
      batch: { ...fresh, aliases: ['ALT-2'] }
    })
    expect(w.findAll('[data-test^="alias-row-"]')).toHaveLength(1)
  })

  it('shows the scanned-alias hint when matchedBarcode is set', () => {
    const w = mount(AliasCard, {
      props: { batch: { ...fresh, aliases: ['B-100-OLD'] }, matchedBarcode: 'B-100-OLD' }
    })
    const banner = w.get('[data-test="alias-hit-banner"]')
    expect(banner.text()).toContain('B-100-OLD')
    expect(banner.text()).toContain('B-100')
  })

  it('disables the form while busy', () => {
    const w = mount(AliasCard, { props: { batch: fresh, busy: true } })
    expect(w.get('[data-test="alias-input"]').attributes('disabled')).toBeDefined()
    expect(w.get('[data-test="alias-bind-btn"]').attributes('disabled')).toBeDefined()
  })
})
