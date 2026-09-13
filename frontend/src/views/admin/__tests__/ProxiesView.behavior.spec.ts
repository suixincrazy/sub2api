import { defineComponent } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import type { Proxy } from '@/types'
import type { AdminProxySource } from '@/api/admin/proxies'
import ProxiesView from '../ProxiesView.vue'

const {
  listProxies,
  getAllProxies,
  testProxy,
  checkProxyQuality,
  listSources,
  createSource,
  updateSource,
  deleteSource,
  syncSource,
  syncAllSources,
  showError,
  showSuccess,
  showInfo,
} = vi.hoisted(() => ({
  listProxies: vi.fn(),
  getAllProxies: vi.fn(),
  testProxy: vi.fn(),
  checkProxyQuality: vi.fn(),
  listSources: vi.fn(),
  createSource: vi.fn(),
  updateSource: vi.fn(),
  deleteSource: vi.fn(),
  syncSource: vi.fn(),
  syncAllSources: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showInfo: vi.fn(),
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    proxies: {
      list: listProxies,
      getAllWithCount: getAllProxies,
      create: vi.fn(),
      update: vi.fn(),
      delete: vi.fn(),
      testProxy,
      checkProxyQuality,
      getProxyAccounts: vi.fn(),
      importNodes: vi.fn(),
      batchDelete: vi.fn(),
      exportData: vi.fn(),
      sources: {
        list: listSources,
        create: createSource,
        update: updateSource,
        delete: deleteSource,
        sync: syncSource,
        syncAll: syncAllSources,
      },
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showInfo }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key,
    }),
  }
})

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}))

vi.mock('@/composables/useSwipeSelect', () => ({
  useSwipeSelect: vi.fn(),
}))

vi.mock('@/composables/usePersistedPageSize', () => ({
  getPersistedPageSize: () => 20,
}))

const SelectStub = defineComponent({
  inheritAttrs: false,
  props: {
    modelValue: { type: [String, Number, Boolean], default: '' },
    options: { type: Array, default: () => [] },
  },
  emits: ['update:modelValue', 'change'],
  methods: {
    handleChange(event: Event) {
      const value = (event.target as HTMLSelectElement).value
      this.$emit('update:modelValue', value)
      this.$emit('change', value)
    },
  },
  template: `
    <select v-bind="$attrs" :value="modelValue" @change="handleChange">
      <option
        v-for="option in options"
        :key="String(option.value)"
        :value="option.value"
      >
        {{ option.label }}
      </option>
    </select>
  `,
})

const DataTableStub = defineComponent({
  props: {
    data: { type: Array, default: () => [] },
  },
  emits: ['sort'],
  template: `
    <div data-test="proxy-table">
      <button data-test="sort-name" @click="$emit('sort', 'name', 'asc')">sort</button>
      <div
        v-for="row in data"
        :key="row.id"
        :data-test="'proxy-row-' + row.id"
      >
        <slot name="cell-select" :row="row" :value="row.id" />
      </div>
    </div>
  `,
})

const PaginationStub = defineComponent({
  inheritAttrs: false,
  props: {
    page: { type: Number, required: true },
    total: { type: Number, required: true },
    pageSize: { type: Number, required: true },
  },
  emits: ['update:page'],
  template: `
    <div v-bind="$attrs">
      <span data-test="pagination-page">{{ page }}</span>
      <button data-test="pagination-next" @click="$emit('update:page', page + 1)">next</button>
    </div>
  `,
})

const BaseDialogStub = defineComponent({
  props: {
    show: { type: Boolean, default: false },
  },
  emits: ['close'],
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})

const ConfirmDialogStub = defineComponent({
  props: {
    show: { type: Boolean, default: false },
  },
  emits: ['confirm', 'cancel'],
  template: `
    <div v-if="show" data-test="confirm-dialog">
      <button data-test="confirm-delete" @click="$emit('confirm')">confirm</button>
    </div>
  `,
})

