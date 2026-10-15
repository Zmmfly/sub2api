import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { nextTick } from 'vue'
import type { TrafficMeasurement, UsageTraffic } from '@/types'
import Icon from '@/components/icons/Icon.vue'
import TrafficCell from '../TrafficCell.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => ({
      'usage.traffic.upstream': 'Upstream',
      'usage.traffic.downstream': 'Downstream',
      'usage.traffic.request': 'Request',
      'usage.traffic.response': 'Response',
      'usage.traffic.clientUpload': 'Client upload',
      'usage.traffic.clientDownload': 'Client download',
      'usage.traffic.original': 'Original',
      'usage.traffic.compressionRate': 'Compression rate',
      'usage.traffic.bodyOnlyHint': 'Actual HTTP body bytes, excluding HTTP headers and TCP/TLS overhead.',
    }[key] ?? key),
  }),
}))

enableAutoUnmount(afterEach)
afterEach(() => {
  document.body.innerHTML = ''
})

const measured = (original: number | null, actual: number | null, compressed = false): TrafficMeasurement => ({
  original_bytes: original,
  actual_bytes: actual,
  compressed,
})

const traffic: UsageTraffic = {
  upstream: {
    request: measured(4096, 2048, true),
    response: measured(1024, 1536, true),
  },
  downstream: {
    request: measured(123, 123),
    response: measured(8192, 4096, true),
  },
}

