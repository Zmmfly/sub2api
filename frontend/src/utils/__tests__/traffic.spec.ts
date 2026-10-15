import { describe, expect, it } from 'vitest'
import { formatTrafficBytes, formatTrafficCompressionRate } from '../traffic'

describe('formatTrafficBytes', () => {
  it.each([
    [undefined, '—'],
    [null, '—'],
    [NaN, '—'],
    [Infinity, '—'],
    [-Infinity, '—'],
    [-1, '—'],
    [0, '0 B'],
    [-0, '0 B'],
    [1, '1 B'],
    [1023, '1023 B'],
    [1024, '1 KiB'],
    [1536, '1.5 KiB'],
    [1280, '1.25 KiB'],
    [1024 ** 2 - 1, '1 MiB'],
    [1024 ** 2, '1 MiB'],
    [1024 ** 3, '1 GiB'],
    [1024 ** 4, '1 TiB'],
    [1024 ** 5, '1 PiB'],
    [1024 ** 6, '1 EiB'],
    [2 ** 63 - 1, '8 EiB'],
    [Number.MAX_SAFE_INTEGER, '8 PiB'],
  ])('formats %s as %s', (bytes, expected) => {
    expect(formatTrafficBytes(bytes)).toBe(expected)
  })
})

describe('formatTrafficCompressionRate', () => {
  it.each([
    [100, 50, '50.0%'],
    [100, 100, '0.0%'],
    [100, 150, '-50.0%'],
    [100, 200, '-100.0%'],
    [100, 0, '100.0%'],
    [3, 1, '66.7%'],
    [100000, 100001, '0.0%'],
    [Number.MAX_VALUE, Number.MAX_VALUE, '0.0%'],
    [0, 0, '—'],
    [0, 10, '—'],
    [undefined, undefined, '—'],
    [null, 0, '—'],
    [100, null, '—'],
    [100, undefined, '—'],
    [NaN, 1, '—'],
    [1, NaN, '—'],
    [Infinity, 1, '—'],
    [1, Infinity, '—'],
    [-1, 1, '—'],
    [1, -1, '—'],
    [Number.MIN_VALUE, Number.MAX_VALUE, '—'],
  ])('formats original %s / actual %s as %s', (original, actual, expected) => {
    expect(formatTrafficCompressionRate(original, actual)).toBe(expected)
  })
})
