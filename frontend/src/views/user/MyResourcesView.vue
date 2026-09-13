<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div :class="resource === 'proxies' ? 'flex flex-wrap items-center gap-3' : 'flex flex-col justify-between gap-4 lg:flex-row lg:items-start'">
          <div :class="resource === 'proxies' ? 'contents' : 'flex flex-1 flex-wrap items-center gap-3'">
            <div :class="['relative w-full', resource === 'proxies' ? 'sm:w-64' : 'sm:w-64']">
              <Icon name="search" size="md" class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-gray-500" />
              <input
                v-model="filters.search"
                type="text"
                class="input pl-10"
                :placeholder="searchPlaceholder"
                @input="handleAlignedSearch"
              />
            </div>
            <Select
              v-if="showPlatformFilter"
              v-model="filters.platform"
              class="w-full sm:w-44"
              :options="platformFilterOptions"
              :searchable="false"
              @change="applyAlignedFilters"
            />
            <Select
              v-if="resource === 'proxies'"
              v-model="filters.type"
              class="w-full sm:w-40"
              :options="proxyKindFilterOptions"
              :searchable="false"
              @change="applyAlignedFilters"
            />
            <Select
              v-if="resource === 'proxies'"
              v-model="filters.protocol"
              class="w-full sm:w-40"
              :options="proxyProtocolFilterOptions"
              :searchable="false"
              @change="applyAlignedFilters"
            />
            <Select
              v-if="resource === 'proxies' && proxySourceFilterOptions.length > 1"
              v-model="filters.source_id"
              class="w-full sm:w-44"
              :options="proxySourceFilterOptions"
              :searchable="false"
              @change="applyAlignedFilters"
            />
            <Select
              v-if="showStatusFilter"
              v-model="filters.status"
              class="w-full sm:w-36"
              :options="statusFilterOptions"
              :searchable="false"
              @change="applyAlignedFilters"
            />
            <template v-if="resource === 'account-logs' || resource === 'upstream-errors'">
              <input v-model.trim="filters.user_id" type="number" min="1" class="input w-full sm:w-32" :placeholder="mr('filters.myUserId')" @keyup.enter="applyAlignedFilters" />
              <input v-model.trim="filters.api_key_id" type="number" min="1" class="input w-full sm:w-32" :placeholder="mr('filters.apiKeyId')" @keyup.enter="applyAlignedFilters" />
              <input v-model.trim="filters.account_id" type="number" min="1" class="input w-full sm:w-32" :placeholder="mr('filters.accountId')" @keyup.enter="applyAlignedFilters" />
              <input v-model="filters.start_date" type="date" class="input w-full sm:w-40" @change="applyAlignedFilters" />
              <input v-model="filters.end_date" type="date" class="input w-full sm:w-40" @change="applyAlignedFilters" />
            </template>
            <button v-if="hasActiveFilters" type="button" class="btn btn-secondary" :title="mr('actions.clearFilters')" @click="clearFilters">
              <Icon name="x" size="sm" />
            </button>
          </div>

          <div :class="resource === 'proxies' ? 'flex w-full grow flex-wrap items-center justify-end gap-2 whitespace-nowrap lg:w-auto' : 'flex w-full flex-shrink-0 flex-wrap items-center justify-end gap-2 lg:w-auto'">
            <button class="btn btn-secondary" :disabled="loading" :title="mr('actions.refresh')" @click="loadData">
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button
              v-if="resource === 'proxies'"
              class="btn btn-secondary"
              :disabled="batchTesting || batchQualityChecking || loading"
              :title="mr('actions.testConnection')"
              @click="batchTestProxies"
            >
              <Icon name="play" size="md" class="mr-2" :class="batchTesting ? 'animate-pulse' : ''" />
              <span>{{ batchTesting ? mr('actions.batchProgress', { completed: proxyBatchProgress.completed, total: proxyBatchProgress.total }) : mr('actions.testConnection') }}</span>
            </button>
            <button
              v-if="resource === 'proxies'"
              class="btn btn-secondary"
              :disabled="batchQualityChecking || batchTesting || loading"
              :title="mr('actions.batchQuality')"
              @click="batchQualityCheckProxies"
            >
              <Icon name="shield" size="md" class="mr-2" :class="batchQualityChecking ? 'animate-pulse' : ''" />
              <span>{{ batchQualityChecking ? mr('actions.batchProgress', { completed: proxyBatchProgress.completed, total: proxyBatchProgress.total }) : mr('actions.batchQuality') }}</span>
            </button>
            <button
              v-if="resource === 'proxies'"
              class="btn btn-secondary"
              type="button"
              :title="mr('actions.sources')"
              :aria-label="mr('actions.sources')"
              @click="openProxySources"
            >
              <Icon name="link" size="md" class="md:mr-2" />
              <span class="hidden md:inline">{{ mr('actions.sources') }}</span>
            </button>
            <div ref="columnSettingsRef" class="relative">
              <button
                class="btn btn-secondary"
                type="button"
                :title="mr('actions.columns')"
                :aria-label="mr('actions.columns')"
                @click="showColumnSettings = !showColumnSettings"
              >
                <Icon name="grid" size="md" class="md:mr-2" />
                <span class="hidden md:inline">{{ mr('actions.columns') }}</span>
              </button>
              <div
                v-if="showColumnSettings && resource === 'proxies'"
                class="absolute right-0 top-full z-50 mt-1 w-64 overflow-hidden rounded-lg border border-gray-200 bg-white shadow-lg dark:border-dark-600 dark:bg-dark-800"
              >
                <div class="p-2">
                  <div class="flex items-center justify-between px-3 py-2 text-xs font-semibold uppercase tracking-wide text-gray-400 dark:text-gray-500">
                    <span>{{ mr('actions.visibleColumns') }}</span>
                    <Icon name="grid" size="sm" />
                  </div>
                  <div class="max-h-64 overflow-y-auto">
                    <button
                      v-for="column in toggleableAlignedColumns"
                      :key="column.key"
                      type="button"
                      class="flex w-full items-center justify-between rounded-md px-3 py-2 text-left text-sm text-gray-700 hover:bg-gray-100 dark:text-gray-300 dark:hover:bg-dark-700"
                      @click="toggleColumn(column.key)"
                    >
                      <span class="truncate">{{ column.label }}</span>
                      <Icon v-if="isColumnVisible(column.key)" name="check" size="sm" class="text-primary-500" />
                    </button>
                  </div>
                </div>
              </div>
              <div v-else-if="showColumnSettings" class="absolute right-0 top-full z-50 mt-1 max-h-80 w-48 overflow-y-auto rounded-lg border border-gray-200 bg-white py-1 shadow-lg dark:border-dark-600 dark:bg-dark-800">
                <button
                  v-for="column in toggleableAlignedColumns"
                  :key="column.key"
                  type="button"
                  class="flex w-full items-center justify-between px-4 py-2 text-left text-sm text-gray-700 hover:bg-gray-100 dark:text-gray-300 dark:hover:bg-dark-700"
                  @click="toggleColumn(column.key)"
                >
                  <span>{{ column.label }}</span>
                  <Icon v-if="isColumnVisible(column.key)" name="check" size="sm" class="text-primary-500" />
                </button>
              </div>
            </div>
            <button
              v-if="resource === 'proxies'"
              class="btn btn-danger"
              :disabled="selectedIds.length === 0 || loading"
              :title="t('admin.proxies.batchDeleteAction')"
              @click="batchDeleteProxies"
            >
              <Icon name="trash" size="md" class="mr-2" />
              {{ t('admin.proxies.batchDeleteAction') }}
            </button>
            <button v-if="resource === 'proxies'" class="btn btn-secondary" :title="mr('actions.importNodes')" :aria-label="mr('actions.importNodes')" @click="openProxyImport">
              {{ t('admin.proxies.dataImport') }}
            </button>
            <button v-if="resource === 'proxies'" class="btn btn-secondary" :title="mr('actions.export')" :aria-label="mr('actions.export')" @click="exportProxies">
              {{ selectedIds.length ? t('admin.proxies.dataExportSelected') : t('admin.proxies.dataExport') }}
            </button>
            <button v-if="resource === 'assigned-subscriptions'" class="btn btn-secondary" @click="openBulkAssign">
              {{ mr('actions.bulkAssign') }}
            </button>
            <button v-if="resource === 'redeem-codes'" class="btn btn-secondary" @click="loadRedeemStats">
              {{ mr('actions.stats') }}
            </button>
            <button v-if="resource === 'redeem-codes'" class="btn btn-secondary" @click="exportRedeemCodes">
              <Icon name="download" size="sm" />
              {{ mr('actions.exportCsv') }}
            </button>
            <button v-if="resource === 'redeem-codes' && selectedIds.length" class="btn btn-secondary" @click="openRedeemBatchUpdate">
              {{ mr('actions.batchEdit') }}
            </button>
            <button v-if="resource === 'redeem-codes' && selectedIds.length" class="btn btn-secondary" @click="batchExpireRedeemCodes">
              {{ mr('actions.batchExpire') }}
            </button>
            <button v-if="resource === 'redeem-codes' && selectedIds.length" class="btn btn-danger" @click="batchDeleteRedeemCodes">
              {{ mr('actions.batchDelete') }}
            </button>
            <button v-if="resource === 'account-logs'" class="btn btn-secondary" @click="exportAccountUsageLogs">
              <Icon name="download" size="sm" />
              {{ mr('actions.exportCsv') }}
            </button>
            <button v-if="config.create" class="btn btn-primary" @click="openCreate">
              <Icon name="plus" size="md" class="mr-2" />
              {{ config.createLabel }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <div :class="resource === 'proxies' ? 'flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden' : 'flex min-h-0 min-w-0 flex-1 flex-col space-y-4'">
          <div v-if="resource === 'redeem-codes' && redeemStats" class="grid gap-3 p-4 sm:grid-cols-2 lg:grid-cols-4">
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.total') }}</div><div class="mt-1 text-xl font-semibold">{{ redeemStats.total_codes || 0 }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.active') }}</div><div class="mt-1 text-xl font-semibold text-emerald-600">{{ redeemStats.active_codes || 0 }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.used') }}</div><div class="mt-1 text-xl font-semibold text-primary-600">{{ redeemStats.used_codes || 0 }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.expired') }}</div><div class="mt-1 text-xl font-semibold text-red-600">{{ redeemStats.expired_codes || 0 }}</div></div>
          </div>
          <div v-if="resource === 'account-logs' && accountUsageStats" class="grid gap-3 p-4 sm:grid-cols-2 lg:grid-cols-4">
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.requests') }}</div><div class="mt-1 text-xl font-semibold">{{ accountUsageStats.requests || 0 }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.tokens') }}</div><div class="mt-1 text-xl font-semibold text-primary-600">{{ Number(accountUsageStats.input_tokens || 0) + Number(accountUsageStats.output_tokens || 0) }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.actualCost') }}</div><div class="mt-1 text-xl font-semibold text-emerald-600">${{ Number(accountUsageStats.actual_cost || 0).toFixed(4) }}</div></div>
            <div class="rounded-md border border-gray-200 p-3 dark:border-dark-700"><div class="text-xs text-gray-500">{{ mr('stats.averageLatency') }}</div><div class="mt-1 text-xl font-semibold">{{ Number(accountUsageStats.average_duration_ms || 0).toFixed(0) }} ms</div></div>
          </div>
          <DataTable
            :columns="alignedColumns"
            :data="items"
            :loading="loading"
            :expandable-actions="resource !== 'proxies'"
            row-key="id"
          >
          <template v-if="selectableResource" #header-select>
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="allVisibleSelected"
              @change="toggleAllVisible"
            />
          </template>
          <template v-if="selectableResource" #cell-select="{ row }">
            <input
              v-if="resource !== 'proxies' || canMutateItem(row)"
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="selectedIds.includes(Number(row.id))"
              @change="toggleSelected(Number(row.id))"
            />
          </template>
          <template #cell-name="{ value }">
            <span class="font-medium text-gray-900 dark:text-white">{{ value }}</span>
          </template>

          <template v-if="resource === 'groups'" #cell-platform="{ value }">
            <span :class="groupPlatformClass(String(value || ''))">
              <PlatformIcon :platform="normalizeGroupPlatform(value)" size="xs" />
              {{ platformLabel(value) }}
            </span>
          </template>
          <template v-if="resource === 'groups'" #cell-subscription_type="{ value }">
            <span :class="['badge', value === 'subscription' ? 'badge-primary' : 'badge-gray']">{{ formatValue(value) }}</span>
          </template>
          <template v-if="resource === 'groups'" #cell-rate_multiplier="{ value }">
            <span class="font-medium text-gray-700 dark:text-gray-300">{{ Number(value || 1) }}x</span>
          </template>
          <template v-if="resource === 'groups'" #cell-is_exclusive="{ value }">
            <span :class="['badge', value ? 'badge-primary' : 'badge-gray']">{{ mr(value ? 'states.exclusive' : 'states.public') }}</span>
          </template>
          <template #cell-account_count="{ row, value }">
            <div v-if="resource === 'groups'" class="space-y-0.5 text-xs">
              <div><span class="text-gray-500">{{ mr('table.available') }}</span><span class="ml-1 font-medium text-emerald-600">{{ row.active_account_count || 0 }}</span></div>
              <div v-if="row.rate_limited_account_count"><span class="text-gray-500">{{ mr('table.rateLimited') }}</span><span class="ml-1 font-medium text-amber-600">{{ row.rate_limited_account_count }}</span></div>
              <div><span class="text-gray-500">{{ mr('table.total') }}</span><span class="ml-1 font-medium text-gray-700 dark:text-gray-300">{{ row.account_count || 0 }}</span></div>
            </div>
            <span v-else-if="resource === 'proxies'" class="inline-flex items-center rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-800 dark:bg-dark-600 dark:text-gray-300">
              {{ t('admin.groups.accountsCount', { count: Number(value || 0) }) }}
            </span>
            <span v-else class="inline-flex items-center rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-800 dark:bg-dark-600 dark:text-gray-300">{{ mr('table.countValue', { count: Number(value || 0) }) }}</span>
          </template>
          <template v-if="resource === 'groups'" #cell-capacity="{ row }">
            <GroupCapacityBadge
              v-if="row.capacity_summary"
              :concurrency-used="Number(row.capacity_summary.concurrency_used || 0)"
              :concurrency-max="Number(row.capacity_summary.concurrency_max || 0)"
              :sessions-used="Number(row.capacity_summary.sessions_used || 0)"
              :sessions-max="Number(row.capacity_summary.sessions_max || 0)"
              :rpm-used="Number(row.capacity_summary.rpm_used || 0)"
              :rpm-max="Number(row.capacity_summary.rpm_max || 0)"
            />
            <span v-else class="text-xs text-gray-400">-</span>
          </template>
          <template v-if="resource === 'groups'" #cell-usage="{ row }">
            <div class="space-y-0.5 text-xs">
              <div><span class="text-gray-400">{{ mr('table.today') }}</span><span class="ml-1 font-medium text-gray-700 dark:text-gray-300">${{ row.today_cost || '0.0000' }}</span></div>
              <div><span class="text-gray-400">{{ mr('table.cumulative') }}</span><span class="ml-1 font-medium text-gray-700 dark:text-gray-300">${{ row.total_cost || '0.0000' }}</span></div>
            </div>
          </template>

          <template v-if="resource === 'proxies'" #cell-visibility="{ row }">
            <span :class="['badge', row.is_public ? 'badge-success' : 'badge-gray']">{{ mr(row.is_public ? 'states.public' : 'states.private') }}</span>
          </template>
          <template v-if="resource === 'proxies'" #cell-kind="{ value }">
            <span :class="['badge', value === 'xray' ? 'badge-primary' : 'badge-gray']">{{ value === 'xray' ? 'Xray' : mr('states.standard') }}</span>
          </template>
          <template v-if="resource === 'proxies'" #cell-protocol="{ value }">
            <span :class="['badge', String(value || '').startsWith('socks5') ? 'badge-primary' : 'badge-gray']">{{ String(value || '-').toUpperCase() }}</span>
          </template>
          <template v-if="resource === 'proxies'" #cell-address="{ row }">
            <span v-if="row.details_hidden" class="whitespace-nowrap text-xs text-gray-400">{{ mr('states.publicDetailsHidden') }}</span>
            <code v-else class="code inline-block max-w-[70%] break-all text-xs sm:max-w-none sm:whitespace-nowrap sm:break-normal">{{ row.host }}:{{ row.port }}</code>
          </template>
          <template v-if="resource === 'proxies'" #cell-auth="{ row }">
            <span v-if="row.details_hidden" class="whitespace-nowrap text-xs text-gray-400">{{ mr('states.publicDetailsHidden') }}</span>
            <span v-else :class="['badge', row.has_auth ? 'badge-primary' : 'badge-gray']">{{ mr(row.has_auth ? 'states.configured' : 'states.none') }}</span>
          </template>
          <template v-if="resource === 'proxies'" #cell-location="{ row }">
            <div class="flex items-center gap-2">
              <span v-if="row.country_code" class="text-base" aria-hidden="true">{{ countryFlag(row.country_code) }}</span>
              <span v-if="formatProxyLocation(row)" class="text-sm text-gray-700 dark:text-gray-200">{{ formatProxyLocation(row) }}</span>
              <span v-else class="text-sm text-gray-400">-</span>
            </div>
          </template>
          <template v-if="resource === 'proxies'" #cell-latency="{ row }">
            <div class="flex flex-col gap-1">
              <span v-if="row.latency_status === 'failed'" class="badge badge-danger" :title="row.latency_message || undefined">
                {{ mr('states.failed') }}
              </span>
              <span v-else-if="typeof row.latency_ms === 'number'" :class="['badge', row.latency_ms < 200 ? 'badge-success' : 'badge-warning']">
                {{ row.latency_ms }}ms
              </span>
              <span v-else class="text-sm text-gray-400">-</span>
              <div v-if="typeof row.quality_checked === 'number'" class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400" :title="row.quality_summary || undefined">
                <span>{{ t('admin.proxies.qualityInline', { grade: row.quality_grade || '-', score: row.quality_score ?? '-' }) }}</span>
                <span class="badge" :class="proxyQualityClass(row.quality_status)">{{ proxyQualityLabel(row.quality_status) }}</span>
              </div>
            </div>
          </template>
          <template v-if="resource === 'proxies'" #cell-expiry="{ row }">
            <span v-if="!row.expires_at" class="text-sm text-gray-400">{{ mr('states.neverExpires') }}</span>
            <span v-else class="text-xs text-gray-700 dark:text-gray-200">{{ formatDateTime(row.expires_at) }}</span>
          </template>
          <template #cell-created_at="{ value }">
            <span class="whitespace-nowrap text-xs text-gray-600 dark:text-gray-300">{{ formatResourceDate(value) }}</span>
          </template>
          <template #cell-status="{ value }">
            <span :class="['badge', value === 'active' ? 'badge-success' : value === 'expired' ? 'badge-danger' : 'badge-gray']">{{ formatValue(value) }}</span>
          </template>
          <template #cell-expires_at="{ value }"><span class="whitespace-nowrap text-sm text-gray-600 dark:text-gray-300">{{ formatResourceDate(value, mr('states.neverExpires')) }}</span></template>
          <template #cell-source_type="{ value }"><span class="badge badge-gray">{{ sourceTypeLabel(value) }}</span></template>
          <template #cell-usage_count="{ row }"><span class="whitespace-nowrap font-medium">{{ Number(row.used_count || 0) }} / {{ Number(row.max_uses || 1) }}</span></template>
          <template #cell-validity_days="{ value }">{{ mr('table.daysValue', { count: Number(value || 0) }) }}</template>
          <template #cell-daily_usage_usd="{ value }">${{ Number(value || 0).toFixed(2) }}</template>
          <template #cell-monthly_usage_usd="{ value }">${{ Number(value || 0).toFixed(2) }}</template>
          <template #cell-input_tokens="{ value }">{{ Number(value || 0).toLocaleString() }}</template>
          <template #cell-output_tokens="{ value }">{{ Number(value || 0).toLocaleString() }}</template>
          <template #cell-total_cost="{ value }">${{ Number(value || 0).toFixed(4) }}</template>
          <template #cell-message="{ value }"><span class="block max-w-80 break-words text-sm" :title="String(value || '')">{{ value || '-' }}</span></template>

          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button v-if="resource === 'groups'" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700" @click="openEdit(row)">
                <Icon name="edit" size="sm" /><span class="text-xs">{{ mr('actions.edit') }}</span>
              </button>
              <button v-if="resource === 'groups'" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-purple-600 dark:hover:bg-dark-700" @click="openGroupOverrides(row)">
                <Icon name="dollar" size="sm" /><span class="text-xs">{{ mr('actions.userRates') }}</span>
              </button>
              <button v-if="resource === 'groups'" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20" @click="deleteItem(row)">
                <Icon name="trash" size="sm" /><span class="text-xs">{{ mr('actions.delete') }}</span>
              </button>
              <button v-if="resource === 'proxies'" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-emerald-50 hover:text-emerald-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-emerald-900/20" :disabled="testingProxyIds.has(Number(row.id))" @click="testProxy(row)">
                <Icon name="checkCircle" size="sm" :class="testingProxyIds.has(Number(row.id)) ? 'animate-pulse' : ''" /><span class="text-xs">{{ mr('actions.testConnection') }}</span>
              </button>
              <button v-if="resource === 'proxies'" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-blue-50 hover:text-blue-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-blue-900/20" :disabled="qualityCheckingProxyIds.has(Number(row.id))" @click="qualityCheckProxy(row)">
                <Icon name="shield" size="sm" :class="qualityCheckingProxyIds.has(Number(row.id)) ? 'animate-pulse' : ''" /><span class="text-xs">{{ mr('actions.quality') }}</span>
              </button>
              <button v-if="resource === 'proxies' && canMutateItem(row)" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700" @click="openEdit(row)">
                <Icon name="edit" size="sm" /><span class="text-xs">{{ mr('actions.edit') }}</span>
              </button>
              <button v-if="resource === 'proxies' && canMutateItem(row)" class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20" @click="deleteItem(row)">
                <Icon name="trash" size="sm" /><span class="text-xs">{{ mr('actions.delete') }}</span>
              </button>
              <button v-if="resource === 'assigned-subscriptions'" class="btn btn-xs btn-secondary" @click="openExtendAssigned(row)">{{ mr('actions.extend') }}</button>
              <button v-if="resource === 'assigned-subscriptions'" class="btn btn-xs btn-secondary" @click="resetAssignedUsage(row)">{{ mr('actions.resetUsage') }}</button>
              <button v-if="resource === 'assigned-subscriptions' && !row.deleted_at" class="btn btn-xs btn-danger" @click="revokeAssigned(row)">{{ mr('actions.revoke') }}</button>
              <button v-if="resource === 'assigned-subscriptions' && row.deleted_at && row.revoked_by_user_id !== row.user_id" class="btn btn-xs btn-secondary" @click="restoreAssigned(row)">{{ mr('actions.restore') }}</button>
              <button v-if="resource === 'redeem-codes'" class="btn btn-xs btn-secondary" @click="openRedeemUsageDetails(row)">{{ mr('actions.details') }}</button>
              <button v-if="resource === 'redeem-codes' && row.status === 'unused'" class="btn btn-xs btn-secondary" @click="expireRedeemCode(row)">{{ mr('actions.expire') }}</button>
              <button v-if="resource === 'redeem-codes'" class="btn btn-xs btn-danger" @click="deleteItem(row)">{{ mr('actions.delete') }}</button>
              <button v-if="resource === 'account-logs' || resource === 'upstream-errors'" class="btn btn-xs btn-secondary" @click="openRecordDetails(row)">
                <Icon name="eye" size="sm" />{{ mr('actions.details') }}
              </button>
            </div>
          </template>
          <template #empty>
            <EmptyState :title="mr('table.empty')" :action-text="config.create ? config.createLabel : undefined" @action="config.create ? openCreate() : undefined" />
          </template>
          </DataTable>
        </div>
      </template>

      <template #pagination>
        <Pagination
          v-if="page.total > 0"
          :page="page.page"
          :total="page.total"
          :page-size="page.page_size"
          @update:page="changePage"
          @update:page-size="changePageSize"
        />
      </template>
    </TablePageLayout>


    <div v-if="recordDetailOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <div class="flex max-h-[calc(100dvh-1rem)] w-full max-w-2xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ recordDetailTitle }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="recordDetailOpen = false">{{ mr('actions.close') }}</button>
        </div>
        <dl class="min-h-0 flex-1 overflow-y-auto overscroll-contain p-4">
          <div v-for="field in recordDetailFields" :key="field.key" class="grid gap-1 border-b border-gray-100 py-3 last:border-b-0 dark:border-dark-700 sm:grid-cols-[10rem_minmax(0,1fr)] sm:gap-4">
            <dt class="text-xs font-medium uppercase text-gray-500 dark:text-dark-300">{{ field.label }}</dt>
            <dd class="min-w-0 whitespace-pre-wrap break-words text-sm text-gray-900 dark:text-white">{{ field.value }}</dd>
          </div>
        </dl>
      </div>
    </div>

    <MyProxyEditorDialog
      :show="editorOpen && resource === 'proxies'"
      :proxy="proxyEditorItem"
      :initial-mode="proxyEditorInitialMode"
      @close="closeProxyEditor"
      @saved="handleProxySaved"
    />

    <div v-if="editorOpen && resource !== 'proxies'" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <div class="flex max-h-[calc(100dvh-1rem)] w-full max-w-3xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ editingId ? t('common.edit') : config.createLabel || t('common.create') }}</h2>
          <button class="btn btn-sm btn-secondary" @click="editorOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 space-y-4 overflow-y-auto overscroll-contain p-4">
          <div v-if="editorMode === 'default'" class="space-y-4">
            <div v-if="resource === 'groups'" class="grid gap-3 md:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.name') }}</span>
                <input v-model.trim="editorForm.group.name" class="input" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.platform') }}</span>
                <Select v-model="editorForm.group.platform" :options="platformOptions" :searchable="false" />
              </label>
              <label v-if="!editingId && copyAccountGroupOptions.length" class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.copyAccountsFromGroups') }}</span>
                <select v-model="editorForm.group.copy_accounts_from_group_ids" class="input min-h-28" multiple>
                  <option v-for="group in copyAccountGroupOptions" :key="group.id" :value="Number(group.id)">{{ group.name }}</option>
                </select>
                <span class="mt-1 block text-xs text-gray-500 dark:text-dark-400">{{ mr('fields.copyAccountsHint') }}</span>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.subscriptionType') }}</span>
                <Select v-model="editorForm.group.subscription_type" :options="groupSubscriptionTypeOptions" :searchable="false" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.status') }}</span>
                <Select v-model="editorForm.group.status" :options="groupStatusOptions" :searchable="false" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.rateMultiplier') }}</span>
                <input v-model.number="editorForm.group.rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.rpmLimit') }}</span>
                <input v-model.number="editorForm.group.rpm_limit" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.dailyLimitUsd') }}</span>
                <input v-model.number="editorForm.group.daily_limit_usd" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.weeklyLimitUsd') }}</span>
                <input v-model.number="editorForm.group.weekly_limit_usd" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.monthlyLimitUsd') }}</span>
                <input v-model.number="editorForm.group.monthly_limit_usd" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.defaultValidityDays') }}</span>
                <input v-model.number="editorForm.group.default_validity_days" type="number" min="1" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.fallbackGroup') }}</span>
                <Select v-model="editorForm.group.fallback_group_id" :options="fallbackGroupOptions" :disabled="referenceLoading" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.invalidRequestFallback') }}</span>
                <Select v-model="editorForm.group.fallback_group_id_on_invalid_request" :options="fallbackGroupOptions" :disabled="referenceLoading" />
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.peak_rate_enabled" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.peakRate') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.model_routing_enabled" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.modelRouting') }}</span>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.peakStart') }}</span>
                <input v-model.trim="editorForm.group.peak_start" class="input" placeholder="09:00" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.peakEnd') }}</span>
                <input v-model.trim="editorForm.group.peak_end" class="input" placeholder="18:00" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.peakMultiplier') }}</span>
                <input v-model.number="editorForm.group.peak_rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.defaultMappedModel') }}</span>
                <input v-model.trim="editorForm.group.default_mapped_model" class="input" />
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.allow_messages_dispatch" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.openaiDispatch') }}</span>
              </label>
              <div v-if="editorForm.group.platform === 'openai'" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <div class="flex items-center justify-between gap-3">
                  <span class="text-sm text-gray-700 dark:text-dark-100">{{ t('admin.groups.openaiLive.allow') }}</span>
                  <button
                    type="button"
                    class="relative inline-flex h-6 w-12 flex-shrink-0 cursor-pointer rounded-full border-2 border-transparent transition-colors duration-200 ease-in-out focus:outline-none"
                    :class="editorForm.group.allow_live ? 'bg-primary-500' : 'bg-gray-300 dark:bg-dark-600'"
                    @click="toggleGroupLive"
                  >
                    <span
                      class="pointer-events-none inline-block h-5 w-5 transform rounded-full bg-white shadow ring-0 transition duration-200 ease-in-out"
                      :class="editorForm.group.allow_live ? 'translate-x-6' : 'translate-x-1'"
                    />
                  </button>
                </div>
                <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">{{ t('admin.groups.openaiLive.hint') }}</p>
              </div>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.require_oauth_only" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.oauthOnly') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.require_privacy_set" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.privacySet') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.claude_code_only" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.claudeCodeOnly') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.is_exclusive" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.exclusive') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.mcp_xml_inject" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.mcpXmlInject') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.allow_image_generation" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imageGeneration') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.allow_batch_image_generation" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.batchImage') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.image_rate_independent" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imageIndependentRate') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.group.video_rate_independent" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.videoIndependentRate') }}</span>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imageMultiplier') }}</span>
                <input v-model.number="editorForm.group.image_rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imagePrice1k') }}</span>
                <input v-model.number="editorForm.group.image_price_1k" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imagePrice2k') }}</span>
                <input v-model.number="editorForm.group.image_price_2k" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.imagePrice4k') }}</span>
                <input v-model.number="editorForm.group.image_price_4k" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.batchImageDiscount') }}</span>
                <input v-model.number="editorForm.group.batch_image_discount_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.batchImageHold') }}</span>
                <input v-model.number="editorForm.group.batch_image_hold_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.videoMultiplier') }}</span>
                <input v-model.number="editorForm.group.video_rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.videoPrice480p') }}</span>
                <input v-model.number="editorForm.group.video_price_480p" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.videoPrice720p') }}</span>
                <input v-model.number="editorForm.group.video_price_720p" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.videoPrice1080p') }}</span>
                <input v-model.number="editorForm.group.video_price_1080p" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.webSearchPricePerCall') }}</span>
                <input v-model.number="editorForm.group.web_search_price_per_call" type="number" min="0" step="0.001" class="input" placeholder="0.01" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.sortOrder') }}</span>
                <input v-model.number="editorForm.group.sort_order" type="number" class="input" />
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.modelRoutingJson') }}</span>
                <textarea v-model="editorForm.group.model_routing_text" class="input min-h-24 font-mono text-xs" spellcheck="false"></textarea>
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.supportedModelScopesJson') }}</span>
                <textarea v-model="editorForm.group.supported_model_scopes_text" class="input min-h-20 font-mono text-xs" spellcheck="false"></textarea>
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.dispatchConfigJson') }}</span>
                <textarea v-model="editorForm.group.messages_dispatch_model_config_text" class="input min-h-20 font-mono text-xs" spellcheck="false"></textarea>
              </label>
              <section class="space-y-3 rounded-md border border-gray-200 p-3 md:col-span-2 dark:border-dark-700">
                <div class="flex flex-wrap items-center justify-between gap-3">
                  <div>
                    <div class="text-sm font-medium text-gray-900 dark:text-white">{{ mr('fields.modelsList') }}</div>
                    <div class="text-xs text-gray-500 dark:text-dark-400">{{ mr('fields.modelsListHint') }}</div>
                  </div>
                  <button type="button" class="relative inline-flex h-6 w-11 items-center rounded-full transition-colors" :class="groupModelAllowlistState.enabled ? 'bg-primary-500' : 'bg-gray-300 dark:bg-dark-600'" @click="groupModelAllowlistState.enabled = !groupModelAllowlistState.enabled">
                    <span class="inline-block h-4 w-4 rounded-full bg-white shadow transition-transform" :class="groupModelAllowlistState.enabled ? 'translate-x-6' : 'translate-x-1'" />
                  </button>
                </div>
                <div v-if="groupModelAllowlistState.enabled" class="space-y-3">
                  <div class="flex flex-col gap-2 sm:flex-row">
                    <input v-model.trim="groupModelInput" class="input min-w-0 flex-1" :placeholder="mr('fields.modelIdPlaceholder')" @keyup.enter.prevent="addGroupModel" />
                    <button type="button" class="btn btn-secondary" :disabled="!groupModelInput" @click="addGroupModel"><Icon name="plus" size="sm" />{{ mr('actions.add') }}</button>
                    <button v-if="editingId" type="button" class="btn btn-secondary" :disabled="groupModelCandidatesLoading" @click="loadGroupModelCandidates"><Icon name="refresh" size="sm" :class="groupModelCandidatesLoading ? 'animate-spin' : ''" />{{ mr('actions.loadCandidates') }}</button>
                  </div>
                  <div v-if="groupModelAllowlistState.items.length" class="flex flex-wrap items-center justify-between gap-2">
                    <span class="text-xs text-gray-500">{{ mr('fields.modelsSelected', { selected: groupModelAllowlistState.items.filter(item => item.selected).length, total: groupModelAllowlistState.items.length }) }}</span>
                    <div class="flex gap-2"><button type="button" class="text-xs font-medium text-primary-600" @click="selectAllModelAllowlistItems(groupModelAllowlistState)">{{ mr('actions.selectAll') }}</button><button type="button" class="text-xs font-medium text-gray-600 dark:text-gray-300" @click="invertModelAllowlistSelection(groupModelAllowlistState)">{{ mr('actions.invertSelection') }}</button></div>
                  </div>
                  <div class="max-h-64 space-y-2 overflow-y-auto">
                    <div v-for="(item, index) in groupModelAllowlistState.items" :key="item.id" class="flex items-center gap-2 rounded-md border border-gray-200 px-3 py-2 dark:border-dark-600">
                      <input v-model="item.selected" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600" />
                      <span class="min-w-0 flex-1 break-all text-sm text-gray-700 dark:text-gray-200">{{ item.id }}</span>
                      <button type="button" class="btn btn-xs btn-secondary" :disabled="index === 0" :title="mr('actions.moveUp')" @click="moveModelAllowlistItem(groupModelAllowlistState, index, index - 1)"><Icon name="chevronUp" size="sm" /></button>
                      <button type="button" class="btn btn-xs btn-secondary" :disabled="index === groupModelAllowlistState.items.length - 1" :title="mr('actions.moveDown')" @click="moveModelAllowlistItem(groupModelAllowlistState, index, index + 1)"><Icon name="chevronDown" size="sm" /></button>
                    </div>
                    <div v-if="!groupModelAllowlistState.items.length" class="py-4 text-center text-sm text-gray-500">{{ mr('fields.noModels') }}</div>
                  </div>
                </div>
              </section>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.description') }}</span>
                <textarea v-model="editorForm.group.description" class="input min-h-20"></textarea>
              </label>
            </div>

            <div v-else-if="resource === 'accounts'" class="grid gap-3 md:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.name') }}</span>
                <input v-model.trim="editorForm.account.name" class="input" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.platform') }}</span>
                <Select v-model="editorForm.account.platform" :options="platformOptions" :searchable="false" :disabled="Boolean(editingId)" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.type') }}</span>
                <Select v-model="editorForm.account.type" :options="accountTypeOptions" :searchable="false" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.status') }}</span>
                <Select v-model="editorForm.account.status" :options="accountStatusOptions" :searchable="false" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.groups') }}</span>
                <select v-model="editorForm.account.group_ids" class="input min-h-32" multiple :disabled="referenceLoading">
                  <option v-for="group in groupOptions" :key="group.id" :value="Number(group.id)">
                    {{ group.name }} · {{ group.platform }} · {{ group.subscription_type }}
                  </option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.proxy') }}</span>
                <Select v-model="editorForm.account.proxy_id" :options="accountProxyOptions" :disabled="referenceLoading" searchable />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.priority') }}</span>
                <input v-model.number="editorForm.account.priority" type="number" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.concurrency') }}</span>
                <input v-model.number="editorForm.account.concurrency" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.loadFactor') }}</span>
                <input v-model.number="editorForm.account.load_factor" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.rateMultiplier') }}</span>
                <input v-model.number="editorForm.account.rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.account.schedulable" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.schedulable') }}</span>
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.account.auto_pause_on_expired" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.autoPauseOnExpired') }}</span>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.expiresAt') }}</span>
                <input v-model="editorForm.account.expires_at" type="datetime-local" class="input" />
              </label>
              <section v-if="accountOAuthEnabled" class="space-y-3 rounded-lg border border-gray-200 p-3 md:col-span-2 dark:border-dark-700">
                <div class="flex flex-wrap items-center justify-between gap-2">
                  <h3 class="text-sm font-medium text-gray-900 dark:text-white">
                    {{ accountOAuthSetupToken ? 'Setup Token authorization' : 'OAuth authorization' }}
                  </h3>
                  <button type="button" class="btn btn-secondary" :disabled="accountOAuth.loading" @click="generateAccountOAuthURL">
                    {{ accountOAuth.loading ? t('common.processing') : mr('actions.generateAuthUrl') }}
                  </button>
                </div>

                <div v-if="editorForm.account.platform === 'gemini'" class="grid gap-3 md:grid-cols-3">
                  <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.oauthType') }}</span>
                    <Select v-model="accountOAuth.oauth_type" :options="oauthTypeOptions" :searchable="false" />
                  </label>
                  <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.projectId') }}</span>
                    <input v-model.trim="accountOAuth.project_id" class="input" autocomplete="off" />
                  </label>
                  <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.tierId') }}</span>
                    <input v-model.trim="accountOAuth.tier_id" class="input" autocomplete="off" />
                  </label>
                </div>

                <div v-if="accountOAuth.auth_url" class="flex min-w-0 items-center gap-2 rounded-md bg-gray-50 p-2 dark:bg-dark-800">
                  <a :href="accountOAuth.auth_url" target="_blank" rel="noopener noreferrer" class="min-w-0 flex-1 truncate text-sm text-primary-600 hover:underline dark:text-primary-400">
                    {{ accountOAuth.auth_url }}
                  </a>
                  <button type="button" class="btn btn-secondary shrink-0" @click="copyAccountOAuthURL">{{ t('common.copy') }}</button>
                </div>

                <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.callbackOrCode') }}</span>
                  <textarea v-model.trim="accountOAuth.callback" class="input min-h-20 font-mono text-xs" spellcheck="false"></textarea>
                </label>
                <button
                  type="button"
                  class="btn btn-primary"
                  :disabled="accountOAuth.loading || !accountOAuth.session_id || !accountOAuth.callback.trim()"
                  @click="exchangeAccountOAuthCode"
                >
                  {{ mr('actions.completeAuthorization') }}
                </button>

                <div v-if="editorForm.account.platform === 'anthropic'" class="grid gap-3 border-t border-gray-200 pt-3 md:grid-cols-[1fr_auto] md:items-end dark:border-dark-700">
                  <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.sessionKey') }}</span>
                    <textarea v-model.trim="accountOAuth.session_key" class="input min-h-20 font-mono text-xs" spellcheck="false"></textarea>
                  </label>
                  <button
                    type="button"
                    class="btn btn-secondary"
                    :disabled="accountOAuth.loading || !accountOAuth.session_key.trim()"
                    @click="exchangeAccountOAuthCookie"
                  >
                    {{ mr('actions.sessionKeyAuthorization') }}
                  </button>
                </div>

                <p v-if="accountOAuth.error" class="text-sm text-red-600 dark:text-red-300">{{ accountOAuth.error }}</p>
              </section>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.credentials') }}</span>
                <textarea v-model="editorForm.account.credentials_text" class="input min-h-24 font-mono text-xs" spellcheck="false"></textarea>
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.extraJson') }}</span>
                <textarea v-model="editorForm.account.extra_text" class="input min-h-20 font-mono text-xs" spellcheck="false"></textarea>
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
                <textarea v-model="editorForm.account.notes" class="input min-h-20"></textarea>
              </label>
            </div>

            <div v-else-if="resource === 'assigned-subscriptions'" class="grid gap-3 md:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.email') }}</span>
                <input v-model.trim="editorForm.assigned.email" class="input" type="email" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.userId') }}</span>
                <input v-model.number="editorForm.assigned.user_id" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.group') }}</span>
                <Select v-model="editorForm.assigned.group_id" :options="subscriptionGroupSelectOptions" :placeholder="mr('fields.selectSubscriptionGroup')" :disabled="referenceLoading" searchable />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.validityDays') }}</span>
                <input v-model.number="editorForm.assigned.validity_days" type="number" min="1" class="input" required />
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
                <textarea v-model="editorForm.assigned.notes" class="input min-h-20"></textarea>
              </label>
            </div>

            <div v-else-if="resource === 'redeem-codes'" class="grid gap-3 md:grid-cols-2">
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.group') }}</span>
                <Select v-model="editorForm.redeem.group_id" :options="subscriptionGroupSelectOptions" :placeholder="mr('fields.selectSubscriptionGroup')" :disabled="referenceLoading" searchable />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.count') }}</span>
                <input v-model.number="editorForm.redeem.count" type="number" min="1" max="1000" class="input" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.validityDays') }}</span>
                <input v-model.number="editorForm.redeem.validity_days" type="number" min="1" class="input" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.codeExpires') }}</span>
                <input v-model="editorForm.redeem.expires_at" type="datetime-local" class="input" />
              </label>
              <label class="flex items-center gap-3 rounded-md border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="editorForm.redeem.repeatable" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.repeatableCode') }}</span>
              </label>
              <label v-if="editorForm.redeem.repeatable" class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.maxUses') }}</span>
                <input v-model.number="editorForm.redeem.max_uses" type="number" min="2" max="10000" class="input" required />
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
                <textarea v-model="editorForm.redeem.notes" class="input min-h-20"></textarea>
              </label>
            </div>
          </div>

          <div v-else-if="editorMode === 'codexSessionImport' || editorMode === 'codexPATImport'" class="grid gap-3 md:grid-cols-2">
              <label v-if="editorMode === 'codexSessionImport'" class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.codexSession') }}</span>
                <textarea v-model.trim="codexImportForm.content" class="input min-h-40 font-mono text-xs" spellcheck="false" required></textarea>
              </label>
              <label v-else class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.codexPat') }}</span>
                <input v-model.trim="codexImportForm.access_token" type="password" class="input font-mono text-xs" autocomplete="new-password" required />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.name') }}</span>
                <input v-model.trim="codexImportForm.name" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.proxy') }}</span>
                <Select v-model="codexImportForm.proxy_id" :options="accountProxyOptions" :disabled="referenceLoading" searchable />
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.groups') }}</span>
                <select v-model="codexImportForm.group_ids" class="input min-h-28" multiple :disabled="referenceLoading">
                  <option v-for="group in openAIGroupOptions" :key="group.id" :value="Number(group.id)">
                    {{ group.name }} · {{ group.subscription_type }}
                  </option>
                </select>
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.concurrency') }}</span>
                <input v-model.number="codexImportForm.concurrency" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.priority') }}</span>
                <input v-model.number="codexImportForm.priority" type="number" min="0" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.rateMultiplier') }}</span>
                <input v-model.number="codexImportForm.rate_multiplier" type="number" min="0" step="0.01" class="input" />
              </label>
              <label class="block">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.expiresAt') }}</span>
                <input v-model="codexImportForm.expires_at" type="datetime-local" class="input" />
              </label>
              <label class="flex items-center gap-2 rounded-lg border border-gray-200 p-3 dark:border-dark-700">
                <input v-model="codexImportForm.auto_pause_on_expired" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
                <span class="text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.autoPauseOnExpired') }}</span>
              </label>
              <label class="block md:col-span-2">
                <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
                <textarea v-model="codexImportForm.notes" class="input min-h-20"></textarea>
              </label>
          </div>

          <details v-if="editorMode === 'default' || editorMode === 'accountImport' || editorMode === 'proxyImport'" class="rounded-lg border border-gray-200 p-3 dark:border-dark-700" :open="editorMode !== 'default'">
            <summary class="cursor-pointer text-sm font-medium text-gray-700 dark:text-dark-100">{{ mr('fields.advancedFields') }}</summary>
            <textarea v-model="editorText" class="input mt-3 min-h-[220px] font-mono text-xs" spellcheck="false"></textarea>
          </details>
          <div v-if="editorError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ editorError }}</div>
        </div>
        <div class="flex shrink-0 justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <button class="btn btn-secondary" @click="editorOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" :disabled="saving" @click="saveEditor">{{ t('common.save') }}</button>
        </div>
      </div>
    </div>

    <div v-if="groupOverridesOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <form class="flex max-h-[calc(100dvh-1rem)] w-full max-w-4xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]" @submit.prevent="saveGroupOverrides">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('overrides.title', { name: groupOverridesTarget?.name || '' }) }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="groupOverridesOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 overflow-y-auto overscroll-contain p-4">
          <div v-if="groupOverridesLoading" class="py-10 text-center text-sm text-gray-500">{{ t('common.loading') }}</div>
          <div v-else-if="groupOverrideRows.length === 0" class="py-10 text-center text-sm text-gray-500">{{ mr('overrides.empty') }}</div>
          <table v-else class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead>
              <tr>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('overrides.user') }}</th>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('overrides.email') }}</th>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('overrides.rate') }}</th>
                <th class="px-3 py-2 text-left font-medium text-gray-500">RPM</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="row in groupOverrideRows" :key="row.user_id">
                <td class="px-3 py-2 text-gray-700 dark:text-dark-100">{{ row.user_name || row.user_id }}</td>
                <td class="px-3 py-2 text-gray-700 dark:text-dark-100">{{ row.user_email || '-' }}</td>
                <td class="px-3 py-2"><input v-model="row.rate_multiplier" type="number" min="0.01" max="1000" step="0.01" class="input w-32" :placeholder="mr('overrides.defaultValue')" /></td>
                <td class="px-3 py-2"><input v-model="row.rpm_override" type="number" min="0" max="1000000" step="1" class="input w-32" :placeholder="mr('overrides.defaultValue')" /></td>
              </tr>
            </tbody>
          </table>
          <div v-if="operationError" class="mt-3 rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ operationError }}</div>
        </div>
        <div class="flex shrink-0 flex-wrap justify-between gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <div class="flex gap-2">
            <button class="btn btn-secondary" type="button" :disabled="saving" @click="clearGroupRates">{{ mr('overrides.clearRates') }}</button>
            <button class="btn btn-secondary" type="button" :disabled="saving" @click="clearGroupRPMs">{{ mr('overrides.clearRPMs') }}</button>
          </div>
          <div class="flex gap-2">
            <button class="btn btn-secondary" type="button" @click="groupOverridesOpen = false">{{ t('common.cancel') }}</button>
            <button class="btn btn-primary" type="submit" :disabled="saving || groupOverridesLoading">{{ t('common.save') }}</button>
          </div>
        </div>
      </form>
    </div>

    <div v-if="redeemUsageOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <div class="flex max-h-[calc(100dvh-1rem)] w-full max-w-2xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]">
        <div class="flex shrink-0 items-center justify-between gap-3 border-b border-gray-200 p-4 dark:border-dark-700">
          <div class="min-w-0">
            <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('redeemUsages.title') }}</h2>
            <code class="block truncate text-xs text-gray-500 dark:text-dark-400">{{ redeemUsageTarget?.code }}</code>
          </div>
          <button class="btn btn-sm btn-secondary" type="button" @click="redeemUsageOpen = false">{{ mr('actions.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 overflow-auto overscroll-contain p-4">
          <div v-if="redeemUsageLoading" class="py-10 text-center text-sm text-gray-500">{{ mr('table.loading') }}</div>
          <div v-else-if="redeemUsages.length === 0" class="py-10 text-center text-sm text-gray-500">{{ mr('redeemUsages.empty') }}</div>
          <table v-else class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead>
              <tr>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('redeemUsages.user') }}</th>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('redeemUsages.userId') }}</th>
                <th class="px-3 py-2 text-left font-medium text-gray-500">{{ mr('redeemUsages.usedAt') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
              <tr v-for="usage in redeemUsages" :key="usage.id">
                <td class="px-3 py-2 text-gray-700 dark:text-dark-100">
                  <div>{{ usage.user_email || '-' }}</div>
                  <div v-if="usage.username" class="text-xs text-gray-500">{{ usage.username }}</div>
                </td>
                <td class="px-3 py-2 font-mono text-xs text-gray-500">#{{ usage.user_id }}</td>
                <td class="whitespace-nowrap px-3 py-2 text-gray-500">{{ formatValue(usage.used_at) }}</td>
              </tr>
            </tbody>
          </table>
          <div v-if="redeemUsageError" class="mt-3 rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ redeemUsageError }}</div>
        </div>
      </div>
    </div>

    <div v-if="accountBatchOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <form class="flex max-h-[calc(100dvh-1rem)] w-full max-w-xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]" @submit.prevent="submitAccountBatchUpdate">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('batch.editAccounts') }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="accountBatchOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 space-y-4 overflow-y-auto overscroll-contain p-4">
          <div class="text-sm text-gray-500 dark:text-dark-400">{{ mr('batch.selectedAccounts', { count: selectedIds.length }) }}</div>
          <label class="flex items-center gap-3">
            <input v-model="accountBatchForm.update_schedulable" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-28 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.schedulable') }}</span>
            <Select v-model="accountBatchForm.schedulable" class="flex-1" :options="booleanStatusOptions" :disabled="!accountBatchForm.update_schedulable" :searchable="false" />
          </label>
          <label class="flex items-center gap-3">
            <input v-model="accountBatchForm.update_status" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-28 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.status') }}</span>
            <Select v-model="accountBatchForm.status" class="flex-1" :options="accountStatusOptions" :disabled="!accountBatchForm.update_status" :searchable="false" />
          </label>
          <label class="flex items-center gap-3">
            <input v-model="accountBatchForm.update_priority" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-28 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.priority') }}</span>
            <input v-model.number="accountBatchForm.priority" type="number" class="input flex-1" :disabled="!accountBatchForm.update_priority" />
          </label>
          <label class="flex items-start gap-3">
            <input v-model="accountBatchForm.update_notes" type="checkbox" class="mt-2 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="mt-2 w-28 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
            <textarea v-model="accountBatchForm.notes" class="input min-h-24 flex-1" :disabled="!accountBatchForm.update_notes"></textarea>
          </label>
          <div v-if="operationError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ operationError }}</div>
        </div>
        <div class="flex shrink-0 justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <button class="btn btn-secondary" type="button" @click="accountBatchOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" type="submit" :disabled="saving">{{ mr('actions.apply') }}</button>
        </div>
      </form>
    </div>

    <div v-if="redeemBatchOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <form class="flex max-h-[calc(100dvh-1rem)] w-full max-w-xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]" @submit.prevent="submitRedeemBatchUpdate">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('batch.editCodes') }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="redeemBatchOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 space-y-4 overflow-y-auto overscroll-contain p-4">
          <div class="text-sm text-gray-500 dark:text-dark-400">{{ mr('batch.selectedCodes', { count: selectedIds.length }) }}</div>
          <label class="flex items-center gap-3">
            <input v-model="redeemBatchForm.update_status" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-32 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.status') }}</span>
            <Select v-model="redeemBatchForm.status" class="flex-1" :options="redeemStatusOptions" :disabled="!redeemBatchForm.update_status" :searchable="false" />
          </label>
          <label class="flex items-center gap-3">
            <input v-model="redeemBatchForm.update_validity_days" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-32 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.validityDays') }}</span>
            <input v-model.number="redeemBatchForm.validity_days" type="number" min="1" class="input flex-1" :disabled="!redeemBatchForm.update_validity_days" />
          </label>
          <label class="flex items-center gap-3">
            <input v-model="redeemBatchForm.update_expires_at" type="checkbox" class="rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="w-32 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.codeExpires') }}</span>
            <input v-model="redeemBatchForm.expires_at" type="datetime-local" class="input flex-1" :disabled="!redeemBatchForm.update_expires_at" />
          </label>
          <label class="flex items-start gap-3">
            <input v-model="redeemBatchForm.update_notes" type="checkbox" class="mt-2 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
            <span class="mt-2 w-32 text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
            <textarea v-model="redeemBatchForm.notes" class="input min-h-24 flex-1" :disabled="!redeemBatchForm.update_notes"></textarea>
          </label>
          <div v-if="operationError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ operationError }}</div>
        </div>
        <div class="flex shrink-0 justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <button class="btn btn-secondary" type="button" @click="redeemBatchOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" type="submit" :disabled="saving">{{ mr('actions.apply') }}</button>
        </div>
      </form>
    </div>

    <div v-if="extendOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <form class="flex max-h-[calc(100dvh-1rem)] w-full max-w-md flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]" @submit.prevent="submitExtendAssigned">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('extend.title') }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="extendOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="min-h-0 flex-1 space-y-3 overflow-y-auto overscroll-contain p-4">
          <div class="text-sm text-gray-500 dark:text-dark-400">{{ extendTarget?.user_email || extendTarget?.user_id }} · {{ extendTarget?.group_name || extendTarget?.group_id }}</div>
          <label class="block">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('extend.days') }}</span>
            <input v-model.number="extendDays" type="number" min="1" max="3650" class="input" required />
          </label>
          <div v-if="operationError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ operationError }}</div>
        </div>
        <div class="flex shrink-0 justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <button class="btn btn-secondary" type="button" @click="extendOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" type="submit" :disabled="saving">{{ mr('actions.extend') }}</button>
        </div>
      </form>
    </div>

    <div v-if="bulkAssignOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <form class="flex max-h-[calc(100dvh-1rem)] w-full max-w-2xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]" @submit.prevent="submitBulkAssign">
        <div class="flex shrink-0 items-center justify-between border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('batch.bulkAssign') }}</h2>
          <button class="btn btn-sm btn-secondary" type="button" @click="bulkAssignOpen = false">{{ t('common.close') }}</button>
        </div>
        <div class="grid min-h-0 flex-1 gap-4 overflow-y-auto overscroll-contain p-4 md:grid-cols-2">
          <label class="block md:col-span-2">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('batch.emails') }}</span>
            <textarea v-model="bulkAssignForm.emails_text" class="input min-h-28" placeholder="one@example.com&#10;two@example.com"></textarea>
          </label>
          <label class="block md:col-span-2">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('batch.userIds') }}</span>
            <input v-model.trim="bulkAssignForm.user_ids_text" class="input" placeholder="101, 102, 103" />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.group') }}</span>
            <Select v-model="bulkAssignForm.group_id" :options="subscriptionGroupSelectOptions" :placeholder="mr('fields.selectSubscriptionGroup')" :disabled="referenceLoading" searchable />
          </label>
          <label class="block">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.validityDays') }}</span>
            <input v-model.number="bulkAssignForm.validity_days" type="number" min="1" class="input" required />
          </label>
          <label class="block md:col-span-2">
            <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.notes') }}</span>
            <textarea v-model="bulkAssignForm.notes" class="input min-h-20"></textarea>
          </label>
          <div v-if="bulkAssignResult" class="rounded-md bg-gray-50 p-3 text-sm text-gray-700 dark:bg-dark-900 dark:text-dark-100 md:col-span-2">
            {{ mr('batch.result', {
              created: bulkAssignResult.created_count || bulkAssignResult.success_count || bulkAssignResult.items?.length || 0,
              failed: bulkAssignResult.failed_count || bulkAssignResult.errors?.length || 0,
            }) }}
          </div>
          <div v-if="operationError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200 md:col-span-2">{{ operationError }}</div>
        </div>
        <div class="flex shrink-0 justify-end gap-2 border-t border-gray-200 p-4 dark:border-dark-700">
          <button class="btn btn-secondary" type="button" @click="bulkAssignOpen = false">{{ t('common.cancel') }}</button>
          <button class="btn btn-primary" type="submit" :disabled="saving">{{ mr('actions.assign') }}</button>
        </div>
      </form>
    </div>

    <div v-if="proxySourcesOpen" class="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-2 sm:p-4">
      <div class="flex max-h-[calc(100dvh-1rem)] w-full max-w-5xl flex-col overflow-hidden rounded-lg bg-white shadow-xl dark:bg-dark-800 sm:max-h-[calc(100dvh-2rem)]">
        <div class="flex shrink-0 flex-wrap items-center justify-between gap-2 border-b border-gray-200 p-4 dark:border-dark-700">
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ mr('sources.title') }}</h2>
          <div class="flex items-center gap-2">
            <button
              class="btn btn-sm btn-secondary"
              type="button"
              :disabled="proxySourcesSyncingAll || proxySourcesLoading || proxySources.length === 0"
              :title="mr('actions.syncAllSources')"
              @click="syncAllProxySourceItems"
            >
              <Icon name="refresh" size="sm" :class="['md:mr-2', proxySourcesSyncingAll ? 'animate-spin' : '']" />
              <span class="hidden md:inline">{{ mr('actions.syncAllSources') }}</span>
            </button>
            <button class="btn btn-sm btn-secondary" type="button" @click="proxySourcesOpen = false">{{ t('common.close') }}</button>
          </div>
        </div>
        <div v-if="proxySourceSyncAllSummary" class="shrink-0 border-b border-gray-200 bg-gray-50 px-4 py-2 text-xs text-gray-600 dark:border-dark-700 dark:bg-dark-900/60 dark:text-dark-200">
          {{ proxySourceSyncAllSummary }}
        </div>
        <div class="grid min-h-0 flex-1 gap-4 overflow-y-auto overscroll-contain p-4 lg:grid-cols-[minmax(0,1fr)_320px]">
          <div class="min-w-0 overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700">
            <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
              <thead class="bg-gray-50 dark:bg-dark-900/70">
                <tr>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('table.name') }}</th>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('columns.visibility') }}</th>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('sources.nodes') }}</th>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('table.interval') }}</th>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('sources.quota') }}</th>
                  <th class="px-3 py-3 text-left font-medium text-gray-500 dark:text-dark-300">{{ mr('table.status') }}</th>
                  <th class="px-3 py-3 text-right font-medium text-gray-500 dark:text-dark-300">{{ mr('table.actions') }}</th>
                </tr>
              </thead>
              <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                <tr v-if="proxySourcesLoading">
                  <td colspan="7" class="px-3 py-8 text-center text-gray-500">{{ mr('table.loading') }}</td>
                </tr>
                <tr v-else-if="proxySources.length === 0">
                  <td colspan="7" class="px-3 py-8 text-center text-gray-500">{{ mr('table.noSources') }}</td>
                </tr>
                <tr v-for="source in proxySources" v-else :key="source.id">
                  <td class="max-w-[260px] px-3 py-3">
                    <div class="truncate font-medium text-gray-900 dark:text-white">{{ source.name }}</div>
                    <div class="truncate text-xs text-gray-500 dark:text-dark-400">{{ source.subscription_url }}</div>
                  </td>
                  <td class="px-3 py-3"><span :class="['badge', source.is_public ? 'badge-success' : 'badge-gray']">{{ mr(source.is_public ? 'states.public' : 'states.private') }}</span></td>
                  <td class="whitespace-nowrap px-3 py-3 text-gray-700 dark:text-dark-100">
                    <button
                      type="button"
                      class="text-primary-600 hover:underline dark:text-primary-400"
                      :title="mr('sources.filterBySource')"
                      @click="filterProxiesBySource(source)"
                    >
                      {{ mr('sources.nodeCount', { active: numberValue(source.active_node_count, 0), total: numberValue(source.node_count, 0) }) }}
                    </button>
                  </td>
                  <td class="whitespace-nowrap px-3 py-3 text-gray-700 dark:text-dark-100">
                    <div>{{ source.refresh_interval_minutes || 0 }}m</div>
                    <div class="text-xs text-gray-500 dark:text-dark-400">
                      {{ source.sync_enabled === false ? mr('sources.syncPaused') : mr('sources.nextSyncAt', { time: formatResourceDate(source.next_sync_at) }) }}
                    </div>
                  </td>
                  <td class="whitespace-nowrap px-3 py-3 text-gray-700 dark:text-dark-100">
                    <div>{{ proxySourceTrafficText(source) }}</div>
                    <div class="text-xs text-gray-500 dark:text-dark-400">{{ mr('sources.expiresAt', { time: formatResourceDate(source.sub_expires_at) }) }}</div>
                  </td>
                  <td class="px-3 py-3 text-gray-700 dark:text-dark-100">
                    <div>{{ source.last_sync_status || '-' }}</div>
                    <div v-if="source.last_sync_error" class="line-clamp-2 text-xs text-red-600 dark:text-red-300">{{ source.last_sync_error }}</div>
                  </td>
                  <td class="px-3 py-3 text-right">
                    <div class="flex justify-end gap-2">
                      <button class="btn btn-xs btn-secondary" type="button" @click="syncProxySourceItem(source)">{{ mr('actions.sync') }}</button>
                      <button class="btn btn-xs btn-secondary" type="button" @click="toggleProxySourceSync(source)">
                        {{ source.sync_enabled === false ? mr('actions.resumeSync') : mr('actions.pauseSync') }}
                      </button>
                      <button class="btn btn-xs btn-secondary" type="button" @click="editProxySource(source)">{{ t('common.edit') }}</button>
                      <button class="btn btn-xs btn-danger" type="button" @click="deleteProxySource(source)">{{ t('common.delete') }}</button>
                    </div>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <form class="space-y-3 rounded-lg border border-gray-200 p-3 dark:border-dark-700" @submit.prevent="saveProxySource">
            <div class="flex items-center justify-between gap-2">
              <h3 class="font-medium text-gray-900 dark:text-white">{{ proxySourceEditingID ? mr('actions.editSource') : mr('actions.addSource') }}</h3>
              <button v-if="proxySourceEditingID" class="btn btn-xs btn-secondary" type="button" @click="resetProxySourceForm">{{ t('common.cancel') }}</button>
            </div>
            <label class="block">
              <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.name') }}</span>
              <input v-model.trim="proxySourceForm.name" class="input" required />
            </label>
            <label class="block">
              <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.subscriptionUrl') }}</span>
              <input v-model.trim="proxySourceForm.subscription_url" class="input" type="url" required />
            </label>
            <label class="block">
              <span class="mb-1 block text-sm text-gray-700 dark:text-dark-100">{{ mr('fields.refreshIntervalMinutes') }}</span>
              <input v-model.number="proxySourceForm.refresh_interval_minutes" class="input" type="number" min="5" required />
            </label>
            <label class="flex cursor-pointer items-center gap-3 rounded-lg border border-gray-200 px-3 py-3 dark:border-dark-700">
              <input v-model="proxySourceForm.is_public" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
              <span class="text-sm font-medium text-gray-700 dark:text-dark-100">{{ mr('fields.publicForAllUsers') }}</span>
            </label>
            <label class="flex cursor-pointer items-center gap-3 rounded-lg border border-gray-200 px-3 py-3 dark:border-dark-700">
              <input v-model="proxySourceForm.sync_enabled" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />
              <span class="text-sm font-medium text-gray-700 dark:text-dark-100">{{ mr('fields.autoSyncEnabled') }}</span>
            </label>
            <div v-if="operationError" class="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/30 dark:text-red-200">{{ operationError }}</div>
            <button class="btn btn-primary w-full" type="submit" :disabled="saving">{{ proxySourceEditingID ? mr('actions.updateSource') : mr('actions.saveSource') }}</button>
          </form>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import GroupCapacityBadge from '@/components/common/GroupCapacityBadge.vue'
