import { flushPromises, mount } from '@vue/test-utils'
import { expect, it, vi } from 'vitest'
import OpsTopTrafficUsersCard from '../OpsTopTrafficUsersCard.vue'

const { getTopTrafficUsers } = vi.hoisted(() => ({ getTopTrafficUsers: vi.fn() }))
vi.mock('@/api/admin/ops', () => ({ opsAPI: { getTopTrafficUsers } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

it('uses the dashboard window and renders identities with formatted totals', async () => {
  const overview = {
    start_time: '2026-07-01T08:15:00Z',
    end_time: '2026-07-01T08:45:00Z',
    platform: 'openai',
    group_id: 7,
  }
  getTopTrafficUsers.mockResolvedValueOnce([
    { user_id: 3, username: 'Alice', email: 'alice@example.com', total_traffic_bytes: 1536 },
    { user_id: 4, username: '', email: 'bob@example.com', total_traffic_bytes: 42 },
  ])
  const wrapper = mount(OpsTopTrafficUsersCard, { props: { overview, refreshToken: 1 } })
  await flushPromises()

  expect(getTopTrafficUsers).toHaveBeenCalledWith(overview, { signal: expect.any(AbortSignal) })
  expect(wrapper.findAll('tbody tr').map(row => row.text())).toEqual([
    expect.stringContaining('Alice'),
    expect.stringContaining('bob@example.com'),
  ])
  expect(wrapper.text()).toContain('alice@example.com')
  expect(wrapper.text()).toContain('1.5 KiB')
  expect(wrapper.text()).toContain('42 B')

  const nextOverview = { ...overview, end_time: '2026-07-01T09:00:00Z', group_id: null }
  getTopTrafficUsers.mockResolvedValueOnce([])
  await wrapper.setProps({ overview: nextOverview, refreshToken: 2 })
  await flushPromises()
  expect(getTopTrafficUsers).toHaveBeenLastCalledWith(nextOverview, { signal: expect.any(AbortSignal) })
  expect(wrapper.text()).toContain('admin.ops.noData')
  wrapper.unmount()
})
