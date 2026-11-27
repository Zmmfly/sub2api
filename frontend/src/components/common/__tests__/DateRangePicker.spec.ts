import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { ref } from 'vue'

import DateRangePicker from '../DateRangePicker.vue'

const messages: Record<string, string> = {
  'dates.today': 'Today',
  'dates.yesterday': 'Yesterday',
  'dates.last24Hours': 'Last 24 Hours',
  'dates.last7Days': 'Last 7 Days',
  'dates.last14Days': 'Last 14 Days',
  'dates.last30Days': 'Last 30 Days',
  'dates.thisMonth': 'This Month',
  'dates.lastMonth': 'Last Month',
  'dates.custom': 'Custom',
  'dates.startDate': 'Start Date',
  'dates.endDate': 'End Date',
  'dates.apply': 'Apply',
  'dates.selectDateRange': 'Select date range'
}

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => messages[key] ?? key,
    locale: ref('en')
  })
}))

const formatLocalDate = (date: Date): string => {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

describe('DateRangePicker', () => {
  it('uses last 24 hours as the default recognized preset', () => {
    const now = new Date()
    const yesterday = new Date(now.getTime() - 24 * 60 * 60 * 1000)

    const wrapper = mount(DateRangePicker, {
      props: {
        startDate: formatLocalDate(yesterday),
        endDate: formatLocalDate(now)
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    expect(wrapper.text()).toContain('Last 24 Hours')
  })

  it('emits range updates with last24Hours preset when applied', async () => {
    const now = new Date()
    const today = formatLocalDate(now)

    const wrapper = mount(DateRangePicker, {
      props: {
        startDate: today,
        endDate: today
      },
      global: {
        stubs: {
          Icon: true
        }
      }
    })

    await wrapper.find('.date-picker-trigger').trigger('click')
    const presetButton = wrapper.findAll('.date-picker-preset').find((node) =>
      node.text().includes('Last 24 Hours')
    )
    expect(presetButton).toBeDefined()

    await presetButton!.trigger('click')
    await wrapper.find('.date-picker-apply').trigger('click')

    const nowAfterClick = new Date()
    const yesterdayAfterClick = new Date(nowAfterClick.getTime() - 24 * 60 * 60 * 1000)
    const expectedStart = formatLocalDate(yesterdayAfterClick)
    const expectedEnd = formatLocalDate(nowAfterClick)

    expect(wrapper.emitted('update:startDate')?.[0]).toEqual([expectedStart])
    expect(wrapper.emitted('update:endDate')?.[0]).toEqual([expectedEnd])
    expect(wrapper.emitted('change')?.[0]).toEqual([
      {
        startDate: expectedStart,
        endDate: expectedEnd,
        preset: 'last24Hours'
      }
    ])
  })

  it('validates 24-hour times and includes the entire ending minute in a custom range', async () => {
    const date = '2026-07-01'
    const wrapper = mount(DateRangePicker, {
      props: { startDate: date, endDate: date, allowTime: true },
      global: { stubs: { Icon: true } },
    })
    await wrapper.get('.date-picker-trigger').trigger('click')
    const custom = wrapper.findAll('.date-picker-preset').find(button => button.text() === 'Custom')
    await custom!.trigger('click')
    const [start, end] = wrapper.findAll('input[placeholder="HH:mm"]')
    await start.setValue('18:45')
    await end.setValue('18:30')
    expect(wrapper.get('.date-picker-apply').attributes()).toHaveProperty('disabled')
    await end.setValue('24:00')
    expect(wrapper.get('.date-picker-apply').attributes()).toHaveProperty('disabled')
    await end.setValue('19:05')
    await wrapper.get('.date-picker-apply').trigger('click')

    const startTime = new Date(`${date}T18:45:00`).toISOString()
    const endTime = new Date(`${date}T19:06:00`).toISOString()
    expect(wrapper.emitted('change')?.[0]).toEqual([{
      startDate: date, endDate: date, preset: 'custom', startTime, endTime,
    }])
    await wrapper.setProps({ startTime, endTime })
    expect(wrapper.get('.date-picker-trigger').text()).toContain('19:05')
    await wrapper.get('.date-picker-trigger').trigger('click')
    const today = wrapper.findAll('.date-picker-preset').find(button => button.text() === 'Today')
    await today!.trigger('click')
    expect(wrapper.findAll('input[placeholder="HH:mm"]')).toHaveLength(0)
    await wrapper.get('.date-picker-apply').trigger('click')
    expect(wrapper.emitted('change')?.[1]?.[0]).not.toHaveProperty('startTime')
    wrapper.unmount()
  })
})
