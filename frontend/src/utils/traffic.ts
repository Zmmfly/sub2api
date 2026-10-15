const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'] as const

const isKnownByteCount = (bytes: number | null | undefined): bytes is number =>
  typeof bytes === 'number' && Number.isFinite(bytes) && bytes >= 0

/** Format measured HTTP body bytes; missing observations must not become zero. */
export function formatTrafficBytes(bytes: number | null | undefined): string {
  if (!isKnownByteCount(bytes)) return '—'

  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit += 1
  }

  // Carry rounding at a unit boundary (rather than displaying "1024 KiB").
  if (Number(value.toFixed(2)) >= 1024 && unit < BYTE_UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${Number(value.toFixed(2))} ${BYTE_UNITS[unit]}`
}

/** Savings relative to the original body. Compression can increase its size. */
export function formatTrafficCompressionRate(
  originalBytes: number | null | undefined,
  actualBytes: number | null | undefined,
): string {
  if (!isKnownByteCount(originalBytes) || originalBytes === 0 || !isKnownByteCount(actualBytes)) {
    return '—'
  }
  const rate = (1 - actualBytes / originalBytes) * 100
  if (!Number.isFinite(rate)) return '—'
  // Normalize rounded negative zero without clamping genuine negative rates.
  return `${Number(rate.toFixed(1)).toFixed(1)}%`
}
