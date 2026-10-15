import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import type { AdminUsageLog } from '@/types'
import UsageTable from '../UsageTable.vue'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('@/utils/ipGeoLookup', () => ({ getEntry: vi.fn(), fetchBatch: vi.fn() }))

const row = {
  id: 1,
  traffic: {
    upstream: {
      request: { original_bytes: 777, actual_bytes: 555, compressed: true },
      response: { original_bytes: 888, actual_bytes: 666, compressed: true },
    },
    downstream: {
      request: { original_bytes: 10, actual_bytes: 10, compressed: false },
      response: { original_bytes: 20, actual_bytes: 20, compressed: false },
    },
  },
} as AdminUsageLog

function mountTable(showUpstreamEndpoint?: boolean) {
  return mount(UsageTable, {
    props: {
      data: [row],
      columns: [{ key: 'traffic', label: 'Traffic', sortable: false }],
      ...(showUpstreamEndpoint === undefined ? {} : { showUpstreamEndpoint }),
    },
    global: { stubs: { Teleport: true } },
  })
}

describe('UsageTable traffic column', () => {
  it('renders the traffic slot in the actual DataTable with admin upstream and downstream rows', () => {
    const wrapper = mountTable()
    expect(wrapper.get('th').text()).toBe('Traffic')
    expect(wrapper.get('td [data-testid="traffic-upstream"]').text()).toContain('555 B')
    expect(wrapper.get('td [data-testid="traffic-downstream"]').text()).toContain('10 B')
    expect(wrapper.findAll('[data-testid="traffic-actual"]')).toHaveLength(4)
    wrapper.unmount()
  })

  it('honors the user view upstream boundary even with a fully populated admin payload', () => {
    const wrapper = mountTable(false)
    expect(wrapper.findAll('[data-testid="traffic-actual"]').map(cell => cell.text())).toEqual(['10 B', '20 B'])
    expect(wrapper.find('[data-testid="traffic-upstream"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-client-upload"]').text()).toContain('10 B')
    expect(wrapper.get('[data-testid="traffic-client-download"]').text()).toContain('20 B')
    expect(wrapper.html()).not.toMatch(/555 B|666 B|777 B|888 B|usage\.traffic\.upstream/)
    wrapper.unmount()
  })

  it('updates the traffic visibility boundary reactively', async () => {
    const wrapper = mountTable()
    await wrapper.setProps({ showUpstreamEndpoint: false })
    expect(wrapper.find('[data-testid="traffic-upstream"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-client-upload"]').text()).toContain('10 B')
    wrapper.unmount()
  })
})