import Pagination from '@/components/common/Pagination.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import type { Column } from '@/components/common/types'
import Icon from '@/components/icons/Icon.vue'
import MyProxyEditorDialog from '@/components/user/MyProxyEditorDialog.vue'
import { myResourcesApi, type ResourceItem, type ResourcePage, type UserOAuthCredentialsResult, type UserProxyTestResult } from '@/api/myResources'
import { useAppStore } from '@/stores/app'
import type { GroupPlatform, ProxyQualityCheckResult, ProxySourceSyncAllResult } from '@/types'
import {
  USER_ACCOUNT_STATUS_OPTIONS,
  USER_GROUP_STATUS_OPTIONS,
  USER_GROUP_SUBSCRIPTION_TYPE_OPTIONS,
  getUserAccountTypeOptions,
} from '@/utils/userResourceOptions'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatBytes, formatDateTime } from '@/utils/format'
import {
  buildModelAllowlistConfig,
  createModelAllowlistState,
  hydrateModelAllowlistState,
  invertModelAllowlistSelection,
  moveModelAllowlistItem,
  selectAllModelAllowlistItems,
  type ModelAllowlistState,
} from '@/views/admin/groupModelAllowlist'

type ResourceKind =
  | 'groups'
  | 'accounts'
  | 'proxies'
  | 'assigned-subscriptions'
  | 'redeem-codes'
  | 'account-logs'
  | 'upstream-errors'

