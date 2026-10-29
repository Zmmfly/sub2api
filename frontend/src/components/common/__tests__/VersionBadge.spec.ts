import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import VersionBadge from '../VersionBadge.vue'

const mocks = vi.hoisted(() => ({
  auth: { isAdmin: true },
  app: {
    currentVersion: 'z0.2.18-10', latestVersion: 'z0.2.18-11', hasUpdate: true,
    versionLoading: false, buildType: 'release', releaseInfo: null,
    fetchVersion: vi.fn(), clearVersionCache: vi.fn()
  },
  rollback: vi.fn(), getRollbackVersions: vi.fn()
}))
vi.mock('@/stores', () => ({ useAuthStore: () => mocks.auth, useAppStore: () => mocks.app }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/system', () => ({
  performUpdate: vi.fn(), restartService: vi.fn(),
  getRollbackVersions: mocks.getRollbackVersions, rollback: mocks.rollback
}))
vi.mock('@/composables/useClipboard', () => ({ useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() }) }))

describe('VersionBadge fork releases', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.auth.isAdmin = true
    mocks.app.hasUpdate = true
    mocks.getRollbackVersions.mockResolvedValue({ versions: [{ version: 'z0.2.18-9', published_at: '' }] })
  })

  it('renders the canonical version for non-admins', () => {
    mocks.auth.isAdmin = false
    const wrapper = mount(VersionBadge, { props: { version: 'z0.2.18-10' } })
    expect(wrapper.text()).toBe('z0.2.18-10')
    wrapper.unmount()
  })

  it('preserves z for an available update', async () => {
    const wrapper = mount(VersionBadge)
    await wrapper.find('button').trigger('click')
    expect(wrapper.text()).toContain('z0.2.18-11')
    expect(wrapper.text()).not.toContain('vz0.2.18')
    wrapper.unmount()
  })

  it('preserves z in admin display and both rollback commands across close/reopen', async () => {
    mocks.app.hasUpdate = false
    const wrapper = mount(VersionBadge)
    const clickText = async (text: string) => {
      const button = wrapper.findAll('button').find((item) => item.text().includes(text))
      expect(button, text).toBeDefined()
      await button!.trigger('click')
      await flushPromises()
    }
    await wrapper.find('button').trigger('click')
    expect(wrapper.text()).toContain('z0.2.18-10')
    expect(wrapper.text()).not.toContain('vz0.2.18')
    await clickText('version.rollback')
    await clickText('z0.2.18-9')
    expect(wrapper.text()).toContain('https://raw.githubusercontent.com/Zmmfly/sub2api/z0.2.18-9/deploy/install.sh')
    expect(wrapper.text()).toContain('rollback z0.2.18-9')
    await clickText('version.deployDocker')
    expect(wrapper.text()).toContain('image: ghcr.io/zmmfly/sub2api:z0.2.18-9')
    await wrapper.find('button').trigger('click')
    await wrapper.find('button').trigger('click')
    expect(wrapper.text()).not.toContain('vz0.2.18')
    expect(mocks.rollback).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
