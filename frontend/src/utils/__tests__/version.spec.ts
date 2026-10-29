import { describe, expect, it } from 'vitest'
import { formatVersion, releaseTag } from '../version'

describe('release version formatting', () => {
  it.each([
    ['z0.2.18-1', 'z0.2.18-1'],
    ['z0.2.18-10', 'z0.2.18-10'],
    ['v0.2.18', 'v0.2.18'],
    ['0.2.18', 'v0.2.18'],
    ['traffic-stat-578c32412b22', 'traffic-stat-578c32412b22'],
    ['dev', 'dev'],
    ['', '']
  ])('preserves %s without double-prefixing', (input, expected) => {
    expect(formatVersion(input)).toBe(expected)
    expect(releaseTag(input)).toBe(expected)
  })
})