interface ColumnConfig {
  key: string
  label: string
  badge?: boolean
  class?: string
}

interface PageConfig {
  title: string
  subtitle: string
  columns: ColumnConfig[]
  create?: boolean
  edit?: boolean
  delete?: boolean
  createLabel?: string
  defaultPayload?: ResourceItem
}

interface GroupOverrideRow {
  user_id: number
  user_name: string
  user_email: string
  rate_multiplier: number | string
  rpm_override: number | string
}

const route = useRoute()
const appStore = useAppStore()
const { t } = useI18n()
const mr = (key: string, params?: Record<string, unknown>) => t(`myResources.${key}`, params || {})
const resource = computed(() => String(route.meta.resource || 'groups') as ResourceKind)

const configs = computed<Record<ResourceKind, PageConfig>>(() => ({
  groups: {
    title: mr('pages.groups.title'),
    subtitle: mr('pages.groups.subtitle'),
    create: true,
    edit: true,
    delete: true,
    createLabel: mr('pages.groups.create'),
    defaultPayload: { name: '', platform: 'anthropic', subscription_type: 'subscription', status: 'active', rate_multiplier: 1 },
    columns: [
      { key: 'name', label: mr('columns.name') },
      { key: 'platform', label: mr('columns.platform'), badge: true },
      { key: 'subscription_type', label: mr('columns.billingType') },
      { key: 'rate_multiplier', label: mr('columns.rateMultiplier') },
      { key: 'is_exclusive', label: mr('columns.groupType') },
      { key: 'account_count', label: mr('columns.accountCount') },
      { key: 'capacity', label: mr('columns.capacity') },
      { key: 'usage', label: mr('columns.usage') },
      { key: 'status', label: mr('columns.status'), badge: true },
    ],
  },
  accounts: {
    title: mr('pages.accounts.title'),
    subtitle: mr('pages.accounts.subtitle'),
    create: true,
    edit: true,
    delete: true,
    createLabel: mr('pages.accounts.create'),
    defaultPayload: { name: '', platform: 'anthropic', type: 'oauth', credentials: {}, group_ids: [], status: 'active', schedulable: true },
    columns: [
      { key: 'name', label: mr('columns.name') },
      { key: 'platform', label: mr('columns.platform'), badge: true },
      { key: 'type', label: mr('columns.type') },
      { key: 'status', label: mr('columns.status'), badge: true },
      { key: 'schedulable', label: mr('columns.schedulable') },
      { key: 'groups', label: mr('columns.group') },
      { key: 'proxy_name', label: mr('columns.proxy') },
      { key: 'priority', label: mr('columns.priority') },
      { key: 'today_request_count', label: mr('columns.todayStats') },
      { key: 'last_used_at', label: mr('columns.lastUsed') },
      { key: 'expires_at', label: mr('columns.expiresAt') },
      { key: 'notes', label: mr('columns.notes') },
    ],
  },
  proxies: {
    title: mr('pages.proxies.title'),
    subtitle: mr('pages.proxies.subtitle'),
    create: true,
    edit: true,
    delete: true,
    createLabel: mr('pages.proxies.create'),
    defaultPayload: { name: '', kind: 'standard', protocol: 'socks5', host: '', port: 1080, status: 'active' },
    columns: [
      { key: 'name', label: mr('columns.name') },
      { key: 'visibility', label: mr('columns.visibility') },
      { key: 'kind', label: mr('columns.proxyMode'), badge: true },
      { key: 'protocol', label: mr('columns.protocol') },
      { key: 'address', label: mr('columns.address') },
      { key: 'auth', label: mr('columns.auth') },
      { key: 'location', label: mr('columns.location') },
      { key: 'account_count', label: mr('columns.accountCount') },
      { key: 'latency', label: mr('columns.latency') },
      { key: 'expiry', label: mr('columns.expiry') },
      { key: 'created_at', label: mr('columns.createdAt') },
      { key: 'status', label: mr('columns.status'), badge: true },
    ],
  },
  'assigned-subscriptions': {
    title: mr('pages.assignedSubscriptions.title'),
    subtitle: mr('pages.assignedSubscriptions.subtitle'),
    create: true,
    createLabel: mr('pages.assignedSubscriptions.create'),
    defaultPayload: { email: '', group_id: 0, validity_days: 30, notes: '' },
    columns: [
      { key: 'user_email', label: mr('columns.user') },
      { key: 'group_name', label: mr('columns.group') },
      { key: 'group_platform', label: mr('columns.platform'), badge: true },
      { key: 'status', label: mr('columns.status'), badge: true },
      { key: 'expires_at', label: mr('columns.expiresAt') },
      { key: 'source_type', label: mr('columns.source') },
      { key: 'daily_usage_usd', label: mr('columns.dailyUsage') },
      { key: 'monthly_usage_usd', label: mr('columns.monthlyUsage') },
      { key: 'notes', label: mr('columns.notes') },
    ],
  },
  'redeem-codes': {
    title: mr('pages.redeemCodes.title'),
    subtitle: mr('pages.redeemCodes.subtitle'),
    create: true,
    delete: true,
    createLabel: mr('pages.redeemCodes.create'),
    defaultPayload: { group_id: 0, validity_days: 30, count: 1, repeatable: false, max_uses: 2, notes: '' },
    columns: [
      { key: 'code', label: mr('columns.code') },
      { key: 'status', label: mr('columns.status'), badge: true },
      { key: 'group_name', label: mr('columns.group') },
      { key: 'validity_days', label: mr('columns.validityDays') },
      { key: 'expires_at', label: mr('columns.redeemDeadline') },
      { key: 'usage_count', label: mr('columns.redemptions') },
      { key: 'notes', label: mr('columns.notes') },
    ],
  },
  'account-logs': {
    title: mr('pages.accountLogs.title'),
    subtitle: mr('pages.accountLogs.subtitle'),
    columns: [
      { key: 'created_at', label: mr('columns.time') },
      { key: 'account_name', label: mr('columns.account') },
      { key: 'group_name', label: mr('columns.group') },
      { key: 'model', label: mr('columns.model') },
      { key: 'input_tokens', label: mr('columns.inputTokens') },
      { key: 'output_tokens', label: mr('columns.outputTokens') },
      { key: 'total_cost', label: mr('columns.cost') },
      { key: 'request_id', label: mr('columns.requestId') },
    ],
  },
  'upstream-errors': {
    title: mr('pages.upstreamErrors.title'),
    subtitle: mr('pages.upstreamErrors.subtitle'),
    columns: [
      { key: 'created_at', label: mr('columns.time') },
      { key: 'account_name', label: mr('columns.account') },
      { key: 'group_name', label: mr('columns.group') },
      { key: 'platform', label: mr('columns.platform'), badge: true },
      { key: 'status_code', label: mr('columns.statusCode') },
      { key: 'type', label: mr('columns.type') },
      { key: 'message', label: mr('columns.error') },
      { key: 'request_id', label: mr('columns.requestId') },
    ],
  },
}))

