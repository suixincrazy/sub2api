<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap-reverse items-start justify-between gap-3">
          <div class="flex w-full min-w-0 basis-full flex-wrap items-center gap-3 lg:w-auto lg:basis-auto lg:flex-1">
            <SearchInput
              v-model="params.search"
              :placeholder="t('admin.accounts.searchAccounts')"
              class="w-full sm:w-64"
              @search="reload"
            />
            <Select v-model="params.platform" class="w-full sm:w-40" :options="platformFilterOptions" @change="reload" />
            <Select v-model="params.type" class="w-full sm:w-40" :options="typeFilterOptions" @change="reload" />
            <Select v-model="params.status" class="w-full sm:w-40" :options="statusFilterOptions" @change="reload" />
            <Select v-model="params.group_id" class="w-full sm:w-44" :options="groupFilterOptions" searchable @change="reload" />
          </div>

          <div class="flex w-full flex-wrap items-center justify-end gap-2 lg:w-auto">
            <button class="btn btn-secondary" :disabled="loading" :title="t('common.refresh')" @click="reload">
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>

            <div ref="autoRefreshDropdownRef" class="relative">
              <button
                class="btn btn-secondary px-2 md:px-3"
                :title="t('admin.accounts.autoRefresh')"
                @click="showAutoRefresh = !showAutoRefresh; showTools = false"
              >
                <Icon name="refresh" size="sm" :class="['md:mr-1.5', autoRefreshEnabled ? 'animate-spin' : '']" />
                <span class="hidden md:inline">
                  {{ autoRefreshEnabled ? t('admin.accounts.autoRefreshCountdown', { seconds: countdown }) : t('admin.accounts.autoRefresh') }}
                </span>
              </button>
              <div v-if="showAutoRefresh" class="absolute right-0 z-50 mt-2 w-56 rounded-lg border border-gray-200 bg-white shadow-lg dark:border-gray-700 dark:bg-gray-800">
                <div class="p-2">
                  <button class="account-option" @click="autoRefreshEnabled = !autoRefreshEnabled">
                    <span>{{ t('admin.accounts.enableAutoRefresh') }}</span>
                    <Icon v-if="autoRefreshEnabled" name="check" size="sm" class="text-primary-500" />
                  </button>
                  <div class="my-1 border-t border-gray-100 dark:border-gray-700"></div>
                  <button v-for="seconds in [10, 30, 60]" :key="seconds" class="account-option" @click="setAutoRefreshInterval(seconds)">
                    <span>{{ seconds }}s</span>
                    <Icon v-if="autoRefreshInterval === seconds" name="check" size="sm" class="text-primary-500" />
                  </button>
                </div>
              </div>
            </div>

            <div ref="accountToolsDropdownRef" class="relative">
              <button
                class="btn btn-secondary px-2 md:px-3"
                :title="t('admin.accounts.moreActions')"
                @click="showTools = !showTools; showAutoRefresh = false"
              >
                <Icon name="more" size="sm" class="md:mr-1.5" />
                <span class="hidden md:inline">{{ t('admin.accounts.moreActions') }}</span>
                <Icon name="chevronDown" size="xs" class="ml-1 hidden md:inline" />
              </button>
              <div v-if="showTools" class="absolute right-0 z-50 mt-2 w-[min(20rem,calc(100vw-2rem))] overflow-hidden rounded-lg border border-gray-200 bg-white shadow-xl dark:border-gray-700 dark:bg-gray-800">
                <div class="max-h-[70vh] overflow-y-auto p-2">
                  <div class="px-2 py-2 text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                    {{ t('admin.accounts.dataActions') }}
                  </div>
                  <button class="account-tools-menu-item" @click="openImport">
                    <span class="account-tools-menu-icon bg-emerald-50 text-emerald-600 dark:bg-emerald-900/30 dark:text-emerald-300"><Icon name="upload" size="sm" /></span>
                    <span class="flex-1 text-left">{{ t('admin.accounts.dataImport') }}</span>
                  </button>
                  <button class="account-tools-menu-item" @click="exportAccounts">
                    <span class="account-tools-menu-icon bg-violet-50 text-violet-600 dark:bg-violet-900/30 dark:text-violet-300"><Icon name="download" size="sm" /></span>
                    <span class="flex-1 text-left">{{ selectedIds.length ? t('admin.accounts.dataExportSelected') : t('admin.accounts.dataExport') }}</span>
                    <span v-if="selectedIds.length" class="rounded-full bg-primary-100 px-2 py-0.5 text-xs font-medium text-primary-700 dark:bg-primary-900/40 dark:text-primary-300">{{ selectedIds.length }}</span>
                  </button>
                  <button class="account-tools-menu-item" @click="openCodexSession">
                    <span class="account-tools-menu-icon bg-sky-50 text-sky-600 dark:bg-sky-900/30 dark:text-sky-300"><Icon name="upload" size="sm" /></span>
                    <span class="flex-1 text-left">{{ mr('actions.codexSession') }}</span>
                  </button>
                  <button class="account-tools-menu-item" @click="openCodexPAT">
                    <span class="account-tools-menu-icon bg-amber-50 text-amber-600 dark:bg-amber-900/30 dark:text-amber-300"><Icon name="key" size="sm" /></span>
                    <span class="flex-1 text-left">{{ mr('actions.codexPat') }}</span>
                  </button>

                  <div class="my-2 border-t border-gray-100 dark:border-gray-700"></div>
                  <div class="flex items-center justify-between px-2 py-2 text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                    <span>{{ t('admin.accounts.viewColumns') }}</span>
                    <Icon name="grid" size="sm" />
                  </div>
                  <button v-for="column in toggleableColumns" :key="column.key" class="account-option" @click="toggleColumn(column.key)">
                    <span class="truncate">{{ column.label }}</span>
                    <Icon v-if="isColumnVisible(column.key)" name="check" size="sm" class="text-primary-500" />
                  </button>
                </div>
              </div>
            </div>

            <button class="btn btn-primary" @click="showCreate = true">{{ t('admin.accounts.createAccount') }}</button>
          </div>
        </div>
      </template>

      <template #table>
        <div v-if="selectedIds.length" class="flex flex-wrap items-center justify-between gap-3 border-b border-primary-100 bg-primary-50 px-4 py-3 dark:border-primary-900/40 dark:bg-primary-900/20">
          <span class="text-sm font-medium text-primary-800 dark:text-primary-200">{{ t('admin.accounts.selectedCount', { count: selectedIds.length }) }}</span>
          <div class="flex flex-wrap gap-2">
            <button class="btn btn-sm btn-secondary" @click="batchRefresh">{{ mr('actions.refresh') }}</button>
            <button class="btn btn-sm btn-secondary" @click="batchClearErrors">{{ mr('actions.clearError') }}</button>
            <button class="btn btn-sm btn-secondary" @click="batchOpen = true">{{ mr('actions.batchEdit') }}</button>
            <button class="btn btn-sm btn-secondary" @click="selectedIds = []">{{ t('common.cancel') }}</button>
          </div>
        </div>

        <DataTable
          :columns="visibleColumns"
          :data="accounts"
          :loading="loading"
          row-key="id"
          server-side-sort
          default-sort-key="name"
          default-sort-order="asc"
          @sort="handleSort"
        >
          <template #header-select>
            <input type="checkbox" class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500" :checked="allSelected" @change="toggleAll" />
          </template>
          <template #cell-select="{ row }">
            <input type="checkbox" class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500" :checked="selectedIds.includes(Number(row.id))" @change="toggleSelected(Number(row.id))" />
          </template>
          <template #cell-name="{ row }">
            <div class="min-w-0 max-w-48">
              <div class="truncate font-medium text-gray-900 dark:text-white" :title="row.name">{{ row.name }}</div>
              <div v-if="credentialEmail(row)" class="max-w-48 truncate text-xs text-gray-500">{{ credentialEmail(row) }}</div>
            </div>
          </template>
          <template #cell-platform_type="{ row }">
            <PlatformTypeBadge :platform="row.platform" :type="row.type" :plan-type="row.credentials?.plan_type" :privacy-mode="row.extra?.privacy_mode" :subscription-expires-at="row.credentials?.subscription_expires_at" />
          </template>
          <template #cell-capacity="{ row }"><AccountCapacityCell :account="row" /></template>
          <template #cell-status="{ row }"><AccountStatusIndicator :account="row" /></template>
          <template #cell-schedulable="{ row }">
            <button class="relative h-6 w-11 rounded-full transition-colors" :class="row.schedulable ? 'bg-primary-600' : 'bg-gray-300 dark:bg-dark-600'" :title="t('admin.accounts.columns.schedulable')" @click="toggleSchedulable(row)">
              <span class="absolute top-0.5 h-5 w-5 rounded-full bg-white shadow transition-transform" :class="row.schedulable ? 'left-5' : 'left-0.5'"></span>
            </button>
          </template>
          <template #cell-today_stats="{ row }"><span class="font-medium text-gray-900 dark:text-white">{{ row.today_request_count || 0 }}</span><span class="ml-1 text-xs text-gray-500">req</span></template>
          <template #cell-groups="{ row }"><div class="flex max-w-56 flex-wrap gap-1"><span v-for="group in row.groups || []" :key="group.id" class="badge badge-gray">{{ group.name }}</span><span v-if="!(row.groups || []).length">-</span></div></template>
          <template #cell-usage="{ row }">
            <div class="text-xs text-gray-500">
              <template v-if="cnUsageSummary(row).length">
                <div v-for="line in cnUsageSummary(row)" :key="line">{{ line }}</div>
              </template>
              <div v-else>{{ row.session_window_status || '-' }}</div>
              <div v-if="row.rate_limit_reset_at">{{ formatDate(row.rate_limit_reset_at) }}</div>
            </div>
          </template>
          <template #cell-proxy="{ row }"><div class="max-w-44"><div class="truncate">{{ row.proxy_name || '-' }}</div><div v-if="row.proxy_protocol" class="text-xs uppercase text-gray-500">{{ row.proxy_protocol }}</div></div></template>
          <template #cell-priority="{ row }">{{ row.priority ?? 0 }}</template>
          <template #cell-rate_multiplier="{ row }"><span class="font-mono text-sm">{{ Number(row.rate_multiplier ?? 1).toFixed(2) }}x</span></template>
          <template #cell-last_used_at="{ row }">{{ formatDate(row.last_used_at) }}</template>
          <template #cell-created_at="{ row }">{{ formatDate(row.created_at) }}</template>
          <template #cell-expires_at="{ row }">{{ formatDate(row.expires_at) }}</template>
          <template #cell-notes="{ row }"><span class="block max-w-48 truncate" :title="row.notes">{{ row.notes || '-' }}</span></template>
          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button class="row-action hover:text-primary-600 dark:hover:text-primary-400" @click="openEdit(row)"><Icon name="edit" size="sm" /><span>{{ t('common.edit') }}</span></button>
              <button class="row-action hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400" @click="askDelete(row)"><Icon name="trash" size="sm" /><span>{{ t('common.delete') }}</span></button>
              <button class="row-action hover:text-gray-900 dark:hover:text-white" @click="openRowMenu(row, $event)"><Icon name="more" size="sm" /><span>{{ t('common.more') }}</span></button>
            </div>
          </template>
        </DataTable>
      </template>

      <template #pagination>
        <Pagination v-if="pagination.total" :page="pagination.page" :total="pagination.total" :page-size="pagination.page_size" @update:page="changePage" @update:pageSize="changePageSize" />
      </template>
    </TablePageLayout>

    <CreateAccountModal
      scope="user"
      :show="showCreate"
      :proxies="proxies"
      :groups="groups"
      @close="showCreate = false"
      @created="handleCreated"
    />
    <EditAccountModal
      scope="user"
      :show="showEdit"
      :account="editingAccount"
      :proxies="proxies"
      :groups="groups"
      @close="closeEdit"
      @updated="handleUpdated"
    />
    <UserAccountActionMenu
      :show="rowMenu.show"
      :account="rowMenu.account"
      :position="rowMenu.position"
      @close="rowMenu.show = false"
      @test="testAccount"
      @refresh="refreshAccount"
      @usage="openAccountUsage"
      @clear-error="clearAccountError"
    />
    <AccountTestModal
      scope="user"
      :show="accountTestOpen"
      :account="accountTestTarget"
      @close="closeAccountTest"
    />

    <BaseDialog :show="textDialogOpen" :title="textDialogTitle" width="wide" @close="closeTextDialog">
      <form id="user-account-text-dialog" class="space-y-3" @submit.prevent="submitTextDialog">
        <textarea v-model="textDialogValue" class="input min-h-72 resize-y font-mono text-xs" spellcheck="false"></textarea>
        <div v-if="dialogError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ dialogError }}</div>
      </form>
      <template #footer>
        <button type="button" class="btn btn-secondary" :disabled="saving" @click="closeTextDialog">{{ t('common.cancel') }}</button>
        <button type="submit" form="user-account-text-dialog" class="btn btn-primary" :disabled="saving">{{ t('common.save') }}</button>
      </template>
    </BaseDialog>

    <BaseDialog :show="batchOpen" :title="mr('batch.editAccounts')" width="normal" @close="batchOpen = false">
      <form id="user-account-batch-edit" class="space-y-5" @submit.prevent="submitBatchEdit">
        <div><label class="input-label">{{ mr('fields.status') }}</label><Select v-model="batchForm.status" :options="accountStatusOptions" /></div>
        <div><label class="input-label">{{ mr('fields.priority') }}</label><input v-model.number="batchForm.priority" class="input" type="number" min="0" /></div>
        <div><label class="input-label">{{ mr('fields.notes') }}</label><textarea v-model="batchForm.notes" class="input min-h-24"></textarea></div>
      </form>
      <template #footer>
        <button type="button" class="btn btn-secondary" @click="batchOpen = false">{{ t('common.cancel') }}</button>
        <button type="submit" form="user-account-batch-edit" class="btn btn-primary" :disabled="saving">{{ mr('actions.apply') }}</button>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="deleteDialogOpen"
      :title="t('admin.accounts.deleteAccount')"
      :message="t('admin.accounts.deleteConfirm', { name: deletingAccount?.name || '' })"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      danger
      @confirm="confirmDelete"
      @cancel="deleteDialogOpen = false"
    />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { myResourcesApi, type ResourceItem } from '@/api/myResources'
