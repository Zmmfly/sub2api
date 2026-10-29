/** Preserve fork and legacy build identities; add v only to bare upstream semver. */
export function formatVersion(version: string): string {
  return /^\d+\.\d+\.\d+(?:[-+].*)?$/.test(version) ? `v${version}` : version
}

export const releaseTag = formatVersion
