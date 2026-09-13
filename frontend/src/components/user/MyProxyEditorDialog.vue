<template>
  <BaseDialog
    :show="show"
    :title="proxy ? mr('proxyEditor.editTitle') : mr('proxyEditor.createTitle')"
    width="normal"
    @close="emit('close')"
  >
    <form id="my-proxy-editor-form" class="space-y-5" @submit.prevent="submit">
      <div v-if="!proxy" class="mb-6 flex flex-wrap items-center border-b border-gray-200 dark:border-dark-600">
        <button
          v-for="option in creationModeOptions"
          :key="option.value"
          type="button"
          :data-test="`proxy-create-mode-${option.value}`"
          :class="[
            '-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors',
            createMode === option.value
              ? 'border-primary-500 text-primary-600 dark:text-primary-400'
              : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-300',
          ]"
          @click="createMode = option.value"
        >
          <Icon :name="option.value === 'standard' ? 'plus' : 'upload'" size="sm" class="mr-1.5 inline" />
          {{ option.label }}
        </button>
      </div>

      <template v-if="createMode === 'standard'">
        <div>
          <label class="input-label">{{ mr('fields.name') }}</label>
          <input
            v-model.trim="form.name"
            class="input"
            data-test="proxy-name"
            :placeholder="mr('proxyEditor.namePlaceholder')"
            :required="inputMode !== 'config'"
          />
        </div>

        <div v-if="!proxy">
          <label class="input-label">{{ mr('proxyEditor.inputType') }}</label>
          <Select
            v-model="inputMode"
            :options="inputModeOptions"
            :searchable="false"
            data-test="proxy-input-mode-selector"
          />
        </div>

        <template v-if="inputMode === 'direct'">
          <div>
            <label class="input-label">{{ mr('fields.protocol') }}</label>
            <Select v-model="form.protocol" :options="standardProtocolOptions" :searchable="false" />
          </div>
          <div class="grid grid-cols-1 gap-3 sm:grid-cols-[minmax(0,1fr)_7.5rem]">
            <div class="min-w-0">
              <label class="input-label">{{ mr('fields.host') }}</label>
              <input v-model.trim="form.host" class="input" data-test="proxy-host" required />
            </div>
            <div>
              <label class="input-label">{{ mr('fields.port') }}</label>
              <input v-model.number="form.port" class="input" type="number" min="1" max="65535" required />
            </div>
          </div>
          <div>
            <label class="input-label">{{ mr('fields.username') }}</label>
            <input
              v-model.trim="form.username"
              class="input"
              autocomplete="off"
              :placeholder="proxy ? mr('fields.leaveBlankToKeep') : ''"
              @input="usernameDirty = true"
            />
          </div>
          <div>
            <label class="input-label">{{ mr('fields.password') }}</label>
            <div class="relative">
              <input
                v-model="form.password"
                class="input pr-10"
                :type="passwordVisible ? 'text' : 'password'"
                autocomplete="new-password"
                :placeholder="proxy ? mr('fields.leaveBlankToKeep') : ''"
                @input="passwordDirty = true"
              />
              <button
                type="button"
                class="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
                :title="passwordVisible ? t('admin.proxies.hideCredentials') : t('admin.proxies.showCredentials')"
                @click="passwordVisible = !passwordVisible"
              >
                <Icon :name="passwordVisible ? 'eyeOff' : 'eye'" size="md" />
              </button>
            </div>
          </div>
        </template>

        <template v-else-if="inputMode === 'xray'">
          <div v-if="proxy" class="grid grid-cols-1 gap-3 sm:grid-cols-[minmax(0,1fr)_7.5rem]">
            <div class="min-w-0">
              <label class="input-label">{{ mr('fields.host') }}</label>
              <input v-model.trim="form.host" class="input" required />
            </div>
            <div>
              <label class="input-label">{{ mr('fields.port') }}</label>
              <input v-model.number="form.port" class="input" type="number" min="1" max="65535" required />
            </div>
          </div>
          <div>
            <label class="input-label">{{ mr('proxyEditor.xrayShareLink') }}</label>
            <textarea
              v-model.trim="form.nodeContent"
              class="input min-h-28 break-all font-mono text-xs"
              :placeholder="proxy ? mr('proxyEditor.xrayEditPlaceholder') : mr('proxyEditor.xrayPlaceholder')"
              :required="!proxy"
              spellcheck="false"
              @input="nodeContentDirty = true"
            ></textarea>
          </div>
        </template>

        <template v-else-if="inputMode === 'source'">
          <div>
            <label class="input-label">{{ mr('fields.subscriptionUrl') }}</label>
            <input v-model.trim="form.subscriptionUrl" class="input" type="url" placeholder="https://example.com/sub" required />
          </div>
          <div>
            <label class="input-label">{{ mr('fields.refreshIntervalMinutes') }}</label>
            <input v-model.number="form.refreshIntervalMinutes" class="input" type="number" min="5" required />
          </div>
        </template>

        <template v-else>
          <div>
            <label class="input-label">{{ mr('proxyEditor.nodeConfig') }}</label>
            <textarea
              v-model="form.nodeContent"
              class="input min-h-44 break-all font-mono text-xs"
              :placeholder="mr('proxyEditor.configPlaceholder')"
              required
              spellcheck="false"
            ></textarea>
          </div>
        </template>

        <div v-if="proxy && (inputMode === 'direct' || inputMode === 'xray')">
          <label class="input-label">{{ mr('fields.status') }}</label>
          <Select v-model="form.status" :options="statusOptions" :searchable="false" />
        </div>
      </template>

      <template v-else>
        <div>
          <label class="input-label">{{ mr('proxyEditor.batchProxyList') }}</label>
          <textarea
            v-model="batchInput"
            class="input min-h-48 break-all font-mono text-xs"
            data-test="proxy-batch-input"
            :placeholder="mr('proxyEditor.batchPlaceholder')"
            required
            spellcheck="false"
          ></textarea>
        </div>
        <div v-if="batchStats.total > 0" class="rounded-lg bg-gray-50 p-4 dark:bg-dark-700">
          <div class="grid grid-cols-2 gap-2 text-center sm:grid-cols-4">
            <div v-for="stat in batchStatsDisplay" :key="stat.key" :data-test="`proxy-stat-${stat.key}`" class="min-w-0 px-1">
              <div class="truncate text-xs text-gray-500 dark:text-dark-300">{{ stat.label }}</div>
              <div :class="['mt-1 text-base font-semibold', stat.class]">{{ stat.value }}</div>
            </div>
          </div>
        </div>
      </template>

      <label class="flex cursor-pointer items-center gap-2">
        <input
          v-model="form.isPublic"
          type="checkbox"
          class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
          data-test="proxy-is-public"
        />
        <span class="text-sm text-gray-700 dark:text-gray-200">{{ mr('fields.publicForAllUsers') }}</span>
      </label>

      <template v-if="createMode === 'standard' && (inputMode === 'direct' || inputMode === 'xray')">
        <div>
          <label class="input-label">{{ mr('fields.expiresAt') }}</label>
          <div class="mb-2 flex flex-wrap gap-2">
            <button
              v-for="days in EXPIRY_PRESETS"
              :key="days"
              type="button"
              class="btn btn-sm"
              :class="expiresDays === days ? 'btn-primary' : 'btn-secondary'"
              @click="expiresDays = days"
            >
              {{ t('admin.proxies.nDays', { days }) }}
            </button>
          </div>
          <input
            v-model.number="expiresDays"
            type="number"
            min="0"
            class="input mb-2"
            :placeholder="t('admin.proxies.expiryDaysPlaceholder')"
          />
          <input v-model="form.expiresAt" class="input" type="date" />
        </div>
        <div>
          <label class="input-label">{{ mr('fields.expiryWarnDays') }}</label>
          <input v-model.number="form.expiryWarnDays" class="input" type="number" min="0" />
        </div>
        <div>
          <label class="input-label">{{ mr('fields.fallbackMode') }}</label>
          <Select v-model="form.fallbackMode" :options="fallbackOptions" :searchable="false" />
        </div>
        <div v-if="form.fallbackMode === 'proxy'">
          <label class="input-label">{{ mr('proxyEditor.backupProxy') }}</label>
          <Select v-model="form.backupProxyId" :options="backupProxyOptions" searchable />
        </div>
      </template>

      <div v-if="errorMessage" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">
        {{ errorMessage }}
      </div>
    </form>

    <template #footer>
      <button type="button" class="btn btn-secondary" :disabled="submitting" @click="emit('close')">
        {{ t('common.cancel') }}
      </button>
      <button type="submit" form="my-proxy-editor-form" class="btn btn-primary" :disabled="submitting || !canSubmit">
        {{ submitLabel }}
      </button>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { myResourcesApi, type ResourceItem } from '@/api/myResources'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'

