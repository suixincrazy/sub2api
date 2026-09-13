<template>
  <Teleport to="body">
    <div v-if="show && account && position">
      <div class="fixed inset-0 z-[9998]" @click="emit('close')"></div>
      <div
        class="fixed z-[9999] w-52 overflow-hidden rounded-xl bg-white shadow-lg ring-1 ring-black/5 dark:bg-dark-800"
        :style="{ top: `${position.top}px`, left: `${position.left}px` }"
        @click.stop
      >
        <div class="py-1">
          <button class="menu-item" @click="run('test')">
            <Icon name="play" size="sm" class="text-emerald-500" />
            {{ t('admin.accounts.testConnection') }}
          </button>
          <button class="menu-item" @click="run('refresh')">
            <Icon name="refresh" size="sm" class="text-violet-500" />
            {{ t('admin.accounts.refreshToken') }}
          </button>
          <button class="menu-item" @click="run('usage')">
            <Icon name="chart" size="sm" class="text-indigo-500" />
            {{ t('admin.accounts.viewStats') }}
          </button>
          <div class="my-1 border-t border-gray-100 dark:border-dark-700"></div>
          <button class="menu-item text-emerald-600" @click="run('clear-error')">
            <Icon name="sync" size="sm" />
            {{ t('myResources.actions.clearError') }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>

<script setup lang="ts">
import { onUnmounted, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import Icon from '@/components/icons/Icon.vue'
import type { Account } from '@/types'

const props = defineProps<{
  show: boolean
  account: Account | null
  position: { top: number; left: number } | null
}>()

const emit = defineEmits<{
  close: []
  test: [account: Account]
  refresh: [account: Account]
  usage: [account: Account]
  'clear-error': [account: Account]
}>()

const { t } = useI18n()

const run = (action: 'test' | 'refresh' | 'usage' | 'clear-error') => {
  if (!props.account) return
  switch (action) {
    case 'test':
      emit('test', props.account)
      break
    case 'refresh':
      emit('refresh', props.account)
      break
    case 'usage':
      emit('usage', props.account)
      break
    case 'clear-error':
      emit('clear-error', props.account)
      break
  }
  emit('close')
}

const handleKeydown = (event: KeyboardEvent) => {
  if (event.key === 'Escape') emit('close')
}

watch(() => props.show, visible => {
  if (visible) window.addEventListener('keydown', handleKeydown)
  else window.removeEventListener('keydown', handleKeydown)
}, { immediate: true })

onUnmounted(() => window.removeEventListener('keydown', handleKeydown))
</script>

<style scoped>
.menu-item {
  @apply flex w-full items-center gap-2 px-4 py-2 text-sm hover:bg-gray-100 dark:hover:bg-dark-700;
}
</style>
