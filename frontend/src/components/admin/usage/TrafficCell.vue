<template>
  <div data-testid="traffic-cell" class="space-y-1 text-xs" :title="t('usage.traffic.bodyOnlyHint')">
    <div
      v-for="row in rows"
      :key="row.key"
      :data-testid="`traffic-${row.key}`"
      class="flex flex-wrap items-baseline gap-x-2 gap-y-0.5"
    >
      <span class="text-gray-500 dark:text-gray-400">{{ row.label }}</span>
      <span
        v-for="measurement in row.measurements"
        :key="measurement.key"
        :data-testid="`traffic-${row.key}-${measurement.key}`"
        class="inline-flex flex-wrap items-baseline gap-x-1"
      >
        <span v-if="measurement.label" class="text-gray-500 dark:text-gray-400">{{ measurement.label }}</span>
        <span data-testid="traffic-actual" class="whitespace-nowrap font-medium tabular-nums text-gray-900 dark:text-white">
          {{ formatTrafficBytes(measurement.value?.actual_bytes) }}
        </span>
        <span
          v-if="measurement.value?.compressed === true"
          data-testid="traffic-compression"
          class="whitespace-nowrap tabular-nums text-gray-500 dark:text-gray-400"
        >
          ({{ t('usage.traffic.original') }} {{ formatTrafficBytes(measurement.value.original_bytes) }};
          {{ t('usage.traffic.compressionRate') }} {{ formatTrafficCompressionRate(measurement.value.original_bytes, measurement.value.actual_bytes) }})
        </span>
      </span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import type { TrafficLink, TrafficMeasurement, UsageTraffic } from '@/types'
import { formatTrafficBytes, formatTrafficCompressionRate } from '@/utils/traffic'

interface TrafficRow {
  key: string
  label: string
  measurements: {
    key: string
    label?: string
    value?: TrafficMeasurement | null
  }[]
}

const props = withDefaults(defineProps<{
  traffic?: UsageTraffic | null
  showUpstream?: boolean
}>(), {
  showUpstream: false,
})
const { t } = useI18n()

const linkRow = (key: 'upstream' | 'downstream', link?: TrafficLink | null): TrafficRow => ({
  key,
  label: t(`usage.traffic.${key}`),
  measurements: [
    { key: 'request', label: t('usage.traffic.request'), value: link?.request },
    { key: 'response', label: t('usage.traffic.response'), value: link?.response },
  ],
})

const rows = computed<TrafficRow[]>(() => {
  if (props.showUpstream) {
    return [linkRow('upstream', props.traffic?.upstream), linkRow('downstream', props.traffic?.downstream)]
  }
  // Deliberately project only downstream, even if an API accidentally sends upstream.
  return [
    {
      key: 'client-upload',
      label: t('usage.traffic.clientUpload'),
      measurements: [{ key: 'request', value: props.traffic?.downstream?.request }],
    },
    {
      key: 'client-download',
      label: t('usage.traffic.clientDownload'),
      measurements: [{ key: 'response', value: props.traffic?.downstream?.response }],
    },
  ]
})
</script>