type CreateMode = 'standard' | 'batch'
type InputMode = 'direct' | 'xray' | 'source' | 'config'
const EXPIRY_PRESETS = [30, 90, 180, 365] as const

const props = withDefaults(defineProps<{
  show: boolean
  proxy?: ResourceItem | null
  initialMode?: CreateMode
}>(), {
  proxy: null,
  initialMode: 'standard',
})

const emit = defineEmits<{
  close: []
  saved: []
}>()

const { t } = useI18n()
const appStore = useAppStore()
const mr = (key: string, params?: Record<string, unknown>) => t(`myResources.${key}`, params || {})

const createMode = ref<CreateMode>('standard')
const inputMode = ref<InputMode>('direct')
const submitting = ref(false)
const errorMessage = ref('')
const batchInput = ref('')
const usernameDirty = ref(false)
const passwordDirty = ref(false)
const nodeContentDirty = ref(false)
const passwordVisible = ref(false)
const availableProxies = ref<ResourceItem[]>([])

const form = reactive({
  name: '',
  protocol: 'socks5',
  host: '',
  port: 1080,
  username: '',
  password: '',
  status: 'active',
  fallbackMode: 'none',
  backupProxyId: 0,
  expiresAt: '',
  expiryWarnDays: 7,
  nodeContent: '',
  subscriptionUrl: '',
  refreshIntervalMinutes: 1440,
  isPublic: false,
})