import { AccountTestModal, CreateAccountModal, EditAccountModal } from '@/components/account'
import AccountCapacityCell from '@/components/account/AccountCapacityCell.vue'
import AccountStatusIndicator from '@/components/account/AccountStatusIndicator.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import SearchInput from '@/components/common/SearchInput.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import type { Column } from '@/components/common/types'
import Icon from '@/components/icons/Icon.vue'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import UserAccountActionMenu from '@/components/user/UserAccountActionMenu.vue'
import { useAppStore } from '@/stores/app'
import type { Account, AdminGroup, Proxy } from '@/types'
import { toUnixSeconds } from '@/utils/accountProxyScope'
import { extractApiErrorMessage } from '@/utils/apiError'
import { USER_ACCOUNT_STATUS_OPTIONS, USER_ACCOUNT_TYPE_OPTIONS } from '@/utils/userResourceOptions'

const { t } = useI18n()
const router = useRouter()
const appStore = useAppStore()
const mr = (key: string, values?: Record<string, unknown>) => t(`myResources.${key}`, values || {})

const loading = ref(false)
const saving = ref(false)
const accounts = ref<ResourceItem[]>([])
const groups = ref<AdminGroup[]>([])
const proxies = ref<Proxy[]>([])
const selectedIds = ref<number[]>([])
const params = reactive({ search: '', platform: '', type: '', status: '', group_id: '' as string | number, sort_by: 'name', sort_order: 'asc' })
const pagination = reactive({ page: 1, page_size: 20, total: 0, pages: 1 })
const hiddenColumns = ref(new Set<string>())

