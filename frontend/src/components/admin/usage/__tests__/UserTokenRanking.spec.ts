import { describe, expect, it, vi, beforeEach } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'

import UserTokenRanking from '../UserTokenRanking.vue'

const getUserBreakdown = vi.fn()

vi.mock('@/api/admin/dashboard', () => ({
  getUserBreakdown: (...args: unknown[]) => getUserBreakdown(...args),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

const item = (id: number, tokens: number, trafficBytes?: number) => ({
  user_id: id,
  email: `u${id}@test.com`,
  requests: 1,
  input_tokens: tokens,
  output_tokens: 0,
  cache_tokens: 0,
  total_tokens: tokens,
  actual_cost: 0.5,
  total_traffic_bytes: trafficBytes,
})

const mountRanking = (props: Record<string, unknown> = {}) =>
  mount(UserTokenRanking, {
    props: {
      startDate: '2026-07-01',
      endDate: '2026-07-08',
      filters: {},
      ...props,
    },
    global: { stubs: { Select: true, LoadingSpinner: true } },
  })

describe('UserTokenRanking', () => {
  beforeEach(() => {
    getUserBreakdown.mockReset()
    getUserBreakdown.mockResolvedValue({ users: [item(1, 100), item(2, 50)] })
  })

  it('loads on mount with shared filters and emits select-user with id + email on row click', async () => {
    const wrapper = mountRanking({ filters: { group_id: 3 }, model: 'claude-fable-5' })
    await flushPromises()

    expect(getUserBreakdown).toHaveBeenCalledWith(expect.objectContaining({
      group_id: 3,
      model: 'claude-fable-5',
      start_date: '2026-07-01',
      end_date: '2026-07-08',
      sort_by: 'total_tokens',
      include_traffic: true,
      limit: 50,
    }), { signal: expect.any(AbortSignal) })

    const rows = wrapper.findAll('tbody tr')
    expect(rows).toHaveLength(2)

    await rows[0].trigger('click')
    expect(wrapper.emitted('select-user')![0]).toEqual([1, 'u1@test.com'])
  })

  it('reloads traffic totals when the time window or shared filters change', async () => {
    const wrapper = mountRanking()
    await flushPromises()
    expect(getUserBreakdown).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ startDate: '2026-07-03', endDate: '2026-07-05', filters: { user_id: 9 } })
    await flushPromises()

    expect(getUserBreakdown).toHaveBeenCalledTimes(2)
    expect(getUserBreakdown).toHaveBeenLastCalledWith(expect.objectContaining({
      user_id: 9,
      start_date: '2026-07-03',
      end_date: '2026-07-05',
      include_traffic: true,
    }), { signal: expect.any(AbortSignal) })
  })

  it('formats traffic in the appropriate binary unit and can rank by it', async () => {
    getUserBreakdown.mockResolvedValue({
      users: [item(1, 100, 1536), item(2, 50, 3 * 1024 ** 3), item(3, 20, 0), item(4, 10)],
    })
    const wrapper = mountRanking()
    await flushPromises()

    expect(wrapper.findAll('tbody tr').map(row => row.get('td:last-child').text()))
      .toEqual(['1.5 KiB', '3 GiB', '0 B', '—'])

    const header = wrapper.findAll('thead th').find(cell => cell.text() === 'usage.traffic.title')
    expect(header).toBeDefined()
    await header!.trigger('click')
    await flushPromises()
    expect(getUserBreakdown).toHaveBeenLastCalledWith(expect.objectContaining({
      sort_by: 'total_traffic_bytes',
      include_traffic: true,
    }), { signal: expect.any(AbortSignal) })
  })

  it('cancels ranking requests when hidden and defers filter changes until visible', async () => {
    getUserBreakdown.mockImplementationOnce(() => new Promise(() => {}))
    const wrapper = mountRanking()
    const { signal } = getUserBreakdown.mock.calls[0][1]

    await wrapper.setProps({ active: false })
    expect(signal.aborted).toBe(true)
    await wrapper.setProps({ filters: { group_id: 4 } })
    expect(getUserBreakdown).toHaveBeenCalledTimes(1)

    await wrapper.setProps({ active: true })
    await flushPromises()
    expect(getUserBreakdown).toHaveBeenCalledTimes(2)
    expect(getUserBreakdown).toHaveBeenLastCalledWith(expect.objectContaining({ group_id: 4 }),
      { signal: expect.any(AbortSignal) })
    wrapper.unmount()
  })
})