const config = computed(() => configs.value[resource.value])
const loading = ref(false)
const saving = ref(false)
const items = ref<ResourceItem[]>([])
const page = reactive<ResourcePage>({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })
const filters = reactive({
  search: '',
  status: '',
  platform: '',
  type: '',
  protocol: '',
  source_id: '',
  user_id: '',
  api_key_id: '',
  account_id: '',
  start_date: '',
  end_date: '',
})
const selectedIds = ref<number[]>([])
const testingProxyIds = ref<Set<number>>(new Set())
const qualityCheckingProxyIds = ref<Set<number>>(new Set())
const batchTesting = ref(false)
const batchQualityChecking = ref(false)
const proxyBatchProgress = reactive({ completed: 0, total: 0 })
const groupOptions = ref<ResourceItem[]>([])
const proxyOptions = ref<ResourceItem[]>([])
const subscriptionGroupOptions = computed(() => groupOptions.value.filter(group => group.subscription_type === 'subscription'))
const openAIGroupOptions = computed(() => groupOptions.value.filter(group => group.platform === 'openai'))
const platformOptions: SelectOption[] = [
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'openai', label: 'OpenAI' },
  { value: 'gemini', label: 'Gemini' },
  { value: 'antigravity', label: 'Antigravity' },
  { value: 'grok', label: 'Grok' },
  { value: 'kimi', label: 'Kimi' },
  { value: 'zhipu', label: 'Zhipu GLM' },
  { value: 'deepseek', label: 'DeepSeek' },
]
const platformFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: mr('filters.allPlatforms') },
  ...platformOptions,
])
const proxyKindOptions = computed<SelectOption[]>(() => [
  { value: 'standard', label: mr('states.standard') },
  { value: 'xray', label: 'Xray' },
])
const proxyKindFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: mr('filters.allProxyKinds') },
  ...proxyKindOptions.value,
])
const standardProxyProtocolOptions: SelectOption[] = [
  { value: 'http', label: 'HTTP' },
  { value: 'https', label: 'HTTPS' },
  { value: 'socks5', label: 'SOCKS5' },
  { value: 'socks5h', label: 'SOCKS5H' },
]
const xrayProxyProtocolOptions: SelectOption[] = [
  { value: 'vmess', label: 'VMess' },
  { value: 'vless', label: 'VLESS' },
  { value: 'trojan', label: 'Trojan' },
  { value: 'ss', label: 'Shadowsocks' },
  { value: 'hysteria', label: 'Hysteria' },
  { value: 'hysteria2', label: 'Hysteria2' },
  { value: 'tuic', label: 'TUIC' },
  { value: 'anytls', label: 'AnyTLS' },
  { value: 'naive', label: 'Naive' },
  { value: 'wireguard', label: 'WireGuard' },
]
const proxyProtocolFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: mr('filters.allProtocols') },
  ...standardProxyProtocolOptions,
  ...xrayProxyProtocolOptions,
])
const showPlatformFilter = computed(() => ['groups', 'accounts', 'assigned-subscriptions', 'account-logs', 'upstream-errors'].includes(resource.value))
const showStatusFilter = computed(() => ['groups', 'accounts', 'proxies', 'assigned-subscriptions', 'redeem-codes'].includes(resource.value))
const searchPlaceholder = computed(() => mr(`filters.searchByResource.${resource.value}`))
const statusFilterOptions = computed<SelectOption[]>(() => {
  const values: Record<ResourceKind, string[]> = {
    groups: ['active', 'inactive'],
    accounts: ['active', 'inactive', 'disabled', 'error'],
    proxies: ['active', 'inactive', 'disabled', 'expired'],
    'assigned-subscriptions': ['active', 'revoked', 'expired'],
    'redeem-codes': ['unused', 'used', 'expired', 'disabled'],
    'account-logs': [],
    'upstream-errors': [],
  }
  return [
    { value: '', label: mr('filters.allStatuses') },
    ...values[resource.value].map(value => ({ value, label: mr(`states.${value}`) })),
  ]
})
const hasActiveFilters = computed(() => Object.values(filters).some(value => String(value).trim() !== ''))
const referenceLoading = ref(false)
const hiddenColumns = ref<Set<string>>(new Set())
const showColumnSettings = ref(false)
const columnSettingsRef = ref<HTMLElement | null>(null)
const editorOpen = ref(false)
const editorText = ref('')
const editorError = ref('')
const editingId = ref<number | null>(null)
const proxyEditorItem = ref<ResourceItem | null>(null)
const proxyEditorInitialMode = ref<'standard' | 'batch'>('standard')
const proxyUsernameDirty = ref(false)
const proxyPasswordDirty = ref(false)
const proxyExtraDirty = ref(false)
const editorMode = ref<'default' | 'accountImport' | 'proxyImport' | 'codexSessionImport' | 'codexPATImport'>('default')
const operationError = ref('')
const groupLiveCapability = ref<{ supported: boolean; reason?: string } | null>(null)
const recordDetailOpen = ref(false)
const recordDetailItem = ref<ResourceItem | null>(null)
const groupOverridesOpen = ref(false)
const groupOverridesLoading = ref(false)
const groupOverridesTarget = ref<ResourceItem | null>(null)
const groupOverrideRows = ref<GroupOverrideRow[]>([])
const accountOAuth = reactive({
  auth_url: '',
  session_id: '',
  state: '',
  callback: '',
  session_key: '',
  project_id: '',
  oauth_type: 'code_assist',
  tier_id: '',
  loading: false,
  error: '',
})
const codexImportForm = reactive({
  content: '',
  access_token: '',
  name: '',
  notes: '',
  group_ids: [] as number[],
  proxy_id: 0,
  concurrency: 3,
  priority: 50,
  rate_multiplier: 1,
  expires_at: '',
  auto_pause_on_expired: true,
})
const accountBatchOpen = ref(false)
const accountBatchForm = reactive({
  update_schedulable: true,
  schedulable: true,
  update_status: false,
  status: 'active',
  update_priority: false,
  priority: 0,
  update_notes: false,
  notes: '',
})
const redeemBatchOpen = ref(false)
const redeemBatchForm = reactive({
  update_status: false,
  status: 'unused',
  update_validity_days: false,
  validity_days: 30,
  update_expires_at: false,
  expires_at: '',
  update_notes: true,
  notes: '',
})
const redeemStats = ref<ResourceItem | null>(null)
const redeemUsageOpen = ref(false)
const redeemUsageLoading = ref(false)
const redeemUsageTarget = ref<ResourceItem | null>(null)
const redeemUsages = ref<ResourceItem[]>([])
const redeemUsageError = ref('')
const accountUsageStats = ref<ResourceItem | null>(null)
const groupModelAllowlistState = reactive<ModelAllowlistState>(createModelAllowlistState())
const groupModelCandidatesLoading = ref(false)
const groupModelInput = ref('')
const extendOpen = ref(false)
const extendTarget = ref<ResourceItem | null>(null)
const extendDays = ref(30)
const bulkAssignOpen = ref(false)
const bulkAssignResult = ref<ResourceItem | null>(null)
const bulkAssignForm = reactive({
  emails_text: '',
  user_ids_text: '',
  group_id: 0,
  validity_days: 30,
  notes: '',
})
const proxySourcesOpen = ref(false)
const proxySourcesLoading = ref(false)
const proxySourcesSyncingAll = ref(false)
const proxySourceSyncAllResult = ref<ProxySourceSyncAllResult | null>(null)
const proxySources = ref<ResourceItem[]>([])
const proxySourceEditingID = ref<number | null>(null)
const proxySourceForm = reactive({
  name: '',
  subscription_url: '',
  refresh_interval_minutes: 1440,
  is_public: false,
  sync_enabled: true,
})
// Source picker for the proxy filter bar. Rendered only when at least one source
// exists, so the bare "all sources" entry never shows up as a dead control.
const proxySourceFilterOptions = computed<SelectOption[]>(() => [
  { value: '', label: mr('filters.allSources') },
  ...proxySources.value.map(source => ({
    value: String(source.id),
    label: stringValue(source.name) || `#${source.id}`,
  })),
])
// One-line recap of the last "sync every source" run. Counts only: node payloads
// and subscription URLs must never reach this strip.
const proxySourceSyncAllSummary = computed<string>(() => {
  const result = proxySourceSyncAllResult.value
  if (!result) return ''
  return mr('sources.syncAllSummary', {
    total: numberValue(result.total, 0),
    success: numberValue(result.success_count, 0),
    partial: numberValue(result.partial_count, 0),
    failed: numberValue(result.failed_count, 0),
    skipped: numberValue(result.skipped_count, 0),
    deferred: numberValue(result.deferred_count, 0),
    created: numberValue(result.created_count, 0),
    updated: numberValue(result.updated_count, 0),
  })
})
const editorForm = reactive({
  group: {
    name: '',
    description: '',
    platform: 'anthropic',
    subscription_type: 'subscription',
    status: 'active',
    rate_multiplier: 1,
    rpm_limit: 0,
    daily_limit_usd: 0,
    weekly_limit_usd: 0,
    monthly_limit_usd: 0,
    default_validity_days: 30,
    fallback_group_id: 0,
    fallback_group_id_on_invalid_request: 0,
    peak_rate_enabled: false,
    peak_start: '',
    peak_end: '',
    peak_rate_multiplier: 1,
    model_routing_enabled: false,
    model_routing_text: '{}',
    supported_model_scopes_text: '[]',
    allow_messages_dispatch: false,
    allow_live: false,
    require_oauth_only: false,
    require_privacy_set: false,
    default_mapped_model: '',
    messages_dispatch_model_config_text: '{}',
    model_allowlist_text: '{}',
    claude_code_only: false,
    is_exclusive: false,
    mcp_xml_inject: false,
    allow_image_generation: false,
    allow_batch_image_generation: false,
    image_rate_independent: false,
    image_rate_multiplier: 1,
    image_price_1k: 0,
    image_price_2k: 0,
    image_price_4k: 0,
    batch_image_discount_multiplier: 0.5,
    batch_image_hold_multiplier: 0.6,
    video_rate_independent: false,
    video_rate_multiplier: 1,
    video_price_480p: 0,
    video_price_720p: 0,
    video_price_1080p: 0,
    web_search_price_per_call: '' as number | string,
    sort_order: 0,
    copy_accounts_from_group_ids: [] as number[],
  },
  account: {
    name: '',
    platform: 'anthropic',
    type: 'oauth',
    status: 'active',
    schedulable: true,
    group_ids: [] as number[],
    proxy_id: 0,
    priority: 0,
    concurrency: 3,
    load_factor: 1,
    rate_multiplier: 1,
    auto_pause_on_expired: true,
    expires_at: '',
    credentials_text: '{}',
    extra_text: '{}',
    notes: '',
  },
  proxy: {
    name: '',
    kind: 'standard',
    protocol: 'socks5',
    host: '',
    port: 1080,
    username: '',
    password: '',
    status: 'active',
    fallback_mode: 'none',
    backup_proxy_id: 0,
    expires_at: '',
    expiry_warn_days: 0,
    extra_text: '{}',
  },
  assigned: {
    email: '',
    user_id: 0,
    group_id: 0,
    validity_days: 30,
    notes: '',
  },
  redeem: {
    group_id: 0,
    count: 1,
    validity_days: 30,
    expires_at: '',
    repeatable: false,
    max_uses: 2,
    notes: '',
  },
})