const showTools = ref(false)
const showAutoRefresh = ref(false)
const autoRefreshEnabled = ref(false)
const autoRefreshInterval = ref(30)
const countdown = ref(30)
const accountToolsDropdownRef = ref<HTMLElement | null>(null)
const autoRefreshDropdownRef = ref<HTMLElement | null>(null)
let timer: ReturnType<typeof setInterval> | undefined

const showCreate = ref(false)
const showEdit = ref(false)
const editingAccount = ref<Account | null>(null)
const accountTestOpen = ref(false)
const accountTestTarget = ref<Account | null>(null)
const deleteDialogOpen = ref(false)
const deletingAccount = ref<ResourceItem | null>(null)
const rowMenu = reactive<{ show: boolean; account: Account | null; position: { top: number; left: number } | null }>({ show: false, account: null, position: null })

const textDialogOpen = ref(false)
const textDialogMode = ref<'import' | 'codex-session' | 'codex-pat'>('import')
const textDialogValue = ref('')
const dialogError = ref('')
const textDialogTitle = computed(() => textDialogMode.value === 'import' ? mr('actions.import') : textDialogMode.value === 'codex-session' ? mr('actions.codexSession') : mr('actions.codexPat'))
const batchOpen = ref(false)
const batchForm = reactive({ status: 'active', priority: 50, notes: '' })

