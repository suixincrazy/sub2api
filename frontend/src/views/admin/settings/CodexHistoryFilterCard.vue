<template>
  <section class="card" aria-labelledby="codex-history-filter-title">
    <div class="flex items-center justify-between gap-4 border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div>
        <h2 id="codex-history-filter-title" class="text-lg font-semibold text-gray-900 dark:text-white">
          {{ t('admin.settings.codexHistoryFilter.title') }}
        </h2>
        <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
          {{ t('admin.settings.codexHistoryFilter.description') }}
        </p>
      </div>
      <Toggle
        :model-value="status?.enabled ?? false"
        :disabled="loading || saving || !status"
        :aria-label="t('admin.settings.codexHistoryFilter.enabled')"
        @update:model-value="save"
      />
    </div>
    <div class="space-y-4 p-6" :aria-busy="loading || saving">
      <p v-if="error" role="alert" class="text-sm text-red-600 dark:text-red-400">{{ error }}</p>
      <p v-if="loading && !status" role="status" class="text-sm text-gray-500">{{ t('common.loading') }}</p>
      <template v-if="status">
        <p class="text-sm font-medium text-gray-900 dark:text-white" role="status">
          {{ saving ? t('admin.settings.codexHistoryFilter.saving') : t(status.enabled ? 'admin.settings.codexHistoryFilter.active' : 'admin.settings.codexHistoryFilter.inactive') }}
        </p>
        <p class="text-sm text-gray-600 dark:text-gray-400">{{ t('admin.settings.codexHistoryFilter.behavior') }}</p>
        <p class="text-sm text-gray-600 dark:text-gray-400">{{ t('admin.settings.codexHistoryFilter.boundaries') }}</p>
        <dl class="grid grid-cols-2 gap-4 md:grid-cols-5">
          <div v-for="metric in metrics" :key="metric.key" class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
            <dt class="text-xs text-gray-500 dark:text-gray-400">{{ t(`admin.settings.codexHistoryFilter.${metric.label}`) }}</dt>
            <dd class="mt-1 text-xl font-semibold text-gray-900 dark:text-white">{{ (status.stats[metric.key] ?? 0).toLocaleString() }}</dd>
          </div>
        </dl>
        <dl class="grid gap-2 text-sm sm:grid-cols-2">
          <div><dt class="inline text-gray-500">{{ t('admin.settings.codexHistoryFilter.lastFiltered') }}: </dt><dd class="inline">{{ lastFiltered }}</dd></div>
          <div><dt class="inline text-gray-500">{{ t('admin.settings.codexHistoryFilter.lastStatus') }}: </dt><dd class="inline">{{ status.stats.last_upstream_status ?? t('admin.settings.codexHistoryFilter.none') }}</dd></div>
          <div><dt class="inline text-gray-500">{{ t('admin.settings.codexHistoryFilter.httpErrors') }}: </dt><dd class="inline">{{ status.stats.upstream_http_errors }}</dd></div>
          <div><dt class="inline text-gray-500">{{ t('admin.settings.codexHistoryFilter.transportErrors') }}: </dt><dd class="inline">{{ status.stats.upstream_errors }}</dd></div>
        </dl>
        <p class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.settings.codexHistoryFilter.statsHint') }}</p>
      </template>
      <button type="button" class="btn btn-secondary btn-sm" :disabled="loading || saving" @click="load">
        {{ t('admin.settings.codexHistoryFilter.refresh') }}
      </button>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'
import { getCodexHistoryFilter, updateCodexHistoryFilter, type CodexHistoryFilterStatus } from '@/api/admin/settings'

const { t } = useI18n()
const status = ref<CodexHistoryFilterStatus | null>(null)
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const metrics = [
  { key: 'filtered_requests', label: 'filteredRequests' },
  { key: 'removed_reasoning_items', label: 'removedReasoning' },
  { key: 'removed_item_ids', label: 'removedIds' },
  { key: 'normalized_agent_text_parts', label: 'normalizedAgentText' },
  { key: 'blocked_requests', label: 'blockedRequests' },
] as const
const lastFiltered = computed(() => status.value?.stats.last_filtered_at
  ? new Date(status.value.stats.last_filtered_at).toLocaleString()
  : t('admin.settings.codexHistoryFilter.none'))

async function load() {
  if (loading.value || saving.value) return
  loading.value = true
  error.value = ''
  try {
    status.value = await getCodexHistoryFilter()
  } catch {
    status.value = null
    error.value = t('admin.settings.codexHistoryFilter.loadFailed')
  } finally {
    loading.value = false
  }
}

async function save(enabled: boolean) {
  if (!status.value || loading.value || saving.value) return
  saving.value = true
  error.value = ''
  try {
    status.value = await updateCodexHistoryFilter(enabled)
  } catch {
    try {
      status.value = await getCodexHistoryFilter()
    } catch {
      status.value = null
    }
    error.value = t('admin.settings.codexHistoryFilter.saveFailed')
  } finally {
    saving.value = false
  }
}

onMounted(load)
</script>