const groupSubscriptionTypeOptions = computed<SelectOption[]>(() => USER_GROUP_SUBSCRIPTION_TYPE_OPTIONS.map(option => ({
  value: option.value,
  label: mr(`states.${option.value}`),
})))
const groupStatusOptions = computed<SelectOption[]>(() => USER_GROUP_STATUS_OPTIONS.map(option => ({
  value: option.value,
  label: mr(`states.${option.value}`),
})))
const accountTypeOptions = computed<SelectOption[]>(() => getUserAccountTypeOptions(editorForm.account.platform))
const accountStatusOptions = computed<SelectOption[]>(() => USER_ACCOUNT_STATUS_OPTIONS.map(option => ({
  value: option.value,
  label: mr(`states.${option.value}`),
})))
const booleanStatusOptions = computed<SelectOption[]>(() => [
  { value: true, label: mr('states.enabled') },
  { value: false, label: mr('states.disabled') },
])
const redeemStatusOptions = computed<SelectOption[]>(() => ['unused', 'expired'].map(value => ({ value, label: mr(`states.${value}`) })))
const oauthTypeOptions: SelectOption[] = [
  { value: 'code_assist', label: 'Code Assist' },
  { value: 'google_one', label: 'Google One' },
  { value: 'ai_studio', label: 'AI Studio' },
]
const fallbackGroupOptions = computed<SelectOption[]>(() => [
  { value: 0, label: mr('fields.noFallback') },
  ...groupOptions.value.map(group => ({ value: Number(group.id), label: `${group.name} · ${group.platform}` })),
])
const copyAccountGroupOptions = computed(() => groupOptions.value.filter(group =>
  group.platform === editorForm.group.platform && Number(group.id) !== Number(editingId.value || 0)))
const subscriptionGroupSelectOptions = computed<SelectOption[]>(() => subscriptionGroupOptions.value.map(group => ({
  value: Number(group.id),
  label: `${group.name} · ${group.platform}`,
})))
const accountProxyOptions = computed<SelectOption[]>(() => [
  { value: 0, label: mr('fields.noProxy') },
  ...proxyOptions.value.map(proxy => ({
    value: Number(proxy.id),
    label: proxy.details_hidden
      ? `${proxy.name} · ${String(proxy.protocol || '').toUpperCase()} · ${mr('states.publicDetailsHidden')}`
      : `${proxy.name} · ${String(proxy.protocol || '').toUpperCase()} · ${proxy.host}:${proxy.port}${proxy.is_public ? ` · ${mr('states.public')}` : ''}`,
  })),
])

const selectableResource = computed(() => ['accounts', 'proxies', 'redeem-codes'].includes(resource.value))
const selectableVisibleItems = computed(() => resource.value === 'proxies' ? items.value.filter(canMutateItem) : items.value)
const allVisibleSelected = computed(() => selectableVisibleItems.value.length > 0 && selectableVisibleItems.value.every(item => selectedIds.value.includes(Number(item.id))))
const visibleColumns = computed(() => config.value.columns.filter(column => !hiddenColumns.value.has(column.key)))
const toggleableAlignedColumns = computed(() => config.value.columns.filter(column => column.key !== 'name'))
const alignedColumns = computed<Column[]>(() => {
  const columns = visibleColumns.value.map(column => ({
    key: column.key,
    label: column.label,
    sortable: false,
    class: column.class,
  }))
  if (selectableResource.value) {
    columns.unshift({ key: 'select', label: '', sortable: false, class: undefined })
  }
  columns.push({ key: 'actions', label: mr('table.actions'), sortable: false, class: undefined })
  return columns
})
const accountOAuthEnabled = computed(() => resource.value === 'accounts' && ['oauth', 'setup-token'].includes(editorForm.account.type))
const accountOAuthSetupToken = computed(() => editorForm.account.type === 'setup-token')

function formatProxyLocation(item: ResourceItem): string {
  return [item.country, item.city].filter(Boolean).join(' · ')
}

function countryFlag(value: unknown): string {
  const code = String(value || '').trim().toUpperCase()
  if (!/^[A-Z]{2}$/.test(code)) return ''
  return String.fromCodePoint(...Array.from(code).map(char => 127397 + char.charCodeAt(0)))
}

function proxyQualityClass(status: unknown): string {
  if (status === 'healthy') return 'badge-success'
  if (status === 'warn') return 'badge-warning'
  return 'badge-danger'
}

function proxyQualityLabel(status: unknown): string {
  if (status === 'healthy') return mr('states.healthy')
  if (status === 'warn') return mr('states.warn')
  if (status === 'challenge') return mr('states.challenge')
  return mr('states.failed')
}

function groupPlatformClass(platform: string): string {
  const tone = platform === 'anthropic'
    ? 'bg-orange-100 text-orange-700 dark:bg-orange-900/30 dark:text-orange-400'
    : platform === 'openai'
      ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-400'
      : platform === 'antigravity'
        ? 'bg-purple-100 text-purple-700 dark:bg-purple-900/30 dark:text-purple-400'
        : platform === 'grok'
          ? 'bg-zinc-200 text-zinc-800 dark:bg-zinc-700 dark:text-zinc-100'
          : platform === 'kimi'
            ? 'bg-pink-100 text-pink-700 dark:bg-pink-900/30 dark:text-pink-400'
            : platform === 'zhipu'
              ? 'bg-indigo-100 text-indigo-700 dark:bg-indigo-900/30 dark:text-indigo-400'
              : platform === 'deepseek'
                ? 'bg-teal-100 text-teal-700 dark:bg-teal-900/30 dark:text-teal-400'
          : 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-400'
  return `inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium ${tone}`
}

function platformLabel(value: unknown): string {
  const platform = String(value || '')
  return platform === 'openai'
    ? 'OpenAI'
    : platform === 'anthropic'
      ? 'Anthropic'
      : platform === 'gemini'
        ? 'Gemini'
        : platform === 'antigravity'
          ? 'Antigravity'
          : platform === 'grok'
            ? 'Grok'
            : platform === 'kimi'
              ? 'Kimi'
              : platform === 'zhipu'
                ? 'Zhipu GLM'
                : platform === 'deepseek'
                  ? 'DeepSeek'
            : platform || '-'
}

function normalizeGroupPlatform(value: unknown): GroupPlatform {
  const platform = String(value || '')
  return ['anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek'].includes(platform)
    ? platform as GroupPlatform
    : 'anthropic'
}

function clearFilters(): void {
  Object.assign(filters, {
    search: '',
    status: '',
    platform: '',
    type: '',
    protocol: '',
    source_id: '',
    user_id: '',
    api_key_id: '',
    account_id: '',
    start_date: '',
    end_date: '',
  })
  page.page = 1
  void loadData()
}

let alignedSearchTimer: ReturnType<typeof setTimeout> | null = null

function handleAlignedSearch(): void {
  if (alignedSearchTimer) clearTimeout(alignedSearchTimer)
  alignedSearchTimer = setTimeout(() => {
    page.page = 1
    void loadData()
  }, 300)
}

function applyAlignedFilters(): void {
  page.page = 1
  void loadData()
}

function columnStorageKey(kind = resource.value): string {
  return `my-resource-hidden-columns:${kind}`
}

function loadColumnSettings(): void {
  try {
    const raw = window.localStorage.getItem(columnStorageKey())
    const parsed = raw ? JSON.parse(raw) : []
    const validKeys = new Set(config.value.columns.map(column => column.key))
    hiddenColumns.value = new Set(Array.isArray(parsed) ? parsed.filter(key => typeof key === 'string' && validKeys.has(key)) : [])
    hiddenColumns.value.delete('name')
  } catch {
    hiddenColumns.value = new Set()
  }
}

function saveColumnSettings(): void {
  window.localStorage.setItem(columnStorageKey(), JSON.stringify([...hiddenColumns.value]))
}

function isColumnVisible(key: string): boolean {
  return !hiddenColumns.value.has(key)
}

function toggleColumn(key: string): void {
  if (key === 'name') return
  const next = new Set(hiddenColumns.value)
  if (next.has(key)) {
    next.delete(key)
  } else {
    next.add(key)
  }
  hiddenColumns.value = next
  saveColumnSettings()
}

function closeColumnSettingsOnOutsideClick(event: MouseEvent): void {
  const target = event.target as Node | null
  if (target && columnSettingsRef.value && !columnSettingsRef.value.contains(target)) {
    showColumnSettings.value = false
  }
}

function populateEditorForm(payload: ResourceItem): void {
  editorForm.group.name = stringValue(payload.name)
  editorForm.group.description = stringValue(payload.description)
  editorForm.group.platform = stringValue(payload.platform, 'anthropic')
  editorForm.group.subscription_type = stringValue(payload.subscription_type, 'subscription')
  editorForm.group.status = stringValue(payload.status, 'active')
  editorForm.group.rate_multiplier = numberValue(payload.rate_multiplier, 1)
  editorForm.group.rpm_limit = numberValue(payload.rpm_limit, 0)
  editorForm.group.daily_limit_usd = numberValue(payload.daily_limit_usd, 0)
  editorForm.group.weekly_limit_usd = numberValue(payload.weekly_limit_usd, 0)
  editorForm.group.monthly_limit_usd = numberValue(payload.monthly_limit_usd, 0)
  editorForm.group.default_validity_days = numberValue(payload.default_validity_days, 30)
  editorForm.group.fallback_group_id = numberValue(payload.fallback_group_id, 0)
  editorForm.group.fallback_group_id_on_invalid_request = numberValue(payload.fallback_group_id_on_invalid_request, 0)
  editorForm.group.peak_rate_enabled = Boolean(payload.peak_rate_enabled)
  editorForm.group.peak_start = stringValue(payload.peak_start)
  editorForm.group.peak_end = stringValue(payload.peak_end)
  editorForm.group.peak_rate_multiplier = numberValue(payload.peak_rate_multiplier, 1)
  editorForm.group.model_routing_enabled = Boolean(payload.model_routing_enabled)
  editorForm.group.model_routing_text = JSON.stringify(payload.model_routing || {}, null, 2)
  editorForm.group.supported_model_scopes_text = JSON.stringify(payload.supported_model_scopes || [], null, 2)
  editorForm.group.allow_messages_dispatch = Boolean(payload.allow_messages_dispatch)
  editorForm.group.allow_live = Boolean(payload.allow_live)
  editorForm.group.require_oauth_only = Boolean(payload.require_oauth_only)
  editorForm.group.require_privacy_set = Boolean(payload.require_privacy_set)
  editorForm.group.default_mapped_model = stringValue(payload.default_mapped_model)
  editorForm.group.messages_dispatch_model_config_text = JSON.stringify(payload.messages_dispatch_model_config || {}, null, 2)
  editorForm.group.model_allowlist_text = JSON.stringify(payload.model_allowlist || {}, null, 2)
  Object.assign(groupModelAllowlistState, hydrateModelAllowlistState(payload.model_allowlist || {}, []))
  editorForm.group.copy_accounts_from_group_ids = idsArray(payload.copy_accounts_from_group_ids)
  editorForm.group.claude_code_only = Boolean(payload.claude_code_only)
  editorForm.group.is_exclusive = Boolean(payload.is_exclusive)
  editorForm.group.mcp_xml_inject = Boolean(payload.mcp_xml_inject)
  editorForm.group.allow_image_generation = Boolean(payload.allow_image_generation)
  editorForm.group.allow_batch_image_generation = Boolean(payload.allow_batch_image_generation)
  editorForm.group.image_rate_independent = Boolean(payload.image_rate_independent)
  editorForm.group.image_rate_multiplier = numberValue(payload.image_rate_multiplier, 1)
  editorForm.group.image_price_1k = numberValue(payload.image_price_1k, 0)
  editorForm.group.image_price_2k = numberValue(payload.image_price_2k, 0)
  editorForm.group.image_price_4k = numberValue(payload.image_price_4k, 0)
  editorForm.group.batch_image_discount_multiplier = numberValue(payload.batch_image_discount_multiplier, 0.5)
  editorForm.group.batch_image_hold_multiplier = numberValue(payload.batch_image_hold_multiplier, 0.6)
  editorForm.group.video_rate_independent = Boolean(payload.video_rate_independent)
  editorForm.group.video_rate_multiplier = numberValue(payload.video_rate_multiplier, 1)
  editorForm.group.video_price_480p = numberValue(payload.video_price_480p, 0)
  editorForm.group.video_price_720p = numberValue(payload.video_price_720p, 0)
  editorForm.group.video_price_1080p = numberValue(payload.video_price_1080p, 0)
  editorForm.group.web_search_price_per_call = payload.web_search_price_per_call == null ? '' : numberValue(payload.web_search_price_per_call, 0.01)
  editorForm.group.sort_order = numberValue(payload.sort_order, 0)

  editorForm.account.name = stringValue(payload.name)
  editorForm.account.platform = stringValue(payload.platform, 'anthropic')
  editorForm.account.type = stringValue(payload.type, 'oauth')
  editorForm.account.status = stringValue(payload.status, 'active')
  editorForm.account.schedulable = Boolean(payload.schedulable ?? true)
  editorForm.account.group_ids = idsArray(payload.group_ids ?? payload.groups)
  editorForm.account.proxy_id = numberValue(payload.proxy_id, 0)
  editorForm.account.priority = numberValue(payload.priority, 0)
  editorForm.account.concurrency = numberValue(payload.concurrency, 3)
  editorForm.account.load_factor = numberValue(payload.load_factor, 1)
  editorForm.account.rate_multiplier = numberValue(payload.rate_multiplier, 1)
  editorForm.account.auto_pause_on_expired = Boolean(payload.auto_pause_on_expired ?? true)
  editorForm.account.expires_at = toDateTimeLocal(payload.expires_at)
  editorForm.account.credentials_text = payload.credentials_redacted ? '' : JSON.stringify(payload.credentials || {}, null, 2)
  editorForm.account.extra_text = JSON.stringify(payload.extra || {}, null, 2)
  editorForm.account.notes = stringValue(payload.notes)

  editorForm.proxy.name = stringValue(payload.name)
  editorForm.proxy.kind = stringValue(payload.kind, 'standard')
  editorForm.proxy.protocol = stringValue(payload.protocol, 'socks5')
  editorForm.proxy.host = stringValue(payload.host)
  editorForm.proxy.port = numberValue(payload.port, 1080)
  editorForm.proxy.username = stringValue(payload.username)
  editorForm.proxy.password = stringValue(payload.password)
  editorForm.proxy.status = stringValue(payload.status, 'active')
  editorForm.proxy.fallback_mode = stringValue(payload.fallback_mode, 'none')
  editorForm.proxy.backup_proxy_id = numberValue(payload.backup_proxy_id, 0)
  editorForm.proxy.expires_at = toDateTimeLocal(payload.expires_at)
  editorForm.proxy.expiry_warn_days = numberValue(payload.expiry_warn_days, 0)
  editorForm.proxy.extra_text = JSON.stringify(payload.extra || {}, null, 2)

  editorForm.assigned.email = stringValue(payload.email)
  editorForm.assigned.user_id = numberValue(payload.user_id, 0)
  editorForm.assigned.group_id = numberValue(payload.group_id, 0)
  editorForm.assigned.validity_days = numberValue(payload.validity_days, 30)
  editorForm.assigned.notes = stringValue(payload.notes)

  editorForm.redeem.group_id = numberValue(payload.group_id, 0)
  editorForm.redeem.count = numberValue(payload.count, 1)
  editorForm.redeem.validity_days = numberValue(payload.validity_days, 30)
  editorForm.redeem.expires_at = toDateTimeLocal(payload.expires_at)
  editorForm.redeem.repeatable = Boolean(payload.repeatable)
  editorForm.redeem.max_uses = numberValue(payload.max_uses, 2)
  editorForm.redeem.notes = stringValue(payload.notes)
}