const allColumns = computed<Column[]>(() => [
  { key: 'select', label: '' },
  { key: 'name', label: t('admin.accounts.columns.name'), sortable: true },
  { key: 'platform_type', label: t('admin.accounts.columns.platformType') },
  { key: 'capacity', label: t('admin.accounts.columns.capacity') },
  { key: 'status', label: t('admin.accounts.columns.status'), sortable: true },
  { key: 'schedulable', label: t('admin.accounts.columns.schedulable'), sortable: true },
  { key: 'today_stats', label: t('admin.accounts.columns.todayStats') },
  { key: 'groups', label: t('admin.accounts.columns.groups') },
  { key: 'usage', label: t('admin.accounts.columns.usageWindows') },
  { key: 'proxy', label: t('admin.accounts.columns.proxy') },
  { key: 'priority', label: t('admin.accounts.columns.priority'), sortable: true },
  { key: 'rate_multiplier', label: t('admin.accounts.columns.billingRateMultiplier'), sortable: true },
  { key: 'last_used_at', label: t('admin.accounts.columns.lastUsed'), sortable: true },
  { key: 'created_at', label: t('admin.accounts.columns.createdAt'), sortable: true },
  { key: 'expires_at', label: t('admin.accounts.columns.expiresAt') },
  { key: 'notes', label: t('admin.accounts.columns.notes') },
  { key: 'actions', label: t('admin.accounts.columns.actions') },
])
const visibleColumns = computed(() => allColumns.value.filter(column => !hiddenColumns.value.has(column.key)))
const toggleableColumns = computed(() => allColumns.value.filter(column => !['select', 'actions'].includes(column.key)))
const allSelected = computed(() => accounts.value.length > 0 && accounts.value.every(row => selectedIds.value.includes(Number(row.id))))

