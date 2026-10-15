<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { OpsTrafficLink, OpsTrafficSummary } from '@/api/admin/ops'
import { formatTrafficBytes } from '@/utils/traffic'

const props = withDefaults(defineProps<{
  traffic?: OpsTrafficSummary | null
  loading?: boolean
}>(), { traffic: null, loading: false })

const { t } = useI18n()

const sides = computed(() => [
  { key: 'upstream', summary: props.traffic?.upstream },
  { key: 'downstream', summary: props.traffic?.downstream }
] as const)

const measurements = [
  { key: 'original', bytes: 'original_bytes', count: 'original_sample_count' },
  { key: 'actual', bytes: 'actual_bytes', count: 'actual_sample_count' }
] as const

function validCount(value: number | null | undefined): value is number {
  return typeof value === 'number' && Number.isSafeInteger(value) && value >= 0
}

const ingestion = computed(() => props.traffic?.ingestion?.scope === 'instance_lifetime' ? props.traffic.ingestion : null)
// These counters are not filtered traffic totals. Only show known problems;
// absent health must never produce a healthy/complete badge.
function positiveCount(value: number | undefined): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : 0
}
const ingestionFailed = computed(() => positiveCount(ingestion.value?.failed))
const ingestionPending = computed(() => positiveCount(ingestion.value?.pending))

// Traffic has its own persisted HTTP request ledger, including failures without
// usage. Each stored request contributes two bodies; pending/lost events are not
// in this denominator. overview.request_count_total has a different population.
const totalBodies = computed(() => {
  const count = props.traffic?.request_count
  return validCount(count) && Number.isSafeInteger(count * 2) ? count * 2 : null
})

function coverageText(count: number | undefined, paired = false): string {
  const total = totalBodies.value
  if (total == null || !validCount(count) || count > total) {
    return t('admin.ops.traffic.coverageUnknown')
  }
  return t(paired ? 'admin.ops.traffic.pairedCoverage' : 'admin.ops.traffic.coverage', { count, total })
}

function coverageState(summary: OpsTrafficLink | undefined): 'partial' | 'unavailable' | null {
  const original = summary?.original_sample_count
  const actual = summary?.actual_sample_count
  if ((!validCount(original) || original === 0) && (!validCount(actual) || actual === 0)) {
    return 'unavailable'
  }
  const total = totalBodies.value
  if (total == null || !validCount(original) || !validCount(actual) || original < total || actual < total) {
    return 'partial'
  }
  return null
}

function compressionLabel(summary: OpsTrafficLink | undefined): string {
  const percent = summary?.compression_percent
  if (summary?.original_bytes === 0 || !validCount(summary?.paired_sample_count) || summary.paired_sample_count === 0) return '—'
  // Do not derive this from original_bytes / actual_bytes: their coverage may
  // differ. The server computes the ratio from the same paired sample set.
  if (typeof percent !== 'number' || !Number.isFinite(percent)) return '—'
  return `${Number(percent.toFixed(1)).toFixed(1)}%`
}
</script>

