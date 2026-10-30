import { createRequire } from 'node:module'
import { pathToFileURL } from 'node:url'
import { describe, expect, it } from 'vitest'

// Exercise the actual production dependency chain without making query-string
// a direct application dependency solely for this regression test.
const require = createRequire(import.meta.url)
const iconsRequire = createRequire(require.resolve('@lobehub/icons'))
const uiRequire = createRequire(iconsRequire.resolve('@lobehub/ui'))
const queryStringPath = uiRequire.resolve('query-string')
const { default: queryString } = await import(/* @vite-ignore */ pathToFileURL(queryStringPath).href)

describe('query-string with the patched URI decoder', () => {
  it('preserves Unicode, spaces, repeated keys, and empty values', () => {
    expect(queryString.parse('name=hello+world&city=%E6%9D%B1%E4%BA%AC&tag=a&tag=b&empty=&flag')).toEqual({
      name: 'hello world', city: '東京', tag: ['a', 'b'], empty: '', flag: null
    })
    const values = { name: 'hello world', city: '東京', tag: ['a', 'b'] }
    expect(queryString.parse(queryString.stringify(values))).toEqual(values)
  })

  it('retains tolerant decoding for malformed percent-encoded input', () => {
    expect(queryString.parse('value=%E0%A4%A&good=%C3%A5&bad=%zz')).toEqual({
      value: '%E0%A4%A', good: 'å', bad: '%zz'
    })
    expect(queryString.parse('value=%FE%FF&partial=%C3%28&percent=%25')).toEqual({
      value: '��', partial: '%C3(', percent: '%'
    })
  })

  it('handles many distinct encoded runs on the malformed-input fallback path', () => {
    // GHSA-qr28-fpjm-wq7f: the trailing invalid escape forces the fallback.
    // Do not use a wall-clock assertion, which would make CI hardware-dependent.
    const encoded = Array.from({ length: 32000 }, (_, i) => {
      const high = Math.floor(i / 256).toString(16).padStart(2, '0')
      const low = (i % 256).toString(16).padStart(2, '0')
      return `%${high}%${low}x`
    }).join('') + '%zz'
    const result = queryString.parse(`value=${encoded}`)
    expect(typeof result.value).toBe('string')
    expect(result.value.endsWith('%zz')).toBe(true)
    expect(result.value.split('x').length).toBeGreaterThanOrEqual(32001)
  })
})
