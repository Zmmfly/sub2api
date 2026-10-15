import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { nextTick } from 'vue'
import OpsTrafficCards from '../OpsTrafficCards.vue'
import type { OpsTrafficDirection, OpsTrafficLink, OpsTrafficSummary } from '@/api/admin/ops'

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

function direction(overrides: Partial<OpsTrafficDirection> = {}): OpsTrafficDirection {
  return {
    original_bytes: 1024,
    actual_bytes: 512,
    original_sample_count: 1,
    actual_sample_count: 1,
    ...overrides
  }
}

function link(overrides: Partial<OpsTrafficLink> = {}): OpsTrafficLink {
  return {
    original_bytes: 4096, actual_bytes: 2048,
    original_sample_count: 4, actual_sample_count: 4, paired_sample_count: 4,
    compression_percent: 50,
    request: direction({ original_sample_count: 2, actual_sample_count: 2 }),
    response: direction({ original_bytes: 3072, actual_bytes: 1536, original_sample_count: 2, actual_sample_count: 2 }),
    ...overrides
  }
}
function traffic(overrides: Partial<OpsTrafficSummary> = {}): OpsTrafficSummary {
  return { source: 'http_requests', request_count: 2, upstream: link(), downstream: link(), ...overrides }
}

const unknownLink = (): OpsTrafficLink => link({
  original_bytes: null, actual_bytes: null,
  original_sample_count: 0, actual_sample_count: 0, paired_sample_count: 0,
  compression_percent: null,
  request: null,
  response: null
})

function mountCards(value?: OpsTrafficSummary | null, loading = false) {
  return mount(OpsTrafficCards, { props: { traffic: value, loading } })
}