describe('TrafficCell', () => {
  it('renders compact admin measurements with labeled direction icons and original bytes in hover tooltips', async () => {
    const wrapper = mount(TrafficCell, { props: { traffic, showUpstream: true } })
    const rows = wrapper.get('[data-testid="traffic-cell"]').element.children
    expect(rows).toHaveLength(2)
    expect(rows[0].getAttribute('data-testid')).toBe('traffic-upstream')
    expect(rows[1].getAttribute('data-testid')).toBe('traffic-downstream')
    const upstream = wrapper.get('[data-testid="traffic-upstream"]')
    expect(upstream.findAll('[data-testid="traffic-actual"]').map(cell => cell.text())).toEqual(['2 KiB', '1.5 KiB'])
    expect(upstream.text()).toContain('Upstream')
    expect(upstream.text()).not.toContain('Request')
    expect(upstream.text()).not.toContain('Response')

    const request = wrapper.get('[data-testid="traffic-upstream-request"]')
    const requestDirection = request.get('[data-testid="traffic-upstream-request-direction"]')
    expect(requestDirection.attributes('aria-label')).toBe('Request')
    expect(requestDirection.attributes('title')).toBe('Request')
    expect(requestDirection.getComponent(Icon).props('name')).toBe('arrowUp')
    expect(request.get('[data-testid="traffic-compression"]').text()).toBe('50.0%')
    expect(request.text()).not.toContain('Original 4 KiB')
    expect(request.text()).not.toContain('Compression rate')

    const response = wrapper.get('[data-testid="traffic-upstream-response"]')
    expect(response.get('[data-testid="traffic-upstream-response-direction"]').attributes('aria-label')).toBe('Response')
    expect(response.get('[data-testid="traffic-upstream-response-direction"]').getComponent(Icon).props('name')).toBe('arrowDown')
    expect(response.get('[data-testid="traffic-compression"]').text()).toBe('-50.0%')
    expect(response.text()).not.toContain('Original 1 KiB')

    const originalTrigger = request.get('[data-testid="traffic-original-tooltip"]')
    expect(request.get('[data-testid="traffic-original-icon"]').attributes('aria-label')).toBe('Original')
    await originalTrigger.trigger('mouseenter')
    await nextTick()
    const visibleTooltip = Array.from(document.body.querySelectorAll<HTMLElement>('[role="tooltip"]')).find(tooltip => tooltip.style.display !== 'none')
    expect(visibleTooltip?.textContent).toBe('Original 4 KiB')
    await originalTrigger.trigger('mouseleave')
    await nextTick()

    expect(wrapper.get('[data-testid="traffic-downstream"]').findAll('[data-testid="traffic-actual"]').map(cell => cell.text())).toEqual(['123 B', '4 KiB'])
    expect(wrapper.get('[data-testid="traffic-downstream-request"]').find('[data-testid="traffic-compression"]').exists()).toBe(false)
    expect(wrapper.get('[data-testid="traffic-downstream-response"]').get('[data-testid="traffic-compression"]').text()).toBe('50.0%')
    expect(wrapper.get('[data-testid="traffic-cell"]').attributes('title')).toContain('excluding HTTP headers and TCP/TLS overhead')
  })

  it('defaults to two client rows and never renders unexpected upstream data, labels, or attributes', () => {
    const wrapper = mount(TrafficCell, { props: { traffic } })
    const rows = wrapper.get('[data-testid="traffic-cell"]').element.children
    expect(rows).toHaveLength(2)
    expect(rows[0].getAttribute('data-testid')).toBe('traffic-client-upload')
    expect(rows[1].getAttribute('data-testid')).toBe('traffic-client-download')
    expect(wrapper.get('[data-testid="traffic-client-upload"] > span').text()).toBe('Client upload')
    expect(wrapper.get('[data-testid="traffic-client-upload"] [data-testid="traffic-actual"]').text()).toBe('123 B')
    expect(wrapper.get('[data-testid="traffic-client-upload-request-direction"]').getComponent(Icon).props('name')).toBe('arrowUp')
    expect(wrapper.get('[data-testid="traffic-client-download"] > span').text()).toBe('Client download')
    expect(wrapper.get('[data-testid="traffic-client-download"] [data-testid="traffic-actual"]').text()).toBe('4 KiB')
    expect(wrapper.get('[data-testid="traffic-client-download-response-direction"]').getComponent(Icon).props('name')).toBe('arrowDown')
    expect(wrapper.get('[data-testid="traffic-client-download"]').get('[data-testid="traffic-compression"]').text()).toBe('50.0%')
    expect(wrapper.get('[data-testid="traffic-client-download"]').text()).not.toContain('Original 8 KiB')
    expect(wrapper.html()).not.toMatch(/upstream|Upstream|2 KiB|1\.5 KiB|-50\.0%/)
    expect(wrapper.findAll('[data-testid="traffic-actual"]')).toHaveLength(2)
  })

  it('does not even read the upstream property in user mode', () => {
    const upstream = vi.fn(() => { throw new Error('upstream is not user data') })
    const payload = Object.defineProperty({ downstream: traffic.downstream }, 'upstream', { get: upstream })
    const wrapper = mount(TrafficCell, { props: { traffic: payload, showUpstream: false } })
    expect(upstream).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="traffic-client-upload"] [data-testid="traffic-actual"]').text()).toBe('123 B')
  })

  it('reactively removes upstream data when changing to user mode', async () => {
    const wrapper = mount(TrafficCell, { props: { traffic, showUpstream: true } })
    expect(wrapper.find('[data-testid="traffic-upstream"]').exists()).toBe(true)
    await wrapper.setProps({ showUpstream: false })
    expect(wrapper.find('[data-testid="traffic-upstream"]').exists()).toBe(false)
    expect(wrapper.html()).not.toContain('1.5 KiB')
    expect(wrapper.find('[data-testid="traffic-client-upload"]').exists()).toBe(true)
  })

  it.each([true, false])('uses placeholders for a missing legacy traffic value in showUpstream=%s mode', (showUpstream) => {
    const wrapper = mount(TrafficCell, { props: { showUpstream } })
    expect(wrapper.findAll('[data-testid="traffic-actual"]').map(cell => cell.text())).toEqual(Array(showUpstream ? 4 : 2).fill('—'))
    expect(wrapper.find('[data-testid="traffic-compression"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('0 B')
  })

  it.each([null, {}, { downstream: null }, { downstream: {} }, { downstream: { request: null, response: null } }])('keeps absent links and measurements unknown: %j', (payload) => {
    const wrapper = mount(TrafficCell, { props: { traffic: payload } })
    expect(wrapper.findAll('[data-testid="traffic-actual"]').map(cell => cell.text())).toEqual(['—', '—'])
    expect(wrapper.find('[data-testid="traffic-compression"]').exists()).toBe(false)
  })

  it.each([
    [measured(100, 50, false), '50 B', null],
    [measured(100, 100, false), '100 B', null],
    [measured(100, 150, false), '150 B', null],
    [measured(100, 50, true), '50 B', '50.0%'],
    [measured(100, 100, true), '100 B', '0.0%'],
    [measured(100, 150, true), '150 B', '-50.0%'],
    [measured(100, 0, true), '0 B', '100.0%'],
    [measured(0, 0, true), '0 B', null],
    [measured(0, 20, true), '20 B', null],
    [measured(null, 20, true), '20 B', null],
    [measured(100, null, true), '—', null],
    [measured(null, null, true), '—', null],
    [measured(null, 0, false), '0 B', null],
  ] as const)('uses compressed flag, not size comparison, for %j', (measurement, actual, compression) => {
    const wrapper = mount(TrafficCell, { props: { traffic: { downstream: { request: measurement } } } })
    const cell = wrapper.get('[data-testid="traffic-client-upload-request"]')
    expect(cell.get('[data-testid="traffic-actual"]').text()).toBe(actual)
    const details = cell.find('[data-testid="traffic-compression"]')
    expect(details.exists()).toBe(compression !== null)
    if (compression !== null) expect(details.text()).toBe(compression)
  })

  it('shows actual bytes without inventing a rate or original value when a compressed original is unavailable', () => {
    const wrapper = mount(TrafficCell, {
      props: { traffic: { downstream: { response: measured(null, 2048, true) } } },
    })
    const measurement = wrapper.get('[data-testid="traffic-client-download-response"]')
    expect(measurement.get('[data-testid="traffic-actual"]').text()).toBe('2 KiB')
    expect(measurement.find('[data-testid="traffic-compression"]').exists()).toBe(false)
    expect(measurement.find('[data-testid="traffic-original-tooltip"]').exists()).toBe(false)
    expect(measurement.text()).not.toContain('0.0%')
    expect(measurement.text()).not.toContain('Original')
  })
})