const platformLabels: Record<string, string> = {
  anthropic: 'Anthropic',
  openai: 'OpenAI',
  gemini: 'Gemini',
  antigravity: 'Antigravity',
  grok: 'Grok',
  kimi: 'Kimi',
  zhipu: 'Zhipu GLM',
  deepseek: 'DeepSeek',
}
const platformOptions: SelectOption[] = Object.entries(platformLabels).map(([value, label]) => ({
  value,
  label,
}))
const platformFilterOptions = computed(() => [{ value: '', label: t('admin.accounts.allPlatforms') }, ...platformOptions])
const typeFilterOptions = computed(() => [{ value: '', label: t('admin.accounts.allTypes') }, ...USER_ACCOUNT_TYPE_OPTIONS.map(option => ({ value: option.value, label: accountTypeLabel(option.value) }))])
const statusFilterOptions = computed(() => [{ value: '', label: t('admin.accounts.allStatus') }, ...USER_ACCOUNT_STATUS_OPTIONS.map(option => ({ value: option.value, label: statusLabel(option.value) }))])
const groupFilterOptions = computed(() => [{ value: '', label: t('admin.accounts.allGroups') }, ...groups.value.map(group => ({ value: String(group.id), label: group.name }))])
const accountStatusOptions = computed(() => USER_ACCOUNT_STATUS_OPTIONS.map(option => ({ value: option.value, label: statusLabel(option.value) })))

