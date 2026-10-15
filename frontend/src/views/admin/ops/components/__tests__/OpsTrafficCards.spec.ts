import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import OpsTrafficCards from '../OpsTrafficCards.vue'
import type { OpsTrafficLink, OpsTrafficSummary } from '@/api/admin/ops'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params ? `${key}:${JSON.stringify(params)}` : key })
}))
enableAutoUnmount(afterEach)
afterEach(() => {
  document.body.innerHTML = ''
})

function bodyTestId(testId: string): HTMLElement {
  const element = document.body.querySelector<HTMLElement>(`[data-testid="${testId}"]`)
  if (!element) throw new Error(`Missing teleported element: ${testId}`)
  return element
}

function tooltipFor(testId: string): HTMLElement {
  const tooltip = bodyTestId(testId).closest<HTMLElement>('[role="tooltip"]')
  if (!tooltip) throw new Error(`Missing tooltip for: ${testId}`)
  return tooltip
}

function link(overrides: Partial<OpsTrafficLink> = {}): OpsTrafficLink {
  return {
    original_bytes: 4096, actual_bytes: 2048,
    original_sample_count: 4, actual_sample_count: 4, paired_sample_count: 4,
    compression_percent: 50, ...overrides
  }
}
function traffic(overrides: Partial<OpsTrafficSummary> = {}): OpsTrafficSummary {
  return { source: 'http_requests', request_count: 2, upstream: link(), downstream: link(), ...overrides }
}

const unknownLink = (): OpsTrafficLink => link({
  original_bytes: null, actual_bytes: null,
  original_sample_count: 0, actual_sample_count: 0, paired_sample_count: 0,
  compression_percent: null
})

function mountCards(value?: OpsTrafficSummary | null, loading = false) {
  return mount(OpsTrafficCards, { props: { traffic: value, loading } })
}

