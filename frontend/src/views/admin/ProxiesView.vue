<template>
  <AppLayout>
    <TablePageLayout>
      <template #filters>
        <div class="flex flex-wrap items-center gap-3">
          <!-- Left: Search + Filters -->
          <div class="relative w-full sm:w-64">
            <Icon
              name="search"
              size="md"
              class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-gray-500"
            />
            <input
              v-model="searchQuery"
              type="text"
              data-test="admin-proxy-search-input"
              :placeholder="t('admin.proxies.searchProxies')"
              class="input pl-10"
              @input="handleSearch"
            />
          </div>

          <div class="w-full sm:w-40">
            <Select
              v-model="filters.protocol"
              :options="protocolOptions"
              :placeholder="t('admin.proxies.allProtocols')"
              data-test="admin-proxy-protocol-filter"
              @change="loadProxies"
            />
          </div>
          <div class="w-full sm:w-36">
            <Select
              v-model="filters.status"
              :options="statusOptions"
              :placeholder="t('admin.proxies.allStatus')"
              data-test="admin-proxy-status-filter"
              @change="loadProxies"
            />
          </div>
          <div class="w-full sm:w-40">
            <Select
              v-model="filters.owner_scope"
              :options="ownerScopeOptions"
              :placeholder="t('admin.proxies.allResourceOwners')"
              data-test="admin-proxy-owner-scope-filter"
              @change="loadProxies"
            />
          </div>
          <div v-if="proxySourceFilterOptions.length > 1" class="w-full sm:w-44">
            <Select
              v-model="filters.source_id"
              :options="proxySourceFilterOptions"
              :placeholder="t('admin.proxies.allSources')"
              data-test="admin-proxy-source-filter"
              @change="loadProxies"
            />
          </div>

          <!-- Right: All action buttons -->
          <div class="flex w-full grow flex-wrap items-center justify-end gap-2 whitespace-nowrap lg:w-auto">
            <button
              @click="loadProxies"
              :disabled="loading"
              class="btn btn-secondary"
              :title="t('common.refresh')"
            >
              <Icon name="refresh" size="md" :class="loading ? 'animate-spin' : ''" />
            </button>
            <button
              type="button"
              class="btn btn-secondary"
              data-test="admin-proxy-source-manager-button"
              @click="openProxySourcesModal"
            >
              <Icon name="cloud" size="md" class="mr-2" />
              {{ t('admin.proxies.sourceManager') }}
            </button>
            <button
              @click="handleBatchTest"
              :disabled="batchTesting || loading"
              class="btn btn-secondary"
              data-test="admin-proxy-batch-test-button"
              :title="t('admin.proxies.testConnection')"
            >
              <Icon name="play" size="md" class="mr-2" />
              {{ t('admin.proxies.testConnection') }}
            </button>
            <button
              @click="handleBatchQualityCheck"
              :disabled="batchQualityChecking || loading"
              class="btn btn-secondary"
              data-test="admin-proxy-batch-quality-button"
              :title="t('admin.proxies.batchQualityCheck')"
            >
              <Icon name="shield" size="md" class="mr-2" :class="batchQualityChecking ? 'animate-pulse' : ''" />
              {{ t('admin.proxies.batchQualityCheck') }}
            </button>
            <button
              @click="openBatchDelete"
              :disabled="selectedCount === 0"
              class="btn btn-danger"
              :title="t('admin.proxies.batchDeleteAction')"
            >
              <Icon name="trash" size="md" class="mr-2" />
              {{ t('admin.proxies.batchDeleteAction') }}
            </button>
            <button @click="showImportData = true" class="btn btn-secondary">
              {{ t('admin.proxies.dataImport') }}
            </button>
            <button @click="showExportDataDialog = true" class="btn btn-secondary">
              {{ selectedCount > 0 ? t('admin.proxies.dataExportSelected') : t('admin.proxies.dataExport') }}
            </button>
            <button
              class="btn btn-primary"
              data-test="admin-proxy-create-button"
              @click="showCreateModal = true"
            >
              <Icon name="plus" size="md" class="mr-2" />
              {{ t('admin.proxies.createProxy') }}
            </button>
          </div>
        </div>
      </template>

      <template #table>
        <div ref="proxyTableRef" class="flex min-h-0 flex-1 flex-col overflow-hidden">
        <DataTable
          :columns="columns"
          :data="proxies"
          :loading="loading"
          :server-side-sort="true"
          default-sort-key="id"
          default-sort-order="desc"
          @sort="handleSort"
        >
          <template #header-select>
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="allVisibleSelected"
              @click.stop
              @change="toggleSelectAllVisible($event)"
            />
          </template>

          <template #cell-select="{ row }">
            <input
              type="checkbox"
              class="h-4 w-4 cursor-pointer rounded border-gray-300 text-primary-600 focus:ring-primary-500"
              :checked="selectedProxyIds.has(row.id)"
              @click.stop
              @change="toggleSelectRow(row.id, $event)"
            />
          </template>

          <template #cell-name="{ value }">
            <span class="font-medium text-gray-900 dark:text-white">{{ value }}</span>
          </template>

          <template #cell-owner_user_id="{ row }">
            <span v-if="row.owner_user_id" class="font-mono text-xs text-primary-600 dark:text-primary-400">
              {{ t('admin.proxies.userResourceOwner', { id: row.owner_user_id }) }}
            </span>
            <span v-else class="text-xs text-gray-500 dark:text-gray-400">
              {{ t('admin.proxies.systemResource') }}
            </span>
          </template>

          <template #cell-visibility="{ row }">
            <span :class="['badge', row.is_public ? 'badge-success' : 'badge-gray']">
              {{ row.is_public ? t('admin.proxies.publicResource') : t('admin.proxies.privateResource') }}
            </span>
          </template>

          <template #cell-kind="{ value }">
            <span :class="['badge', value === 'xray' ? 'badge-primary' : 'badge-gray']">
              {{ value === 'xray' ? 'Xray' : t('admin.proxies.standardProxy') }}
            </span>
          </template>

          <template #cell-protocol="{ value }">
            <span
              v-if="value"
              :class="['badge', value.startsWith('socks5') ? 'badge-primary' : 'badge-gray']"
            >
              {{ value.toUpperCase() }}
            </span>
            <span v-else class="text-sm text-gray-400">-</span>
          </template>

          <template #cell-address="{ row }">
            <div class="flex items-center gap-1.5">
              <code class="code text-xs">{{ row.host }}:{{ row.port }}</code>
              <div class="relative">
                <button
                  type="button"
                  class="rounded p-0.5 text-gray-400 hover:text-primary-600 dark:hover:text-primary-400"
                  :title="t('admin.proxies.copyProxyUrl')"
                  @click.stop="copyProxyUrl(row)"
                  @contextmenu.prevent="toggleCopyMenu(row.id)"
                >
                  <Icon name="copy" size="sm" />
                </button>
                <!-- 右键展开格式选择菜单 -->
                <div
                  v-if="copyMenuProxyId === row.id"
                  class="absolute left-0 top-full z-50 mt-1 w-auto min-w-[180px] rounded-lg border border-gray-200 bg-white py-1 shadow-lg dark:border-dark-500 dark:bg-dark-700"
                >
                  <button
                    v-for="fmt in getCopyFormats(row)"
                    :key="fmt.label"
                    class="flex w-full items-center gap-2 px-3 py-1.5 text-left text-xs hover:bg-gray-100 dark:hover:bg-dark-600"
                    @click.stop="copyFormat(fmt.value)"
                  >
                    <span class="truncate font-mono text-gray-600 dark:text-gray-300">{{ fmt.label }}</span>
                  </button>
                </div>
              </div>
            </div>
          </template>

          <template #cell-auth="{ row }">
            <div v-if="row.username || row.password" class="flex items-center gap-1.5">
              <div class="flex flex-col text-xs">
                <span v-if="row.username" class="font-mono text-gray-700 dark:text-gray-200">
                  {{ visiblePasswordIds.has(row.id) ? row.username : '••••••' }}
                </span>
                <span v-if="row.password" class="font-mono text-gray-500 dark:text-gray-400">
                  {{ visiblePasswordIds.has(row.id) ? row.password : '••••••' }}
                </span>
              </div>
              <button
                v-if="row.username || row.password"
                type="button"
                class="ml-1 rounded p-0.5 text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
                :title="visiblePasswordIds.has(row.id) ? t('admin.proxies.hideCredentials') : t('admin.proxies.showCredentials')"
                @click.stop="visiblePasswordIds.has(row.id) ? visiblePasswordIds.delete(row.id) : visiblePasswordIds.add(row.id)"
              >
                <Icon :name="visiblePasswordIds.has(row.id) ? 'eyeOff' : 'eye'" size="sm" />
              </button>
            </div>
            <span v-else class="text-sm text-gray-400">-</span>
          </template>

          <template #cell-location="{ row }">
            <div class="flex items-center gap-2">
              <img
                v-if="row.country_code"
                :src="flagUrl(row.country_code)"
                :alt="row.country || row.country_code"
                class="h-4 w-6 rounded-sm"
              />
              <span v-if="formatLocation(row)" class="text-sm text-gray-700 dark:text-gray-200">
                {{ formatLocation(row) }}
              </span>
              <span v-else class="text-sm text-gray-400">-</span>
            </div>
          </template>

          <template #cell-account_count="{ row, value }">
            <button
              v-if="(value || 0) > 0"
              type="button"
              class="inline-flex items-center rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-primary-700 hover:bg-gray-200 dark:bg-dark-600 dark:text-primary-300 dark:hover:bg-dark-500"
              @click="openAccountsModal(row)"
            >
              {{ t('admin.groups.accountsCount', { count: value || 0 }) }}
            </button>
            <span
              v-else
              class="inline-flex items-center rounded bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-800 dark:bg-dark-600 dark:text-gray-300"
            >
              {{ t('admin.groups.accountsCount', { count: 0 }) }}
            </span>
          </template>

          <template #cell-latency="{ row }">
            <div class="flex flex-col gap-1">
              <span
                v-if="row.latency_status === 'failed'"
                class="badge badge-danger"
                :title="row.latency_message || undefined"
              >
                {{ t('admin.proxies.latencyFailed') }}
              </span>
              <span
                v-else-if="typeof row.latency_ms === 'number'"
                :class="['badge', row.latency_ms < 200 ? 'badge-success' : 'badge-warning']"
              >
                {{ row.latency_ms }}ms
              </span>
              <span v-else class="text-sm text-gray-400">-</span>
              <div
                v-if="typeof row.quality_checked === 'number'"
                class="flex items-center gap-1 text-xs text-gray-500 dark:text-gray-400"
                :title="row.quality_summary || undefined"
              >
                <span>{{ t('admin.proxies.qualityInline', { grade: row.quality_grade || '-', score: row.quality_score ?? '-' }) }}</span>
                <span class="badge" :class="qualityOverallClass(row.quality_status)">
                  {{ qualityOverallLabel(row.quality_status) }}
                </span>
              </div>
            </div>
          </template>

          <template #cell-expiry="{ row }">
            <span v-if="!row.expires_at" class="text-sm text-gray-400">{{ t('admin.proxies.neverExpires') }}</span>
            <div v-else class="flex flex-col text-xs">
              <span class="text-gray-700 dark:text-gray-200">{{ formatDateTime(row.expires_at) }}</span>
              <span :class="expiryBadgeClass(row)">{{ expiryLabel(row) }}</span>
            </div>
          </template>

          <template #cell-created_at="{ row }">
            <span class="text-xs text-gray-600 dark:text-gray-300">{{ formatDateTime(row.created_at) }}</span>
          </template>

          <template #cell-status="{ value }">
            <span
              :class="[
                'badge',
                value === 'active' ? 'badge-success' : value === 'expired' ? 'badge-danger' : 'badge-danger'
              ]"
            >
              {{ t('admin.accounts.status.' + value) }}
            </span>
          </template>

          <template #cell-actions="{ row }">
            <div class="flex items-center gap-1">
              <button
                @click="handleTestConnection(row)"
                :disabled="testingProxyIds.has(row.id)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-emerald-50 hover:text-emerald-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-emerald-900/20 dark:hover:text-emerald-400"
              >
                <svg
                  v-if="testingProxyIds.has(row.id)"
                  class="h-4 w-4 animate-spin"
                  fill="none"
                  viewBox="0 0 24 24"
                >
                  <circle
                    class="opacity-25"
                    cx="12"
                    cy="12"
                    r="10"
                    stroke="currentColor"
                    stroke-width="4"
                  ></circle>
                  <path
                    class="opacity-75"
                    fill="currentColor"
                    d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
                  ></path>
                </svg>
                <Icon v-else name="checkCircle" size="sm" />
                <span class="text-xs">{{ t('admin.proxies.testConnection') }}</span>
              </button>
              <button
                @click="handleQualityCheck(row)"
                :disabled="qualityCheckingProxyIds.has(row.id)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-blue-50 hover:text-blue-600 disabled:cursor-not-allowed disabled:opacity-50 dark:hover:bg-blue-900/20 dark:hover:text-blue-400"
              >
                <svg
                  v-if="qualityCheckingProxyIds.has(row.id)"
                  class="h-4 w-4 animate-spin"
                  fill="none"
                  viewBox="0 0 24 24"
                >
                  <circle
                    class="opacity-25"
                    cx="12"
                    cy="12"
                    r="10"
                    stroke="currentColor"
                    stroke-width="4"
                  ></circle>
                  <path
                    class="opacity-75"
                    fill="currentColor"
                    d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
                  ></path>
                </svg>
                <Icon v-else name="shield" size="sm" />
                <span class="text-xs">{{ t('admin.proxies.qualityCheck') }}</span>
              </button>
              <button
                @click="handleEdit(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:hover:bg-dark-700 dark:hover:text-primary-400"
              >
                <Icon name="edit" size="sm" />
                <span class="text-xs">{{ t('common.edit') }}</span>
              </button>
              <button
                @click="handleDelete(row)"
                class="flex flex-col items-center gap-0.5 rounded-lg p-1.5 text-gray-500 transition-colors hover:bg-red-50 hover:text-red-600 dark:hover:bg-red-900/20 dark:hover:text-red-400"
              >
                <Icon name="trash" size="sm" />
                <span class="text-xs">{{ t('common.delete') }}</span>
              </button>
            </div>
          </template>

          <template #empty>
            <EmptyState
              :title="t('admin.proxies.noProxiesYet')"
              :description="t('admin.proxies.createFirstProxy')"
              :action-text="t('admin.proxies.createProxy')"
              @action="showCreateModal = true"
            />
          </template>
        </DataTable>
        </div>
      </template>

      <template #pagination>
        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </template>
    </TablePageLayout>

    <!-- Create Proxy Modal -->
    <BaseDialog
      :show="showCreateModal"
      :title="t('admin.proxies.createProxy')"
      width="normal"
      @close="closeCreateModal"
    >
      <div
        class="mb-6 flex flex-wrap items-center border-b border-gray-200 dark:border-dark-600"
        data-test="admin-proxy-create-mode-tabs"
      >
        <button
          type="button"
          data-test="admin-proxy-create-mode-standard"
          :class="[
            '-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors',
            createMode === 'standard'
              ? 'border-primary-500 text-primary-600 dark:text-primary-400'
              : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-300',
          ]"
          @click="createMode = 'standard'"
        >
          <Icon name="plus" size="sm" class="mr-1.5 inline" />
          {{ t('admin.proxies.standardAdd') }}
        </button>
        <button
          type="button"
          data-test="admin-proxy-create-mode-batch"
          :class="[
            '-mb-px border-b-2 px-4 py-2 text-sm font-medium transition-colors',
            createMode === 'batch'
              ? 'border-primary-500 text-primary-600 dark:text-primary-400'
              : 'border-transparent text-gray-500 hover:text-gray-700 dark:text-gray-400 dark:hover:text-gray-300',
          ]"
          @click="createMode = 'batch'"
        >
          <Icon name="upload" size="sm" class="mr-1.5 inline" />
          {{ t('admin.proxies.batchAdd') }}
        </button>
      </div>

      <form id="create-proxy-form" class="space-y-5" @submit.prevent="handleCreateProxy">
        <template v-if="createMode === 'standard'">
          <div data-test="admin-proxy-name-field">
            <label class="input-label">
              {{ inputMode === 'config' ? t('admin.proxies.importNamePrefix') : t('admin.proxies.name') }}
            </label>
            <input
              v-model.trim="createForm.name"
              type="text"
              :required="inputMode !== 'config'"
              class="input"
              data-test="admin-proxy-name-input"
              :placeholder="inputMode === 'config'
                ? t('admin.proxies.importNamePrefixPlaceholder')
                : t('admin.proxies.enterProxyName')"
            />
          </div>

          <div data-test="admin-proxy-input-mode-field">
            <label class="input-label">{{ t('admin.proxies.creationMethod') }}</label>
            <Select
              v-model="inputMode"
              :options="inputModeOptions"
              :searchable="false"
              data-test="admin-proxy-input-mode-selector"
            />
          </div>

          <template v-if="inputMode === 'direct'">
            <div data-test="admin-proxy-protocol-field">
              <label class="input-label">{{ t('admin.proxies.protocol') }}</label>
              <Select
                v-model="createForm.protocol"
                :options="standardProtocolSelectOptions"
                :searchable="false"
                data-test="admin-proxy-protocol-selector"
              />
            </div>
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
              <div>
                <label class="input-label">{{ t('admin.proxies.host') }}</label>
                <input
                  v-model.trim="createForm.host"
                  type="text"
                  required
                  :placeholder="t('admin.proxies.form.hostPlaceholder')"
                  class="input"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.proxies.port') }}</label>
                <input
                  v-model.number="createForm.port"
                  type="number"
                  required
                  min="1"
                  max="65535"
                  :placeholder="t('admin.proxies.form.portPlaceholder')"
                  class="input"
                />
              </div>
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.username') }}</label>
              <input
                v-model.trim="createForm.username"
                type="text"
                class="input"
                :placeholder="t('admin.proxies.optionalAuth')"
              />
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.password') }}</label>
              <div class="relative">
                <input
                  v-model="createForm.password"
                  :type="createPasswordVisible ? 'text' : 'password'"
                  class="input pr-10"
                  :placeholder="t('admin.proxies.optionalAuth')"
                />
                <button
                  type="button"
                  class="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
                  :title="createPasswordVisible ? t('admin.proxies.hideCredentials') : t('admin.proxies.showCredentials')"
                  @click="createPasswordVisible = !createPasswordVisible"
                >
                  <Icon :name="createPasswordVisible ? 'eyeOff' : 'eye'" size="md" />
                </button>
              </div>
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.expiresAt') }}</label>
              <div class="mb-2 flex flex-wrap gap-2">
                <button
                  v-for="d in EXPIRY_PRESETS"
                  :key="d"
                  type="button"
                  class="btn btn-sm"
                  :class="createForm.expires_at === addDaysToBase('', d) ? 'btn-primary' : 'btn-secondary'"
                  @click="createExpiresDays = d"
                >
                  {{ t('admin.proxies.nDays', { days: d }) }}
                </button>
              </div>
              <input
                v-model.number="createExpiresDays"
                type="number"
                min="0"
                class="input mb-2"
                :placeholder="t('admin.proxies.expiryDaysPlaceholder')"
              />
              <input v-model="createForm.expires_at" type="date" max="9999-12-31" class="input" />
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.fallbackMode') }}</label>
              <Select v-model="createForm.fallback_mode" :options="[
                { label: t('admin.proxies.fallbackNone'), value: 'none' },
                { label: t('admin.proxies.fallbackProxy'), value: 'proxy' },
                { label: t('admin.proxies.fallbackDirect'), value: 'direct' },
              ]" />
            </div>
            <div v-if="createForm.fallback_mode === 'proxy'">
              <label class="input-label">{{ t('admin.proxies.backupProxy') }}</label>
              <Select v-model="createForm.backup_proxy_id" :options="backupProxyOptions()" />
            </div>
          </template>

          <template v-else-if="inputMode === 'xray'">
            <div>
              <label class="input-label">{{ t('admin.proxies.shareLinkInput') }}</label>
              <textarea
                v-model.trim="createForm.import_content"
                rows="7"
                required
                class="input break-all font-mono text-xs"
                data-test="admin-proxy-share-input"
                :placeholder="t('admin.proxies.shareLinkPlaceholder')"
              ></textarea>
              <p class="input-hint mt-2">{{ t('admin.proxies.shareLinkHint') }}</p>
            </div>
          </template>

          <template v-else-if="inputMode === 'source'">
            <div>
              <label class="input-label">{{ t('admin.proxies.subscriptionUrl') }}</label>
              <input
                v-model.trim="createForm.subscription_url"
                type="url"
                required
                class="input"
                data-test="admin-proxy-source-url"
                :placeholder="t('admin.proxies.subscriptionUrlPlaceholder')"
              />
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.refreshIntervalMinutes') }}</label>
              <input
                v-model.number="createForm.refresh_interval_minutes"
                type="number"
                min="5"
                max="10080"
                required
                class="input"
              />
              <p class="input-hint mt-2">{{ t('admin.proxies.refreshIntervalHint') }}</p>
            </div>
          </template>

          <template v-else>
            <div>
              <label class="input-label">{{ t('admin.proxies.configFile') }}</label>
              <div
                class="flex flex-col gap-3 rounded-md border border-dashed border-gray-300 bg-gray-50 px-4 py-3 dark:border-dark-600 dark:bg-dark-800 sm:flex-row sm:items-center sm:justify-between"
              >
                <div class="min-w-0">
                  <div class="truncate text-sm text-gray-700 dark:text-dark-200" :title="configFileName">
                    {{ configFileName || t('admin.proxies.configFileNotSelected') }}
                  </div>
                  <div class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                    {{ t('admin.proxies.configFileTypes') }}
                  </div>
                </div>
                <div class="flex shrink-0 items-center gap-2">
                  <button
                    type="button"
                    class="btn btn-secondary"
                    data-test="admin-proxy-config-file-button"
                    :disabled="configFileReading"
                    @click="openConfigFilePicker"
                  >
                    <Icon name="upload" size="sm" class="mr-2" />
                    {{ configFileName ? t('admin.proxies.replaceConfigFile') : t('common.chooseFile') }}
                  </button>
                  <button
                    v-if="configFileName"
                    type="button"
                    class="inline-flex h-9 w-9 items-center justify-center rounded-md text-gray-500 hover:bg-gray-200 hover:text-gray-700 focus:outline-none focus:ring-2 focus:ring-primary-500 dark:text-dark-300 dark:hover:bg-dark-700 dark:hover:text-white"
                    data-test="admin-proxy-config-file-clear"
                    :title="t('admin.proxies.clearConfigFile')"
                    :aria-label="t('admin.proxies.clearConfigFile')"
                    @click="clearConfigFile"
                  >
                    <Icon name="x" size="sm" />
                  </button>
                </div>
              </div>
              <input
                ref="configFileInput"
                type="file"
                class="hidden"
                accept=".json,.yaml,.yml,.txt,.conf,application/json,application/yaml,text/yaml,text/plain"
                data-test="admin-proxy-config-file-input"
                @change="handleConfigFileChange"
              />
            </div>
            <div>
              <label class="input-label">{{ t('admin.proxies.configInput') }}</label>
              <textarea
                v-model="createForm.import_content"
                rows="10"
                required
                class="input break-all font-mono text-xs"
                data-test="admin-proxy-config-input"
                :placeholder="t('admin.proxies.configPlaceholder')"
              ></textarea>
              <p class="input-hint mt-2">{{ t('admin.proxies.configHint') }}</p>
            </div>
          </template>
        </template>

        <template v-else>
          <div>
            <label class="input-label">{{ t('admin.proxies.importNamePrefix') }}</label>
            <input
              v-model.trim="createForm.name"
              type="text"
              class="input"
              :placeholder="t('admin.proxies.importNamePrefixPlaceholder')"
            />
          </div>
          <div>
            <label class="input-label">{{ t('admin.proxies.batchInput') }}</label>
            <textarea
              v-model="batchInput"
              rows="10"
              required
              class="input break-all font-mono text-xs"
              data-test="admin-proxy-batch-input"
              :placeholder="t('admin.proxies.batchModernPlaceholder')"
              @input="parseBatchInput"
            ></textarea>
            <p class="input-hint mt-2">{{ t('admin.proxies.batchModernHint') }}</p>
          </div>
          <div v-if="batchParseResult.total > 0" class="rounded-md bg-gray-50 p-4 dark:bg-dark-700">
            <div class="grid grid-cols-2 gap-2 text-center sm:grid-cols-4">
              <div>
                <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.proxies.totalCount') }}</div>
                <div class="mt-1 font-semibold text-gray-900 dark:text-white">{{ batchParseResult.total }}</div>
              </div>
              <div>
                <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.proxies.validCount') }}</div>
                <div class="mt-1 font-semibold text-emerald-600">{{ batchParseResult.valid }}</div>
              </div>
              <div>
                <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.proxies.invalidLabel') }}</div>
                <div class="mt-1 font-semibold text-amber-600">{{ batchParseResult.invalid }}</div>
              </div>
              <div>
                <div class="text-xs text-gray-500 dark:text-gray-400">{{ t('admin.proxies.duplicateLabel') }}</div>
                <div class="mt-1 font-semibold text-gray-600 dark:text-gray-300">{{ batchParseResult.duplicate }}</div>
              </div>
            </div>
          </div>
        </template>

        <label class="flex cursor-pointer items-center gap-2">
          <input
            v-model="createForm.is_public"
            type="checkbox"
            class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
            data-test="admin-proxy-is-public"
          />
          <span class="text-sm text-gray-700 dark:text-gray-200">{{ t('admin.proxies.publicToUsers') }}</span>
        </label>
      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button @click="closeCreateModal" type="button" class="btn btn-secondary">
            {{ t('common.cancel') }}
          </button>
          <button
            type="submit"
            form="create-proxy-form"
            :disabled="submitting || !createFormReady"
            class="btn btn-primary"
          >
            <svg
              v-if="submitting"
              class="-ml-1 mr-2 h-4 w-4 animate-spin"
              fill="none"
              viewBox="0 0 24 24"
            >
              <circle
                class="opacity-25"
                cx="12"
                cy="12"
                r="10"
                stroke="currentColor"
                stroke-width="4"
              ></circle>
              <path
                class="opacity-75"
                fill="currentColor"
                d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
              ></path>
            </svg>
            {{ createSubmitLabel }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- System proxy subscription sources -->
    <BaseDialog
      :show="showProxySourcesModal"
      :title="t('admin.proxies.sourceManagerTitle')"
      width="wide"
      @close="closeProxySourcesModal"
    >
      <div data-test="admin-proxy-source-manager">
        <div
          v-if="proxySourceError"
          class="mb-5 rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-900/20 dark:text-red-300"
        >
          {{ proxySourceError }}
        </div>

        <div class="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,1.6fr)_minmax(280px,0.9fr)]">
          <section class="min-w-0">
            <div class="mb-3 flex flex-wrap items-center justify-between gap-3">
              <h4 class="text-sm font-semibold text-gray-900 dark:text-white">
                {{ t('admin.proxies.sourceManager') }}
              </h4>
              <div class="flex items-center gap-2">
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  data-test="admin-proxy-source-sync-all"
                  :disabled="proxySourcesSyncingAll || proxySourcesLoading || proxySources.length === 0"
                  :title="t('admin.proxies.sourceSyncAll')"
                  @click="syncAllProxySources"
                >
                  <Icon name="refresh" size="sm" class="mr-2" :class="proxySourcesSyncingAll ? 'animate-spin' : ''" />
                  {{ t('admin.proxies.sourceSyncAll') }}
                </button>
                <button
                  type="button"
                  class="btn btn-secondary btn-sm"
                  :disabled="proxySourcesLoading"
                  :title="t('common.refresh')"
                  @click="loadProxySources"
                >
                  <Icon name="refresh" size="sm" :class="proxySourcesLoading ? 'animate-spin' : ''" />
                </button>
              </div>
            </div>

            <div
              v-if="proxySourceSyncAllSummary"
              class="mb-3 rounded-md border border-gray-200 bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:border-dark-600 dark:bg-dark-700/40 dark:text-gray-300"
              data-test="admin-proxy-source-sync-all-summary"
            >
              {{ proxySourceSyncAllSummary }}
            </div>

            <div
              v-if="proxySourcesLoading"
              class="flex min-h-44 items-center justify-center text-sm text-gray-500 dark:text-gray-400"
            >
              {{ t('admin.proxies.sourceLoading') }}
            </div>
            <div
              v-else-if="proxySources.length === 0"
              class="flex min-h-44 items-center justify-center rounded-md border border-dashed border-gray-300 px-4 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-gray-400"
            >
              {{ t('admin.proxies.sourceEmpty') }}
            </div>
            <template v-else>
              <div
                class="overflow-x-auto rounded-md border border-gray-200 dark:border-dark-600"
                data-test="admin-proxy-source-table-scroll"
              >
                <table class="w-full min-w-[1040px] divide-y divide-gray-200 text-left text-sm dark:divide-dark-600">
                <thead class="bg-gray-50 dark:bg-dark-700/60">
                  <tr>
                    <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnName') }}
                    </th>
                    <th class="px-4 py-3 text-center font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnVisibility') }}
                    </th>
                    <th class="px-4 py-3 text-center font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnNodes') }}
                    </th>
                    <th class="px-4 py-3 text-center font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnInterval') }}
                    </th>
                    <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnQuota') }}
                    </th>
                    <th class="px-4 py-3 font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnStatus') }}
                    </th>
                    <th class="px-4 py-3 text-right font-medium text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceColumnActions') }}
                    </th>
                  </tr>
                </thead>
                <tbody class="divide-y divide-gray-100 bg-white dark:divide-dark-700 dark:bg-dark-800">
                  <tr v-for="source in proxySources" :key="source.id">
                    <td class="px-4 py-3 align-top">
                      <div class="max-w-[260px] font-medium text-gray-900 dark:text-white">
                        {{ source.name }}
                      </div>
                      <div
                        class="mt-1 max-w-[260px] truncate font-mono text-xs text-gray-500 dark:text-gray-400"
                        :title="maskSubscriptionUrl(source.subscription_url)"
                      >
                        {{ maskSubscriptionUrl(source.subscription_url) }}
                      </div>
                    </td>
                    <td class="px-4 py-3 text-center align-top">
                      <span :class="['badge', source.is_public ? 'badge-success' : 'badge-gray']">
                        {{ source.is_public ? t('admin.proxies.publicResource') : t('admin.proxies.privateResource') }}
                      </span>
                    </td>
                    <td class="px-4 py-3 text-center align-top">
                      <button
                        type="button"
                        class="text-primary-600 hover:underline dark:text-primary-400"
                        :data-test="`admin-proxy-source-filter-${source.id}`"
                        :title="t('admin.proxies.sourceFilterBySource')"
                        @click="filterProxiesBySource(source)"
                      >
                        {{ t('admin.proxies.sourceNodeCount', {
                          active: Number(source.active_node_count) || 0,
                          total: Number(source.node_count) || 0,
                        }) }}
                      </button>
                    </td>
                    <td class="whitespace-nowrap px-4 py-3 text-center align-top text-gray-600 dark:text-gray-300">
                      {{ t('admin.proxies.sourceIntervalValue', { minutes: source.refresh_interval_minutes }) }}
                      <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                        {{ source.sync_enabled === false
                          ? t('admin.proxies.sourceSyncPausedHint')
                          : t('admin.proxies.sourceNextSyncAt', { time: source.next_sync_at ? formatDateTime(source.next_sync_at) : '-' }) }}
                      </div>
                    </td>
                    <td class="whitespace-nowrap px-4 py-3 align-top text-gray-600 dark:text-gray-300">
                      <div>{{ proxySourceTrafficText(source) }}</div>
                      <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">
                        {{ t('admin.proxies.sourceExpiresAt', { time: source.sub_expires_at ? formatDateTime(source.sub_expires_at) : '-' }) }}
                      </div>
                    </td>
                    <td class="px-4 py-3 align-top">
                      <span :class="['badge', proxySourceStatusClass(source.last_sync_status)]">
                        {{ proxySourceStatusLabel(source.last_sync_status) }}
                      </span>
                      <div class="mt-1 whitespace-nowrap text-xs text-gray-500 dark:text-gray-400">
                        {{ source.last_synced_at ? formatDateTime(source.last_synced_at) : '-' }}
                      </div>
                      <div
                        v-if="source.last_sync_error"
                        class="mt-1 max-w-[220px] truncate text-xs text-red-600 dark:text-red-400"
                        :title="source.last_sync_error"
                      >
                        {{ source.last_sync_error }}
                      </div>
                    </td>
                    <td class="px-4 py-3 align-top">
                      <div class="flex justify-end gap-1.5">
                        <button
                          type="button"
                          class="btn btn-secondary btn-sm !px-2"
                          :data-test="`admin-proxy-source-sync-${source.id}`"
                          :disabled="syncingProxySourceId === source.id"
                          :title="t('admin.proxies.sourceSync')"
                          :aria-label="t('admin.proxies.sourceSync')"
                          @click="syncProxySource(source)"
                        >
                          <Icon
                            name="refresh"
                            size="sm"
                            :class="syncingProxySourceId === source.id ? 'animate-spin' : ''"
                          />
                        </button>
                        <button
                          type="button"
                          class="btn btn-secondary btn-sm !px-2"
                          :data-test="`admin-proxy-source-toggle-${source.id}`"
                          :disabled="togglingProxySourceId === source.id"
                          :title="source.sync_enabled === false ? t('admin.proxies.sourceResumeSync') : t('admin.proxies.sourcePauseSync')"
                          :aria-label="source.sync_enabled === false ? t('admin.proxies.sourceResumeSync') : t('admin.proxies.sourcePauseSync')"
                          @click="toggleProxySourceSync(source)"
                        >
                          <Icon :name="source.sync_enabled === false ? 'play' : 'ban'" size="sm" />
                        </button>
                        <button
                          type="button"
                          class="btn btn-secondary btn-sm !px-2"
                          :data-test="`admin-proxy-source-edit-${source.id}`"
                          :title="t('common.edit')"
                          :aria-label="t('common.edit')"
                          @click="editProxySource(source)"
                        >
                          <Icon name="edit" size="sm" />
                        </button>
                        <button
                          type="button"
                          class="btn btn-danger btn-sm !px-2"
                          :data-test="`admin-proxy-source-delete-${source.id}`"
                          :disabled="proxySourceDeleting"
                          :title="t('common.delete')"
                          :aria-label="t('common.delete')"
                          @click="proxySourcePendingDelete = source"
                        >
                          <Icon name="trash" size="sm" />
                        </button>
                      </div>
                    </td>
                  </tr>
                </tbody>
                </table>
              </div>
              <Pagination
                v-if="proxySourcePagination.total > PROXY_SOURCE_PAGE_SIZE"
                :total="proxySourcePagination.total"
                :page="proxySourcePagination.page"
                :page-size="PROXY_SOURCE_PAGE_SIZE"
                :show-page-size-selector="false"
                data-test="admin-proxy-source-pagination"
                @update:page="handleProxySourcePageChange"
              />
            </template>
          </section>

          <section class="border-t border-gray-200 pt-5 dark:border-dark-600 lg:border-l lg:border-t-0 lg:pl-6 lg:pt-0">
            <h4 class="mb-4 text-sm font-semibold text-gray-900 dark:text-white">
              {{ editingProxySourceId ? t('admin.proxies.sourceEditTitle') : t('admin.proxies.sourceAddTitle') }}
            </h4>
            <form id="proxy-source-manager-form" class="space-y-5" @submit.prevent="saveProxySource">
              <div>
                <label class="input-label">{{ t('admin.proxies.subscriptionName') }}</label>
                <input
                  v-model.trim="proxySourceForm.name"
                  type="text"
                  required
                  class="input"
                  :placeholder="t('admin.proxies.subscriptionNamePlaceholder')"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.proxies.subscriptionUrl') }}</label>
                <input
                  v-model.trim="proxySourceForm.subscription_url"
                  type="url"
                  required
                  class="input"
                  :placeholder="t('admin.proxies.subscriptionUrlPlaceholder')"
                />
              </div>
              <div>
                <label class="input-label">{{ t('admin.proxies.refreshIntervalMinutes') }}</label>
                <input
                  v-model.number="proxySourceForm.refresh_interval_minutes"
                  type="number"
                  min="5"
                  max="10080"
                  required
                  class="input"
                />
                <p class="input-hint mt-2">{{ t('admin.proxies.refreshIntervalHint') }}</p>
              </div>
              <label class="flex cursor-pointer items-center gap-2">
                <input
                  v-model="proxySourceForm.sync_enabled"
                  type="checkbox"
                  data-test="admin-proxy-source-sync-enabled"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                <span class="text-sm text-gray-700 dark:text-gray-200">{{ t('admin.proxies.sourceAutoSyncEnabled') }}</span>
              </label>
              <label class="flex cursor-pointer items-center gap-2">
                <input
                  v-model="proxySourceForm.is_public"
                  type="checkbox"
                  class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500"
                />
                <span class="text-sm text-gray-700 dark:text-gray-200">{{ t('admin.proxies.publicToUsers') }}</span>
              </label>
              <div class="flex flex-wrap justify-end gap-2">
                <button
                  v-if="editingProxySourceId"
                  type="button"
                  class="btn btn-secondary"
                  :disabled="proxySourceSaving"
                  @click="resetProxySourceForm"
                >
                  {{ t('common.cancel') }}
                </button>
                <button
                  type="submit"
                  class="btn btn-primary"
                  :disabled="proxySourceSaving || !proxySourceFormReady"
                >
                  {{ proxySourceSaving
                    ? t('admin.proxies.saving')
                    : editingProxySourceId
                      ? t('admin.proxies.sourceUpdate')
                      : t('admin.proxies.sourceSave') }}
                </button>
              </div>
            </form>
          </section>
        </div>
      </div>

      <template #footer>
        <div class="flex justify-end">
          <button type="button" class="btn btn-secondary" @click="closeProxySourcesModal">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <ConfirmDialog
      :show="Boolean(proxySourcePendingDelete)"
      :title="t('admin.proxies.sourceDeleteTitle')"
      :message="t('admin.proxies.sourceDeleteConfirm', { name: proxySourcePendingDelete?.name || '' })"
      :confirm-text="t('common.delete')"
      danger
      @confirm="confirmDeleteProxySource"
      @cancel="proxySourcePendingDelete = null"
    />

    <!-- Edit Proxy Modal -->
    <BaseDialog
      :show="showEditModal"
      :title="t('admin.proxies.editProxy')"
      width="normal"
      @close="closeEditModal"
    >
      <form
        v-if="editingProxy"
        id="edit-proxy-form"
        @submit.prevent="handleUpdateProxy"
        class="space-y-5"
      >
        <div>
          <label class="input-label">{{ t('admin.proxies.name') }}</label>
          <input v-model="editForm.name" type="text" required class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.kind') }}</label>
          <Select v-model="editForm.kind" :options="proxyKindOptions" :searchable="false" @change="normalizeEditProtocol" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.protocol') }}</label>
          <Select v-model="editForm.protocol" :options="editProtocolSelectOptions" :searchable="false" />
        </div>
        <div class="grid grid-cols-2 gap-4">
          <div>
            <label class="input-label">{{ t('admin.proxies.host') }}</label>
            <input v-model="editForm.host" type="text" required class="input" />
          </div>
          <div>
            <label class="input-label">{{ t('admin.proxies.port') }}</label>
            <input
              v-model.number="editForm.port"
              type="number"
              required
              min="1"
              max="65535"
              class="input"
            />
          </div>
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.username') }}</label>
          <input v-model="editForm.username" type="text" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.password') }}</label>
          <div class="relative">
            <input
              v-model="editForm.password"
              :type="editPasswordVisible ? 'text' : 'password'"
              :placeholder="t('admin.proxies.leaveEmptyToKeep')"
              class="input pr-10"
              @input="editPasswordDirty = true"
            />
            <button
              type="button"
              class="absolute right-3 top-1/2 -translate-y-1/2 text-gray-400 hover:text-gray-600 dark:hover:text-gray-300"
              @click="editPasswordVisible = !editPasswordVisible"
            >
              <Icon :name="editPasswordVisible ? 'eyeOff' : 'eye'" size="md" />
            </button>
          </div>
        </div>
        <div v-if="editForm.kind === 'xray'">
          <label class="input-label">{{ t('admin.proxies.xrayNodeUri') }}</label>
          <textarea v-model.trim="editForm.xray_raw" rows="4" class="input break-all font-mono text-xs" :placeholder="t('admin.proxies.xrayNodeUriPlaceholder')"></textarea>
          <p class="input-hint mt-1">{{ t('admin.proxies.xrayNodeUriHint') }}</p>
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.status') }}</label>
          <Select v-model="editForm.status" :options="editStatusOptions" />
        </div>
        <label class="flex items-center gap-2">
          <input
            v-model="editForm.is_public"
            type="checkbox"
            :disabled="editingProxy.owner_user_id != null"
            class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500 disabled:cursor-not-allowed disabled:opacity-50"
          />
          <span class="text-sm text-gray-700 dark:text-gray-200">{{ t('admin.proxies.publicToUsers') }}</span>
        </label>
        <div>
          <label class="input-label">{{ t('admin.proxies.expiresAt') }}</label>
          <div class="mb-2 flex flex-wrap gap-2">
            <button
              v-for="d in EXPIRY_PRESETS"
              :key="d"
              type="button"
              class="btn btn-sm"
              :class="editForm.expires_at === addDaysToBase(editBaseDate, d) ? 'btn-primary' : 'btn-secondary'"
              @click="editExpiresDays = d"
            >
              {{ t('admin.proxies.nDays', { days: d }) }}
            </button>
          </div>
          <input
            v-model.number="editExpiresDays"
            type="number"
            min="0"
            class="input mb-2"
            :placeholder="t('admin.proxies.expiryDaysPlaceholder')"
          />
          <input v-model="editForm.expires_at" type="date" max="9999-12-31" class="input" />
        </div>
        <div>
          <label class="input-label">{{ t('admin.proxies.fallbackMode') }}</label>
          <Select v-model="editForm.fallback_mode" :options="[
            { label: t('admin.proxies.fallbackNone'), value: 'none' },
            { label: t('admin.proxies.fallbackProxy'), value: 'proxy' },
            { label: t('admin.proxies.fallbackDirect'), value: 'direct' },
          ]" />
        </div>
        <div v-if="editForm.fallback_mode === 'proxy'">
          <label class="input-label">{{ t('admin.proxies.backupProxy') }}</label>
          <Select v-model="editForm.backup_proxy_id" :options="backupProxyOptions(editingProxy?.id)" />
        </div>

      </form>

      <template #footer>
        <div class="flex justify-end gap-3">
          <button @click="closeEditModal" type="button" class="btn btn-secondary">
            {{ t('common.cancel') }}
          </button>
          <button
            v-if="editingProxy"
            type="submit"
            form="edit-proxy-form"
            :disabled="submitting"
            class="btn btn-primary"
          >
            <svg
              v-if="submitting"
              class="-ml-1 mr-2 h-4 w-4 animate-spin"
              fill="none"
              viewBox="0 0 24 24"
            >
              <circle
                class="opacity-25"
                cx="12"
                cy="12"
                r="10"
                stroke="currentColor"
                stroke-width="4"
              ></circle>
              <path
                class="opacity-75"
                fill="currentColor"
                d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
              ></path>
            </svg>
            {{ submitting ? t('admin.proxies.updating') : t('common.update') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Delete Confirmation Dialog -->
    <ConfirmDialog
      :show="showDeleteDialog"
      :title="t('admin.proxies.deleteProxy')"
      :message="t('admin.proxies.deleteConfirm', { name: deletingProxy?.name })"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmDelete"
      @cancel="showDeleteDialog = false"
    />

    <!-- Batch Delete Confirmation Dialog -->
    <ConfirmDialog
      :show="showBatchDeleteDialog"
      :title="t('admin.proxies.batchDelete')"
      :message="t('admin.proxies.batchDeleteConfirm', { count: selectedCount })"
      :confirm-text="t('common.delete')"
      :cancel-text="t('common.cancel')"
      :danger="true"
      @confirm="confirmBatchDelete"
      @cancel="showBatchDeleteDialog = false"
    />
    <ConfirmDialog
      :show="showExportDataDialog"
      :title="t('admin.proxies.dataExport')"
      :message="t('admin.proxies.dataExportConfirmMessage')"
      :confirm-text="t('admin.proxies.dataExportConfirm')"
      :cancel-text="t('common.cancel')"
      @confirm="handleExportData"
      @cancel="showExportDataDialog = false"
    />

    <ImportDataModal
      :show="showImportData"
      @close="showImportData = false"
      @imported="handleDataImported"
    />

    <BaseDialog
      :show="showQualityReportDialog"
      :title="t('admin.proxies.qualityReportTitle')"
      width="normal"
      @close="closeQualityReportDialog"
    >
      <div v-if="qualityReport" class="space-y-4">
        <div class="rounded-lg border border-gray-200 bg-gray-50 p-4 dark:border-dark-600 dark:bg-dark-700">
          <div class="flex items-center justify-between gap-4">
            <div>
              <div class="text-sm text-gray-500 dark:text-gray-400">
                {{ qualityReportProxy?.name || '-' }}
              </div>
              <div class="mt-1 text-sm text-gray-700 dark:text-gray-200">
                {{ qualityReport.summary }}
              </div>
            </div>
            <div class="text-right">
              <div class="text-2xl font-semibold text-gray-900 dark:text-white">
                {{ qualityReport.score }}
              </div>
              <div class="text-xs text-gray-500 dark:text-gray-400">
                {{ t('admin.proxies.qualityGrade', { grade: qualityReport.grade }) }}
              </div>
            </div>
          </div>
          <div class="mt-3 grid grid-cols-2 gap-2 text-xs text-gray-600 dark:text-gray-300">
            <div>{{ t('admin.proxies.qualityExitIP') }}: {{ qualityReport.exit_ip || '-' }}</div>
            <div>{{ t('admin.proxies.qualityCountry') }}: {{ qualityReport.country || '-' }}</div>
            <div>
              {{ t('admin.proxies.qualityBaseLatency') }}:
              {{ typeof qualityReport.base_latency_ms === 'number' ? `${qualityReport.base_latency_ms}ms` : '-' }}
            </div>
            <div>{{ t('admin.proxies.qualityCheckedAt') }}: {{ new Date(qualityReport.checked_at * 1000).toLocaleString() }}</div>
          </div>
        </div>

        <div class="max-h-80 overflow-auto rounded-lg border border-gray-200 dark:border-dark-600">
          <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
            <thead class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-800 dark:text-dark-400">
              <tr>
                <th class="whitespace-nowrap px-3 py-2 text-left">{{ t('admin.proxies.qualityTableTarget') }}</th>
                <th class="whitespace-nowrap px-3 py-2 text-left">{{ t('admin.proxies.qualityTableStatus') }}</th>
                <th class="whitespace-nowrap px-3 py-2 text-left">HTTP</th>
                <th class="whitespace-nowrap px-3 py-2 text-left">{{ t('admin.proxies.qualityTableLatency') }}</th>
                <th class="px-3 py-2 text-left">{{ t('admin.proxies.qualityTableMessage') }}</th>
              </tr>
            </thead>
            <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-900">
              <tr v-for="item in qualityReport.items" :key="item.target">
                <td class="whitespace-nowrap px-3 py-2 text-gray-900 dark:text-white">{{ qualityTargetLabel(item.target) }}</td>
                <td class="whitespace-nowrap px-3 py-2">
                  <span class="badge whitespace-nowrap" :class="qualityStatusClass(item.status)">{{ qualityStatusLabel(item.status) }}</span>
                </td>
                <td class="whitespace-nowrap px-3 py-2 text-gray-600 dark:text-gray-300">{{ item.http_status ?? '-' }}</td>
                <td class="whitespace-nowrap px-3 py-2 text-gray-600 dark:text-gray-300">
                  {{ typeof item.latency_ms === 'number' ? `${item.latency_ms}ms` : '-' }}
                </td>
                <td class="px-3 py-2 text-gray-600 dark:text-gray-300">
                  <span>{{ item.message || '-' }}</span>
                  <span v-if="item.cf_ray" class="ml-1 text-xs text-gray-400">(cf-ray: {{ item.cf_ray }})</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>
      <template #footer>
        <div class="flex justify-end">
          <button @click="closeQualityReportDialog" class="btn btn-secondary">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </BaseDialog>

    <!-- Proxy Accounts Dialog -->
    <BaseDialog
      :show="showAccountsModal"
      :title="t('admin.proxies.accountsTitle', { name: accountsProxy?.name || '' })"
      width="normal"
      @close="closeAccountsModal"
    >
      <div v-if="accountsLoading" class="flex items-center justify-center py-8 text-sm text-gray-500">
        <Icon name="refresh" size="md" class="mr-2 animate-spin" />
        {{ t('common.loading') }}
      </div>
      <div v-else-if="proxyAccounts.length === 0" class="py-6 text-center text-sm text-gray-500">
        {{ t('admin.proxies.accountsEmpty') }}
      </div>
      <div v-else class="max-h-80 overflow-auto">
        <table class="min-w-full divide-y divide-gray-200 text-sm dark:divide-dark-700">
          <thead class="bg-gray-50 text-xs uppercase text-gray-500 dark:bg-dark-800 dark:text-dark-400">
            <tr>
              <th class="px-4 py-2 text-left">{{ t('admin.proxies.accountName') }}</th>
              <th class="px-4 py-2 text-left">{{ t('admin.accounts.columns.platformType') }}</th>
              <th class="px-4 py-2 text-left">{{ t('admin.proxies.accountNotes') }}</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-gray-200 bg-white dark:divide-dark-700 dark:bg-dark-900">
            <tr v-for="account in proxyAccounts" :key="account.id">
              <td class="px-4 py-2 font-medium text-gray-900 dark:text-white">{{ account.name }}</td>
              <td class="px-4 py-2">
                <PlatformTypeBadge :platform="account.platform" :type="account.type" />
              </td>
              <td class="px-4 py-2 text-gray-600 dark:text-gray-300">
                {{ account.notes || '-' }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <template #footer>
        <div class="flex justify-end">
          <button @click="closeAccountsModal" class="btn btn-secondary">
            {{ t('common.close') }}
          </button>
        </div>
      </template>
    </BaseDialog>
  </AppLayout>
</template>

<script setup lang="ts">
import { ref, reactive, computed, onMounted, onUnmounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import { adminAPI } from '@/api/admin'
import { getAdminProxyImportCount } from '@/api/admin/proxies'
import type { AdminProxyImportResult, AdminProxySource } from '@/api/admin/proxies'
import type { Proxy, ProxyAccountSummary, ProxyKind, ProxyProtocol, ProxyQualityCheckResult, ProxySourceSyncAllResult } from '@/types'
import type { Column } from '@/components/common/types'
import AppLayout from '@/components/layout/AppLayout.vue'
import TablePageLayout from '@/components/layout/TablePageLayout.vue'
import DataTable from '@/components/common/DataTable.vue'
import Pagination from '@/components/common/Pagination.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import ImportDataModal from '@/components/admin/proxy/ImportDataModal.vue'
import Select from '@/components/common/Select.vue'
import Icon from '@/components/icons/Icon.vue'
import PlatformTypeBadge from '@/components/common/PlatformTypeBadge.vue'
import { useClipboard } from '@/composables/useClipboard'
import { useSwipeSelect } from '@/composables/useSwipeSelect'
import { useTableSelection } from '@/composables/useTableSelection'
import { getPersistedPageSize } from '@/composables/usePersistedPageSize'
import { formatBytes, formatDateTime } from '@/utils/format'
import { proxyExpiryBadgeClass, proxyExpiryLabelKey } from '@/utils/proxyExpiry'
import { extractApiErrorMessage } from '@/utils/apiError'

const { t } = useI18n()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()

const columns = computed<Column[]>(() => [
  { key: 'select', label: '', sortable: false },
  { key: 'name', label: t('admin.proxies.columns.name'), sortable: true },
  { key: 'owner_user_id', label: t('admin.proxies.columns.owner'), sortable: false },
  { key: 'visibility', label: t('admin.proxies.columns.visibility'), sortable: false },
  { key: 'kind', label: t('admin.proxies.columns.kind'), sortable: false },
  { key: 'protocol', label: t('admin.proxies.columns.protocol'), sortable: true },
  { key: 'address', label: t('admin.proxies.columns.address'), sortable: false },
  { key: 'auth', label: t('admin.proxies.columns.auth'), sortable: false },
  { key: 'location', label: t('admin.proxies.columns.location'), sortable: false },
  { key: 'account_count', label: t('admin.proxies.columns.accounts'), sortable: true },
  { key: 'latency', label: t('admin.proxies.columns.latency'), sortable: false },
  { key: 'expiry', label: t('admin.proxies.columns.expiry'), sortable: true },
  { key: 'created_at', label: t('admin.proxies.columns.createdAt'), sortable: true },
  { key: 'status', label: t('admin.proxies.columns.status'), sortable: true },
  { key: 'actions', label: t('admin.proxies.columns.actions'), sortable: false }
])

// Filter options
const protocolOptions = computed(() => [
  { value: '', label: t('admin.proxies.allProtocols') },
  { value: 'http', label: 'HTTP' },
  { value: 'https', label: 'HTTPS' },
  { value: 'socks5', label: 'SOCKS5' },
  { value: 'socks5h', label: 'SOCKS5H' },
  { value: 'vmess', label: 'VMess' },
  { value: 'vless', label: 'VLESS' },
  { value: 'trojan', label: 'Trojan' },
  { value: 'ss', label: 'Shadowsocks' },
  { value: 'hysteria', label: 'Hysteria' },
  { value: 'hysteria2', label: 'Hysteria2' },
  { value: 'tuic', label: 'TUIC' },
  { value: 'anytls', label: 'AnyTLS' },
  { value: 'naive', label: 'Naive' },
  { value: 'wireguard', label: 'WireGuard' }
])

const statusOptions = computed(() => [
  { value: '', label: t('admin.proxies.allStatus') },
  { value: 'active', label: t('admin.accounts.status.active') },
  { value: 'inactive', label: t('admin.accounts.status.inactive') },
  { value: 'expired', label: t('admin.proxies.expired') }
])

const ownerScopeOptions = computed(() => [
  { value: '', label: t('admin.proxies.allResourceOwners') },
  { value: 'system', label: t('admin.proxies.systemResources') },
  { value: 'user', label: t('admin.proxies.userResources') }
])

// Source names for the list filter. Loaded independently of the manager modal so
// the filter keeps working before the modal has ever been opened.
const proxySourceFilterItems = ref<AdminProxySource[]>([])
const proxySourceFilterOptions = computed(() => [
  { value: '', label: t('admin.proxies.allSources') },
  ...proxySourceFilterItems.value.map(source => ({
    value: String(source.id),
    label: source.name || `#${source.id}`
  }))
])

// Form options
const standardProtocolSelectOptions = computed(() => [
  { value: 'http', label: t('admin.proxies.protocols.http') },
  { value: 'https', label: t('admin.proxies.protocols.https') },
  { value: 'socks5', label: t('admin.proxies.protocols.socks5') },
  { value: 'socks5h', label: t('admin.proxies.protocols.socks5h') }
])
const xrayProtocolSelectOptions = computed(() => [
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
])
const proxyKindOptions = computed(() => [
  { value: 'standard', label: t('admin.proxies.standardProxy') },
  { value: 'xray', label: 'Xray' },
])

const editStatusOptions = computed(() => [
  { value: 'active', label: t('admin.accounts.status.active') },
  { value: 'inactive', label: t('admin.accounts.status.inactive') }
])

const proxies = ref<Proxy[]>([])
const visiblePasswordIds = reactive(new Set<number>())
const copyMenuProxyId = ref<number | null>(null)
const loading = ref(false)
const searchQuery = ref('')
const filters = reactive({
  protocol: '',
  status: '',
  owner_scope: '',
  source_id: ''
})
const pagination = reactive({
  page: 1,
  page_size: getPersistedPageSize(),
  total: 0,
  pages: 0
})
const sortState = reactive({
  sort_by: 'id',
  sort_order: 'desc' as 'asc' | 'desc'
})

const showCreateModal = ref(false)
const createPasswordVisible = ref(false)
const showEditModal = ref(false)
const editPasswordVisible = ref(false)
const editPasswordDirty = ref(false)
const showImportData = ref(false)
const showDeleteDialog = ref(false)
const showBatchDeleteDialog = ref(false)
const showExportDataDialog = ref(false)
const showAccountsModal = ref(false)
const submitting = ref(false)
const exportingData = ref(false)
const testingProxyIds = ref<Set<number>>(new Set())
const qualityCheckingProxyIds = ref<Set<number>>(new Set())
const batchTesting = ref(false)
const batchQualityChecking = ref(false)
const proxyTableRef = ref<HTMLElement | null>(null)
const {
  selectedSet: selectedProxyIds,
  selectedCount,
  allVisibleSelected,
  isSelected,
  select,
  deselect,
  clear: clearSelectedProxies,
  removeMany: removeSelectedProxies,
  toggleVisible,
  batchUpdate
} = useTableSelection<Proxy>({
  rows: proxies,
  getId: (proxy) => proxy.id
})
useSwipeSelect(proxyTableRef, {
  isSelected,
  select,
  deselect,
  batchUpdate
})
const accountsProxy = ref<Proxy | null>(null)
const proxyAccounts = ref<ProxyAccountSummary[]>([])
const accountsLoading = ref(false)
const editingProxy = ref<Proxy | null>(null)
const deletingProxy = ref<Proxy | null>(null)
const showQualityReportDialog = ref(false)
const qualityReportProxy = ref<Proxy | null>(null)
const qualityReport = ref<ProxyQualityCheckResult | null>(null)
const showProxySourcesModal = ref(false)
const proxySources = ref<AdminProxySource[]>([])
const proxySourcesLoading = ref(false)
const PROXY_SOURCE_PAGE_SIZE = 100
const proxySourcePagination = reactive({
  page: 1,
  total: 0,
  pages: 0,
})
const proxySourceSaving = ref(false)
const editingProxySourceId = ref<number | null>(null)
const syncingProxySourceId = ref<number | null>(null)
const togglingProxySourceId = ref<number | null>(null)
const proxySourcesSyncingAll = ref(false)
const proxySourceSyncAllResult = ref<ProxySourceSyncAllResult | null>(null)
const proxySourceDeleting = ref(false)
const proxySourcePendingDelete = ref<AdminProxySource | null>(null)
const proxySourceError = ref('')
const proxySourceForm = reactive({
  name: '',
  subscription_url: '',
  refresh_interval_minutes: 1440,
  is_public: false,
  sync_enabled: true,
})

// One-line recap of the last "sync every source" run. Counts only: node payloads
// and subscription URLs must never reach this strip.
const proxySourceSyncAllSummary = computed(() => {
  const result = proxySourceSyncAllResult.value
  if (!result) return ''
  return t('admin.proxies.sourceSyncAllSummary', {
    total: Number(result.total) || 0,
    success: Number(result.success_count) || 0,
    partial: Number(result.partial_count) || 0,
    failed: Number(result.failed_count) || 0,
    skipped: Number(result.skipped_count) || 0,
    deferred: Number(result.deferred_count) || 0,
    created: Number(result.created_count) || 0,
    updated: Number(result.updated_count) || 0,
  })
})

type CreateMode = 'standard' | 'batch'
type InputMode = 'direct' | 'xray' | 'source' | 'config'

const inputModeOptions = computed<Array<{ value: InputMode; label: string }>>(() => [
  { value: 'direct', label: t('myResources.proxyEditor.standardProxy') },
  { value: 'xray', label: t('myResources.proxyEditor.xrayShare') },
  { value: 'source', label: t('myResources.proxyEditor.providerSubscription') },
  { value: 'config', label: t('myResources.proxyEditor.nodeConfig') },
])

// Creation/import state
const createMode = ref<CreateMode>('standard')
const inputMode = ref<InputMode>('direct')
const batchInput = ref('')
const configFileInput = ref<HTMLInputElement | null>(null)
const configFileName = ref('')
const configFileReading = ref(false)
const batchParseResult = reactive({
  total: 0,
  valid: 0,
  invalid: 0,
  duplicate: 0
})

const createForm = reactive({
  name: '',
  is_public: false,
  kind: 'standard' as ProxyKind,
  protocol: 'http' as ProxyProtocol,
  host: '',
  port: 8080,
  username: '',
  password: '',
  expires_at: '' as string,
  fallback_mode: 'none' as 'none' | 'proxy' | 'direct',
  backup_proxy_id: null as number | null,
  expiry_warn_days: 7 as number,
  import_content: '',
  subscription_url: '',
  refresh_interval_minutes: 1440
})

const editForm = reactive({
  name: '',
  is_public: false,
  kind: 'standard' as ProxyKind,
  protocol: 'http' as ProxyProtocol,
  host: '',
  port: 8080,
  username: '',
  password: '',
  status: 'active' as 'active' | 'inactive' | 'disabled' | 'expired',
  expires_at: '' as string,
  fallback_mode: 'none' as 'none' | 'proxy' | 'direct',
  backup_proxy_id: null as number | null,
  expiry_warn_days: 7 as number,
  xray_raw: '',
})

const editProtocolSelectOptions = computed(() => editForm.kind === 'xray' ? xrayProtocolSelectOptions.value : standardProtocolSelectOptions.value)
const normalizeEditProtocol = () => {
  if (!editProtocolSelectOptions.value.some(option => option.value === editForm.protocol)) {
    editForm.protocol = editProtocolSelectOptions.value[0].value as ProxyProtocol
  }
}

const createFormReady = computed(() => {
  if (createMode.value === 'batch') {
    return batchParseResult.valid > 0
  }
  if (inputMode.value === 'direct') {
    const port = Number(createForm.port)
    return Boolean(
      createForm.name.trim()
      && createForm.host.trim()
      && Number.isFinite(port)
      && port >= 1
      && port <= 65535
    )
  }
  if (inputMode.value === 'xray') {
    return Boolean(createForm.name.trim() && createForm.import_content.trim())
  }
  if (inputMode.value === 'source') {
    const interval = Number(createForm.refresh_interval_minutes)
    return Boolean(
      createForm.name.trim()
      && createForm.subscription_url.trim()
      && Number.isFinite(interval)
      && interval >= 5
      && interval <= 10080
    )
  }
  if (inputMode.value === 'config') {
    return Boolean(createForm.import_content.trim())
  }
  return false
})

const createSubmitLabel = computed(() => {
  const mode = createMode.value === 'batch' ? 'batch' : inputMode.value
  if (submitting.value) {
    return mode === 'direct'
      ? t('admin.proxies.creating')
      : t('admin.proxies.importing')
  }
  return mode === 'direct' || mode === 'source'
    ? t('common.create')
    : t('admin.proxies.dataImportButton')
})

const proxySourceFormReady = computed(() => {
  const interval = Number(proxySourceForm.refresh_interval_minutes)
  return Boolean(
    proxySourceForm.name.trim()
    && proxySourceForm.subscription_url.trim()
    && Number.isInteger(interval)
    && interval >= 5
    && interval <= 10080
  )
})

const allProxiesForBackup = ref<Proxy[]>([])
const loadBackupProxyOptions = async () => {
  allProxiesForBackup.value = await adminAPI.proxies.getAllWithCount()
}
const backupProxyOptions = (excludeId?: number) =>
  allProxiesForBackup.value
    .filter(p => p.id !== excludeId)
    .map(p => ({ label: `${p.name} (${p.host}:${p.port})`, value: p.id }))

let abortController: AbortController | null = null

const isAbortError = (error: unknown) => {
  if (!error || typeof error !== 'object') return false
  const maybeError = error as { name?: string; code?: string }
  return maybeError.name === 'AbortError' || maybeError.code === 'ERR_CANCELED'
}

const toggleSelectRow = (id: number, event: Event) => {
  const target = event.target as HTMLInputElement
  if (target.checked) {
    select(id)
    return
  }
  deselect(id)
}

const toggleSelectAllVisible = (event: Event) => {
  const target = event.target as HTMLInputElement
  toggleVisible(target.checked)
}

const buildProxyQueryFilters = () => ({
  protocol: filters.protocol || undefined,
  status: (filters.status || undefined) as 'active' | 'inactive' | 'expired' | undefined,
  owner_scope: (filters.owner_scope || undefined) as 'system' | 'user' | undefined,
  source_id: filters.source_id ? Number(filters.source_id) : undefined,
  search: searchQuery.value || undefined,
  sort_by: sortState.sort_by,
  sort_order: sortState.sort_order
})

const loadProxies = async () => {
  if (abortController) {
    abortController.abort()
  }
  const currentAbortController = new AbortController()
  abortController = currentAbortController
  loading.value = true
  try {
    const response = await adminAPI.proxies.list(
      pagination.page,
      pagination.page_size,
      buildProxyQueryFilters(),
      { signal: currentAbortController.signal }
    )
    if (currentAbortController.signal.aborted || abortController !== currentAbortController) {
      return
    }
    proxies.value = response.items
    pagination.total = response.total
    pagination.pages = response.pages
  } catch (error) {
    if (isAbortError(error)) {
      return
    }
    appStore.showError(t('admin.proxies.failedToLoad'))
    console.error('Error loading proxies:', error)
  } finally {
    if (abortController === currentAbortController) {
      loading.value = false
      abortController = null
    }
  }
}

const resetProxySourceForm = () => {
  editingProxySourceId.value = null
  proxySourceForm.name = ''
  proxySourceForm.subscription_url = ''
  proxySourceForm.refresh_interval_minutes = 1440
  proxySourceForm.is_public = false
  proxySourceForm.sync_enabled = true
}

const ensureProxySourceFilterOptions = async (force = false) => {
  if (!force && proxySourceFilterItems.value.length > 0) return
  try {
    const response = await adminAPI.proxies.sources.list(1, PROXY_SOURCE_PAGE_SIZE)
    proxySourceFilterItems.value = response.items ?? []
  } catch {
    // The filter is optional; the proxy table must still load without it.
    proxySourceFilterItems.value = []
  }
}

const filterProxiesBySource = async (source: AdminProxySource) => {
  filters.source_id = String(source.id)
  closeProxySourcesModal()
  pagination.page = 1
  await loadProxies()
}

// Remaining / total traffic reported by the subscription provider. Providers that
// send no usage header leave both counters at zero, which reads as "unknown".
const proxySourceTrafficText = (source: AdminProxySource) => {
  const used = Math.max(0, Number(source.sub_traffic_used) || 0)
  const total = Math.max(0, Number(source.sub_traffic_total) || 0)
  if (total <= 0 && used <= 0) return '-'
  if (total <= 0) return t('admin.proxies.sourceTrafficUsedOnly', { used: formatBytes(used) })
  return t('admin.proxies.sourceTrafficRemaining', {
    remaining: formatBytes(Math.max(0, total - used)),
    total: formatBytes(total),
  })
}

// Pause/resume only flips sync_enabled; the other fields are resent unchanged
// because the update endpoint replaces the whole source record.
const toggleProxySourceSync = async (source: AdminProxySource) => {
  togglingProxySourceId.value = source.id
  proxySourceError.value = ''
  const nextEnabled = source.sync_enabled === false
  try {
    await adminAPI.proxies.sources.update(source.id, {
      name: source.name,
      subscription_url: source.subscription_url,
      refresh_interval_minutes: source.refresh_interval_minutes,
      is_public: source.is_public,
      sync_enabled: nextEnabled,
    })
    appStore.showSuccess(
      nextEnabled ? t('admin.proxies.sourceSyncResumed') : t('admin.proxies.sourceSyncPaused')
    )
    await loadProxySources()
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceSaveFailed'))
  } finally {
    togglingProxySourceId.value = null
  }
}

const syncAllProxySources = async () => {
  if (proxySourcesSyncingAll.value) return
  proxySourcesSyncingAll.value = true
  proxySourceError.value = ''
  try {
    const result = await adminAPI.proxies.sources.syncAll()
    proxySourceSyncAllResult.value = result || null
    const failed = Number(result?.failed_count) || 0
    if (failed > 0) {
      appStore.showInfo(t('admin.proxies.sourceSyncAllPartial', { failed }))
    } else {
      appStore.showSuccess(t('admin.proxies.sourceSyncAllDone'))
    }
    await Promise.all([loadProxySources(), loadProxies()])
    void ensureProxySourceFilterOptions(true)
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceSyncFailed'))
  } finally {
    proxySourcesSyncingAll.value = false
  }
}

const loadProxySources = async () => {
  proxySourcesLoading.value = true
  proxySourceError.value = ''
  try {
    let response = await adminAPI.proxies.sources.list(
      proxySourcePagination.page,
      PROXY_SOURCE_PAGE_SIZE,
    )
    let totalPages = Math.max(
      1,
      Number(response.pages) || Math.ceil(Number(response.total || 0) / PROXY_SOURCE_PAGE_SIZE),
    )
    if (proxySourcePagination.page > totalPages) {
      proxySourcePagination.page = totalPages
      response = await adminAPI.proxies.sources.list(
        proxySourcePagination.page,
        PROXY_SOURCE_PAGE_SIZE,
      )
      totalPages = Math.max(
        1,
        Number(response.pages) || Math.ceil(Number(response.total || 0) / PROXY_SOURCE_PAGE_SIZE),
      )
    }
    proxySources.value = response.items ?? []
    proxySourcePagination.total = Math.max(0, Number(response.total) || 0)
    proxySourcePagination.pages = totalPages
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceLoadFailed'))
  } finally {
    proxySourcesLoading.value = false
  }
}

const openProxySourcesModal = async () => {
  proxySourceError.value = ''
  resetProxySourceForm()
  proxySourcePagination.page = 1
  showProxySourcesModal.value = true
  await loadProxySources()
}

const handleProxySourcePageChange = async (page: number) => {
  const nextPage = Math.min(Math.max(1, page), Math.max(1, proxySourcePagination.pages))
  if (nextPage === proxySourcePagination.page) return
  proxySourcePagination.page = nextPage
  await loadProxySources()
}

const closeProxySourcesModal = () => {
  showProxySourcesModal.value = false
  proxySourcePendingDelete.value = null
  proxySourceError.value = ''
  resetProxySourceForm()
}

const editProxySource = (source: AdminProxySource) => {
  editingProxySourceId.value = source.id
  proxySourceForm.name = source.name
  proxySourceForm.subscription_url = source.subscription_url
  proxySourceForm.refresh_interval_minutes = source.refresh_interval_minutes
  proxySourceForm.is_public = source.is_public
  proxySourceForm.sync_enabled = source.sync_enabled !== false
  proxySourceError.value = ''
}

const saveProxySource = async () => {
  if (!proxySourceFormReady.value) return

  proxySourceSaving.value = true
  proxySourceError.value = ''
  try {
    const payload = {
      name: proxySourceForm.name.trim(),
      subscription_url: proxySourceForm.subscription_url.trim(),
      refresh_interval_minutes: Number(proxySourceForm.refresh_interval_minutes),
      is_public: proxySourceForm.is_public,
      sync_enabled: proxySourceForm.sync_enabled,
    }
    if (editingProxySourceId.value) {
      await adminAPI.proxies.sources.update(editingProxySourceId.value, payload)
    } else {
      await adminAPI.proxies.sources.create(payload)
    }
    appStore.showSuccess(t('admin.proxies.sourceSaved'))
    resetProxySourceForm()
    await loadProxySources()
    void ensureProxySourceFilterOptions(true)
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceSaveFailed'))
  } finally {
    proxySourceSaving.value = false
  }
}

const syncProxySource = async (source: AdminProxySource) => {
  syncingProxySourceId.value = source.id
  proxySourceError.value = ''
  try {
    const result = await adminAPI.proxies.sources.sync(source.id)
    const stats = getImportResultStats(result)
    if (stats.errors.length > 0) {
      appStore.showInfo(t('admin.proxies.sourceSyncPartial', { count: stats.imported, errors: stats.errors.length }))
    } else {
      appStore.showSuccess(t('admin.proxies.sourceSynced', { count: stats.imported }))
    }
    await Promise.all([loadProxySources(), loadProxies()])
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceSyncFailed'))
  } finally {
    syncingProxySourceId.value = null
  }
}

const confirmDeleteProxySource = async () => {
  const source = proxySourcePendingDelete.value
  if (!source) return

  proxySourceDeleting.value = true
  proxySourceError.value = ''
  try {
    await adminAPI.proxies.sources.delete(source.id)
    if (editingProxySourceId.value === source.id) {
      resetProxySourceForm()
    }
    proxySourcePendingDelete.value = null
    appStore.showSuccess(t('admin.proxies.sourceDeleted'))
    const remainingTotal = Math.max(0, proxySourcePagination.total - 1)
    const lastPage = Math.max(1, Math.ceil(remainingTotal / PROXY_SOURCE_PAGE_SIZE))
    if (proxySourcePagination.page > lastPage) {
      proxySourcePagination.page = lastPage
    }
    await loadProxySources()
    void ensureProxySourceFilterOptions(true)
  } catch (error: unknown) {
    proxySourceError.value = extractApiErrorMessage(error, t('admin.proxies.sourceDeleteFailed'))
  } finally {
    proxySourceDeleting.value = false
  }
}

const maskSubscriptionUrl = (value?: string | null) => {
  const raw = String(value || '').trim()
  if (!raw) return t('admin.proxies.sourceUrlHidden')
  try {
    const parsed = new URL(raw)
    return parsed.protocol + '//' + parsed.host + '/***'
  } catch {
    return t('admin.proxies.sourceUrlHidden')
  }
}

const proxySourceStatusClass = (status?: string) => {
  switch (String(status || '').toLowerCase()) {
    case 'success':
      return 'badge-success'
    case 'partial':
      return 'badge-warning'
    case 'syncing':
      return 'badge-primary'
    case 'error':
      return 'badge-danger'
    default:
      return 'badge-gray'
  }
}

const proxySourceStatusLabel = (status?: string) => {
  switch (String(status || '').toLowerCase()) {
    case 'success':
      return t('admin.proxies.sourceStatusSuccess')
    case 'partial':
      return t('admin.proxies.sourceStatusPartial')
    case 'error':
      return t('admin.proxies.sourceStatusError')
    case 'syncing':
      return t('admin.proxies.sourceStatusSyncing')
    default:
      return t('admin.proxies.sourceStatusPending')
  }
}

let searchTimeout: ReturnType<typeof setTimeout>
const handleSearch = () => {
  clearTimeout(searchTimeout)
  searchTimeout = setTimeout(() => {
    pagination.page = 1
    loadProxies()
  }, 300)
}

const handlePageChange = (page: number) => {
  pagination.page = page
  loadProxies()
}

const handlePageSizeChange = (pageSize: number) => {
  pagination.page_size = pageSize
  pagination.page = 1
  loadProxies()
}

const handleSort = (key: string, order: 'asc' | 'desc') => {
  sortState.sort_by = key
  sortState.sort_order = order
  pagination.page = 1
  loadProxies()
}

const closeCreateModal = () => {
  showCreateModal.value = false
  createMode.value = 'standard'
  inputMode.value = 'direct'
  createForm.name = ''
  createForm.is_public = false
  createForm.kind = 'standard'
  createForm.protocol = 'http'
  createForm.host = ''
  createForm.port = 8080
  createForm.username = ''
  createForm.password = ''
  createForm.expires_at = ''
  createForm.fallback_mode = 'none'
  createForm.backup_proxy_id = null
  createForm.expiry_warn_days = 7
  createForm.import_content = ''
  createForm.subscription_url = ''
  createForm.refresh_interval_minutes = 1440
  createPasswordVisible.value = false
  configFileName.value = ''
  configFileReading.value = false
  if (configFileInput.value) {
    configFileInput.value.value = ''
  }
  batchInput.value = ''
  batchParseResult.total = 0
  batchParseResult.valid = 0
  batchParseResult.invalid = 0
  batchParseResult.duplicate = 0
}

const handleDataImported = () => {
  showImportData.value = false
  loadProxies()
}

const CONFIG_FILE_EXTENSIONS = ['.json', '.yaml', '.yml', '.txt', '.conf'] as const
const MAX_CONFIG_FILE_SIZE = 8 * 1024 * 1024

const openConfigFilePicker = () => {
  configFileInput.value?.click()
}

const clearConfigFile = () => {
  configFileName.value = ''
  createForm.import_content = ''
  if (configFileInput.value) {
    configFileInput.value.value = ''
  }
}

const readFileAsText = async (sourceFile: File): Promise<string> => {
  if (typeof sourceFile.text === 'function') {
    return sourceFile.text()
  }
  if (typeof sourceFile.arrayBuffer === 'function') {
    const buffer = await sourceFile.arrayBuffer()
    return new TextDecoder().decode(buffer)
  }
  return await new Promise<string>((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result ?? ''))
    reader.onerror = () => reject(reader.error || new Error('Failed to read proxy configuration file'))
    reader.readAsText(sourceFile)
  })
}

const handleConfigFileChange = async (event: Event) => {
  const target = event.target as HTMLInputElement
  const file = target.files?.[0]
  if (!file) return

  const lowerName = file.name.toLowerCase()
  if (!CONFIG_FILE_EXTENSIONS.some((extension) => lowerName.endsWith(extension))) {
    appStore.showError(t('admin.proxies.configFileUnsupported'))
    target.value = ''
    return
  }
  if (file.size > MAX_CONFIG_FILE_SIZE) {
    appStore.showError(t('admin.proxies.configFileTooLarge'))
    target.value = ''
    return
  }

  configFileReading.value = true
  try {
    const content = await readFileAsText(file)
    if (!content.trim()) {
      appStore.showError(t('admin.proxies.configFileEmpty'))
      return
    }
    createForm.import_content = content
    configFileName.value = file.name
  } catch {
    appStore.showError(t('admin.proxies.configFileReadFailed'))
  } finally {
    configFileReading.value = false
    target.value = ''
  }
}

const MODERN_PROXY_URI_PATTERN = /^(https?|socks(?:5h?)?|vmess|vless|trojan|ss|hysteria|hy2|hysteria2|tuic|anytls|naive(?:\+https|\+quic)?|wireguard|wg):\/\/\S+$/i

const parseBatchInput = () => {
  const lines = batchInput.value
    .split(/\r?\n/)
    .map((line) => line.trim())
    .filter(Boolean)
  const seen = new Set<string>()
  let invalid = 0
  let duplicate = 0
  let valid = 0

  for (const line of lines) {
    if (!MODERN_PROXY_URI_PATTERN.test(line)) {
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

  batchParseResult.total = lines.length
  batchParseResult.valid = valid
  batchParseResult.invalid = invalid
  batchParseResult.duplicate = duplicate
}

const getImportResultStats = (result: AdminProxyImportResult) => {
  const imported = getAdminProxyImportCount(result)
  const errors = Array.isArray(result.errors)
    ? result.errors.filter((error) => Boolean(typeof error === 'string' ? error.trim() : error.message || error.error))
    : []
  return { imported, errors }
}

const handleCreateProxy = async () => {
  const mode: InputMode | 'batch' = createMode.value === 'batch' ? 'batch' : inputMode.value

  if (mode === 'direct' || mode === 'xray' || mode === 'source') {
    if (!createForm.name.trim()) {
      appStore.showError(t('admin.proxies.nameRequired'))
      return
    }
  }
  if (mode === 'direct') {
    if (!createForm.host.trim()) {
      appStore.showError(t('admin.proxies.hostRequired'))
      return
    }
    const port = Number(createForm.port)
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      appStore.showError(t('admin.proxies.portInvalid'))
      return
    }
  } else if (mode === 'source') {
    if (!createForm.subscription_url.trim()) {
      appStore.showError(t('admin.proxies.subscriptionUrlRequired'))
      return
    }
    const interval = Number(createForm.refresh_interval_minutes)
    if (!Number.isInteger(interval) || interval < 5 || interval > 10080) {
      appStore.showError(t('admin.proxies.refreshIntervalHint'))
      return
    }
  } else {
    const content = mode === 'batch' ? batchInput.value : createForm.import_content
    if (!content.trim()) {
      appStore.showError(t('admin.proxies.importContentRequired'))
      return
    }
    if (mode === 'batch' && batchParseResult.valid === 0) {
      appStore.showError(t('admin.proxies.importContentRequired'))
      return
    }
  }

  submitting.value = true
  try {
    if (mode === 'direct') {
      await adminAPI.proxies.create({
        name: createForm.name.trim(),
        is_public: createForm.is_public,
        kind: 'standard' as ProxyKind,
        protocol: createForm.protocol,
        host: createForm.host.trim(),
        port: Number(createForm.port),
        username: createForm.username.trim() || null,
        password: createForm.password.trim() || null,
        expires_at: createForm.expires_at ? Math.floor(new Date(createForm.expires_at).getTime() / 1000) : null,
        fallback_mode: createForm.fallback_mode,
        backup_proxy_id: createForm.fallback_mode === 'proxy' ? createForm.backup_proxy_id : null,
        expiry_warn_days: createForm.expiry_warn_days,
        extra: {},
      })
      appStore.showSuccess(t('admin.proxies.proxyCreated'))
    } else if (mode === 'source') {
      const source = await adminAPI.proxies.sources.create({
        name: createForm.name.trim(),
        subscription_url: createForm.subscription_url.trim(),
        refresh_interval_minutes: Number(createForm.refresh_interval_minutes),
        is_public: createForm.is_public,
      })
      try {
        const result = await adminAPI.proxies.sources.sync(source.id)
        const stats = getImportResultStats(result)
        if (stats.errors.length > 0) {
          appStore.showInfo(t('admin.proxies.sourceSyncPartial', { count: stats.imported, errors: stats.errors.length }))
        } else {
          appStore.showSuccess(t('admin.proxies.sourceCreated', { count: stats.imported }))
        }
      } catch (syncError: unknown) {
        const message = extractApiErrorMessage(syncError, t('admin.proxies.sourceSyncFailed'))
        closeCreateModal()
        await openProxySourcesModal()
        proxySourceError.value = message
        appStore.showError(t('admin.proxies.sourceCreatedSyncFailed'))
        await loadProxies()
        return
      }
    } else {
      const content = mode === 'batch' ? batchInput.value : createForm.import_content
      const result = await adminAPI.proxies.importNodes({
        name_prefix: createForm.name.trim() || undefined,
        content: content.trim(),
        is_public: createForm.is_public,
      })
      const stats = getImportResultStats(result)
      if (stats.imported > 0 && stats.errors.length > 0) {
        appStore.showInfo(t('admin.proxies.importPartial', { count: stats.imported, errors: stats.errors.length }))
      } else if (stats.imported > 0) {
        appStore.showSuccess(t('admin.proxies.importSuccess', { count: stats.imported }))
      } else {
        appStore.showError(t('admin.proxies.failedToImport'))
        return
      }
    }
    closeCreateModal()
    await loadProxies()
  } catch (error: unknown) {
    appStore.showError(extractApiErrorMessage(error, t('admin.proxies.failedToCreate')))
    console.error('Error creating or importing proxy:', error)
  } finally {
    submitting.value = false
  }
}

const handleEdit = (proxy: Proxy) => {
  editingProxy.value = proxy
  editForm.name = proxy.name
  editForm.is_public = Boolean(proxy.is_public)
  editForm.kind = proxy.kind || 'standard'
  editForm.protocol = proxy.protocol
  editForm.host = proxy.host
  editForm.port = proxy.port
  editForm.username = proxy.username || ''
  editForm.password = proxy.password || ''
  editForm.status = proxy.status === 'expired' ? 'inactive' : proxy.status
  editForm.expires_at = proxy.expires_at ? proxy.expires_at.slice(0, 10) : ''
  editForm.fallback_mode = proxy.fallback_mode || 'none'
  editForm.backup_proxy_id = proxy.backup_proxy_id ?? null
  editForm.expiry_warn_days = proxy.expiry_warn_days ?? 7
  editForm.xray_raw = typeof proxy.extra?.raw === 'string' ? proxy.extra.raw : ''
  editPasswordVisible.value = false
  editPasswordDirty.value = false
  showEditModal.value = true
}

const closeEditModal = () => {
  showEditModal.value = false
  editingProxy.value = null
  editPasswordVisible.value = false
  editPasswordDirty.value = false
}

const handleUpdateProxy = async () => {
  if (!editingProxy.value) return
  if (!editForm.name.trim()) {
    appStore.showError(t('admin.proxies.nameRequired'))
    return
  }
  if (!editForm.host.trim()) {
    appStore.showError(t('admin.proxies.hostRequired'))
    return
  }
  if (editForm.port < 1 || editForm.port > 65535) {
    appStore.showError(t('admin.proxies.portInvalid'))
    return
  }
  if (editForm.kind === 'xray' && !editForm.xray_raw.trim()) {
    appStore.showError(t('admin.proxies.xrayNodeUriRequired'))
    return
  }

  submitting.value = true
  try {
    const updateData: any = {
      name: editForm.name.trim(),
      is_public: editForm.is_public,
      kind: editForm.kind,
      protocol: editForm.protocol,
      host: editForm.host.trim(),
      port: editForm.port,
      username: editForm.username.trim() || null,
      status: editForm.status,
      expires_at: editForm.expires_at ? Math.floor(new Date(editForm.expires_at).getTime() / 1000) : null,
      fallback_mode: editForm.fallback_mode,
      backup_proxy_id: editForm.fallback_mode === 'proxy' ? editForm.backup_proxy_id : null,
      expiry_warn_days: editForm.expiry_warn_days,
      extra: editForm.kind === 'xray'
        ? { ...(editingProxy.value.extra || {}), raw: editForm.xray_raw.trim() }
        : { ...(editingProxy.value.extra || {}) },
    }

    // Only include password if user actually modified the field
    if (editPasswordDirty.value) {
      updateData.password = editForm.password.trim() || null
    }

    await adminAPI.proxies.update(editingProxy.value.id, updateData)
    appStore.showSuccess(t('admin.proxies.proxyUpdated'))
    closeEditModal()
    loadProxies()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.failedToUpdate'))
    console.error('Error updating proxy:', error)
  } finally {
    submitting.value = false
  }
}

const applyLatencyResult = (
  proxyId: number,
  result: {
    success: boolean
    latency_ms?: number
    message?: string
    ip_address?: string
    country?: string
    country_code?: string
    region?: string
    city?: string
  }
) => {
  const target = proxies.value.find((proxy) => proxy.id === proxyId)
  if (!target) return
  if (result.success) {
    target.latency_status = 'success'
    target.latency_ms = result.latency_ms
    target.ip_address = result.ip_address
    target.country = result.country
    target.country_code = result.country_code
    target.region = result.region
    target.city = result.city
  } else {
    target.latency_status = 'failed'
    target.latency_ms = undefined
    target.ip_address = undefined
    target.country = undefined
    target.country_code = undefined
    target.region = undefined
    target.city = undefined
  }
  target.latency_message = result.message
}

const summarizeQualityStatus = (result: ProxyQualityCheckResult): Proxy['quality_status'] => {
  if (result.challenge_count > 0) return 'challenge'
  if (result.failed_count > 0) {
    const baseConnected = result.items?.some((item) => item.target === 'base_connectivity' && item.status === 'pass') ?? false
    return baseConnected ? 'warn' : 'failed'
  }
  if (result.warn_count > 0) return 'warn'
  return 'healthy'
}

const applyQualityResult = (proxyId: number, result: ProxyQualityCheckResult) => {
  const target = proxies.value.find((proxy) => proxy.id === proxyId)
  if (!target) return
  target.quality_status = summarizeQualityStatus(result)
  target.quality_score = result.score
  target.quality_grade = result.grade
  target.quality_summary = result.summary
  target.quality_checked = result.checked_at
}

const formatLocation = (proxy: Proxy) => {
  const parts = [proxy.country, proxy.city].filter(Boolean) as string[]
  return parts.join(' · ')
}

const flagUrl = (code: string) =>
  `https://unpkg.com/flag-icons/flags/4x3/${code.toLowerCase()}.svg`

const startTestingProxy = (proxyId: number) => {
  testingProxyIds.value = new Set([...testingProxyIds.value, proxyId])
}

const stopTestingProxy = (proxyId: number) => {
  const next = new Set(testingProxyIds.value)
  next.delete(proxyId)
  testingProxyIds.value = next
}

const startQualityCheckingProxy = (proxyId: number) => {
  qualityCheckingProxyIds.value = new Set([...qualityCheckingProxyIds.value, proxyId])
}

const stopQualityCheckingProxy = (proxyId: number) => {
  const next = new Set(qualityCheckingProxyIds.value)
  next.delete(proxyId)
  qualityCheckingProxyIds.value = next
}

const runProxyTest = async (proxyId: number, notify: boolean) => {
  startTestingProxy(proxyId)
  try {
    const result = await adminAPI.proxies.testProxy(proxyId)
    applyLatencyResult(proxyId, result)
    if (notify) {
      if (result.success) {
        const message = result.latency_ms
          ? t('admin.proxies.proxyWorkingWithLatency', { latency: result.latency_ms })
          : t('admin.proxies.proxyWorking')
        appStore.showSuccess(message)
      } else {
        appStore.showError(result.message || t('admin.proxies.proxyTestFailed'))
      }
    }
    return result
  } catch (error: any) {
    const message = error.response?.data?.detail || t('admin.proxies.failedToTest')
    applyLatencyResult(proxyId, { success: false, message })
    if (notify) {
      appStore.showError(message)
    }
    console.error('Error testing proxy:', error)
    return null
  } finally {
    stopTestingProxy(proxyId)
  }
}

const handleTestConnection = async (proxy: Proxy) => {
  await runProxyTest(proxy.id, true)
}

const handleQualityCheck = async (proxy: Proxy) => {
  startQualityCheckingProxy(proxy.id)
  try {
    const result = await adminAPI.proxies.checkProxyQuality(proxy.id)
    qualityReportProxy.value = proxy
    qualityReport.value = result
    showQualityReportDialog.value = true

    const baseStep = result.items.find((item) => item.target === 'base_connectivity')
    if (baseStep && baseStep.status === 'pass') {
      applyLatencyResult(proxy.id, {
        success: true,
        latency_ms: result.base_latency_ms,
        message: result.summary,
        ip_address: result.exit_ip,
        country: result.country,
        country_code: result.country_code
      })
    }
    applyQualityResult(proxy.id, result)

    appStore.showSuccess(
      t('admin.proxies.qualityCheckDone', { score: result.score, grade: result.grade })
    )
  } catch (error: any) {
    const message = error.response?.data?.detail || t('admin.proxies.qualityCheckFailed')
    appStore.showError(message)
    console.error('Error checking proxy quality:', error)
  } finally {
    stopQualityCheckingProxy(proxy.id)
  }
}

const runBatchProxyQualityChecks = async (ids: number[]) => {
  if (ids.length === 0) return { total: 0, healthy: 0, warn: 0, challenge: 0, failed: 0 }

  const concurrency = 3
  let index = 0
  let healthy = 0
  let warn = 0
  let challenge = 0
  let failed = 0

  const worker = async () => {
    while (index < ids.length) {
      const current = ids[index]
      index++
      startQualityCheckingProxy(current)
      try {
        const result = await adminAPI.proxies.checkProxyQuality(current)
        const target = proxies.value.find((proxy) => proxy.id === current)
        if (target) {
          const baseStep = result.items.find((item) => item.target === 'base_connectivity')
          if (baseStep && baseStep.status === 'pass') {
            applyLatencyResult(current, {
              success: true,
              latency_ms: result.base_latency_ms,
              message: result.summary,
              ip_address: result.exit_ip,
              country: result.country,
              country_code: result.country_code
            })
          }
        }
        applyQualityResult(current, result)
        if (result.challenge_count > 0) {
          challenge++
        } else if (result.failed_count > 0) {
          failed++
        } else if (result.warn_count > 0) {
          warn++
        } else {
          healthy++
        }
      } catch {
        failed++
      } finally {
        stopQualityCheckingProxy(current)
      }
    }
  }

  const workers = Array.from({ length: Math.min(concurrency, ids.length) }, () => worker())
  await Promise.all(workers)
  return {
    total: ids.length,
    healthy,
    warn,
    challenge,
    failed
  }
}

const closeQualityReportDialog = () => {
  showQualityReportDialog.value = false
  qualityReportProxy.value = null
  qualityReport.value = null
}

const qualityStatusClass = (status: string) => {
  if (status === 'pass') return 'badge-success'
  if (status === 'warn') return 'badge-warning'
  if (status === 'challenge') return 'badge-danger'
  return 'badge-danger'
}

const qualityStatusLabel = (status: string) => {
  if (status === 'pass') return t('admin.proxies.qualityStatusPass')
  if (status === 'warn') return t('admin.proxies.qualityStatusWarn')
  if (status === 'challenge') return t('admin.proxies.qualityStatusChallenge')
  return t('admin.proxies.qualityStatusFail')
}

// 有效期「选天数」⇄ 日历联动:天数自 base 起算(创建=今天;编辑=代理创建日),本地日历日 round-trip 稳定;canonical 仍是 expires_at 日期串
const EXPIRY_PRESETS = [7, 30, 90, 180]
const toLocalDateStr = (dt: Date): string => {
  const y = dt.getFullYear()
  const m = String(dt.getMonth() + 1).padStart(2, '0')
  const d = String(dt.getDate()).padStart(2, '0')
  return `${y}-${m}-${d}`
}
// base 为空 → 今天本地 00:00;否则该日期本地 00:00
const baseDateOrToday = (baseDateStr: string): Date => {
  const base = baseDateStr ? new Date(`${baseDateStr}T00:00:00`) : new Date()
  base.setHours(0, 0, 0, 0)
  return base
}
// base + N 天 → 本地 YYYY-MM-DD;N≤0/空 → '' 表示永不过期
const addDaysToBase = (baseDateStr: string, n: number | null): string => {
  const days = Number(n)
  if (!days || days <= 0) return ''
  const dt = baseDateOrToday(baseDateStr)
  dt.setDate(dt.getDate() + days)
  return toLocalDateStr(dt)
}
// target 相对 base 的整天数(本地日历差,避免时区/时刻抖动)
const daysFromBase = (baseDateStr: string, targetDateStr: string): number | null => {
  if (!targetDateStr) return null
  const target = new Date(`${targetDateStr}T00:00:00`)
  return Math.round((target.getTime() - baseDateOrToday(baseDateStr).getTime()) / 86400000)
}
// 编辑时有效期自「代理创建日」起算;创建时无 created_at → base='' 用今天
const editBaseDate = computed(() =>
  editingProxy.value?.created_at ? editingProxy.value.created_at.slice(0, 10) : '',
)
const createExpiresDays = computed<number | null>({
  get: () => daysFromBase('', createForm.expires_at),
  set: (v) => {
    createForm.expires_at = addDaysToBase('', v)
  },
})
const editExpiresDays = computed<number | null>({
  get: () => daysFromBase(editBaseDate.value, editForm.expires_at),
  set: (v) => {
    editForm.expires_at = addDaysToBase(editBaseDate.value, v)
  },
})

const expiryLabel = (row: Proxy): string => {
  const { key, params } = proxyExpiryLabelKey(row.expires_at, row.status)
  return params ? t(key, params) : t(key)
}

const expiryBadgeClass = (row: Proxy): string =>
  proxyExpiryBadgeClass(row.expires_at, row.status)

const qualityOverallClass = (status?: string) => {
  if (status === 'healthy') return 'badge-success'
  if (status === 'warn') return 'badge-warning'
  if (status === 'challenge') return 'badge-danger'
  return 'badge-danger'
}

const qualityOverallLabel = (status?: string) => {
  if (status === 'healthy') return t('admin.proxies.qualityStatusHealthy')
  if (status === 'warn') return t('admin.proxies.qualityStatusWarn')
  if (status === 'challenge') return t('admin.proxies.qualityStatusChallenge')
  return t('admin.proxies.qualityStatusFail')
}

const qualityTargetLabel = (target: string) => {
  switch (target) {
    case 'base_connectivity':
      return t('admin.proxies.qualityTargetBase')
    case 'openai':
      return 'OpenAI'
    case 'anthropic':
      return 'Anthropic'
    case 'gemini':
      return 'Gemini'
    case 'grok':
      return 'Grok'
    case 'kimi':
      return 'Kimi'
    case 'zhipu':
      return 'Zhipu GLM'
    case 'deepseek':
      return 'DeepSeek'
    case 'minimax':
      return 'MiniMax'
    default:
      return target
  }
}

const fetchAllProxiesForBatch = async (): Promise<Proxy[]> => {
  const pageSize = 200
  const result: Proxy[] = []
  let page = 1
  let totalPages = 1

  while (page <= totalPages) {
    const response = await adminAPI.proxies.list(
      page,
      pageSize,
      buildProxyQueryFilters(),
    )
    result.push(...response.items)
    totalPages = response.pages || 1
    page++
  }

  return result
}

const runBatchProxyTests = async (ids: number[]) => {
  if (ids.length === 0) return
  const concurrency = 5
  let index = 0

  const worker = async () => {
    while (index < ids.length) {
      const current = ids[index]
      index++
      await runProxyTest(current, false)
    }
  }

  const workers = Array.from({ length: Math.min(concurrency, ids.length) }, () => worker())
  await Promise.all(workers)
}

const handleBatchTest = async () => {
  if (batchTesting.value) return

  batchTesting.value = true
  try {
    let ids: number[] = []
    if (selectedCount.value > 0) {
      ids = Array.from(selectedProxyIds.value)
    } else {
      const allProxies = await fetchAllProxiesForBatch()
      ids = allProxies.map((proxy) => proxy.id)
    }

    if (ids.length === 0) {
      appStore.showInfo(t('admin.proxies.batchTestEmpty'))
      return
    }

    await runBatchProxyTests(ids)
    appStore.showSuccess(t('admin.proxies.batchTestDone', { count: ids.length }))
    loadProxies()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.batchTestFailed'))
    console.error('Error batch testing proxies:', error)
  } finally {
    batchTesting.value = false
  }
}

const handleBatchQualityCheck = async () => {
  if (batchQualityChecking.value) return

  batchQualityChecking.value = true
  try {
    let ids: number[] = []
    if (selectedCount.value > 0) {
      ids = Array.from(selectedProxyIds.value)
    } else {
      const allProxies = await fetchAllProxiesForBatch()
      ids = allProxies.map((proxy) => proxy.id)
    }

    if (ids.length === 0) {
      appStore.showInfo(t('admin.proxies.batchQualityEmpty'))
      return
    }

    const summary = await runBatchProxyQualityChecks(ids)
    appStore.showSuccess(
      t('admin.proxies.batchQualityDone', {
        count: summary.total,
        healthy: summary.healthy,
        warn: summary.warn,
        challenge: summary.challenge,
        failed: summary.failed
      })
    )
    loadProxies()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.batchQualityFailed'))
    console.error('Error batch checking quality:', error)
  } finally {
    batchQualityChecking.value = false
  }
}

const formatExportTimestamp = () => {
  const now = new Date()
  const pad2 = (value: number) => String(value).padStart(2, '0')
  return `${now.getFullYear()}${pad2(now.getMonth() + 1)}${pad2(now.getDate())}${pad2(now.getHours())}${pad2(now.getMinutes())}${pad2(now.getSeconds())}`
}

const handleExportData = async () => {
  if (exportingData.value) return
  exportingData.value = true
  try {
    const dataPayload = await adminAPI.proxies.exportData(
      selectedCount.value > 0
        ? { ids: Array.from(selectedProxyIds.value) }
        : {
            filters: buildProxyQueryFilters()
          }
    )
    const timestamp = formatExportTimestamp()
    const filename = `sub2api-proxy-${timestamp}.json`
    const blob = new Blob([JSON.stringify(dataPayload, null, 2)], { type: 'application/json' })
    const url = URL.createObjectURL(blob)
    const link = document.createElement('a')
    link.href = url
    link.download = filename
    link.click()
    URL.revokeObjectURL(url)
    appStore.showSuccess(t('admin.proxies.dataExported'))
  } catch (error: any) {
    appStore.showError(error?.message || t('admin.proxies.dataExportFailed'))
  } finally {
    exportingData.value = false
    showExportDataDialog.value = false
  }
}

const handleDelete = (proxy: Proxy) => {
  if ((proxy.account_count || 0) > 0) {
    appStore.showError(t('admin.proxies.deleteBlockedInUse'))
    return
  }
  deletingProxy.value = proxy
  showDeleteDialog.value = true
}

const openBatchDelete = () => {
  if (selectedCount.value === 0) {
    return
  }
  showBatchDeleteDialog.value = true
}

const confirmDelete = async () => {
  if (!deletingProxy.value) return

  try {
    await adminAPI.proxies.delete(deletingProxy.value.id)
    appStore.showSuccess(t('admin.proxies.proxyDeleted'))
    showDeleteDialog.value = false
    removeSelectedProxies([deletingProxy.value.id])
    deletingProxy.value = null
    loadProxies()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.failedToDelete'))
    console.error('Error deleting proxy:', error)
  }
}

const confirmBatchDelete = async () => {
  const ids = Array.from(selectedProxyIds.value)
  if (ids.length === 0) {
    showBatchDeleteDialog.value = false
    return
  }

  try {
    const result = await adminAPI.proxies.batchDelete(ids)
    const deleted = result.deleted_ids?.length || 0
    const skipped = result.skipped?.length || 0

    if (deleted > 0) {
      appStore.showSuccess(t('admin.proxies.batchDeleteDone', { deleted, skipped }))
    } else if (skipped > 0) {
      appStore.showInfo(t('admin.proxies.batchDeleteSkipped', { skipped }))
    }

    clearSelectedProxies()
    showBatchDeleteDialog.value = false
    loadProxies()
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.batchDeleteFailed'))
    console.error('Error batch deleting proxies:', error)
  }
}

const openAccountsModal = async (proxy: Proxy) => {
  accountsProxy.value = proxy
  proxyAccounts.value = []
  accountsLoading.value = true
  showAccountsModal.value = true

  try {
    proxyAccounts.value = await adminAPI.proxies.getProxyAccounts(proxy.id)
  } catch (error: any) {
    appStore.showError(error.response?.data?.detail || t('admin.proxies.accountsFailed'))
    console.error('Error loading proxy accounts:', error)
  } finally {
    accountsLoading.value = false
  }
}

const closeAccountsModal = () => {
  showAccountsModal.value = false
  accountsProxy.value = null
  proxyAccounts.value = []
}

// ── Proxy URL copy ──
function buildAuthPart(row: any): string {
  const user = row.username ? encodeURIComponent(row.username) : ''
  const pass = row.password ? encodeURIComponent(row.password) : ''
  if (user && pass) return `${user}:${pass}@`
  if (user) return `${user}@`
  if (pass) return `:${pass}@`
  return ''
}

function buildProxyUrl(row: any): string {
  if (row.kind === 'xray' && typeof row.extra?.raw === 'string' && row.extra.raw.trim()) {
    return row.extra.raw.trim()
  }
  return `${row.protocol}://${buildAuthPart(row)}${row.host}:${row.port}`
}

function getCopyFormats(row: any) {
  if (row.kind === 'xray') {
    const nodeURI = buildProxyUrl(row)
    return [{ label: nodeURI, value: nodeURI }]
  }
  const hasAuth = row.username || row.password
  const fullUrl = buildProxyUrl(row)
  const formats = [
    { label: fullUrl, value: fullUrl },
  ]
  if (hasAuth) {
    const withoutProtocol = fullUrl.replace(/^[^:]+:\/\//, '')
    formats.push({ label: withoutProtocol, value: withoutProtocol })
  }
  formats.push({ label: `${row.host}:${row.port}`, value: `${row.host}:${row.port}` })
  return formats
}

function copyProxyUrl(row: any) {
  copyToClipboard(buildProxyUrl(row), t('admin.proxies.urlCopied'))
  copyMenuProxyId.value = null
}

function toggleCopyMenu(id: number) {
  copyMenuProxyId.value = copyMenuProxyId.value === id ? null : id
}

function copyFormat(value: string) {
  copyToClipboard(value, t('admin.proxies.urlCopied'))
  copyMenuProxyId.value = null
}

function closeCopyMenu() {
  copyMenuProxyId.value = null
}

onMounted(() => {
  loadProxies()
  loadBackupProxyOptions()
  void ensureProxySourceFilterOptions()
  document.addEventListener('click', closeCopyMenu)
})

onUnmounted(() => {
  clearTimeout(searchTimeout)
  abortController?.abort()
  document.removeEventListener('click', closeCopyMenu)
})
</script>
