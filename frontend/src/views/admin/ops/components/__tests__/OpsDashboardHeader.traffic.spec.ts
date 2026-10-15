import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import OpsDashboardHeader from '../OpsDashboardHeader.vue'
import OpsTrafficCards from '../OpsTrafficCards.vue'
import { opsAPI, type OpsDashboardOverview, type OpsTrafficSummary } from '@/api/admin/ops'

vi.mock('@/api', () => ({ adminAPI: { groups: { getAll: vi.fn().mockResolvedValue([]) } } }))
vi.mock('@/api/admin/ops', () => ({ opsAPI: { getRealtimeTrafficSummary: vi.fn().mockResolvedValue({ enabled: true, summary: null }) } }))
vi.mock('@/stores', () => ({ useAdminSettingsStore: () => ({ opsRealtimeMonitoringEnabled: true, setOpsRealtimeMonitoringEnabledLocal: vi.fn() }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => params ? `${key}:${JSON.stringify(params)}` : key })
}))
enableAutoUnmount(afterEach)

const traffic: OpsTrafficSummary = {
  source: 'http_requests',
  request_count: 2,
  upstream: { original_bytes: 2048, actual_bytes: 1024, original_sample_count: 4, actual_sample_count: 4, paired_sample_count: 4, compression_percent: 50 },
  downstream: { original_bytes: null, actual_bytes: null, original_sample_count: 0, actual_sample_count: 0, paired_sample_count: 0, compression_percent: null }
}
const overview = { tps: { avg: 242.7 }, request_count_total: 99, traffic } as OpsDashboardOverview

function mountHeader() {
  return mount(OpsDashboardHeader, {
    props: { overview, platform: 'openai', groupId: 7, timeRange: '6h', queryMode: 'preagg', loading: false, lastUpdated: null },
    global: { stubs: { Select: true, HelpTooltip: true, BaseDialog: true, Icon: true } }
  })
}

describe('OpsDashboardHeader traffic integration', () => {
  it('places three overview traffic cards after output TPS and uses traffic ledger counts, not usage/error counts', async () => {
    const wrapper = mountHeader()
    await flushPromises()
    const cards = wrapper.getComponent(OpsTrafficCards)
    expect(cards.props('traffic')).toEqual(traffic)
    expect(cards.findAll('article')).toHaveLength(3)
    expect(wrapper.html().indexOf('data-testid="traffic-cards"')).toBeGreaterThan(wrapper.html().indexOf('data-testid="output-tps-card"'))
    expect(wrapper.html().indexOf('data-testid="traffic-cards"')).toBeLessThan(wrapper.html().indexOf('admin.ops.memory'))
    expect(wrapper.get('[data-testid="traffic-upstream-original"]').text()).toBe('2 KiB')
    // The overview usage/error population can differ from traffic's HTTP ledger.
    expect(cards.props('traffic')?.request_count).toBe(2)
    expect(cards.props('traffic')?.request_count).not.toBe(overview.request_count_total)
    expect(wrapper.get('[data-testid="traffic-upstream-original-coverage"]').text()).toBe('admin.ops.traffic.coverage:{"count":4,"total":4}')
  })

  it('reacts to the existing overview/loading props without an independent fetch, including fullscreen', async () => {
    const wrapper = mountHeader()
    await flushPromises()
    vi.mocked(opsAPI.getRealtimeTrafficSummary).mockClear()
    await wrapper.setProps({ loading: true, fullscreen: true })
    expect(wrapper.getComponent(OpsTrafficCards).props('loading')).toBe(true)
    expect(wrapper.find('[data-testid="traffic-upstream-original"]').exists()).toBe(false)

    const nextTraffic = { ...traffic, upstream: { ...traffic.upstream, actual_bytes: 512, compression_percent: 75 } }
    await wrapper.setProps({ loading: false, overview: { ...overview, traffic: nextTraffic } })
    await flushPromises()
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('512 B')
    expect(wrapper.get('[data-testid="traffic-upstream-compression"]').text()).toBe('75.0%')
    expect(opsAPI.getRealtimeTrafficSummary).not.toHaveBeenCalled()

    await wrapper.setProps({ overview: { ...overview, traffic: undefined } })
    expect(wrapper.get('[data-testid="traffic-upstream-actual"]').text()).toBe('—')
    expect(wrapper.get('[data-testid="traffic-downstream-compression"]').text()).toBe('—')
  })
})