describe('OpsTrafficCards', () => {
  it('shows three overview-sized cards with original/actual totals and both compression rates', () => {
    const wrapper = mountCards(traffic({
      downstream: link({ original_bytes: 2048, actual_bytes: 2560, compression_percent: -25 })
    }))
    const articles = wrapper.findAll('article')
    expect(articles).toHaveLength(3)
    for (const article of articles) {
      expect(article.classes()).toEqual(expect.arrayContaining(['min-w-0', 'rounded-2xl', 'bg-gray-50', 'p-4', 'dark:bg-dark-900']))
    }
    expect(wrapper.get('[data-testid="traffic-upstream-card"]').attributes('style')).toContain('order: 8')
    expect(wrapper.get('[data-testid="traffic-downstream-card"]').attributes('style')).toContain('order: 9')
    expect(wrapper.get('[data-testid="traffic-composition-card"]').attributes('style')).toContain('order: 10')
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('4 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('2.5 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('50.0%')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('-25.0%')
    expect(bodyTestId('traffic-scope').textContent).toBe('admin.ops.traffic.scope')
    expect(bodyTestId('traffic-compression-hint').textContent).toBe('admin.ops.traffic.compressionHint')
  })

  it('keeps long scope, formula and coverage details hidden until their help icons are hovered', async () => {
    const wrapper = mountCards(traffic())
    const composition = wrapper.get('[data-testid="traffic-composition-card"]')
    expect(composition.text()).not.toContain('admin.ops.traffic.scope')
    expect(composition.text()).not.toContain('admin.ops.traffic.compressionHint')
    expect(composition.text()).not.toContain('admin.ops.traffic.pairedCoverage')

    const compositionTooltip = tooltipFor('traffic-scope')
    expect(compositionTooltip.style.display).toBe('none')
    const compositionTrigger = wrapper.get('[data-testid="traffic-composition-tooltip"]')
    await compositionTrigger.trigger('mouseenter')
    await nextTick()
    expect(compositionTooltip.style.display).not.toBe('none')
    expect(compositionTooltip.textContent).toContain('admin.ops.traffic.scope')
    expect(compositionTooltip.textContent).toContain('admin.ops.traffic.compressionHint')
    expect(compositionTooltip.textContent).toContain('admin.ops.traffic.pairedCoverage')
    expect(compositionTooltip.textContent).toContain('admin.ops.traffic.upstream')
    expect(compositionTooltip.textContent).toContain('admin.ops.traffic.downstream')
    await compositionTrigger.trigger('mouseleave')
    await nextTick()
    expect(compositionTooltip.style.display).toBe('none')

    const coverageTooltip = tooltipFor('traffic-upstream-original-coverage')
    const coverageTrigger = wrapper.get('[data-testid="traffic-upstream-coverage-tooltip"]')
    await coverageTrigger.trigger('mouseenter')
    await nextTick()
    expect(coverageTooltip.style.display).not.toBe('none')
    expect(coverageTooltip.textContent).toContain('admin.ops.traffic.coverage')
  })

  it.each([undefined, null, traffic({ upstream: unknownLink(), downstream: unknownLink() })])(
    'shows missing/legacy measurements as unknown, never zero: %s', (value) => {
      const wrapper = mountCards(value)
      for (const side of ['upstream', 'downstream']) {
        for (const metric of ['original', 'actual', 'compression']) {
          expect(wrapper.get(`[data-testid="traffic-${side}-${metric}"]`).text()).toBe('—')
        }
        expect(wrapper.get(`[data-testid="traffic-${side}-coverage-state"]`).text()).toBe('admin.ops.traffic.unavailable')
      }
      expect(wrapper.text()).not.toContain('0 B')
    }
  )

  it('distinguishes measured empty bodies from no measurements and keeps zero-original compression unknown', () => {
    const wrapper = mountCards(traffic({
      upstream: link({ original_bytes: 0, actual_bytes: 0, compression_percent: null }),
      downstream: link({ original_bytes: null, actual_bytes: 0, original_sample_count: 0, paired_sample_count: 0, compression_percent: null })
    }))
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('—')
  })

  it('shows per-metric and paired coverage and uses the server paired ratio rather than independently covered totals', () => {
    const wrapper = mountCards(traffic({
      request_count: 5,
      upstream: link({ original_bytes: 100, actual_bytes: 500, original_sample_count: 4, actual_sample_count: 7, paired_sample_count: 2, compression_percent: 25 })
    }))
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.original')
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.coverage:{"count":4,"total":10}')
    expect(bodyTestId('traffic-upstream-actual-coverage').textContent).toContain('admin.ops.traffic.actual')
    expect(bodyTestId('traffic-upstream-actual-coverage').textContent).toContain('admin.ops.traffic.coverage:{"count":7,"total":10}')
    expect(bodyTestId('traffic-upstream-paired-coverage').textContent).toContain('admin.ops.traffic.upstream')
    expect(bodyTestId('traffic-upstream-paired-coverage').textContent).toContain('admin.ops.traffic.pairedCoverage:{"count":2,"total":10}')
    expect(wrapper.get('[data-testid="traffic-upstream-coverage-state"]').text()).toBe('admin.ops.traffic.partial')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('25.0%')
  })

  it('does not derive a compression rate when there are no paired samples or their original sum is zero', () => {
    const wrapper = mountCards(traffic({
      upstream: link({ original_sample_count: 2, actual_sample_count: 2, paired_sample_count: 0, compression_percent: null }),
      // Independently observed originals can be nonzero even if paired originals are zero.
      downstream: link({ paired_sample_count: 1, compression_percent: null })
    }))
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('—')
  })

  it('uses binary units for large byte totals without overflowing card values', () => {
    const wrapper = mountCards(traffic({
      upstream: link({ original_bytes: 1024 ** 6, actual_bytes: 1024 ** 5 }),
      downstream: link({ original_bytes: 1024 ** 4, actual_bytes: 1024 ** 3 })
    }))
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('1 EiB')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('1 PiB')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('1 TiB')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('1 GiB')
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').classes()).toContain('[overflow-wrap:anywhere]')
  })

  it('marks loading and hides stale totals, coverage and ratios until the filtered overview arrives', async () => {
    const wrapper = mountCards(traffic(), true)
    expect(wrapper.get('[data-testid="traffic-upstream-card"]').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('[data-testid="traffic-composition-card"]').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('[role="status"]').text()).toBe('admin.ops.loadingText')
    expect(wrapper.find('[data-testid="traffic-upstream-original"]').exists()).toBe(false)
    expect(document.body.querySelector('[data-testid="traffic-upstream-original-coverage"]')).toBeNull()
    expect(wrapper.find('[data-testid="traffic-upstream-compression"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="traffic-upstream-coverage-state"]').exists()).toBe(false)
    await wrapper.setProps({ loading: false, traffic: traffic({ upstream: link({ original_bytes: 1024 ** 2 }) }) })
    expect(wrapper.get('[data-testid="traffic-upstream-card"]').attributes('aria-busy')).toBe('false')
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('1 MiB')
  })

  it('handles invalid byte values, nonfinite ratios and unavailable coverage without fabricated zeroes', () => {
    const wrapper = mountCards(traffic({
      request_count: Number.NaN,
      upstream: link({ original_bytes: Number.NaN, actual_bytes: -1, compression_percent: Number.POSITIVE_INFINITY })
    }))
    for (const metric of ['original', 'actual', 'compression']) {
      expect(wrapper.get(`[data-testid="traffic-upstream-${metric}"]`).text()).toBe('—')
    }
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.coverageUnknown')
  })
})

describe('OpsTrafficCards ingestion health', () => {
  it.each([undefined, null, { scope: 'instance_lifetime' as const, pending: 0, failed: 0, flushed: 100 }])(
    'does not claim healthy/complete ingestion when there is no known problem: %s', (ingestion) => {
      const wrapper = mountCards(traffic({ ingestion }))
      expect(wrapper.find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
      expect(wrapper.text()).not.toContain('healthy')
      expect(wrapper.text()).not.toContain('complete')
    }
  )

  it('prominently warns about sticky record loss despite full persisted measurement coverage', async () => {
    const wrapper = mountCards(traffic({
      ingestion: { scope: 'instance_lifetime', pending: 0, failed: 3, flushed: 100 }
    }))
    const warning = wrapper.get('[data-testid="traffic-ingestion-warning"]')
    expect(warning.attributes('role')).toBe('alert')
    expect(warning.classes()).toContain('bg-red-50')
    expect(warning.get('[data-testid="traffic-ingestion-failed-short"]').text()).toBe('admin.ops.traffic.ingestionFailedShort:{"count":3}')
    expect(warning.text()).not.toContain('admin.ops.traffic.ingestionScope')
    const tooltip = tooltipFor('traffic-ingestion-failed')
    expect(tooltip.style.display).toBe('none')
    await warning.trigger('mouseenter')
    await nextTick()
    expect(tooltip.style.display).not.toBe('none')
    expect(bodyTestId('traffic-ingestion-failed').textContent).toBe('admin.ops.traffic.ingestionFailed:{"count":3}')
    expect(bodyTestId('traffic-ingestion-scope').textContent).toBe('admin.ops.traffic.ingestionScope')
    expect(bodyTestId('traffic-ingestion-coverage').textContent).toBe('admin.ops.traffic.ingestionCoverage')
    expect(document.body.querySelector('[data-testid="traffic-ingestion-pending"]')).toBeNull()
    // Ingestion lifetime counts must not be added to the filtered denominator.
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.coverage:{"count":4,"total":4}')
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('4 KiB')
  })

  it('shows pending persistence without claiming record loss and clears it on the next overview', async () => {
    const wrapper = mountCards(traffic({
      ingestion: { scope: 'instance_lifetime', pending: 2, failed: 0, flushed: 100 }
    }))
    const warning = wrapper.get('[data-testid="traffic-ingestion-warning"]')
    expect(warning.attributes('role')).toBe('status')
    expect(warning.classes()).toContain('bg-amber-50')
    expect(warning.get('[data-testid="traffic-ingestion-pending-short"]').text()).toBe('admin.ops.traffic.ingestionPendingShort:{"count":2}')
    expect(bodyTestId('traffic-ingestion-pending').textContent).toBe('admin.ops.traffic.ingestionPending:{"count":2}')
    expect(document.body.querySelector('[data-testid="traffic-ingestion-failed"]')).toBeNull()
    await wrapper.setProps({ traffic: traffic({ ingestion: { scope: 'instance_lifetime', pending: 0, failed: 0, flushed: 102 } }) })
    expect(wrapper.find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
  })

  it('keeps the failure warning after pending records flush, including for an empty selected range', async () => {
    const wrapper = mountCards(traffic({
      request_count: 0, upstream: unknownLink(), downstream: unknownLink(),
      ingestion: { scope: 'instance_lifetime', pending: 2, failed: 3, flushed: 100 }
    }))
    expect(wrapper.get('[data-testid="traffic-ingestion-warning"]').attributes('role')).toBe('alert')
    expect(bodyTestId('traffic-ingestion-failed').textContent).toContain('"count":3')
    expect(bodyTestId('traffic-ingestion-pending').textContent).toContain('"count":2')
    await wrapper.setProps({ traffic: traffic({
      request_count: 0, upstream: unknownLink(), downstream: unknownLink(),
      ingestion: { scope: 'instance_lifetime', pending: 0, failed: 3, flushed: 102 }
    }) })
    expect(wrapper.get('[data-testid="traffic-ingestion-warning"]').attributes('role')).toBe('alert')
    expect(document.body.querySelector('[data-testid="traffic-ingestion-pending"]')).toBeNull()
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('—')
  })

  it('hides stale ingestion status while loading and uses the new overview health afterwards', async () => {
    const wrapper = mountCards(traffic({
      ingestion: { scope: 'instance_lifetime', pending: 2, failed: 3, flushed: 100 }
    }), true)
    expect(wrapper.find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
    await wrapper.setProps({ loading: false })
    expect(wrapper.get('[data-testid="traffic-ingestion-warning"]').attributes('role')).toBe('alert')
    await wrapper.setProps({ traffic: traffic({ ingestion: null }) })
    expect(wrapper.find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
  })
})
