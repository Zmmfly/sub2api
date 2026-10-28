import { describe, expect, it, beforeEach, afterEach, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import KeyUsageView from '../KeyUsageView.vue'

enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'setInterval', 'clearInterval', 'Date', 'performance'] })
  vi.stubGlobal('requestAnimationFrame', vi.fn((callback: FrameRequestCallback) =>
    window.setTimeout(() => callback(performance.now()), 16)))
  vi.stubGlobal('cancelAnimationFrame', vi.fn((id: number) => window.clearTimeout(id)))
})

afterEach(() => {
  vi.clearAllTimers()
  vi.useRealTimers()
  vi.unstubAllGlobals()
})

const { showInfo, showSuccess, showError, fetchPublicSettings } = vi.hoisted(() => ({
  showInfo: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn(),
  fetchPublicSettings: vi.fn(),
}))

const messages: Record<string, string> = {
  'keyUsage.title': 'API Key Usage',
  'keyUsage.subtitle': 'Usage status',
  'keyUsage.placeholder': 'sk-test',
  'keyUsage.query': 'Query',
  'keyUsage.querying': 'Querying...',
  'keyUsage.privacyNote': 'Privacy note',
  'keyUsage.dateRange': 'Date Range:',
  'keyUsage.dateRangeToday': 'Today',
  'keyUsage.dateRange7d': '7 Days',
  'keyUsage.dateRange30d': '30 Days',
  'keyUsage.dateRange90d': '90 Days',
  'keyUsage.dateRangeCustom': 'Custom',
  'keyUsage.apply': 'Apply',
  'keyUsage.used': 'Used',
  'keyUsage.detailInfo': 'Detail Information',
  'keyUsage.tokenStats': 'Token Statistics',
  'keyUsage.dailyDetail': 'Daily Detail',
  'keyUsage.date': 'Date',
  'keyUsage.requests': 'Requests',
  'keyUsage.inputTokens': 'Input Tokens',
  'keyUsage.outputTokens': 'Output Tokens',
  'keyUsage.cacheReadTokens': 'Cache Read',
  'keyUsage.cacheWriteTokens': 'Cache Write',
  'keyUsage.cost': 'Cost',
  'keyUsage.quotaMode': 'Key Quota Mode',
  'keyUsage.walletBalance': 'Wallet Balance',
  'keyUsage.totalQuota': 'Total Quota',
  'keyUsage.limit5h': '5-Hour Limit',
  'keyUsage.limitDaily': 'Daily Limit',
  'keyUsage.limit7d': '7-Day Limit',
  'keyUsage.limitWeekly': 'Weekly Limit',
  'keyUsage.limitMonthly': 'Monthly Limit',
  'keyUsage.remainingQuota': 'Remaining Quota',
  'keyUsage.usedQuota': 'Used Quota',
  'keyUsage.subscriptionType': 'Subscription Type',
  'keyUsage.billingType': 'Billing Type',
  'keyUsage.todayRequests': 'Today Requests',
  'keyUsage.todayInputTokens': 'Today Input',
  'keyUsage.todayOutputTokens': 'Today Output',
  'keyUsage.todayTokens': 'Today Tokens',
  'keyUsage.todayCacheCreation': 'Today Cache Creation',
  'keyUsage.todayCacheRead': 'Today Cache Read',
  'keyUsage.todayCost': 'Today Cost',
  'keyUsage.rpmTpm': 'RPM / TPM',
  'keyUsage.totalRequests': 'Total Requests',
  'keyUsage.totalInputTokens': 'Total Input',
  'keyUsage.totalOutputTokens': 'Total Output',
  'keyUsage.totalTokensLabel': 'Total Tokens',
  'keyUsage.totalCacheCreation': 'Total Cache Creation',
  'keyUsage.totalCacheRead': 'Total Cache Read',
  'keyUsage.totalCost': 'Total Cost',
  'keyUsage.avgDuration': 'Avg Duration',
  'keyUsage.querySuccess': 'Query successful',
  'keyUsage.queryFailed': 'Query failed',
  'keyUsage.queryFailedRetry': 'Query failed, please try again later',
  'home.viewDocs': 'Docs',
  'home.switchToLight': 'Light',
  'home.switchToDark': 'Dark',
  'home.footer.allRightsReserved': 'All rights reserved.',
}

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => messages[key] ?? key,
      locale: { value: 'en' },
    }),
  }
})

const appStoreState = vi.hoisted(() => ({
  cachedPublicSettings: null as Record<string, unknown> | null,
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({
    get cachedPublicSettings() {
      return appStoreState.cachedPublicSettings
    },
    siteName: 'Sub2API',
    siteLogo: '',
    docUrl: '',
    publicSettingsLoaded: true,
    fetchPublicSettings,
    showInfo,
    showSuccess,
    showError,
  }),
}))