const creationModeOptions = computed(() => [
  { value: 'standard' as const, label: mr('proxyEditor.standardCreate') },
  { value: 'batch' as const, label: mr('proxyEditor.batchCreate') },
])
const inputModeOptions = computed<Array<{ value: InputMode; label: string }>>(() => [
  { value: 'direct', label: mr('proxyEditor.standardProxy') },
  { value: 'xray', label: mr('proxyEditor.xrayShare') },
  { value: 'source', label: mr('proxyEditor.providerSubscription') },
  { value: 'config', label: mr('proxyEditor.nodeConfig') },
])
const standardProtocolOptions = computed<SelectOption[]>(() => [
  { value: 'http', label: 'HTTP' },
  { value: 'https', label: 'HTTPS' },
  { value: 'socks5', label: 'SOCKS5' },
  { value: 'socks5h', label: 'SOCKS5H' },
])
const statusOptions = computed<SelectOption[]>(() => ['active', 'inactive', 'disabled'].map(value => ({
  value,
  label: mr(`states.${value}`),
})))
const fallbackOptions = computed<SelectOption[]>(() => [
  { value: 'none', label: mr('proxyEditor.noFallback') },
  { value: 'proxy', label: mr('proxyEditor.fallbackProxy') },
  { value: 'direct', label: mr('proxyEditor.fallbackDirect') },
])
const backupProxyOptions = computed<SelectOption[]>(() => [
  { value: 0, label: mr('proxyEditor.selectBackupProxy') },
  ...availableProxies.value
    .filter(item => Number(item.id) !== Number(props.proxy?.id) && (item.is_owned === true || item.is_public === true))
    .map(item => ({ value: Number(item.id), label: proxyOptionLabel(item) })),
])