const createProxy = (id: number): Proxy => ({
  id,
  name: `proxy-${id}`,
  kind: 'standard',
  protocol: 'http',
  host: 'proxy.example.com',
  port: 8080,
  status: 'active',
  is_public: false,
  account_count: 0,
  fallback_mode: 'none',
  expiry_warn_days: 7,
  created_at: '2026-07-30T00:00:00Z',
  updated_at: '2026-07-30T00:00:00Z',
} as Proxy)

const createProxySource = (id: number, name = `source-${id}`): AdminProxySource => ({
  id,
  owner_user_id: null,
  name,
  subscription_url: `https://source-${id}.example.com/subscription`,
  refresh_interval_minutes: 60,
  is_public: false,
  last_sync_status: 'success',
})

const paginated = <T,>(items: T[], page = 1, total = items.length, pages = 1) => ({
  items,
  total,
  page,
  page_size: 20,
  pages,
})

const mountView = (): VueWrapper => mount(ProxiesView, {
  global: {
    stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      TablePageLayout: {
        template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>',
      },
      DataTable: DataTableStub,
      Pagination: PaginationStub,
      BaseDialog: BaseDialogStub,
      ConfirmDialog: ConfirmDialogStub,
      EmptyState: true,
      ImportDataModal: true,
      Select: SelectStub,
      Icon: true,
      PlatformTypeBadge: true,
    },
  },
})

const openConfigCreateForm = async (wrapper: VueWrapper) => {
  await wrapper.get('[data-test="admin-proxy-create-button"]').trigger('click')
  await wrapper.get('[data-test="admin-proxy-input-mode-selector"]').setValue('config')
  await flushPromises()
}