const toAccount = (item: ResourceItem): Account => ({
  ...item,
  id: Number(item.id),
  owner_user_id: item.owner_user_id == null ? null : Number(item.owner_user_id),
  proxy_id: item.proxy_id == null ? null : Number(item.proxy_id),
  expires_at: toUnixSeconds(item.expires_at),
  group_ids: Array.isArray(item.group_ids) ? item.group_ids.map(Number) : (item.groups || []).map((group: ResourceItem) => Number(group.id)),
  credentials: item.credentials || {},
  credentials_status: item.credentials_status || {},
} as unknown as Account)

async function loadReferences() {
  const [groupPage, proxyPage] = await Promise.all([
    myResourcesApi.groups.list({ page: 1, page_size: 1000 }),
    myResourcesApi.proxies.list({ page: 1, page_size: 1000 }),
  ])
  groups.value = groupPage.items as unknown as AdminGroup[]
  proxies.value = proxyPage.items as unknown as Proxy[]
}

async function reload() {
  loading.value = true
  try {
    const result = await myResourcesApi.accounts.list({
      ...params,
      group_id: params.group_id || undefined,
      page: pagination.page,
      page_size: pagination.page_size,
    })
    accounts.value = result.items
    pagination.total = result.total
    pagination.pages = result.pages
    selectedIds.value = selectedIds.value.filter(id => accounts.value.some(row => Number(row.id) === id))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.loadAccountFailed')))
  } finally {
    loading.value = false
  }
}

async function openEdit(row: ResourceItem) {
  try {
    editingAccount.value = toAccount(await myResourcesApi.accounts.get(Number(row.id)))
    showEdit.value = true
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.loadAccountFailed')))
  }
}

function closeEdit() {
  showEdit.value = false
  editingAccount.value = null
}

async function handleCreated() {
  showCreate.value = false
  await Promise.all([loadReferences(), reload()])
}

async function handleUpdated() {
  closeEdit()
  await reload()
}

function askDelete(row: ResourceItem) {
  deletingAccount.value = row
  deleteDialogOpen.value = true
}

async function confirmDelete() {
  if (!deletingAccount.value) return
  try {
    await myResourcesApi.accounts.delete(Number(deletingAccount.value.id))
    deleteDialogOpen.value = false
    deletingAccount.value = null
    await reload()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.deleteFailed')))
  }
}

function openRowMenu(row: ResourceItem, event: MouseEvent) {
  const trigger = event.currentTarget as HTMLElement
  const rect = trigger.getBoundingClientRect()
  const menuWidth = 208
  const menuHeight = 190
  rowMenu.account = toAccount(row)
  rowMenu.position = {
    top: Math.max(8, Math.min(rect.bottom + 4, window.innerHeight - menuHeight - 8)),
    left: Math.max(8, Math.min(rect.right - menuWidth, window.innerWidth - menuWidth - 8)),
  }
  rowMenu.show = true
}

function testAccount(account: Account) {
  accountTestTarget.value = account
  accountTestOpen.value = true
}

function closeAccountTest() {
  accountTestOpen.value = false
  accountTestTarget.value = null
}

async function refreshAccount(account: Account | ResourceItem) {
  try {
    await myResourcesApi.accounts.refresh(Number(account.id))
    appStore.showSuccess(mr('messages.accountRefreshed'))
    await reload()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.refreshFailed')))
  }
}

