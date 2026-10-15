<template>
  <div data-testid="traffic-cell" class="space-y-1 text-xs" :title="t('usage.traffic.bodyOnlyHint')">
    <div
      v-for="row in rows"
      :key="row.key"
      :data-testid="`traffic-${row.key}`"
      class="flex flex-wrap items-center gap-x-3 gap-y-0.5"
    >
      <span class="text-gray-500 dark:text-gray-400">{{ row.label }}</span>
      <span
        v-for="measurement in row.measurements"
        :key="measurement.key"
        :data-testid="`traffic-${row.key}-${measurement.key}`"
        class="inline-flex items-center gap-1 whitespace-nowrap"
      >
        <span
          v-if="measurement.label"
          role="img"
          class="inline-flex items-center"
          :aria-label="measurement.label"
          :title="measurement.label"
          :data-testid="`traffic-${row.key}-${measurement.key}-direction`"
        >
          <Icon
            :name="measurement.key === 'request' ? 'arrowDown' : 'arrowUp'"
            size="sm"
            class="h-3.5 w-3.5"
            :class="measurement.key === 'request' ? 'text-emerald-500' : 'text-violet-500'"
            aria-hidden="true"
          />
        </span>
        <span data-testid="traffic-actual" class="whitespace-nowrap font-medium tabular-nums text-gray-900 dark:text-white">
          {{ formatTrafficBytes(measurement.value?.actual_bytes) }}
        </span>
        <template v-if="compressionRate(measurement.value) !== null">
          <span
            data-testid="traffic-compression"
            class="whitespace-nowrap tabular-nums text-gray-500 dark:text-gray-400"
            :aria-label="`${t('usage.traffic.compressionRate')} ${compressionRate(measurement.value)}`"
          >{{ compressionRate(measurement.value) }}</span>
          <HelpTooltip
            class="ml-0"
            width-class="w-max max-w-[calc(100vw-2rem)]"
            :content="`${t('usage.traffic.original')} ${formatTrafficBytes(measurement.value?.original_bytes)}`"
            data-testid="traffic-original-tooltip"
          >
            <template #trigger>
              <span
                role="img"
                class="inline-flex cursor-help items-center text-gray-400 transition-colors hover:text-primary-600 dark:text-gray-500 dark:hover:text-primary-400"
                :aria-label="t('usage.traffic.original')"
                :title="t('usage.traffic.original')"
                data-testid="traffic-original-icon"
              >
                <Icon name="infoCircle" size="xs" aria-hidden="true" />
              </span>
            </template>
          </HelpTooltip>
        </template>
      </span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import HelpTooltip from '@/components/common/HelpTooltip.vue'
import Icon from '@/components/icons/Icon.vue'
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

function compressionRate(value?: TrafficMeasurement | null): string | null {
  if (value?.compressed !== true) return null
  const rate = formatTrafficCompressionRate(value.original_bytes, value.actual_bytes)
  return rate === '—' ? null : rate
}

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
      measurements: [{ key: 'request', label: t('usage.traffic.request'), value: props.traffic?.downstream?.request }],
    },
    {
      key: 'client-download',
      label: t('usage.traffic.clientDownload'),
      measurements: [{ key: 'response', label: t('usage.traffic.response'), value: props.traffic?.downstream?.response }],
    },
  ]
})
</script>