function mergeEditorFormPayload(payload: ResourceItem): ResourceItem {
  if (editorMode.value !== 'default') return payload
  const out: ResourceItem = { ...payload }
  if (resource.value === 'groups') {
    out.name = editorForm.group.name
    out.description = editorForm.group.description
    out.platform = editorForm.group.platform
    out.subscription_type = editorForm.group.subscription_type
    out.status = editorForm.group.status
    out.rate_multiplier = Number(editorForm.group.rate_multiplier || 0)
    out.rpm_limit = Number(editorForm.group.rpm_limit || 0)
    out.daily_limit_usd = nullablePositiveNumber(editorForm.group.daily_limit_usd)
    out.weekly_limit_usd = nullablePositiveNumber(editorForm.group.weekly_limit_usd)
    out.monthly_limit_usd = nullablePositiveNumber(editorForm.group.monthly_limit_usd)
    out.default_validity_days = Number(editorForm.group.default_validity_days || 30)
    out.fallback_group_id = editorForm.group.fallback_group_id > 0 ? Number(editorForm.group.fallback_group_id) : null
    out.fallback_group_id_on_invalid_request = editorForm.group.fallback_group_id_on_invalid_request > 0 ? Number(editorForm.group.fallback_group_id_on_invalid_request) : null
    out.peak_rate_enabled = editorForm.group.peak_rate_enabled
    out.peak_start = editorForm.group.peak_start
    out.peak_end = editorForm.group.peak_end
    out.peak_rate_multiplier = Number(editorForm.group.peak_rate_multiplier || 1)
    out.model_routing_enabled = editorForm.group.model_routing_enabled
    out.model_routing = parseJSONField(editorForm.group.model_routing_text, {})
    out.supported_model_scopes = parseJSONField(editorForm.group.supported_model_scopes_text, [])
    out.allow_messages_dispatch = editorForm.group.allow_messages_dispatch
    out.allow_live = editorForm.group.allow_live
    out.require_oauth_only = editorForm.group.require_oauth_only
    out.require_privacy_set = editorForm.group.require_privacy_set
    out.default_mapped_model = editorForm.group.default_mapped_model
    out.messages_dispatch_model_config = parseJSONField(editorForm.group.messages_dispatch_model_config_text, {})
    out.model_allowlist = buildModelAllowlistConfig(groupModelAllowlistState)
    if (!editingId.value && editorForm.group.copy_accounts_from_group_ids.length) {
      out.copy_accounts_from_group_ids = editorForm.group.copy_accounts_from_group_ids.map(Number)
    }
    out.claude_code_only = editorForm.group.claude_code_only
    out.is_exclusive = editorForm.group.is_exclusive
    out.mcp_xml_inject = editorForm.group.mcp_xml_inject
    out.allow_image_generation = editorForm.group.allow_image_generation
    out.allow_batch_image_generation = editorForm.group.allow_batch_image_generation
    out.image_rate_independent = editorForm.group.image_rate_independent
    out.image_rate_multiplier = Number(editorForm.group.image_rate_multiplier || 1)
    out.image_price_1k = nullablePositiveNumber(editorForm.group.image_price_1k)
    out.image_price_2k = nullablePositiveNumber(editorForm.group.image_price_2k)
    out.image_price_4k = nullablePositiveNumber(editorForm.group.image_price_4k)
    out.batch_image_discount_multiplier = Number(editorForm.group.batch_image_discount_multiplier || 0)
    out.batch_image_hold_multiplier = Number(editorForm.group.batch_image_hold_multiplier || 0)
    out.video_rate_independent = editorForm.group.video_rate_independent
    out.video_rate_multiplier = Number(editorForm.group.video_rate_multiplier || 1)
    out.video_price_480p = nullablePositiveNumber(editorForm.group.video_price_480p)
    out.video_price_720p = nullablePositiveNumber(editorForm.group.video_price_720p)
    out.video_price_1080p = nullablePositiveNumber(editorForm.group.video_price_1080p)
    out.web_search_price_per_call = nullableNonNegativeNumber(editorForm.group.web_search_price_per_call)
    out.sort_order = Number(editorForm.group.sort_order || 0)
    return out
  }
  if (resource.value === 'accounts') {
    out.name = editorForm.account.name
    out.platform = editorForm.account.platform
    out.type = editorForm.account.type
    out.status = editorForm.account.status
    out.schedulable = editorForm.account.schedulable
    out.group_ids = editorForm.account.group_ids.map(Number).filter(id => Number.isFinite(id) && id > 0)
    out.proxy_id = editorForm.account.proxy_id > 0 ? Number(editorForm.account.proxy_id) : null
    out.priority = Number(editorForm.account.priority || 0)
    out.concurrency = Number(editorForm.account.concurrency || 0)
    out.load_factor = Number(editorForm.account.load_factor || 0)
    out.rate_multiplier = Number(editorForm.account.rate_multiplier || 0)
    out.auto_pause_on_expired = editorForm.account.auto_pause_on_expired
    out.notes = editorForm.account.notes
    const expiresAt = isoFromDateTimeLocal(editorForm.account.expires_at)
    out.expires_at = expiresAt || null
    const credentialsText = editorForm.account.credentials_text.trim()
    if (credentialsText) {
      out.credentials = JSON.parse(credentialsText)
    } else {
      delete out.credentials
    }
    out.extra = parseJSONField(editorForm.account.extra_text, {})
    return out
  }
  if (resource.value === 'proxies') {
    out.name = editorForm.proxy.name
    out.kind = editorForm.proxy.kind
    out.protocol = editorForm.proxy.protocol
    out.host = editorForm.proxy.host
    out.port = Number(editorForm.proxy.port || 0)
    if (!editingId.value || proxyUsernameDirty.value) {
      out.username = editorForm.proxy.username
    } else {
      delete out.username
    }
    if (!editingId.value || proxyPasswordDirty.value) {
      out.password = editorForm.proxy.password
    } else {
      delete out.password
    }
    out.status = editorForm.proxy.status
    out.fallback_mode = editorForm.proxy.fallback_mode
    out.backup_proxy_id = editorForm.proxy.backup_proxy_id > 0 ? Number(editorForm.proxy.backup_proxy_id) : null
    out.expiry_warn_days = Number(editorForm.proxy.expiry_warn_days || 0)
    const expiresAt = isoFromDateTimeLocal(editorForm.proxy.expires_at)
    out.expires_at = expiresAt || null
    if (!editingId.value || proxyExtraDirty.value) {
      const extra = parseJSONField(editorForm.proxy.extra_text, {}) as ResourceItem
      delete extra.redacted
      out.extra = extra
    } else {
      delete out.extra
    }
    return out
  }
  if (resource.value === 'assigned-subscriptions') {
    if (editorForm.assigned.email) out.email = editorForm.assigned.email
    if (editorForm.assigned.user_id > 0) out.user_id = Number(editorForm.assigned.user_id)
    out.group_id = Number(editorForm.assigned.group_id || 0)
    out.validity_days = Number(editorForm.assigned.validity_days || 0)
    out.notes = editorForm.assigned.notes
    return out
  }
  if (resource.value === 'redeem-codes') {
    out.group_id = Number(editorForm.redeem.group_id || 0)
    out.count = Number(editorForm.redeem.count || 0)
    out.validity_days = Number(editorForm.redeem.validity_days || 0)
    out.repeatable = editorForm.redeem.repeatable
    out.max_uses = editorForm.redeem.repeatable ? Number(editorForm.redeem.max_uses || 0) : 1
    out.notes = editorForm.redeem.notes
    const expiresAt = isoFromDateTimeLocal(editorForm.redeem.expires_at)
    if (expiresAt) out.expires_at = expiresAt
    return out
  }
  return out
}

function stringValue(value: unknown, fallback = ''): string {
  return value === null || value === undefined ? fallback : String(value)
}

function numberValue(value: unknown, fallback = 0): number {
  const n = Number(value)
  return Number.isFinite(n) ? n : fallback
}

function nullablePositiveNumber(value: unknown): number | null {
  const n = Number(value)
  return Number.isFinite(n) && n > 0 ? n : null
}

function nullableNonNegativeNumber(value: unknown): number | null {
  if (value === '' || value === null || value === undefined) return null
  const n = Number(value)
  return Number.isFinite(n) && n >= 0 ? n : null
}

function parseJSONField(value: string, fallback: unknown): unknown {
  const trimmed = value.trim()
  if (!trimmed) return fallback
  return JSON.parse(trimmed)
}

function idsArray(value: unknown): number[] {
  if (!Array.isArray(value)) return []
  return value
    .map(item => typeof item === 'object' && item !== null ? (item as ResourceItem).id : item)
    .map(item => Number(item))
    .filter(id => Number.isFinite(id) && id > 0)
}

function parseIdList(value: string): number[] {
  return value
    .split(/[,\s]+/)
    .map(part => Number(part.trim()))
    .filter(id => Number.isFinite(id) && id > 0)
}

function parseTextList(value: string): string[] {
  return value
    .split(/[,\s;]+/)
    .map(part => part.trim())
    .filter(Boolean)
}

function toDateTimeLocal(value: unknown): string {
  if (!value) return ''
  const date = new Date(String(value))
  if (Number.isNaN(date.getTime())) return ''
  const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000)
  return local.toISOString().slice(0, 16)
}

function isoFromDateTimeLocal(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? undefined : date.toISOString()
}

function resetAccountOAuthSession(): void {
  accountOAuth.auth_url = ''
  accountOAuth.session_id = ''
  accountOAuth.state = ''
  accountOAuth.callback = ''
  accountOAuth.session_key = ''
  accountOAuth.loading = false
  accountOAuth.error = ''
}

function resetAccountOAuth(): void {
  resetAccountOAuthSession()
  accountOAuth.project_id = ''
  accountOAuth.oauth_type = 'code_assist'
  accountOAuth.tier_id = ''
}

function selectedAccountProxyID(): number | undefined {
  const proxyID = Number(editorForm.account.proxy_id)
  return Number.isFinite(proxyID) && proxyID > 0 ? proxyID : undefined
}

function accountOAuthStateFromURL(rawURL: string): string {
  try {
    return new URL(rawURL).searchParams.get('state') || ''
  } catch {
    return ''
  }
}

function parseAccountOAuthCallback(rawValue: string): { code: string; state: string } {
  const trimmed = rawValue.trim()
  let code = trimmed
  let state = accountOAuth.state.trim()
  if (!trimmed.includes('code=')) return { code, state }

  try {
    const callbackURL = trimmed.includes('?')
      ? new URL(trimmed)
      : new URL(`http://localhost/callback?${trimmed.replace(/^\?/, '')}`)
    code = callbackURL.searchParams.get('code') || code
    state = callbackURL.searchParams.get('state') || state
  } catch {
    const codeMatch = trimmed.match(/[?&]code=([^&]+)/)
    const stateMatch = trimmed.match(/[?&]state=([^&]+)/)
    if (codeMatch?.[1]) code = decodeURIComponent(codeMatch[1])
    if (stateMatch?.[1]) state = decodeURIComponent(stateMatch[1])
  }
  return { code: code.trim(), state: state.trim() }
}

function currentAccountExtraForOAuth(): Record<string, any> | null {
  try {
    const parsed = parseJSONField(editorForm.account.extra_text, {})
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      throw new Error(mr('messages.extraJsonObject'))
    }
    return parsed as Record<string, any>
  } catch (error) {
    accountOAuth.error = extractApiErrorMessage(error, mr('messages.invalidExtraJson'))
    return null
  }
}

function applyAccountOAuthResult(result: UserOAuthCredentialsResult, currentExtra: Record<string, any>): void {
  if (!result?.credentials || typeof result.credentials !== 'object' || Array.isArray(result.credentials)) {
    throw new Error(mr('messages.oauthCredentialsMissing'))
  }
  editorForm.account.credentials_text = JSON.stringify(result.credentials, null, 2)
  if (result.extra && typeof result.extra === 'object' && !Array.isArray(result.extra)) {
    editorForm.account.extra_text = JSON.stringify({ ...currentExtra, ...result.extra }, null, 2)
  }
  if (!editorForm.account.name.trim() && result.suggested_name) {
    editorForm.account.name = result.suggested_name
  }
  accountOAuth.error = ''
  appStore.showSuccess(mr('messages.oauthCredentialsApplied'))
}

async function generateAccountOAuthURL(): Promise<void> {
  accountOAuth.loading = true
  accountOAuth.error = ''
  accountOAuth.auth_url = ''
  accountOAuth.session_id = ''
  accountOAuth.state = ''
  accountOAuth.callback = ''
  try {
    const result = await myResourcesApi.accounts.oauth.authURL({
      platform: editorForm.account.platform,
      proxy_id: selectedAccountProxyID(),
      setup_token: accountOAuthSetupToken.value,
      project_id: editorForm.account.platform === 'gemini' ? accountOAuth.project_id || undefined : undefined,
      oauth_type: editorForm.account.platform === 'gemini' ? accountOAuth.oauth_type : undefined,
      tier_id: editorForm.account.platform === 'gemini' ? accountOAuth.tier_id || undefined : undefined,
    })
    accountOAuth.auth_url = stringValue(result.auth_url)
    accountOAuth.session_id = stringValue(result.session_id)
    accountOAuth.state = stringValue(result.state) || accountOAuthStateFromURL(accountOAuth.auth_url)
    if (!accountOAuth.auth_url || !accountOAuth.session_id) {
      throw new Error(mr('messages.oauthUrlMissing'))
    }
  } catch (error) {
    accountOAuth.error = extractApiErrorMessage(error, mr('messages.generateAuthUrlFailed'))
  } finally {
    accountOAuth.loading = false
  }
}

async function exchangeAccountOAuthCode(): Promise<void> {
  const currentExtra = currentAccountExtraForOAuth()
  if (!currentExtra) return
  const callback = parseAccountOAuthCallback(accountOAuth.callback)
  if (!callback.code || !accountOAuth.session_id) return

  accountOAuth.loading = true
  accountOAuth.error = ''
  try {
    const result = await myResourcesApi.accounts.oauth.exchange({
      platform: editorForm.account.platform,
      proxy_id: selectedAccountProxyID(),
      setup_token: accountOAuthSetupToken.value,
      session_id: accountOAuth.session_id,
      code: callback.code,
      state: callback.state || undefined,
      oauth_type: editorForm.account.platform === 'gemini' ? accountOAuth.oauth_type : undefined,
      tier_id: editorForm.account.platform === 'gemini' ? accountOAuth.tier_id || undefined : undefined,
    })
    applyAccountOAuthResult(result, currentExtra)
    accountOAuth.session_id = ''
  } catch (error) {
    accountOAuth.error = extractApiErrorMessage(error, mr('messages.completeAuthorizationFailed'))
  } finally {
    accountOAuth.loading = false
  }
}

async function exchangeAccountOAuthCookie(): Promise<void> {
  const currentExtra = currentAccountExtraForOAuth()
  if (!currentExtra || !accountOAuth.session_key.trim()) return

  accountOAuth.loading = true
  accountOAuth.error = ''
  try {
    const result = await myResourcesApi.accounts.oauth.cookie({
      proxy_id: selectedAccountProxyID(),
      setup_token: accountOAuthSetupToken.value,
      session_key: accountOAuth.session_key.trim(),
    })
    applyAccountOAuthResult(result, currentExtra)
    accountOAuth.session_key = ''
  } catch (error) {
    accountOAuth.error = extractApiErrorMessage(error, mr('messages.sessionKeyAuthorizationFailed'))
  } finally {
    accountOAuth.loading = false
  }
}

async function copyAccountOAuthURL(): Promise<void> {
  try {
    await navigator.clipboard.writeText(accountOAuth.auth_url)
    appStore.showSuccess(mr('messages.authUrlCopied'))
  } catch (error) {
    accountOAuth.error = extractApiErrorMessage(error, mr('messages.copyFailed'))
  }
}

async function loadData(): Promise<void> {
  loading.value = true
  if (resource.value !== 'account-logs') accountUsageStats.value = null
  try {
    const params = {
      page: page.page,
      page_size: page.page_size,
      search: filters.search || undefined,
      status: filters.status || undefined,
      platform: filters.platform || undefined,
      type: filters.type || undefined,
      protocol: filters.protocol || undefined,
      source_id: filters.source_id ? Number(filters.source_id) : undefined,
      user_id: filters.user_id ? Number(filters.user_id) : undefined,
      api_key_id: filters.api_key_id ? Number(filters.api_key_id) : undefined,
      account_id: filters.account_id ? Number(filters.account_id) : undefined,
      start_date: filters.start_date || undefined,
      end_date: filters.end_date || undefined,
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    }
    let result: ResourcePage
    switch (resource.value) {
      case 'groups':
        result = await myResourcesApi.groups.list(params)
        {
          const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
          const [usageSummary, capacitySummary] = await Promise.all([
            myResourcesApi.groups.usageSummary(timezone).catch(() => []),
            myResourcesApi.groups.capacitySummary().catch(() => []),
          ])
          const usageByGroup = new Map(usageSummary.map(item => [Number(item.group_id), item]))
          const capacityByGroup = new Map(capacitySummary.map(item => [Number(item.group_id), item]))
          result.items = (result.items || []).map(item => {
            const groupID = Number(item.id)
            const usage = usageByGroup.get(groupID)
            const capacity = capacityByGroup.get(groupID)
            return {
              ...item,
              today_cost: Number(usage?.today_cost || 0).toFixed(4),
              total_cost: Number(usage?.total_cost || 0).toFixed(4),
              capacity_summary: capacity || null,
              capacity: capacity
                ? `C ${capacity.concurrency_used || 0}/${capacity.concurrency_max || 0} · S ${capacity.sessions_used || 0}/${capacity.sessions_max || 0} · RPM ${capacity.rpm_used || 0}/${capacity.rpm_max || 0}`
                : '-',
            }
          })
        }
        break
      case 'accounts':
        result = await myResourcesApi.accounts.list(params)
        break
      case 'proxies':
        result = await myResourcesApi.proxies.list(params)
        break
      case 'assigned-subscriptions':
        result = await myResourcesApi.assignedSubscriptions.list(params)
        break
      case 'redeem-codes':
        result = await myResourcesApi.redeemCodes.list(params)
        break
      case 'account-logs':
        {
          const [pageResult, stats] = await Promise.all([
          myResourcesApi.usage.accountLogs(params),
          myResourcesApi.usage.accountStats(params),
          ])
          result = pageResult
          accountUsageStats.value = stats
        }
        break
      case 'upstream-errors':
        result = await myResourcesApi.usage.upstreamErrors(params)
        break
    }
    items.value = result.items || []
    Object.assign(page, result)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.loadFailed')))
  } finally {
    loading.value = false
  }
}

async function ensureReferenceOptions(): Promise<void> {
  const shouldLoadGroups = ['groups', 'accounts', 'assigned-subscriptions', 'redeem-codes'].includes(resource.value)
  const shouldLoadProxies = resource.value === 'accounts'
  if (!shouldLoadGroups && !shouldLoadProxies) return

  referenceLoading.value = true
  try {
    const requests: Promise<void>[] = []
    if (shouldLoadGroups) {
      requests.push(myResourcesApi.groups.list({ page: 1, page_size: 1000 }).then(result => {
        groupOptions.value = result.items || []
      }))
    }
    if (shouldLoadProxies) {
      requests.push(myResourcesApi.proxies.list({ page: 1, page_size: 1000 }).then(result => {
        proxyOptions.value = result.items || []
      }))
    }
    await Promise.all(requests)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.loadOptionsFailed')))
  } finally {
    referenceLoading.value = false
  }
}

async function openCreate(): Promise<void> {
  if (resource.value === 'proxies') {
    editingId.value = null
    proxyEditorItem.value = null
    proxyEditorInitialMode.value = 'standard'
    editorOpen.value = true
    return
  }
  await ensureReferenceOptions()
  editingId.value = null
  editorMode.value = 'default'
  editorError.value = ''
  resetAccountOAuth()
  const payload = { ...(config.value.defaultPayload || {}) }
  populateEditorForm(payload)
  proxyUsernameDirty.value = true
  proxyPasswordDirty.value = true
  proxyExtraDirty.value = true
  editorText.value = JSON.stringify(payload, null, 2)
  editorOpen.value = true
}

async function toggleGroupLive(): Promise<void> {
  if (editorForm.group.allow_live) {
    editorForm.group.allow_live = false
    return
  }

  try {
    groupLiveCapability.value ||= await myResourcesApi.groups.liveCapability()
  } catch {
    groupLiveCapability.value = { supported: false }
  }

  if (groupLiveCapability.value.supported) {
    editorForm.group.allow_live = true
    return
  }

  const confirmed = window.confirm(
    `${t('admin.groups.openaiLive.unsupportedTitle')}\n\n${t('admin.groups.openaiLive.unsupportedMessage')}`,
  )
  if (confirmed) editorForm.group.allow_live = true
}