const batchStats = computed(() => {
  const lines = batchInput.value.split(/\r?\n/).map(line => line.trim()).filter(Boolean)
  const seen = new Set<string>()
  let valid = 0
  let invalid = 0
  let duplicate = 0
  for (const line of lines) {
    const normalized = line.toLowerCase()
    if (!/^(https?|socks(?:5h?)?|vmess|vless|trojan|ss|hysteria|hy2|hysteria2|tuic|anytls|naive(?:\+https|\+quic)?|wireguard|wg):\/\//.test(normalized)) {
      invalid++
      continue
    }
    if (seen.has(line)) {
      duplicate++
      continue
    }
    seen.add(line)
    valid++
  }
  return { total: lines.length, valid, invalid, duplicate }
})
const batchStatsDisplay = computed(() => [
  { key: 'total', label: mr('proxyEditor.totalLines'), value: batchStats.value.total, class: 'text-gray-900 dark:text-white' },
  { key: 'valid', label: mr('proxyEditor.validLines'), value: batchStats.value.valid, class: 'text-emerald-600' },
  { key: 'invalid', label: mr('proxyEditor.invalidLines'), value: batchStats.value.invalid, class: 'text-red-600' },
  { key: 'duplicate', label: mr('proxyEditor.duplicateLines'), value: batchStats.value.duplicate, class: 'text-amber-600' },
])
const canSubmit = computed(() => {
  if (props.proxy) return Boolean(form.name && form.host && form.port)
  if (createMode.value === 'batch') return batchStats.value.valid > 0
  if (!form.name && inputMode.value !== 'config') return false
  if (inputMode.value === 'direct') return Boolean(form.host && form.port)
  if (inputMode.value === 'source') return Boolean(form.subscriptionUrl)
  return Boolean(form.nodeContent.trim())
})
const submitLabel = computed(() => {
  if (submitting.value) return t('common.loading')
  if (props.proxy) return t('common.save')
  return createMode.value === 'batch' ? mr('proxyEditor.batchCreate') : t('common.create')
})

const expiryBaseDate = computed(() => toDateInput(props.proxy?.created_at) || toDateInput(new Date()))
const expiresDays = computed<number | null>({
  get: () => daysBetween(expiryBaseDate.value, form.expiresAt),
  set: days => {
    form.expiresAt = addDays(expiryBaseDate.value, days)
  },
})

function proxyOptionLabel(item: ResourceItem): string {
  const base = `${String(item.name || '-')} / ${String(item.protocol || '-').toUpperCase()}`
  if (item.details_hidden) return `${base} / ${mr('states.publicDetailsHidden')}`
  return `${base} / ${String(item.host || '-')}:${Number(item.port || 0)}`
}

function toDateInput(value: unknown): string {
  if (!value) return ''
  const numeric = typeof value === 'number' ? value : Number.NaN
  const date = new Date(Number.isFinite(numeric) && numeric < 1_000_000_000_000 ? numeric * 1000 : String(value))
  if (Number.isNaN(date.getTime())) return ''
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000)
  return local.toISOString().slice(0, 10)
}

function daysBetween(base: string, value: string): number | null {
  if (!base || !value) return null
  const difference = new Date(`${value}T00:00:00`).getTime() - new Date(`${base}T00:00:00`).getTime()
  return Math.max(0, Math.round(difference / 86_400_000))
}

function addDays(base: string, days: number | null): string {
  if (days == null || !Number.isFinite(Number(days)) || Number(days) <= 0) return ''
  const date = new Date(`${base}T00:00:00`)
  date.setDate(date.getDate() + Number(days))
  return toDateInput(date)
}

function resetForm(): void {
  createMode.value = props.initialMode
  inputMode.value = props.proxy?.kind === 'xray' ? 'xray' : 'direct'
  errorMessage.value = ''
  batchInput.value = ''
  usernameDirty.value = false
  passwordDirty.value = false
  nodeContentDirty.value = false
  passwordVisible.value = false
  form.name = String(props.proxy?.name || '')
  form.protocol = String(props.proxy?.protocol || 'socks5')
  form.host = String(props.proxy?.host || '')
  form.port = Number(props.proxy?.port || 1080)
  form.username = ''
  form.password = ''
  form.status = String(props.proxy?.status || 'active')
  form.fallbackMode = String(props.proxy?.fallback_mode || 'none')
  form.backupProxyId = Number(props.proxy?.backup_proxy_id || 0)
  form.expiresAt = toDateInput(props.proxy?.expires_at)
  form.expiryWarnDays = Number(props.proxy?.expiry_warn_days ?? 7)
  form.nodeContent = ''
  form.subscriptionUrl = ''
  form.refreshIntervalMinutes = 1440
  form.isPublic = Boolean(props.proxy?.is_public)
}