<template>
  <section
    class="grid min-w-0 grid-cols-1 gap-4 md:grid-cols-3"
    data-testid="traffic-cards"
    :aria-label="t('admin.ops.traffic.title')"
    :aria-busy="loading"
  >
    <span v-if="loading" class="sr-only" role="status">{{ t('admin.ops.loadingText') }}</span>

    <div
      v-if="!loading && (ingestionFailed > 0 || ingestionPending > 0)"
      class="min-w-0 rounded-xl border p-3 text-xs md:col-span-3"
      :class="ingestionFailed > 0 ? 'border-red-200 bg-red-50 text-red-800 dark:border-red-900 dark:bg-red-950/40 dark:text-red-300' : 'border-amber-200 bg-amber-50 text-amber-800 dark:border-amber-900 dark:bg-amber-950/40 dark:text-amber-300'"
      :role="ingestionFailed > 0 ? 'alert' : 'status'"
      data-testid="traffic-ingestion-warning"
    >
      <p v-if="ingestionFailed > 0" class="font-semibold" data-testid="traffic-ingestion-failed">{{ t('admin.ops.traffic.ingestionFailed', { count: ingestionFailed }) }}</p>
      <p v-if="ingestionPending > 0" class="font-semibold" data-testid="traffic-ingestion-pending">{{ t('admin.ops.traffic.ingestionPending', { count: ingestionPending }) }}</p>
      <p class="mt-1" data-testid="traffic-ingestion-scope">{{ t('admin.ops.traffic.ingestionScope') }}</p>
      <p class="mt-1" data-testid="traffic-ingestion-coverage">{{ t('admin.ops.traffic.ingestionCoverage') }}</p>
    </div>

    <article
      v-for="side in sides"
      :key="side.key"
      class="min-w-0 rounded-2xl bg-gray-50 p-4 dark:bg-dark-900"
      :data-testid="`traffic-${side.key}-card`"
    >
      <div class="flex flex-wrap items-center justify-between gap-2">
        <h2 class="text-xs font-bold text-gray-500 dark:text-gray-400">{{ t(`admin.ops.traffic.${side.key}`) }}</h2>
        <span
          v-if="!loading && coverageState(side.summary)"
          class="text-[10px] text-amber-700 dark:text-amber-400"
          :data-testid="`traffic-${side.key}-coverage-state`"
        >{{ t(`admin.ops.traffic.${coverageState(side.summary)}`) }}</span>
      </div>

      <dl class="mt-3 space-y-3">
        <div v-for="metric in measurements" :key="metric.key">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t(`admin.ops.traffic.${metric.key}`) }}</dt>
          <dd class="mt-1">
            <div v-if="loading" class="h-7 w-28 animate-pulse rounded bg-gray-200 dark:bg-dark-700" aria-hidden="true"></div>
            <div
              v-else
              class="min-w-0 text-2xl font-black tabular-nums text-gray-900 [overflow-wrap:anywhere] dark:text-white"
              :data-testid="`traffic-${side.key}-${metric.key}`"
            >{{ formatTrafficBytes(side.summary?.[metric.bytes]) }}</div>
            <div v-if="loading" class="mt-1 h-3 w-32 animate-pulse rounded bg-gray-200 dark:bg-dark-700" aria-hidden="true"></div>
            <div
              v-else
              class="mt-1 text-[10px] text-gray-500 dark:text-gray-400"
              :data-testid="`traffic-${side.key}-${metric.key}-coverage`"
            >{{ coverageText(side.summary?.[metric.count]) }}</div>
          </dd>
        </div>
      </dl>
    </article>

    <article class="min-w-0 rounded-2xl bg-gray-50 p-4 dark:bg-dark-900" data-testid="traffic-composition-card">
      <h2 class="text-xs font-bold text-gray-500 dark:text-gray-400">{{ t('admin.ops.traffic.composition') }}</h2>
      <p class="mt-1 text-[10px] text-gray-500 dark:text-gray-400">{{ t('admin.ops.traffic.compression') }}</p>
      <dl class="mt-3 space-y-3">
        <div v-for="side in sides" :key="side.key">
          <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t(`admin.ops.traffic.${side.key}`) }}</dt>
          <dd class="mt-1">
            <div v-if="loading" class="h-7 w-24 animate-pulse rounded bg-gray-200 dark:bg-dark-700" aria-hidden="true"></div>
            <div
              v-else
              class="min-w-0 text-2xl font-black tabular-nums text-gray-900 [overflow-wrap:anywhere] dark:text-white"
              :data-testid="`traffic-${side.key}-compression`"
            >{{ compressionLabel(side.summary) }}</div>
            <div v-if="loading" class="mt-1 h-3 w-32 animate-pulse rounded bg-gray-200 dark:bg-dark-700" aria-hidden="true"></div>
            <div
              v-else
              class="mt-1 text-[10px] text-gray-500 dark:text-gray-400"
              :data-testid="`traffic-${side.key}-paired-coverage`"
            >{{ coverageText(side.summary?.paired_sample_count, true) }}</div>
          </dd>
        </div>
      </dl>
      <p class="mt-3 text-[10px] leading-relaxed text-gray-500 dark:text-gray-400" data-testid="traffic-compression-hint">{{ t('admin.ops.traffic.compressionHint') }}</p>
    </article>

    <p class="text-[10px] leading-relaxed text-gray-500 dark:text-gray-400 md:col-span-3" data-testid="traffic-scope">{{ t('admin.ops.traffic.scope') }}</p>
  </section>
</template>