async function openEdit(item: ResourceItem): Promise<void> {
  if (resource.value === 'proxies') {
    editingId.value = Number(item.id)
    proxyEditorItem.value = item
    proxyEditorInitialMode.value = 'standard'
    editorOpen.value = true
    return
  }
  await ensureReferenceOptions()
  editingId.value = Number(item.id)
  editorMode.value = 'default'
  editorError.value = ''
  resetAccountOAuth()
  populateEditorForm(item)
  proxyUsernameDirty.value = false
  proxyPasswordDirty.value = false
  proxyExtraDirty.value = false
  editorText.value = JSON.stringify(item, null, 2)
  editorOpen.value = true
  if (resource.value === 'groups') void loadGroupModelCandidates()
}

async function saveEditor(): Promise<void> {
  if (editorMode.value === 'codexSessionImport' || editorMode.value === 'codexPATImport') {
    const expiresAt = isoFromDateTimeLocal(codexImportForm.expires_at)
    const basePayload: ResourceItem = {
      name: codexImportForm.name || undefined,
      notes: codexImportForm.notes || undefined,
      group_ids: codexImportForm.group_ids.map(Number).filter(id => Number.isFinite(id) && id > 0),
      proxy_id: codexImportForm.proxy_id > 0 ? Number(codexImportForm.proxy_id) : undefined,
      concurrency: Number(codexImportForm.concurrency),
      priority: Number(codexImportForm.priority),
      rate_multiplier: Number(codexImportForm.rate_multiplier),
      expires_at: expiresAt ? Math.floor(new Date(expiresAt).getTime() / 1000) : undefined,
      auto_pause_on_expired: codexImportForm.auto_pause_on_expired,
    }
    saving.value = true
    editorError.value = ''
    try {
      if (editorMode.value === 'codexSessionImport') {
        const result = await myResourcesApi.accounts.importCodexSessions({ ...basePayload, content: codexImportForm.content })
        appStore.showSuccess(mr('messages.importedCodexSessions', { count: result?.created_count || 0 }))
      } else {
        await myResourcesApi.accounts.importCodexPAT({ ...basePayload, access_token: codexImportForm.access_token })
        appStore.showSuccess(mr('messages.codexPatCreated'))
      }
      resetCodexImportForm()
      editorOpen.value = false
      await loadData()
    } catch (error) {
      editorError.value = extractApiErrorMessage(error, mr('messages.codexImportFailed'))
    } finally {
      saving.value = false
    }
    return
  }

  let payload: ResourceItem
  try {
    payload = JSON.parse(editorText.value || '{}')
    payload = mergeEditorFormPayload(payload)
  } catch (error) {
    editorError.value = extractApiErrorMessage(error, mr('messages.invalidJson'))
    return
  }
  saving.value = true
  try {
    if (editorMode.value === 'accountImport') {
      const result = await myResourcesApi.accounts.import(payload)
      appStore.showSuccess(mr('messages.importedAccounts', { count: result?.created_count || 0 }))
      editorOpen.value = false
      await loadData()
      return
    }
    if (editorMode.value === 'proxyImport') {
      const result = await myResourcesApi.proxies.importNodes(payload as { name_prefix?: string; content: string })
      appStore.showSuccess(mr('messages.importedProxies', { count: result?.created?.length || 0 }))
      editorOpen.value = false
      await loadData()
      return
    }
    if (resource.value === 'groups') {
      editingId.value ? await myResourcesApi.groups.update(editingId.value, payload) : await myResourcesApi.groups.create(payload)
    } else if (resource.value === 'accounts') {
      editingId.value ? await myResourcesApi.accounts.update(editingId.value, payload) : await myResourcesApi.accounts.create(payload)
    } else if (resource.value === 'proxies') {
      editingId.value ? await myResourcesApi.proxies.update(editingId.value, payload) : await myResourcesApi.proxies.create(payload)
    } else if (resource.value === 'assigned-subscriptions') {
      await myResourcesApi.assignedSubscriptions.assign(payload as any)
    } else if (resource.value === 'redeem-codes') {
      const generated = await myResourcesApi.redeemCodes.generate(payload)
      editorText.value = JSON.stringify(generated, null, 2)
      appStore.showSuccess(mr('messages.codesGenerated'))
      await loadData()
      return
    }
    editorOpen.value = false
    appStore.showSuccess(t('common.saved'))
    await loadData()
  } catch (error) {
    editorError.value = extractApiErrorMessage(error, mr('messages.saveFailed'))
  } finally {
    saving.value = false
  }
}

function valueAt(item: ResourceItem, key: string): any {
  return key.split('.').reduce((acc, part) => acc?.[part], item)
}

function formatValue(value: any): string {
  if (value === null || value === undefined || value === '') return '-'
  if (typeof value === 'boolean') return mr(value ? 'states.yes' : 'states.no')
  if (Array.isArray(value)) return value.map(v => typeof v === 'object' ? (v.name || v.id || JSON.stringify(v)) : String(v)).join(', ')
  if (typeof value === 'object') return JSON.stringify(value)
  if (typeof value === 'string' && ['active', 'inactive', 'disabled', 'expired', 'revoked', 'unused', 'used', 'error', 'standard', 'subscription'].includes(value)) {
    return mr(`states.${value}`)
  }
  return String(value)
}

function formatResourceDate(value: unknown, emptyLabel = '-'): string {
  if (!value) return emptyLabel
  const parsed = new Date(String(value))
  return Number.isNaN(parsed.getTime()) ? String(value) : formatDateTime(String(value))
}

function sourceTypeLabel(value: unknown): string {
  const normalized = String(value || '').trim().toLowerCase()
  if (!normalized) return '-'
  const known = ['manual', 'direct', 'redeem_code', 'admin', 'purchase']
  return known.includes(normalized) ? mr(`sources.types.${normalized}`) : normalized.split('_').join(' ')
}

function addGroupModel(): void {
  const model = groupModelInput.value.trim()
  if (!model) return
  const existing = groupModelAllowlistState.items.find(item => item.id === model)
  if (existing) existing.selected = true
  else groupModelAllowlistState.items.push({ id: model, selected: true })
  groupModelInput.value = ''
}

async function loadGroupModelCandidates(): Promise<void> {
  if (!editingId.value) return
  groupModelCandidatesLoading.value = true
  try {
    const result = await myResourcesApi.groups.modelCandidates(editingId.value, editorForm.group.platform)
    const current = buildModelAllowlistConfig(groupModelAllowlistState)
    Object.assign(groupModelAllowlistState, hydrateModelAllowlistState(current, result.models || []))
  } catch (error) {
    editorError.value = extractApiErrorMessage(error, mr('messages.modelCandidatesFailed'))
  } finally {
    groupModelCandidatesLoading.value = false
  }
}

function openRecordDetails(item: ResourceItem): void {
  recordDetailItem.value = item
  recordDetailOpen.value = true
}

const recordDetailTitle = computed(() => resource.value === 'upstream-errors'
  ? mr('details.upstreamErrorTitle')
  : mr('details.accountLogTitle'))

const recordDetailFields = computed(() => {
  if (!recordDetailItem.value) return []
  const accountFields = [
    ['id', 'details.fields.id'], ['created_at', 'columns.time'], ['request_id', 'columns.requestId'],
    ['account_name', 'columns.account'], ['group_name', 'columns.group'], ['model', 'columns.model'],
    ['requested_model', 'details.fields.requestedModel'], ['upstream_model', 'details.fields.upstreamModel'],
    ['input_tokens', 'columns.inputTokens'], ['output_tokens', 'columns.outputTokens'],
    ['cache_creation_tokens', 'details.fields.cacheCreationTokens'], ['cache_read_tokens', 'details.fields.cacheReadTokens'],
    ['total_cost', 'details.fields.accountCost'], ['actual_cost', 'details.fields.userCost'],
    ['rate_multiplier', 'details.fields.rateMultiplier'], ['account_rate_multiplier', 'details.fields.accountRateMultiplier'],
    ['billing_type', 'details.fields.billingType'], ['stream', 'details.fields.stream'],
    ['duration_ms', 'details.fields.duration'], ['first_token_ms', 'details.fields.firstToken'],
    ['ip_address', 'details.fields.ipAddress'], ['user_agent', 'details.fields.userAgent'],
  ]
  const errorFields = [
    ['id', 'details.fields.id'], ['created_at', 'columns.time'], ['request_id', 'columns.requestId'],
    ['client_request_id', 'details.fields.clientRequestId'], ['account_name', 'columns.account'],
    ['group_name', 'columns.group'], ['platform', 'columns.platform'], ['model', 'columns.model'],
    ['requested_model', 'details.fields.requestedModel'], ['upstream_model', 'details.fields.upstreamModel'],
    ['phase', 'details.fields.phase'], ['type', 'columns.type'], ['error_owner', 'details.fields.errorOwner'],
    ['error_source', 'details.fields.errorSource'], ['severity', 'details.fields.severity'],
    ['status_code', 'columns.statusCode'], ['upstream_status_code', 'details.fields.upstreamStatusCode'],
    ['message', 'columns.error'], ['upstream_error_message', 'details.fields.upstreamError'],
    ['request_path', 'details.fields.requestPath'], ['stream', 'details.fields.stream'],
    ['user_agent', 'details.fields.userAgent'], ['upstream_errors', 'details.fields.attempts'],
  ]
  const fields = resource.value === 'upstream-errors' ? errorFields : accountFields
  return fields.map(([key, labelKey]) => {
    const value = valueAt(recordDetailItem.value as ResourceItem, key)
    let formatted = formatValue(value)
    if (key.endsWith('_at')) formatted = formatResourceDate(value)
    if (['total_cost', 'actual_cost'].includes(key)) formatted = `$${Number(value || 0).toFixed(4)}`
    if (['input_tokens', 'output_tokens', 'cache_creation_tokens', 'cache_read_tokens'].includes(key)) formatted = Number(value || 0).toLocaleString()
    if (['duration_ms', 'first_token_ms'].includes(key) && value != null) formatted = `${Number(value)} ms`
    if (key === 'upstream_errors' && value) formatted = JSON.stringify(value, null, 2)
    return { key, label: mr(labelKey), value: formatted }
  })
})

function canMutateItem(item: ResourceItem): boolean {
  if (resource.value === 'proxies') return item.is_owned === true
  return true
}

async function openRedeemUsageDetails(item: ResourceItem): Promise<void> {
  redeemUsageTarget.value = item
  redeemUsages.value = []
  redeemUsageError.value = ''
  redeemUsageOpen.value = true
  redeemUsageLoading.value = true
  try {
    const result = await myResourcesApi.redeemCodes.usages(Number(item.id), { page: 1, page_size: 100 })
    redeemUsages.value = result.items
  } catch (error) {
    redeemUsageError.value = extractApiErrorMessage(error, mr('redeemUsages.loadFailed'))
  } finally {
    redeemUsageLoading.value = false
  }
}

async function deleteItem(item: ResourceItem): Promise<void> {
  if (!window.confirm(mr('messages.deleteConfirm'))) return
  try {
    const id = Number(item.id)
    if (resource.value === 'groups') await myResourcesApi.groups.delete(id)
    if (resource.value === 'accounts') await myResourcesApi.accounts.delete(id)
    if (resource.value === 'proxies') await myResourcesApi.proxies.delete(id)
    if (resource.value === 'redeem-codes') await myResourcesApi.redeemCodes.delete(id)
    appStore.showSuccess(t('common.deleted'))
    await loadData()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.deleteFailed')))
  }
}

function updateRunningProxySet(target: typeof testingProxyIds, proxyID: number, running: boolean): void {
  const next = new Set(target.value)
  if (running) next.add(proxyID)
  else next.delete(proxyID)
  target.value = next
}

function applyProxyTestResult(proxyID: number, result: UserProxyTestResult): void {
  const target = items.value.find(item => Number(item.id) === proxyID)
  if (!target) return
  target.latency_status = result.success ? 'success' : 'failed'
  target.latency_ms = result.success ? result.latency_ms : undefined
  target.latency_message = result.message
  target.ip_address = result.success ? result.ip_address : undefined
  target.country = result.success ? result.country : undefined
  target.country_code = result.success ? result.country_code : undefined
  target.region = result.success ? result.region : undefined
  target.city = result.success ? result.city : undefined
}

function applyProxyQualityResult(proxyID: number, result: ProxyQualityCheckResult): void {
  const target = items.value.find(item => Number(item.id) === proxyID)
  if (!target) return
  const baseConnected = result.items?.some(item => item.target === 'base_connectivity' && item.status === 'pass') ?? false
  target.quality_status = result.challenge_count > 0
    ? 'challenge'
    : result.failed_count > 0
      ? baseConnected ? 'warn' : 'failed'
      : result.warn_count > 0
        ? 'warn'
        : 'healthy'
  target.quality_score = result.score
  target.quality_grade = result.grade
  target.quality_summary = result.summary
  target.quality_checked = result.checked_at
  if (result.base_latency_ms != null) {
    applyProxyTestResult(proxyID, {
      success: true,
      message: result.summary,
      latency_ms: result.base_latency_ms,
      ip_address: result.exit_ip,
      country: result.country,
      country_code: result.country_code,
    })
  }
}

async function runProxyTest(proxyID: number, notify: boolean): Promise<UserProxyTestResult | null> {
  updateRunningProxySet(testingProxyIds, proxyID, true)
  try {
    const result = await myResourcesApi.proxies.test(proxyID)
    applyProxyTestResult(proxyID, result)
    if (notify) {
      if (result.success) appStore.showSuccess(result.message || mr('messages.testSuccess'))
      else appStore.showError(result.message || mr('messages.testFailed'))
    }
    return result
  } catch (error) {
    if (notify) appStore.showError(extractApiErrorMessage(error, mr('messages.testFailed')))
    return null
  } finally {
    updateRunningProxySet(testingProxyIds, proxyID, false)
  }
}

async function testProxy(item: ResourceItem): Promise<void> {
  await runProxyTest(Number(item.id), true)
}

async function runProxyQualityCheck(proxyID: number, notify: boolean): Promise<ProxyQualityCheckResult | null> {
  updateRunningProxySet(qualityCheckingProxyIds, proxyID, true)
  try {
    const result = await myResourcesApi.proxies.qualityCheck(proxyID)
    applyProxyQualityResult(proxyID, result)
    if (notify) appStore.showSuccess(mr('messages.qualityResult', { grade: result.grade || '-', summary: result.summary || '' }))
    return result
  } catch (error) {
    if (notify) appStore.showError(extractApiErrorMessage(error, mr('messages.qualityFailed')))
    return null
  } finally {
    updateRunningProxySet(qualityCheckingProxyIds, proxyID, false)
  }
}

async function qualityCheckProxy(item: ResourceItem): Promise<void> {
  await runProxyQualityCheck(Number(item.id), true)
}

async function proxyIDsForBatch(): Promise<number[]> {
  if (selectedIds.value.length > 0) return [...selectedIds.value]
  const ids: number[] = []
  let currentPage = 1
  let pages = 1
  do {
    const result = await myResourcesApi.proxies.list({
      page: currentPage,
      page_size: 200,
      search: filters.search || undefined,
      status: filters.status || undefined,
      type: filters.type || undefined,
      protocol: filters.protocol || undefined,
      source_id: filters.source_id ? Number(filters.source_id) : undefined,
      owned_only: true,
    })
    ids.push(...result.items.map(item => Number(item.id)).filter(id => id > 0))
    pages = Math.max(1, Number(result.pages || 1))
    currentPage++
  } while (currentPage <= pages)
  return Array.from(new Set(ids))
}

async function runConcurrentProxyBatch(ids: number[], concurrency: number, worker: (id: number) => Promise<void>): Promise<void> {
  let index = 0
  proxyBatchProgress.completed = 0
  proxyBatchProgress.total = ids.length
  const run = async () => {
    while (index < ids.length) {
      const current = ids[index++]
      await worker(current)
      proxyBatchProgress.completed++
    }
  }
  await Promise.all(Array.from({ length: Math.min(concurrency, ids.length) }, () => run()))
}

async function batchTestProxies(): Promise<void> {
  if (batchTesting.value || batchQualityChecking.value) return
  batchTesting.value = true
  try {
    const ids = await proxyIDsForBatch()
    if (ids.length === 0) {
      appStore.showInfo(mr('messages.batchProxyEmpty'))
      return
    }
    let success = 0
    let failed = 0
    await runConcurrentProxyBatch(ids, 5, async id => {
      const result = await runProxyTest(id, false)
      if (result?.success) success++
      else failed++
    })
    const message = mr('messages.batchTestResult', { total: ids.length, success, failed })
    if (failed > 0) appStore.showWarning(message)
    else appStore.showSuccess(message)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.testFailed')))
  } finally {
    batchTesting.value = false
    proxyBatchProgress.completed = 0
    proxyBatchProgress.total = 0
  }
}

async function batchQualityCheckProxies(): Promise<void> {
  if (batchQualityChecking.value || batchTesting.value) return
  batchQualityChecking.value = true
  try {
    const ids = await proxyIDsForBatch()
    if (ids.length === 0) {
      appStore.showInfo(mr('messages.batchProxyEmpty'))
      return
    }
    const stats = { healthy: 0, warn: 0, challenge: 0, failed: 0 }
    await runConcurrentProxyBatch(ids, 3, async id => {
      const result = await runProxyQualityCheck(id, false)
      if (!result) stats.failed++
      else if (result.challenge_count > 0) stats.challenge++
      else if (result.failed_count > 0) stats.failed++
      else if (result.warn_count > 0) stats.warn++
      else stats.healthy++
    })
    const message = mr('messages.batchQualityResult', { total: ids.length, ...stats })
    if (stats.failed > 0 || stats.challenge > 0) appStore.showWarning(message)
    else appStore.showSuccess(message)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.qualityFailed')))
  } finally {
    batchQualityChecking.value = false
    proxyBatchProgress.completed = 0
    proxyBatchProgress.total = 0
  }
}

async function batchDeleteProxies(): Promise<void> {
  const ids = [...selectedIds.value]
  if (ids.length === 0 || !window.confirm(t('admin.proxies.batchDeleteConfirm', { count: ids.length }))) return

  const results = await Promise.allSettled(ids.map(id => myResourcesApi.proxies.delete(id)))
  const deletedIds = ids.filter((_, index) => results[index].status === 'fulfilled')
  const failed = ids.length - deletedIds.length
  selectedIds.value = selectedIds.value.filter(id => !deletedIds.includes(id))

  if (deletedIds.length > 0) {
    const message = t('admin.proxies.batchDeleteDone', { deleted: deletedIds.length, skipped: failed })
    if (failed > 0) appStore.showWarning(message)
    else appStore.showSuccess(message)
    await loadData()
    return
  }

  appStore.showError(t('admin.proxies.batchDeleteFailed'))
}

async function exportProxies(): Promise<void> {
  try {
    const data = await myResourcesApi.proxies.export({ ids: selectedIds.value })
    downloadJson('my-proxies.json', data)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.exportFailed')))
  }
}

async function exportRedeemCodes(): Promise<void> {
  try {
    const blob = await myResourcesApi.redeemCodes.export({ ...filters, ids: selectedIds.value })
    downloadBlob('my-redeem-codes.csv', blob)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.exportFailed')))
  }
}

async function exportAccountUsageLogs(): Promise<void> {
  try {
    const blob = await myResourcesApi.usage.exportAccountLogs({
      search: filters.search || undefined,
      platform: filters.platform || undefined,
      user_id: filters.user_id ? Number(filters.user_id) : undefined,
      api_key_id: filters.api_key_id ? Number(filters.api_key_id) : undefined,
      account_id: filters.account_id ? Number(filters.account_id) : undefined,
      start_date: filters.start_date || undefined,
      end_date: filters.end_date || undefined,
      timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    })
    downloadBlob('my-account-usage.csv', blob)
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.exportFailed')))
  }
}