async function loadBackupProxies(): Promise<void> {
  try {
    const result = await myResourcesApi.proxies.list({ page: 1, page_size: 1000 })
    availableProxies.value = result.items || []
  } catch {
    availableProxies.value = []
  }
}

async function importContent(content: string, namePrefix?: string): Promise<number> {
  const result = await myResourcesApi.proxies.importNodes({
    name_prefix: namePrefix || undefined,
    content,
    is_public: form.isPublic,
  }) as ResourceItem
  const created = Array.isArray(result?.created) ? result.created as ResourceItem[] : []
  const errors = Array.isArray(result?.errors) ? result.errors.map(String) : []
  if (created.length === 1 && namePrefix?.trim() && String(created[0].name || '') !== namePrefix.trim()) {
    await myResourcesApi.proxies.update(Number(created[0].id), { name: namePrefix.trim() })
  }
  if (errors.length > 0) {
    errorMessage.value = errors.slice(0, 3).join('\n')
  }
  return created.length
}

async function submit(): Promise<void> {
  if (!canSubmit.value || submitting.value) return
  submitting.value = true
  errorMessage.value = ''
  try {
    let importedCount = 0
    if (!props.proxy && createMode.value === 'batch') {
      importedCount = await importContent(batchInput.value, 'node')
    } else if (!props.proxy && inputMode.value === 'xray') {
      importedCount = await importContent(form.nodeContent, form.name)
    } else if (!props.proxy && inputMode.value === 'config') {
      importedCount = await importContent(form.nodeContent, form.name)
    } else if (!props.proxy && inputMode.value === 'source') {
      const source = await myResourcesApi.proxies.sources.create({
        name: form.name,
        subscription_url: form.subscriptionUrl,
        refresh_interval_minutes: Number(form.refreshIntervalMinutes),
        is_public: form.isPublic,
      })
      const result = await myResourcesApi.proxies.sources.sync(Number(source.id)) as ResourceItem
      importedCount = Number(result?.created_count || result?.imported_count || 0)
    } else {
      const payload: ResourceItem = {
        name: form.name,
        kind: inputMode.value === 'xray' ? 'xray' : 'standard',
        protocol: form.protocol,
        host: form.host,
        port: Number(form.port),
        status: form.status,
        fallback_mode: form.fallbackMode,
        backup_proxy_id: form.fallbackMode === 'proxy' && form.backupProxyId > 0 ? Number(form.backupProxyId) : null,
        expires_at: form.expiresAt ? new Date(form.expiresAt).toISOString() : null,
        expiry_warn_days: Number(form.expiryWarnDays),
        is_public: form.isPublic,
      }
      if (!props.proxy || usernameDirty.value) payload.username = form.username
      if (!props.proxy || passwordDirty.value) payload.password = form.password
      if (inputMode.value === 'xray' && nodeContentDirty.value) payload.extra = { raw: form.nodeContent }
      if (props.proxy) await myResourcesApi.proxies.update(Number(props.proxy.id), payload)
      else await myResourcesApi.proxies.create(payload)
    }

    if (importedCount > 0) appStore.showSuccess(mr('messages.importedProxies', { count: importedCount }))
    else if (!errorMessage.value) appStore.showSuccess(mr('messages.proxySaved'))
    if (errorMessage.value && importedCount === 0) return
    emit('saved')
  } catch (error) {
    errorMessage.value = extractApiErrorMessage(error, mr('messages.saveFailed'))
  } finally {
    submitting.value = false
  }
}

watch(
  () => props.show,
  show => {
    if (!show) return
    resetForm()
    void loadBackupProxies()
  },
  { immediate: true },
)
</script>
