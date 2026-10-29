import { describe, expect, it } from 'vitest'
import { createI18n } from 'vue-i18n'
import en from '../locales/en'
import zh from '../locales/zh'

describe('i18n runtime compatibility', () => {
  it('translates and interpolates traffic messages across locale changes', () => {
    const { global: i18n } = createI18n({
      legacy: false,
      locale: 'en',
      fallbackLocale: 'en',
      messages: { en, zh }
    })

    expect(i18n.t('admin.ops.traffic.title')).toBe('HTTP body traffic')
    expect(i18n.t('admin.ops.traffic.coverage', { count: 2, total: 3 })).toBe('Measured 2/3 bodies')
    i18n.locale.value = 'zh'
    expect(i18n.t('admin.ops.traffic.title')).toBe('HTTP 正文流量')
    expect(i18n.t('admin.ops.traffic.coverage', { count: 2, total: 3 })).toBe('已采集 2/3 个正文')
    i18n.locale.value = 'en'
    expect(i18n.t('admin.ops.traffic.ingestionPendingShort', { count: 4 })).toBe('4 pending')
  })

  it('neutralizes iframe srcdoc in escaped HTML translations', () => {
    const { global: i18n } = createI18n({
      legacy: false,
      locale: 'en',
      escapeParameter: true,
      warnHtmlMessage: false,
      messages: { en: { message: '<iframe srcdoc="<b>untrusted</b>"></iframe>' } }
    })
    const container = document.createElement('div')
    container.innerHTML = i18n.t('message')
    expect(container.querySelector('iframe')).not.toBeNull()
    expect(container.querySelector('iframe')?.hasAttribute('srcdoc')).toBe(false)
  })
})