async function clearAccountError(account: Account | ResourceItem) {
  try {
    await myResourcesApi.accounts.clearError(Number(account.id))
    appStore.showSuccess(mr('messages.accountRefreshed'))
    await reload()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.refreshFailed')))
  }
}

function openAccountUsage(account: Account) {
  void router.push({ path: '/my/usage/account-logs', query: { account_id: String(account.id) } })
}

async function toggleSchedulable(row: ResourceItem) {
  try {
    const next = row.schedulable !== true
    await myResourcesApi.accounts.setSchedulable(Number(row.id), next)
    row.schedulable = next
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.scheduleUpdateFailed')))
  }
}

async function runSelected(action: (id: number) => Promise<unknown>) {
  const results = await Promise.allSettled(selectedIds.value.map(action))
  const failed = results.filter(result => result.status === 'rejected').length
  if (failed) appStore.showError(mr('messages.batchPartialFailed', { count: failed }))
  else appStore.showSuccess(mr('messages.batchCompleted'))
  await reload()
}

async function batchRefresh() { await runSelected(id => myResourcesApi.accounts.refresh(id)) }
async function batchClearErrors() { await runSelected(id => myResourcesApi.accounts.clearError(id)) }

async function submitBatchEdit() {
  saving.value = true
  try {
    await myResourcesApi.accounts.batchUpdate(selectedIds.value, {
      status: batchForm.status,
      priority: Number(batchForm.priority),
      notes: batchForm.notes,
    })
    batchOpen.value = false
    selectedIds.value = []
    await reload()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.batchFailed')))
  } finally {
    saving.value = false
  }
}

function openImport() {
  textDialogMode.value = 'import'
  textDialogValue.value = JSON.stringify({ accounts: [], proxies: [] }, null, 2)
  openTextDialog()
}

function openCodexSession() {
  textDialogMode.value = 'codex-session'
  textDialogValue.value = ''
  openTextDialog()
}

function openCodexPAT() {
  textDialogMode.value = 'codex-pat'
  textDialogValue.value = ''
  openTextDialog()
}

function openTextDialog() {
  dialogError.value = ''
  textDialogOpen.value = true
  showTools.value = false
}

function closeTextDialog() {
  textDialogOpen.value = false
  dialogError.value = ''
}

async function submitTextDialog() {
  saving.value = true
  dialogError.value = ''
  try {
    if (textDialogMode.value === 'import') await myResourcesApi.accounts.import(JSON.parse(textDialogValue.value))
    else if (textDialogMode.value === 'codex-session') await myResourcesApi.accounts.importCodexSessions({ content: textDialogValue.value })
    else await myResourcesApi.accounts.importCodexPAT({ access_token: textDialogValue.value })
    closeTextDialog()
    await reload()
  } catch (error) {
    dialogError.value = extractApiErrorMessage(error, mr('messages.importFailed'))
  } finally {
    saving.value = false
  }
}

