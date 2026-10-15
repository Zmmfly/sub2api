import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import OpsTrafficCards from '../OpsTrafficCards.vue'
import type { OpsTrafficLink, OpsTrafficSummary } from '@/api/admin/ops'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params ? `${key}:${JSON.stringify(params)}` : key })
}))
enableAutoUnmount(afterEach)

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
  it('shows three responsive cards with original/actual totals and both compression rates', () => {
    const wrapper = mountCards(traffic({
      downstream: link({ original_bytes: 2048, actual_bytes: 2560, compression_percent: -25 })
    }))
    expect(wrapper.findAll('article')).toHaveLength(3)
    expect(wrapper.classes()).toContain('grid-cols-1')
    expect(wrapper.classes()).toContain('md:grid-cols-3')
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('4 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-original"]').text()).toBe('2 KiB')
    expect(wrapper.get('[data-testid="traffic-downstream-actual"]').text()).toBe('2.5 KiB')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('50.0%')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('-25.0%')
    expect(wrapper.get('[data-testid="traffic-scope"]').text()).toBe('admin.ops.traffic.scope')
    expect(wrapper.get('[data-testid="traffic-compression-hint"]').text()).toBe('admin.ops.traffic.compressionHint')
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
    expect(wrapper.get('[data-testid="traffic-upstream-original-coverage"]').text()).toBe('admin.ops.traffic.coverage:{"count":4,"total":10}')
    expect(wrapper.get('[data-testid="traffic-upstream-actual-coverage"]').text()).toBe('admin.ops.traffic.coverage:{"count":7,"total":10}')
    expect(wrapper.get('[data-testid="traffic-upstream-paired-coverage"]').text()).toBe('admin.ops.traffic.pairedCoverage:{"count":2,"total":10}')
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
    expect(wrapper.attributes('aria-busy')).toBe('true')
    expect(wrapper.get('[role="status"]').text()).toBe('admin.ops.loadingText')
    expect(wrapper.find('[data-testid="traffic-upstream-original"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="traffic-upstream-original-coverage"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="traffic-upstream-compression"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="traffic-upstream-coverage-state"]').exists()).toBe(false)
    await wrapper.setProps({ loading: false, traffic: traffic({ upstream: link({ original_bytes: 1024 ** 2 }) }) })
    expect(wrapper.attributes('aria-busy')).toBe('false')
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
    expect(wrapper.get('[data-testid="traffic-upstream-original-coverage"]').text()).toBe('admin.ops.traffic.coverageUnknown')
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

  it('prominently warns about sticky record loss despite full persisted measurement coverage', () => {
    const wrapper = mountCards(traffic({
      ingestion: { scope: 'instance_lifetime', pending: 0, failed: 3, flushed: 100 }
    }))
    const warning = wrapper.get('[data-testid="traffic-ingestion-warning"]')
    expect(warning.attributes('role')).toBe('alert')
    expect(warning.classes()).toContain('bg-red-50')
    expect(warning.get('[data-testid="traffic-ingestion-failed"]').text()).toBe('admin.ops.traffic.ingestionFailed:{"count":3}')
    expect(warning.get('[data-testid="traffic-ingestion-scope"]').text()).toBe('admin.ops.traffic.ingestionScope')
    expect(warning.get('[data-testid="traffic-ingestion-coverage"]').text()).toBe('admin.ops.traffic.ingestionCoverage')
    expect(warning.find('[data-testid="traffic-ingestion-pending"]').exists()).toBe(false)
    // Ingestion lifetime counts must not be added to the filtered denominator.
    expect(wrapper.get('[data-testid="traffic-upstream-original-coverage"]').text()).toBe('admin.ops.traffic.coverage:{"count":4,"total":4}')
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('4 KiB')
  })

  it('shows pending persistence without claiming record loss and clears it on the next overview', async () => {
    const wrapper = mountCards(traffic({
      ingestion: { scope: 'instance_lifetime', pending: 2, failed: 0, flushed: 100 }
    }))
    const warning = wrapper.get('[data-testid="traffic-ingestion-warning"]')
    expect(warning.attributes('role')).toBe('status')
    expect(warning.classes()).toContain('bg-amber-50')
    expect(warning.get('[data-testid="traffic-ingestion-pending"]').text()).toBe('admin.ops.traffic.ingestionPending:{"count":2}')
    expect(warning.find('[data-testid="traffic-ingestion-failed"]').exists()).toBe(false)
    await wrapper.setProps({ traffic: traffic({ ingestion: { scope: 'instance_lifetime', pending: 0, failed: 0, flushed: 102 } }) })
    expect(wrapper.find('[data-testid="traffic-ingestion-warning"]').exists()).toBe(false)
  })

  it('keeps the failure warning after pending records flush, including for an empty selected range', async () => {
    const wrapper = mountCards(traffic({
      request_count: 0, upstream: unknownLink(), downstream: unknownLink(),
      ingestion: { scope: 'instance_lifetime', pending: 2, failed: 3, flushed: 100 }
    }))
    expect(wrapper.get('[data-testid="traffic-ingestion-warning"]').attributes('role')).toBe('alert')
    expect(wrapper.get('[data-testid="traffic-ingestion-failed"]').text()).toContain('"count":3')
    expect(wrapper.get('[data-testid="traffic-ingestion-pending"]').text()).toContain('"count":2')
    await wrapper.setProps({ traffic: traffic({
      request_count: 0, upstream: unknownLink(), downstream: unknownLink(),
      ingestion: { scope: 'instance_lifetime', pending: 0, failed: 3, flushed: 102 }
    }) })
    expect(wrapper.get('[data-testid="traffic-ingestion-warning"]').attributes('role')).toBe('alert')
    expect(wrapper.find('[data-testid="traffic-ingestion-pending"]').exists()).toBe(false)
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