async function loadRedeemStats(): Promise<void> {
  try {
    const stats = await myResourcesApi.redeemCodes.stats()
    redeemStats.value = stats || null
    appStore.showSuccess(mr('messages.statsSummary', { total: stats?.total_codes || 0, active: stats?.active_codes || 0, used: stats?.used_codes || 0 }))
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.statsFailed')))
  }
}

async function openGroupOverrides(item: ResourceItem): Promise<void> {
  groupOverridesTarget.value = item
  groupOverridesOpen.value = true
  groupOverridesLoading.value = true
  groupOverrideRows.value = []
  operationError.value = ''
  try {
    const groupID = Number(item.id)
    const [overrides, subscriptions] = await Promise.all([
      myResourcesApi.groups.userOverrides(groupID),
      myResourcesApi.assignedSubscriptions.list({ page: 1, page_size: 1000, group_id: groupID }),
    ])
    const overrideByUser = new Map(overrides.map(entry => [Number(entry.user_id), entry]))
    const rows = new Map<number, GroupOverrideRow>()
    for (const subscription of subscriptions.items || []) {
      const userID = Number(subscription.user_id)
      if (!Number.isFinite(userID) || userID <= 0 || rows.has(userID)) continue
      const override = overrideByUser.get(userID)
      rows.set(userID, {
        user_id: userID,
        user_name: stringValue(subscription.username || override?.user_name),
        user_email: stringValue(subscription.user_email || override?.user_email),
        rate_multiplier: override?.rate_multiplier ?? '',
        rpm_override: override?.rpm_override ?? '',
      })
    }
    for (const override of overrides) {
      const userID = Number(override.user_id)
      if (!Number.isFinite(userID) || userID <= 0 || rows.has(userID)) continue
      rows.set(userID, {
        user_id: userID,
        user_name: stringValue(override.user_name),
        user_email: stringValue(override.user_email),
        rate_multiplier: override.rate_multiplier ?? '',
        rpm_override: override.rpm_override ?? '',
      })
    }
    groupOverrideRows.value = [...rows.values()]
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('overrides.loadFailed'))
  } finally {
    groupOverridesLoading.value = false
  }
}

async function saveGroupOverrides(): Promise<void> {
  if (!groupOverridesTarget.value) return
  const groupID = Number(groupOverridesTarget.value.id)
  const rateEntries = groupOverrideRows.value
    .filter(row => row.rate_multiplier !== '')
    .map(row => ({ user_id: row.user_id, rate_multiplier: Number(row.rate_multiplier) }))
  const rpmEntries = groupOverrideRows.value
    .filter(row => row.rpm_override !== '')
    .map(row => ({ user_id: row.user_id, rpm_override: Number(row.rpm_override) }))
  if (rateEntries.some(entry => !Number.isFinite(entry.rate_multiplier) || entry.rate_multiplier <= 0)) {
    operationError.value = mr('overrides.invalidRate')
    return
  }
  if (rpmEntries.some(entry => !Number.isInteger(entry.rpm_override) || entry.rpm_override < 0)) {
    operationError.value = mr('overrides.invalidRPM')
    return
  }
  saving.value = true
  operationError.value = ''
  try {
    await Promise.all([
      myResourcesApi.groups.setRateMultipliers(groupID, rateEntries),
      myResourcesApi.groups.setRPMOverrides(groupID, rpmEntries),
    ])
    appStore.showSuccess(mr('overrides.saved'))
    groupOverridesOpen.value = false
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('overrides.saveFailed'))
  } finally {
    saving.value = false
  }
}

async function clearGroupRates(): Promise<void> {
  if (!groupOverridesTarget.value || !window.confirm(mr('overrides.clearRatesConfirm'))) return
  saving.value = true
  try {
    await myResourcesApi.groups.clearRateMultipliers(Number(groupOverridesTarget.value.id))
    for (const row of groupOverrideRows.value) row.rate_multiplier = ''
    appStore.showSuccess(mr('overrides.ratesCleared'))
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('overrides.clearRatesFailed'))
  } finally {
    saving.value = false
  }
}

async function clearGroupRPMs(): Promise<void> {
  if (!groupOverridesTarget.value || !window.confirm(mr('overrides.clearRPMsConfirm'))) return
  saving.value = true
  try {
    await myResourcesApi.groups.clearRPMOverrides(Number(groupOverridesTarget.value.id))
    for (const row of groupOverrideRows.value) row.rpm_override = ''
    appStore.showSuccess(mr('overrides.rpmsCleared'))
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('overrides.clearRPMsFailed'))
  } finally {
    saving.value = false
  }
}

async function submitAccountBatchUpdate(): Promise<void> {
  const fields: ResourceItem = {}
  if (accountBatchForm.update_schedulable) fields.schedulable = accountBatchForm.schedulable
  if (accountBatchForm.update_status) fields.status = accountBatchForm.status
  if (accountBatchForm.update_priority) fields.priority = Number(accountBatchForm.priority || 0)
  if (accountBatchForm.update_notes) fields.notes = accountBatchForm.notes
  if (Object.keys(fields).length === 0) {
    operationError.value = mr('messages.batchSelectField')
    return
  }
  saving.value = true
  try {
    await myResourcesApi.accounts.batchUpdate(selectedIds.value, fields)
    selectedIds.value = []
    accountBatchOpen.value = false
    appStore.showSuccess(mr('messages.batchCompleted'))
    await loadData()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.batchFailed'))
  } finally {
    saving.value = false
  }
}

function openRedeemBatchUpdate(): void {
  operationError.value = ''
  redeemBatchOpen.value = true
}

async function submitRedeemBatchUpdate(): Promise<void> {
  const fields: ResourceItem = {}
  if (redeemBatchForm.update_status) fields.status = redeemBatchForm.status
  if (redeemBatchForm.update_validity_days) fields.validity_days = Number(redeemBatchForm.validity_days || 0)
  if (redeemBatchForm.update_expires_at) fields.expires_at = redeemBatchForm.expires_at ? new Date(redeemBatchForm.expires_at).toISOString() : null
  if (redeemBatchForm.update_notes) fields.notes = redeemBatchForm.notes
  if (Object.keys(fields).length === 0) {
    operationError.value = mr('messages.batchSelectField')
    return
  }
  saving.value = true
  try {
    await myResourcesApi.redeemCodes.batchUpdate(selectedIds.value, fields)
    selectedIds.value = []
    redeemBatchOpen.value = false
    appStore.showSuccess(mr('messages.batchCompleted'))
    await loadData()
    if (redeemStats.value) await loadRedeemStats()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.batchFailed'))
  } finally {
    saving.value = false
  }
}

async function openProxySources(): Promise<void> {
  operationError.value = ''
  proxySourcesOpen.value = true
  resetProxySourceForm()
  await loadProxySources()
}

function resetProxySourceForm(): void {
  proxySourceEditingID.value = null
  proxySourceForm.name = ''
  proxySourceForm.subscription_url = ''
  proxySourceForm.refresh_interval_minutes = 1440
  proxySourceForm.is_public = false
  proxySourceForm.sync_enabled = true
}

async function loadProxySources(): Promise<void> {
  proxySourcesLoading.value = true
  try {
    const result = await myResourcesApi.proxies.sources.list({ page: 1, page_size: 100 })
    proxySources.value = result.items || []
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceLoadFailed'))
  } finally {
    proxySourcesLoading.value = false
  }
}

// Fills the source filter options without opening the modal. Failures stay silent
// because the filter is optional: the proxy table must still load without it.
async function ensureProxySourceOptions(): Promise<void> {
  if (proxySources.value.length > 0 || proxySourcesLoading.value) return
  try {
    const result = await myResourcesApi.proxies.sources.list({ page: 1, page_size: 100 })
    proxySources.value = result.items || []
  } catch {
    proxySources.value = []
  }
}

function filterProxiesBySource(source: ResourceItem): void {
  filters.source_id = String(source.id)
  proxySourcesOpen.value = false
  applyAlignedFilters()
}

async function saveProxySource(): Promise<void> {
  operationError.value = ''
  saving.value = true
  try {
    if (proxySourceEditingID.value) {
      await myResourcesApi.proxies.sources.update(proxySourceEditingID.value, { ...proxySourceForm })
    } else {
      await myResourcesApi.proxies.sources.create({ ...proxySourceForm })
    }
    resetProxySourceForm()
    appStore.showSuccess(mr('messages.sourceSaved'))
    await loadProxySources()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceSaveFailed'))
  } finally {
    saving.value = false
  }
}

function editProxySource(source: ResourceItem): void {
  proxySourceEditingID.value = Number(source.id)
  proxySourceForm.name = stringValue(source.name)
  proxySourceForm.subscription_url = stringValue(source.subscription_url)
  proxySourceForm.refresh_interval_minutes = numberValue(source.refresh_interval_minutes, 1440)
  proxySourceForm.is_public = Boolean(source.is_public)
  proxySourceForm.sync_enabled = source.sync_enabled !== false
  operationError.value = ''
}

// Remaining / total traffic reported by the subscription provider. Providers that
// send no usage header leave both counters at zero, which reads as "unknown".
function proxySourceTrafficText(source: ResourceItem): string {
  const used = numberValue(source.sub_traffic_used, 0)
  const total = numberValue(source.sub_traffic_total, 0)
  if (total <= 0 && used <= 0) return '-'
  if (total <= 0) return mr('sources.trafficUsedOnly', { used: formatBytes(used) })
  const remaining = Math.max(0, total - used)
  return mr('sources.trafficRemaining', { remaining: formatBytes(remaining), total: formatBytes(total) })
}

// Pause/resume only flips sync_enabled; the other fields are resent unchanged
// because the update endpoint replaces the whole source record.
async function toggleProxySourceSync(source: ResourceItem): Promise<void> {
  operationError.value = ''
  const nextEnabled = source.sync_enabled === false
  try {
    await myResourcesApi.proxies.sources.update(Number(source.id), {
      name: stringValue(source.name),
      subscription_url: stringValue(source.subscription_url),
      refresh_interval_minutes: numberValue(source.refresh_interval_minutes, 1440),
      is_public: Boolean(source.is_public),
      sync_enabled: nextEnabled,
    })
    appStore.showSuccess(mr(nextEnabled ? 'messages.sourceSyncResumed' : 'messages.sourceSyncPaused'))
    await loadProxySources()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceSaveFailed'))
  }
}

async function syncAllProxySourceItems(): Promise<void> {
  if (proxySourcesSyncingAll.value) return
  operationError.value = ''
  proxySourcesSyncingAll.value = true
  try {
    const result = await myResourcesApi.proxies.sources.syncAll()
    proxySourceSyncAllResult.value = result || null
    const failed = numberValue(result?.failed_count, 0)
    if (failed > 0) appStore.showWarning(mr('messages.sourceSyncAllPartial', { failed }))
    else appStore.showSuccess(mr('messages.sourceSyncAllDone'))
    await loadProxySources()
    await loadData()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceSyncFailed'))
  } finally {
    proxySourcesSyncingAll.value = false
  }
}

async function syncProxySourceItem(source: ResourceItem): Promise<void> {
  operationError.value = ''
  try {
    const result = await myResourcesApi.proxies.sources.sync(Number(source.id))
    appStore.showSuccess(mr('messages.sourceSynced', { count: result?.imported_count || 0 }))
    await loadProxySources()
    await loadData()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceSyncFailed'))
  }
}

async function deleteProxySource(source: ResourceItem): Promise<void> {
  if (!window.confirm(mr('messages.sourceDeleteConfirm'))) return
  operationError.value = ''
  try {
    await myResourcesApi.proxies.sources.delete(Number(source.id))
    if (proxySourceEditingID.value === Number(source.id)) resetProxySourceForm()
    appStore.showSuccess(mr('messages.sourceDeleted'))
    await loadProxySources()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.sourceDeleteFailed'))
  }
}

function downloadJson(filename: string, data: unknown): void {
  downloadBlob(filename, new Blob([JSON.stringify(data, null, 2)], { type: 'application/json;charset=utf-8' }))
}

function downloadBlob(filename: string, blob: Blob): void {
  const url = URL.createObjectURL(blob)
  const link = document.createElement('a')
  link.href = url
  link.download = filename
  link.click()
  URL.revokeObjectURL(url)
}

function openProxyImport(): void {
  editingId.value = null
  proxyEditorItem.value = null
  proxyEditorInitialMode.value = 'batch'
  editorOpen.value = true
}

function closeProxyEditor(): void {
  editorOpen.value = false
  proxyEditorItem.value = null
  proxyEditorInitialMode.value = 'standard'
}

async function handleProxySaved(): Promise<void> {
  closeProxyEditor()
  await loadData()
}

function resetCodexImportForm(): void {
  codexImportForm.content = ''
  codexImportForm.access_token = ''
  codexImportForm.name = ''
  codexImportForm.notes = ''
  codexImportForm.group_ids = []
  codexImportForm.proxy_id = 0
  codexImportForm.concurrency = 3
  codexImportForm.priority = 50
  codexImportForm.rate_multiplier = 1
  codexImportForm.expires_at = ''
  codexImportForm.auto_pause_on_expired = true
}

function openExtendAssigned(item: ResourceItem): void {
  operationError.value = ''
  extendTarget.value = item
  extendDays.value = 30
  extendOpen.value = true
}

async function submitExtendAssigned(): Promise<void> {
  if (!extendTarget.value) return
  const days = Number(extendDays.value || 0)
  if (days <= 0) {
    operationError.value = mr('messages.extendDaysInvalid')
    return
  }
  saving.value = true
  try {
    await myResourcesApi.assignedSubscriptions.extend(Number(extendTarget.value.id), days)
    extendOpen.value = false
    appStore.showSuccess(mr('messages.extendSuccess'))
    await loadData()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.extendFailed'))
  } finally {
    saving.value = false
  }
}

async function openBulkAssign(): Promise<void> {
  await ensureReferenceOptions()
  operationError.value = ''
  bulkAssignResult.value = null
  bulkAssignForm.emails_text = ''
  bulkAssignForm.user_ids_text = ''
  bulkAssignForm.group_id = 0
  bulkAssignForm.validity_days = 30
  bulkAssignForm.notes = ''
  bulkAssignOpen.value = true
}

async function submitBulkAssign(): Promise<void> {
  const emails = parseTextList(bulkAssignForm.emails_text)
  const userIds = parseIdList(bulkAssignForm.user_ids_text)
  if (emails.length === 0 && userIds.length === 0) {
    operationError.value = mr('messages.assignTargetRequired')
    return
  }
  if (bulkAssignForm.group_id <= 0 || bulkAssignForm.validity_days <= 0) {
    operationError.value = mr('messages.assignGroupRequired')
    return
  }
  saving.value = true
  operationError.value = ''
  try {
    const result = await myResourcesApi.assignedSubscriptions.bulkAssign({
      emails,
      user_ids: userIds,
      group_id: Number(bulkAssignForm.group_id),
      validity_days: Number(bulkAssignForm.validity_days),
      notes: bulkAssignForm.notes,
    })
    bulkAssignResult.value = result || {}
    appStore.showSuccess(mr('messages.bulkAssignCompleted'))
    await loadData()
  } catch (error) {
    operationError.value = extractApiErrorMessage(error, mr('messages.bulkAssignFailed'))
  } finally {
    saving.value = false
  }
}

async function revokeAssigned(item: ResourceItem): Promise<void> {
  if (!window.confirm(mr('messages.revokeConfirm'))) return
  try {
    await myResourcesApi.assignedSubscriptions.revoke(Number(item.id))
    appStore.showSuccess(mr('messages.revoked'))
    await loadData()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.revokeFailed')))
  }
}

async function restoreAssigned(item: ResourceItem): Promise<void> {
  try {
    await myResourcesApi.assignedSubscriptions.restore(Number(item.id))
    appStore.showSuccess(mr('messages.restored'))
    await loadData()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.restoreFailed')))
  }
}

async function resetAssignedUsage(item: ResourceItem): Promise<void> {
  if (!window.confirm(mr('messages.resetUsageConfirm'))) return
  try {
    await myResourcesApi.assignedSubscriptions.resetUsage(Number(item.id))
    appStore.showSuccess(mr('messages.usageReset'))
    await loadData()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.resetUsageFailed')))
  }
}

async function expireRedeemCode(item: ResourceItem): Promise<void> {
  try {
    await myResourcesApi.redeemCodes.expire(Number(item.id))
    appStore.showSuccess(mr('messages.codeExpired'))
    await loadData()
  } catch (error) {
    appStore.showError(extractApiErrorMessage(error, mr('messages.operationFailed')))
  }
}

function toggleSelected(id: number): void {
  selectedIds.value = selectedIds.value.includes(id)
    ? selectedIds.value.filter(item => item !== id)
    : [...selectedIds.value, id]
}

function toggleAllVisible(): void {
  const ids = selectableVisibleItems.value.map(item => Number(item.id))
  if (allVisibleSelected.value) {
    selectedIds.value = selectedIds.value.filter(id => !ids.includes(id))
  } else {
    selectedIds.value = Array.from(new Set([...selectedIds.value, ...ids]))
  }
}

async function batchDeleteRedeemCodes(): Promise<void> {
  if (!window.confirm(mr('messages.batchDeleteCodesConfirm', { count: selectedIds.value.length }))) return
  await myResourcesApi.redeemCodes.batchDelete(selectedIds.value)
  selectedIds.value = []
  await loadData()
}

async function batchExpireRedeemCodes(): Promise<void> {
  await myResourcesApi.redeemCodes.batchExpire(selectedIds.value)
  selectedIds.value = []
  await loadData()
}

function changePage(nextPage: number): void {
  page.page = nextPage
  void loadData()
}

function changePageSize(nextPageSize: number): void {
  page.page = 1
  page.page_size = nextPageSize
  void loadData()
}

function applyAccountFilterFromRoute(): void {
  if (resource.value !== 'account-logs' && resource.value !== 'upstream-errors') return
  const accountID = Number(route.query.account_id)
  filters.account_id = Number.isInteger(accountID) && accountID > 0 ? String(accountID) : ''
}

watch(resource, () => {
  page.page = 1
  selectedIds.value = []
  showColumnSettings.value = false
  Object.assign(filters, {
    search: '', status: '', platform: '', type: '', protocol: '', source_id: '',
    user_id: '', api_key_id: '', account_id: '', start_date: '', end_date: '',
  })
  applyAccountFilterFromRoute()
  loadColumnSettings()
  void loadData()
  void ensureProxySourceOptions()
})

watch(() => route.query.account_id, () => {
  if (resource.value !== 'account-logs' && resource.value !== 'upstream-errors') return
  applyAccountFilterFromRoute()
  page.page = 1
  void loadData()
})

watch(() => editorForm.proxy.kind, kind => {
  const options = kind === 'xray' ? xrayProxyProtocolOptions : standardProxyProtocolOptions
  if (!options.some(option => option.value === editorForm.proxy.protocol)) {
    editorForm.proxy.protocol = String(options[0]?.value || 'socks5')
  }
})

watch(() => editorForm.group.platform, platform => {
  if (platform !== 'openai') editorForm.group.allow_live = false
})

watch(() => editorForm.account.platform, platform => {
  const typeOptions = getUserAccountTypeOptions(platform)
  if (!typeOptions.some(option => option.value === editorForm.account.type)) {
    editorForm.account.type = typeOptions[0]?.value || 'oauth'
  }
  resetAccountOAuthSession()
  if (platform !== 'gemini') {
    accountOAuth.project_id = ''
    accountOAuth.oauth_type = 'code_assist'
    accountOAuth.tier_id = ''
  }
})

watch(() => editorForm.account.type, () => {
  resetAccountOAuthSession()
})

onMounted(() => {
  applyAccountFilterFromRoute()
  loadColumnSettings()
  document.addEventListener('click', closeColumnSettingsOnOutsideClick)
  void loadData()
  void ensureProxySourceOptions()
})

onUnmounted(() => {
  if (alignedSearchTimer) clearTimeout(alignedSearchTimer)
  document.removeEventListener('click', closeColumnSettingsOnOutsideClick)
})
</script>