describe('KeyUsageView daily detail', () => {
  beforeEach(() => {
    showInfo.mockReset()
    showSuccess.mockReset()
    showError.mockReset()
    fetchPublicSettings.mockReset()
    localStorage.clear()

    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        mode: 'quota_limited',
        isValid: true,
        status: 'active',
        quota: {
          limit: 10,
          used: 1,
          remaining: 9,
          unit: 'USD',
        },
        usage: {
          today: {
            requests: 1,
            input_tokens: 10,
            output_tokens: 20,
            cache_creation_tokens: 0,
            cache_read_tokens: 0,
            total_tokens: 30,
            actual_cost: 0.01,
          },
          total: {
            requests: 12,
            input_tokens: 100,
            output_tokens: 200,
            cache_creation_tokens: 10,
            cache_read_tokens: 30,
            total_tokens: 340,
            actual_cost: 0.12,
          },
          rpm: 0,
          tpm: 0,
        },
        daily_usage: [
          {
            date: '2026-05-19',
            requests: 12,
            input_tokens: 100,
            output_tokens: 200,
            cache_read_tokens: 30,
            cache_write_tokens: 10,
            total_tokens: 340,
            cost: 0.15,
            actual_cost: 0.12,
          },
        ],
      }),
    }))
  })

  it('renders daily usage detail rows after a successful query', async () => {
    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })

    await wrapper.find('input').setValue('sk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()

    const fetchMock = vi.mocked(fetch)
    expect(fetchMock).toHaveBeenCalledWith(
      expect.stringContaining('/v1/usage?'),
      expect.objectContaining({
        headers: { Authorization: 'Bearer sk-test-key' },
      })
    )
    expect(String(fetchMock.mock.calls[0][0])).toContain('days=30')

    const text = wrapper.text()
    expect(text).toContain('Daily Detail')
    expect(text).toContain('Date')
    expect(text).toContain('Cache Read')
    expect(text).toContain('Cache Write')
    expect(text).toContain('2026-05-19')
    expect(text).toContain('12')
    expect(text).toContain('100')
    expect(text).toContain('200')
    expect(text).toContain('30')
    expect(text).toContain('10')
    expect(text).toContain('$0.12')

    wrapper.unmount()
  })

  it('queries the current local calendar date near midnight', async () => {
    vi.setSystemTime(new Date(2026, 6, 13, 0, 30))

    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })

    await wrapper.find('input').setValue('sk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()

    const requestUrl = String(vi.mocked(fetch).mock.calls[0][0])
    expect(requestUrl).toContain('start_date=2026-07-13')
    expect(requestUrl).toContain('end_date=2026-07-13')

    wrapper.unmount()
  })
})

describe('KeyUsageView subscription feature flag', () => {
  beforeEach(() => {
    localStorage.clear()
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        mode: 'wallet',
        isValid: true,
        status: 'active',
        balance: 5.5,
        usage: {
          today: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
          total: { requests: 0, input_tokens: 0, output_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0, total_tokens: 0, actual_cost: 0 },
          rpm: 0,
          tpm: 0,
        },
        daily_usage: [],
      }),
    }))
  })

  afterEach(() => {
    appStoreState.cachedPublicSettings = null
  })

  async function mountAndQuery() {
    const wrapper = mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })
    await wrapper.find('input').setValue('sk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()
    return wrapper
  }

  it('labels the wallet row "Subscription Type" while subscriptions are enabled', async () => {
    const wrapper = await mountAndQuery()

    expect(wrapper.text()).toContain('Subscription Type')
    expect(wrapper.text()).toContain('Wallet Balance')
    wrapper.unmount()
  })

  it('drops the "Subscription" wording from the wallet row when subscriptions are disabled', async () => {
    appStoreState.cachedPublicSettings = { subscription_enabled: false }
    const wrapper = await mountAndQuery()

    expect(wrapper.text()).toContain('Billing Type')
    expect(wrapper.text()).not.toContain('Subscription Type')
    expect(wrapper.text()).toContain('Wallet Balance')
    wrapper.unmount()
  })
})

