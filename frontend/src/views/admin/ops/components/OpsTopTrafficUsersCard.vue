<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { opsAPI, type OpsDashboardOverview, type OpsTopTrafficUser } from '@/api/admin/ops'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import { formatTrafficBytes } from '@/utils/traffic'

const props = defineProps<{
  overview: Pick<OpsDashboardOverview, 'start_time' | 'end_time' | 'platform' | 'group_id'>
  refreshToken: number
}>()

const { t } = useI18n()
const users = ref<OpsTopTrafficUser[]>([])
const loading = ref(true)
const failed = ref(false)

watch(() => props.refreshToken, async (_token, _previous, onCleanup) => {
  const controller = new AbortController()
  onCleanup(() => controller.abort())

  const { start_time, end_time, platform, group_id } = props.overview
  loading.value = true
  failed.value = false
  try {
    const result = await opsAPI.getTopTrafficUsers({ start_time, end_time, platform, group_id }, { signal: controller.signal })
    if (!controller.signal.aborted) users.value = result
  } catch {
    if (!controller.signal.aborted) failed.value = true
  } finally {
    if (!controller.signal.aborted) loading.value = false
  }
}, { immediate: true })
</script>

<template>
  <section class="card p-4 sm:p-5" :aria-busy="loading">
    <h2 class="text-base font-semibold text-gray-900 dark:text-white">{{ t('admin.ops.topTrafficUsers.title') }}</h2>

    <div v-if="loading" class="py-6 text-center"><LoadingSpinner /></div>
    <p v-else-if="failed" class="py-6 text-center text-sm text-rose-600 dark:text-rose-400">
      {{ t('admin.ops.topTrafficUsers.loadFailed') }}
    </p>
    <p v-else-if="users.length === 0" class="py-6 text-center text-sm text-gray-500 dark:text-gray-400">
      {{ t('admin.ops.noData') }}
    </p>

    <div v-else class="mt-3 overflow-x-auto">
      <table class="w-full text-sm">
        <thead class="border-b border-gray-200 text-gray-500 dark:border-dark-700 dark:text-gray-400">
          <tr>
            <th scope="col" class="py-2 text-left font-medium">{{ t('admin.usage.user') }}</th>
            <th scope="col" class="py-2 pl-4 text-right font-medium">{{ t('usage.traffic.title') }}</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-gray-100 dark:divide-dark-700/50">
          <tr v-for="user in users" :key="user.user_id">
            <td class="py-2">
              <div class="break-all font-medium text-gray-900 dark:text-white">{{ user.username || user.email || `#${user.user_id}` }}</div>
              <div v-if="user.username && user.email && user.username !== user.email" class="break-all text-xs text-gray-500 dark:text-gray-400">
                {{ user.email }}
              </div>
            </td>
            <td class="whitespace-nowrap py-2 pl-4 text-right font-medium tabular-nums text-gray-900 dark:text-white">
              {{ formatTrafficBytes(user.total_traffic_bytes) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </section>
</template>
