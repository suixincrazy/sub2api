<template>
  <div class="border-t border-gray-200 pt-4 dark:border-dark-600" data-testid="codex-history-filter-setting">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label class="input-label mb-0">{{ t('admin.accounts.openai.historyFilter') }}</label>
        <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.accounts.openai.historyFilterDesc') }}
        </p>
      </div>
      <Toggle
        data-testid="codex-history-filter-toggle"
        :aria-label="t('admin.accounts.openai.historyFilter')"
        :model-value="effectiveEnabled"
        :disabled="modelValue === null && gatewayEnabled === null"
        class="disabled:cursor-wait disabled:opacity-50"
        @update:model-value="$emit('update:modelValue', $event)"
      />
    </div>
    <p v-if="modelValue === null" class="mt-2 text-xs text-gray-500 dark:text-gray-400">
      {{ loadError ? t('admin.accounts.openai.historyFilterLoadError') : gatewayEnabled === null ? t('common.loading') : t('admin.accounts.openai.historyFilterInherited', { state: t(gatewayEnabled ? 'common.enabled' : 'common.disabled') }) }}
      <button v-if="loadError" type="button" class="ml-2 text-primary-600" @click="loadGatewayDefault">
        {{ t('common.refresh') }}
      </button>
    </p>
    <button
      v-else
      type="button"
      data-testid="codex-history-filter-inherit"
      class="mt-2 text-xs text-primary-600"
      @click="$emit('update:modelValue', null)"
    >
      {{ t('admin.accounts.openai.historyFilterInherit') }}
    </button>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Toggle from '@/components/common/Toggle.vue'

const props = defineProps<{ modelValue: boolean | null }>()
defineEmits<{ 'update:modelValue': [value: boolean | null] }>()
const { t } = useI18n()
const gatewayEnabled = ref<boolean | null>(null)
const loadError = ref(false)
const effectiveEnabled = computed(() => props.modelValue ?? gatewayEnabled.value ?? false)

async function loadGatewayDefault() {
  loadError.value = false
  try {
    gatewayEnabled.value = (await adminAPI.settings.getCodexHistoryFilter()).enabled
  } catch {
    loadError.value = true
  }
}

onMounted(loadGatewayDefault)
</script>