describe('admin ProxiesView behavior', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()

    listProxies.mockResolvedValue(paginated([]))
    getAllProxies.mockResolvedValue([])
    listSources.mockResolvedValue(paginated([]))
    createSource.mockResolvedValue(createProxySource(1))
    updateSource.mockResolvedValue(createProxySource(1))
    deleteSource.mockResolvedValue({ message: 'success' })
    syncSource.mockResolvedValue({ created_count: 0 })
    syncAllSources.mockResolvedValue({
      total: 0,
      success_count: 0,
      partial_count: 0,
      failed_count: 0,
      skipped_count: 0,
      deferred_count: 0,
      created_count: 0,
      updated_count: 0,
      items: [],
    })
    testProxy.mockResolvedValue({ success: true, latency_ms: 10, message: 'ok' })
    checkProxyQuality.mockResolvedValue({
      score: 100,
      grade: 'A',
      summary: 'ok',
      checked_at: '2026-07-30T00:00:00Z',
      challenge_count: 0,
      failed_count: 0,
      warn_count: 0,
      items: [],
    })
  })

  afterEach(() => {
    vi.clearAllTimers()
    vi.useRealTimers()
  })

  it('loads a selected configuration file into the editable import field', async () => {
    const wrapper = mountView()
    await flushPromises()
    await openConfigCreateForm(wrapper)

    const content = 'proxies:\n  - name: test-node\n    type: socks5\n'
    const file = new File(['unused'], 'nodes.yaml', { type: 'application/yaml' })
    Object.defineProperty(file, 'text', {
      configurable: true,
      value: vi.fn().mockResolvedValue(content),
    })
    const input = wrapper.get('[data-test="admin-proxy-config-file-input"]')
    Object.defineProperty(input.element, 'files', {
      configurable: true,
      value: [file],
    })

    await input.trigger('change')
    await flushPromises()

    expect((wrapper.get('[data-test="admin-proxy-config-input"]').element as HTMLTextAreaElement).value)
      .toBe(content)
    expect(wrapper.text()).toContain('nodes.yaml')

    await wrapper.get('[data-test="admin-proxy-config-file-clear"]').trigger('click')
    expect((wrapper.get('[data-test="admin-proxy-config-input"]').element as HTMLTextAreaElement).value)
      .toBe('')

    wrapper.unmount()
  })

  it('reports unsupported and unreadable configuration files without replacing existing content', async () => {
    const wrapper = mountView()
    await flushPromises()
    await openConfigCreateForm(wrapper)

    const textarea = wrapper.get('[data-test="admin-proxy-config-input"]')
    await textarea.setValue('{"outbounds":[]}')
    const input = wrapper.get('[data-test="admin-proxy-config-file-input"]')

    const unsupported = new File(['ignored'], 'nodes.exe', { type: 'application/octet-stream' })
    Object.defineProperty(input.element, 'files', {
      configurable: true,
      value: [unsupported],
    })
    await input.trigger('change')
    expect(showError).toHaveBeenCalledWith('admin.proxies.configFileUnsupported')

    const unreadable = new File(['ignored'], 'nodes.json', { type: 'application/json' })
    Object.defineProperty(unreadable, 'text', {
      configurable: true,
      value: vi.fn().mockRejectedValue(new Error('read failed')),
    })
    Object.defineProperty(input.element, 'files', {
      configurable: true,
      value: [unreadable],
    })
    await input.trigger('change')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('admin.proxies.configFileReadFailed')
    expect((textarea.element as HTMLTextAreaElement).value).toBe('{"outbounds":[]}')

    wrapper.unmount()
  })

  it.each([
    { ownerScope: 'system', action: 'admin-proxy-batch-test-button', method: testProxy },
    { ownerScope: 'user', action: 'admin-proxy-batch-quality-button', method: checkProxyQuality },
  ])('keeps every active filter on each $ownerScope batch page', async ({ ownerScope, action, method }) => {
    listProxies.mockImplementation(async (page: number, pageSize: number) => {
      if (pageSize === 200) {
        return paginated([createProxy(200 + page)], page, 2, 2)
      }
      return paginated([])
    })

    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="admin-proxy-protocol-filter"]').setValue('https')
    await wrapper.get('[data-test="admin-proxy-status-filter"]').setValue('active')
    await wrapper.get('[data-test="admin-proxy-owner-scope-filter"]').setValue(ownerScope)
    await wrapper.get('[data-test="admin-proxy-search-input"]').setValue('needle')
    await wrapper.get('[data-test="sort-name"]').trigger('click')
    await flushPromises()
    listProxies.mockClear()
    method.mockClear()

    await wrapper.get(`[data-test="${action}"]`).trigger('click')
    await flushPromises()

    const batchCalls = listProxies.mock.calls.filter(([, pageSize]) => pageSize === 200)
    expect(batchCalls).toHaveLength(2)
    expect(batchCalls.map(([page]) => page)).toEqual([1, 2])
    for (const [, , query] of batchCalls) {
      expect(query).toEqual({
        protocol: 'https',
        status: 'active',
        owner_scope: ownerScope,
        search: 'needle',
        sort_by: 'name',
        sort_order: 'asc',
      })
    }
    expect(method.mock.calls.map(([id]) => id).sort()).toEqual([201, 202])

    wrapper.unmount()
  })

  it('does not fetch all pages when rows were explicitly selected', async () => {
    listProxies.mockResolvedValue(paginated([createProxy(11)]))
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="proxy-row-11"] input[type="checkbox"]').setValue(true)
    listProxies.mockClear()
    testProxy.mockClear()

    await wrapper.get('[data-test="admin-proxy-batch-test-button"]').trigger('click')
    await flushPromises()

    expect(listProxies.mock.calls.some(([, pageSize]) => pageSize === 200)).toBe(false)
    expect(testProxy).toHaveBeenCalledTimes(1)
    expect(testProxy).toHaveBeenCalledWith(11)

    wrapper.unmount()
  })

  it('manages the 101st source and returns to page one after deleting the last page item', async () => {
    const source101 = createProxySource(101, 'source-101')
    let deleted = false
    listSources.mockImplementation(async (page: number, pageSize: number) => {
      expect(pageSize).toBe(100)
      if (page === 2 && !deleted) {
        return { ...paginated([source101], 2, 101, 2), page_size: 100 }
      }
      return {
        ...paginated([createProxySource(1)], 1, deleted ? 100 : 101, deleted ? 1 : 2),
        page_size: 100,
      }
    })
    syncSource.mockResolvedValue({
      imported_count: 1,
      created_count: 0,
      updated_count: 1,
    })
    deleteSource.mockImplementation(async () => {
      deleted = true
      return { message: 'success' }
    })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="admin-proxy-source-manager-button"]').trigger('click')
    await flushPromises()

    const sourcePagination = wrapper.get('[data-test="admin-proxy-source-pagination"]')
    expect(sourcePagination.get('[data-test="pagination-page"]').text()).toBe('1')
    await sourcePagination.get('[data-test="pagination-next"]').trigger('click')
    await flushPromises()

    expect(listSources).toHaveBeenCalledWith(2, 100)
    expect(wrapper.text()).toContain('source-101')

    await wrapper.get('[data-test="admin-proxy-source-edit-101"]').trigger('click')
    expect((wrapper.get('#proxy-source-manager-form input[type="text"]').element as HTMLInputElement).value)
      .toBe('source-101')

    await wrapper.get('[data-test="admin-proxy-source-sync-101"]').trigger('click')
    await flushPromises()
    expect(syncSource).toHaveBeenCalledWith(101)
    expect(showSuccess).toHaveBeenCalledWith('admin.proxies.sourceSynced:{"count":1}')

    await wrapper.get('[data-test="admin-proxy-source-delete-101"]').trigger('click')
    await wrapper.get('[data-test="confirm-delete"]').trigger('click')
    await flushPromises()

    expect(deleteSource).toHaveBeenCalledWith(101)
    expect(listSources).toHaveBeenLastCalledWith(1, 100)
    expect(wrapper.text()).not.toContain('source-101')

    wrapper.unmount()
  })

  it('pauses a single source, syncs every source, and filters the list by source', async () => {
    const managed: AdminProxySource = {
      ...createProxySource(7, 'source-7'),
      node_count: 9,
      active_node_count: 4,
    }
    listSources.mockResolvedValue({ ...paginated([managed], 1, 1, 1), page_size: 100 })
    syncAllSources.mockResolvedValue({
      total: 1,
      success_count: 1,
      partial_count: 0,
      failed_count: 0,
      skipped_count: 0,
      deferred_count: 0,
      created_count: 2,
      updated_count: 3,
      items: [],
    })

    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="admin-proxy-source-manager-button"]').trigger('click')
    await flushPromises()

    // Pausing resends the whole record with only sync_enabled flipped.
    await wrapper.get('[data-test="admin-proxy-source-toggle-7"]').trigger('click')
    await flushPromises()
    expect(updateSource).toHaveBeenCalledWith(7, {
      name: 'source-7',
      subscription_url: 'https://source-7.example.com/subscription',
      refresh_interval_minutes: 60,
      is_public: false,
      sync_enabled: false,
    })
    expect(showSuccess).toHaveBeenCalledWith('admin.proxies.sourceSyncPaused')

    // Sync-all reports counts only: no node payloads, no subscription URLs.
    await wrapper.get('[data-test="admin-proxy-source-sync-all"]').trigger('click')
    await flushPromises()
    expect(syncAllSources).toHaveBeenCalledTimes(1)
    expect(showSuccess).toHaveBeenCalledWith('admin.proxies.sourceSyncAllDone')
    const summary = wrapper.get('[data-test="admin-proxy-source-sync-all-summary"]').text()
    expect(summary).toContain('admin.proxies.sourceSyncAllSummary')
    expect(summary).toContain('"created":2')
    expect(summary).toContain('"updated":3')
    expect(summary).not.toContain('source-7.example.com')

    // The node-count button closes the manager and reloads page one filtered.
    listProxies.mockClear()
    await wrapper.get('[data-test="admin-proxy-source-filter-7"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('[data-test="admin-proxy-source-manager"]').exists()).toBe(false)
    const filteredCall = listProxies.mock.calls.at(-1)
    expect(filteredCall?.[0]).toBe(1)
    expect(filteredCall?.[2]).toMatchObject({ source_id: 7 })

    wrapper.unmount()
  })
})