describe('KeyUsageView ring animation lifecycle', () => {
  const stages = [
    { stage: 'the initial frame', elapsed: 0 },
    { stage: 'the start delay', elapsed: 16 },
    { stage: 'the percentage animation', elapsed: 82 },
  ]

  function usageResponse(used = 1) {
    return new Response(JSON.stringify({
      mode: 'quota_limited',
      isValid: true,
      status: 'active',
      quota: { limit: 10, used, remaining: 10 - used, unit: 'USD' },
    }))
  }

  function deferredResponse() {
    let resolve!: (response: Response) => void
    let reject!: (reason: Error) => void
    const promise = new Promise<Response>((resolveResponse, rejectResponse) => {
      resolve = resolveResponse
      reject = rejectResponse
    })
    return { promise, resolve, reject }
  }

  function mountView() {
    return mount(KeyUsageView, {
      global: {
        stubs: {
          RouterLink: { template: '<a><slot /></a>' },
          LocaleSwitcher: true,
          Icon: true,
        },
      },
    })
  }

  async function query(wrapper: ReturnType<typeof mountView>) {
    await wrapper.find('input').setValue('sk-test-key')
    await wrapper.find('input').trigger('keydown.enter')
    await flushPromises()
    await nextTick()
  }

  beforeEach(() => {
    showSuccess.mockReset()
    showError.mockReset()
    localStorage.clear()
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockReturnValue({ matches: false }),
    })
    vi.stubGlobal('fetch', vi.fn().mockImplementation(async () => usageResponse()))
  })

  it.each(stages)('cancels $stage on unmount before globals are restored', async ({ elapsed }) => {
    const wrapper = mountView()
    await query(wrapper)
    await vi.advanceTimersByTimeAsync(elapsed)
    expect(vi.getTimerCount()).toBe(2) // Refresh interval plus one animation task.

    const requestFrame = vi.mocked(requestAnimationFrame)
    const frameCount = requestFrame.mock.calls.length
    wrapper.unmount()

    expect(vi.getTimerCount()).toBe(0)
    vi.unstubAllGlobals()
    await vi.advanceTimersByTimeAsync(1500)
    expect(requestFrame).toHaveBeenCalledTimes(frameCount)
  })

  it('does not start a queued nextTick animation after unmount', async () => {
    const wrapper = mountView()
    // Query success runs synchronously after the animation's nextTick is queued.
    showSuccess.mockImplementationOnce(() => wrapper.unmount())
    await query(wrapper)

    expect(showSuccess).toHaveBeenCalledOnce()
    expect(requestAnimationFrame).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('ignores a query response that arrives after unmount', async () => {
    const pending = deferredResponse()
    vi.mocked(fetch).mockReturnValueOnce(pending.promise)
    const wrapper = mountView()
    await query(wrapper)
    wrapper.unmount()

    pending.resolve(usageResponse())
    await flushPromises()
    await nextTick()

    expect(requestAnimationFrame).not.toHaveBeenCalled()
    expect(showSuccess).not.toHaveBeenCalled()
    expect(showError).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('ignores a query failure that arrives after unmount', async () => {
    const pending = deferredResponse()
    vi.mocked(fetch).mockReturnValueOnce(pending.promise)
    const wrapper = mountView()
    await query(wrapper)
    wrapper.unmount()

    pending.reject(new Error('request failed after unmount'))
    await flushPromises()

    expect(showError).not.toHaveBeenCalled()
    expect(showSuccess).not.toHaveBeenCalled()
    expect(requestAnimationFrame).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
  })

  it.each(stages)('cancels $stage when a new query starts', async ({ elapsed }) => {
    const wrapper = mountView()
    await query(wrapper)
    await vi.advanceTimersByTimeAsync(elapsed)
    const requestFrame = vi.mocked(requestAnimationFrame)
    const obsoleteFrame = requestFrame.mock.calls.at(-1)![0]
    const pending = deferredResponse()
    vi.mocked(fetch).mockReturnValueOnce(pending.promise)

    await query(wrapper)
    expect(vi.getTimerCount()).toBe(1) // Only the refresh interval remains.
    const frameCount = requestFrame.mock.calls.length
    // Even an already-delivered callback must not restart the obsolete animation.
    obsoleteFrame(performance.now())
    await vi.advanceTimersByTimeAsync(1500)
    expect(requestFrame).toHaveBeenCalledTimes(frameCount)
    expect(vi.getTimerCount()).toBe(1)

    pending.resolve(usageResponse(8))
    await flushPromises()
    await nextTick()
    expect(wrapper.find('.text-3xl.tabular-nums').text()).toBe('0%')
    await vi.advanceTimersByTimeAsync(1200)
    expect(wrapper.find('.text-3xl.tabular-nums').text()).toBe('80%')
    expect(vi.getTimerCount()).toBe(1)
  })

  it('finishes the percentage animation without leaving animation tasks', async () => {
    const wrapper = mountView()
    await query(wrapper)
    expect(wrapper.find('.text-3xl.tabular-nums').text()).toBe('0%')

    await vi.advanceTimersByTimeAsync(1200)

    expect(wrapper.find('.text-3xl.tabular-nums').text()).toBe('10%')
    expect(vi.getTimerCount()).toBe(1)
    wrapper.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})