async function exportAccounts() {
  try {
    const data = await myResourcesApi.accounts.export({ ids: selectedIds.value, include_proxies: true })
    const blob = new Blob([JSON.stringify(data, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = 'my-accounts.json'
    link.click()
    URL.revokeObjectURL(url)
    showTools.value = false
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.exportFailed')))
  }
}

function handleSort(key: string, order: 'asc' | 'desc') {
  params.sort_by = key === 'platform_type' ? 'platform' : key
  params.sort_order = order
  void reload()
}

function toggleSelected(id: number) {
  selectedIds.value = selectedIds.value.includes(id) ? selectedIds.value.filter(value => value !== id) : [...selectedIds.value, id]
}

function toggleAll() {
  selectedIds.value = allSelected.value ? [] : accounts.value.map(row => Number(row.id))
}

function isColumnVisible(key: string) { return !hiddenColumns.value.has(key) }
function toggleColumn(key: string) {
  const next = new Set(hiddenColumns.value)
  next.has(key) ? next.delete(key) : next.add(key)
  hiddenColumns.value = next
  localStorage.setItem('my-account-hidden-columns', JSON.stringify([...next]))
}

function changePage(page: number) { pagination.page = page; void reload() }
function changePageSize(size: number) { pagination.page_size = size; pagination.page = 1; void reload() }
function setAutoRefreshInterval(seconds: number) { autoRefreshInterval.value = seconds; countdown.value = seconds; showAutoRefresh.value = false }
function statusLabel(status: unknown) { return t(`admin.accounts.status.${String(status || 'inactive')}`) }
function accountTypeLabel(type: unknown) {
  const value = String(type || '')
  if (value === 'oauth') return 'OAuth'
  if (value === 'setup-token') return 'Setup Token'
  if (value === 'apikey') return 'API Key'
  if (value === 'service_account') return mr('fields.serviceAccount')
  if (value === 'bedrock') return 'Bedrock'
  if (value === 'upstream') return 'Upstream'
  return value
}
function credentialEmail(row: ResourceItem) { return String(row.extra?.email || row.credentials?.email || '') }
function cnUsageSummary(row: ResourceItem): string[] {
  if (!['kimi', 'zhipu', 'deepseek'].includes(String(row.platform || ''))) return []
  const extra = (row.extra && typeof row.extra === 'object' ? row.extra : {}) as Record<string, unknown>
  const platform = String(row.platform)
  const lines: string[] = []
  const balance = extra[`${platform}_balance`]
  if (typeof balance === 'number') {
    const currency = typeof extra[`${platform}_balance_currency`] === 'string' ? String(extra[`${platform}_balance_currency`]) : ''
    lines.push(`${currency ? `${currency} ` : ''}${balance.toFixed(2)}`)
  }
  const used5h = extra[`${platform}_5h_used_percent`]
  const weekly = extra[`${platform}_weekly_used_percent`]
  if (typeof used5h === 'number') lines.push(`5h ${used5h.toFixed(0)}%`)
  if (typeof weekly === 'number') lines.push(`7d ${weekly.toFixed(0)}%`)
  return lines
}
function formatDate(value: unknown) {
  if (!value) return '-'
  const normalized = typeof value === 'number' && value < 1_000_000_000_000 ? value * 1000 : value
  const date = new Date(normalized as string | number)
  return Number.isNaN(date.getTime()) ? '-' : date.toLocaleString()
}

function handleClickOutside(event: MouseEvent) {
  const target = event.target as Node
  if (accountToolsDropdownRef.value && !accountToolsDropdownRef.value.contains(target)) showTools.value = false
  if (autoRefreshDropdownRef.value && !autoRefreshDropdownRef.value.contains(target)) showAutoRefresh.value = false
}

watch(autoRefreshEnabled, () => { countdown.value = autoRefreshInterval.value })

onMounted(async () => {
  const saved = localStorage.getItem('my-account-hidden-columns')
  if (saved) {
    try { hiddenColumns.value = new Set(JSON.parse(saved)) }
    catch { localStorage.removeItem('my-account-hidden-columns') }
  }
  hiddenColumns.value.delete('usage')
  document.addEventListener('click', handleClickOutside)
  await loadReferences()
  await reload()
  timer = setInterval(() => {
    if (!autoRefreshEnabled.value) return
    countdown.value -= 1
    if (countdown.value <= 0) {
      countdown.value = autoRefreshInterval.value
      void reload()
    }
  }, 1000)
})

onBeforeUnmount(() => {
  if (timer) clearInterval(timer)
  document.removeEventListener('click', handleClickOutside)
})
</script>

<style scoped>
.account-option {
  @apply flex w-full items-center justify-between rounded-md px-3 py-2 text-left text-sm text-gray-700 transition-colors hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-gray-700;
}

.account-tools-menu-item {
  @apply flex w-full items-center gap-3 rounded-md px-3 py-2 text-sm text-gray-700 transition-colors hover:bg-gray-100 dark:text-gray-200 dark:hover:bg-gray-700;
}

.account-tools-menu-icon {
  @apply inline-flex h-8 w-8 flex-shrink-0 items-center justify-center rounded-md;
}

.row-action {
  @apply flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-xs text-gray-500 transition-colors hover:bg-gray-100 dark:hover:bg-dark-700;
}
</style>