describe('OpsTrafficCards', () => {
  it('shows two overview-sized cards and keeps each compression rate beside its actual total', () => {
    const wrapper = mountCards(traffic({
      downstream: link({ original_bytes: 2048, actual_bytes: 2560, compression_percent: -25 })
    }))
    const articles = wrapper.findAll('article')
    expect(articles).toHaveLength(2)
    for (const article of articles) {
      expect(article.classes()).toEqual(expect.arrayContaining(['min-w-0', 'rounded-2xl', 'bg-gray-50', 'p-4', 'dark:bg-dark-900']))
    }
    expect(wrapper.get('[data-testid="traffic-upstream-card"]').attributes('style')).toContain('order: 8')
    expect(wrapper.get('[data-testid="traffic-downstream-card"]').attributes('style')).toContain('order: 9')
    expect(wrapper.find('[data-testid="traffic-composition-card"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('4 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('2.5 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-actual-row"]').get('[data-testid="traffic-upstream-compression"]').text()).toBe('50.0%')
    expect(wrapper.get('[data-testid="traffic-downstream-actual-row"]').get('[data-testid="traffic-downstream-compression"]').text()).toBe('-25.0%')
    expect(wrapper.get('[data-testid="traffic-upstream-original-row"]').find('[data-testid="traffic-upstream-compression"]').exists()).toBe(false)
    expect(wrapper.findAll('[data-testid$="-breakdown-tooltip"]')).toHaveLength(4)
    for (const testId of ['traffic-upstream-original', 'traffic-upstream-actual', 'traffic-downstream-original', 'traffic-downstream-actual']) {
      const target = wrapper.get(`[data-testid="${testId}"]`)
      expect(target.find('svg').exists()).toBe(false)
      expect(target.classes()).toContain('cursor-help')
    }
    expect(wrapper.get('[data-testid="traffic-upstream-coverage-tooltip"] svg').exists()).toBe(true)
    expect(bodyTestId('traffic-upstream-scope').textContent).toBe('admin.ops.traffic.scope')
    expect(bodyTestId('traffic-upstream-compression-hint').textContent).toBe('admin.ops.traffic.compressionHint')
  })

  it('keeps long scope, formula and coverage details hidden until their help icons are hovered', async () => {
    const wrapper = mountCards(traffic())
    const upstream = wrapper.get('[data-testid="traffic-upstream-card"]')
    expect(upstream.text()).not.toContain('admin.ops.traffic.scope')
    expect(upstream.text()).not.toContain('admin.ops.traffic.compressionHint')
    expect(upstream.text()).not.toContain('admin.ops.traffic.pairedCoverage')

    const detailsTooltip = tooltipFor('traffic-upstream-scope')
    expect(detailsTooltip.style.display).toBe('none')
    const detailsTrigger = wrapper.get('[data-testid="traffic-upstream-coverage-tooltip"]')
    await detailsTrigger.trigger('mouseenter')
    await nextTick()
    expect(detailsTooltip.style.display).not.toBe('none')
    expect(detailsTooltip.textContent).toContain('admin.ops.traffic.scope')
    expect(detailsTooltip.textContent).toContain('admin.ops.traffic.compressionHint')
    expect(detailsTooltip.textContent).toContain('admin.ops.traffic.pairedCoverage')
    await detailsTrigger.trigger('mouseleave')
    await nextTick()
    expect(detailsTooltip.style.display).toBe('none')
  })

  it('shows direction-specific original and actual bytes with request-count coverage in separate tooltips', async () => {
    const wrapper = mountCards(traffic({
      downstream: link({
        original_sample_count: 2,
        actual_sample_count: 2,
        paired_sample_count: 2,
        request: direction({ original_bytes: 100, actual_bytes: 50, original_sample_count: 1, actual_sample_count: 1 }),
        response: direction({ original_bytes: 200, actual_bytes: 150, original_sample_count: 1, actual_sample_count: 1 })
      })
    }))

    const originalTooltip = tooltipFor('traffic-upstream-request-original')
    expect(originalTooltip.style.display).toBe('none')
    const originalTrigger = wrapper.get('[data-testid="traffic-upstream-original"]')
    originalTrigger.element.dispatchEvent(new MouseEvent('mouseenter', { bubbles: true }))
    await nextTick()
    expect(originalTooltip.style.display).not.toBe('none')
    expect(bodyTestId('traffic-upstream-request-original').textContent).toBe('1 KiB')
    expect(bodyTestId('traffic-upstream-response-original').textContent).toBe('3 KiB')
    expect(bodyTestId('traffic-upstream-request-original-coverage').textContent).toContain('admin.ops.traffic.directionCoverage:{"count":2,"total":2}')
    expect(bodyTestId('traffic-upstream-response-original-coverage').textContent).toContain('admin.ops.traffic.directionCoverage:{"count":2,"total":2}')
    originalTrigger.element.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }))
    await nextTick()
    expect(originalTooltip.style.display).toBe('none')

    const actualTrigger = wrapper.get('[data-testid="traffic-upstream-actual"]')
    actualTrigger.element.dispatchEvent(new MouseEvent('mouseenter', { bubbles: true }))
    await nextTick()
    const actualTooltip = tooltipFor('traffic-upstream-request-actual')
    expect(actualTooltip.style.display).not.toBe('none')
    expect(bodyTestId('traffic-upstream-request-actual').textContent).toBe('512 B')
    expect(bodyTestId('traffic-upstream-response-actual').textContent).toBe('1.5 KiB')
    expect(bodyTestId('traffic-upstream-request-actual-coverage').textContent).toContain('admin.ops.traffic.directionCoverage:{"count":2,"total":2}')
    expect(bodyTestId('traffic-upstream-response-actual-coverage').textContent).toContain('admin.ops.traffic.directionCoverage:{"count":2,"total":2}')

    actualTrigger.element.dispatchEvent(new MouseEvent('mouseleave', { bubbles: true }))
    await nextTick()
    expect(actualTooltip.style.display).toBe('none')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('50.0%')

    expect(bodyTestId('traffic-downstream-request-original').textContent).toBe('100 B')
    expect(bodyTestId('traffic-downstream-response-original').textContent).toBe('200 B')
    expect(bodyTestId('traffic-downstream-request-actual').textContent).toBe('50 B')
    expect(bodyTestId('traffic-downstream-response-actual').textContent).toBe('150 B')
  })

  it.each([undefined, null, traffic({ upstream: unknownLink(), downstream: unknownLink() })])(
    'shows missing/legacy measurements as unknown, never zero: %s', (value) => {
      const wrapper = mountCards(value)
      for (const side of ['upstream', 'downstream']) {
        for (const metric of ['original', 'actual', 'compression']) {
          expect(wrapper.get(`[data-testid="traffic-${side}-${metric}"]`).text()).toBe('—')
        }
        expect(wrapper.get(`[data-testid="traffic-${side}-coverage-state"]`).text()).toBe('admin.ops.traffic.unavailable')
        for (const direction of ['request', 'response']) {
          expect(bodyTestId(`traffic-${side}-${direction}-original`).textContent).toBe('—')
          expect(bodyTestId(`traffic-${side}-${direction}-actual`).textContent).toBe('—')
          expect(bodyTestId(`traffic-${side}-${direction}-original-coverage`).textContent).toContain('admin.ops.traffic.coverageUnknown')
        }
      }
      expect(wrapper.text()).not.toContain('0 B')
    }
  )

  it('distinguishes measured empty bodies from no measurements and keeps zero-original compression unknown', () => {
    const wrapper = mountCards(traffic({
      upstream: link({
        original_bytes: 0,
        actual_bytes: 0,
        compression_percent: null,
        request: direction({ original_bytes: 0, actual_bytes: 0, original_sample_count: 2, actual_sample_count: 2 }),
        response: direction({ original_bytes: 0, actual_bytes: 0, original_sample_count: 2, actual_sample_count: 2 })
      }),
      downstream: link({ original_bytes: null, actual_bytes: 0, original_sample_count: 0, paired_sample_count: 0, compression_percent: null })
    }))
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('—')
    expect(bodyTestId('traffic-upstream-request-original').textContent).toBe('0 B')
    expect(bodyTestId('traffic-upstream-response-actual').textContent).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('0 B')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('—')
  })

  it('shows per-metric and paired coverage and uses the server paired ratio rather than independently covered totals', () => {
    const wrapper = mountCards(traffic({
      request_count: 5,
      upstream: link({ original_bytes: 100, actual_bytes: 500, original_sample_count: 4, actual_sample_count: 7, paired_sample_count: 2, compression_percent: 25, request: null, response: null })
    }))
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.original')
    expect(bodyTestId('traffic-upstream-original-coverage').textContent).toContain('admin.ops.traffic.coverage:{"count":4,"total":10}')
    expect(bodyTestId('traffic-upstream-actual-coverage').textContent).toContain('admin.ops.traffic.actual')
    expect(bodyTestId('traffic-upstream-actual-coverage').textContent).toContain('admin.ops.traffic.coverage:{"count":7,"total":10}')
    expect(bodyTestId('traffic-upstream-paired-coverage').textContent).toContain('admin.ops.traffic.compression')
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
    expect(wrapper.get('[data-testid="traffic-downstream-card"]').attributes('aria-busy')).toBe('true')
    expect(wrapper.get('[role="status"]').text()).toBe('admin.ops.loadingText')
    expect(wrapper.find('[data-testid="traffic-upstream-original"]').exists()).toBe(false)
    expect(document.body.querySelector('[data-testid="traffic-upstream-original-coverage"]')).toBeNull()
    expect(document.body.querySelector('[data-testid="traffic-upstream-request-original"]')).toBeNull()
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
    expect(warning.get('[data-testid="traffic-ingestion-instance"]').text()).toBe('admin.ops.traffic.instance')
    expect(warning.find('svg').exists()).toBe(true)
    expect(warning.get('[data-testid="traffic-ingestion-failed-short"]').text()).toBe('admin.ops.traffic.ingestionFailedShort:{"count":3}')
    expect(wrapper.get('[data-testid="traffic-upstream-card"]').find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-downstream-card"]').find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(true)
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
