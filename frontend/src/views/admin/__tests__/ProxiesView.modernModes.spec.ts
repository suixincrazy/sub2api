import { existsSync, readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(process.cwd(), 'src/views/admin/ProxiesView.vue'), 'utf8')
const apiSource = readFileSync(resolve(process.cwd(), 'src/api/admin/proxies.ts'), 'utf8')
const createAccountSource = readFileSync(resolve(process.cwd(), 'src/components/account/CreateAccountModal.vue'), 'utf8')
const editAccountSource = readFileSync(resolve(process.cwd(), 'src/components/account/EditAccountModal.vue'), 'utf8')

describe('admin proxy modern mode support', () => {
  it('keeps official create tabs and uses the same four add methods as My Proxies', () => {
    expect(source).toContain('data-test="admin-proxy-create-mode-standard"')
    expect(source).toContain('data-test="admin-proxy-create-mode-batch"')
    expect(source).toContain('data-test="admin-proxy-input-mode-selector"')
    expect(source).toContain('v-model="inputMode"')
    expect(source).toContain(':options="inputModeOptions"')
    expect(source).toContain("{ value: 'direct', label:")
    expect(source).toContain("{ value: 'xray', label:")
    expect(source).toContain("{ value: 'source', label:")
    expect(source).toContain("{ value: 'config', label:")
    expect(source).toContain("t('myResources.proxyEditor.standardProxy')")
    expect(source).toContain("t('myResources.proxyEditor.xrayShare')")
    expect(source).toContain("t('myResources.proxyEditor.providerSubscription')")
    expect(source).toContain("t('myResources.proxyEditor.nodeConfig')")
    expect(source).not.toContain(':options="createModeOptions"')
    expect(source).not.toContain('data-test="admin-proxy-mode-direct"')
  })

  it('renders name, add method, and protocol in the requested order with one submit form', () => {
    const nameIndex = source.indexOf('data-test="admin-proxy-name-field"')
    const inputModeIndex = source.indexOf('data-test="admin-proxy-input-mode-field"')
    const protocolIndex = source.indexOf('data-test="admin-proxy-protocol-field"')

    expect(nameIndex).toBeGreaterThan(0)
    expect(inputModeIndex).toBeGreaterThan(nameIndex)
    expect(protocolIndex).toBeGreaterThan(inputModeIndex)
    expect(source.match(/id="create-proxy-form"/g)).toHaveLength(1)
  })

  it('supports selecting local Sing-box and Clash configuration files', () => {
    expect(source).toContain('data-test="admin-proxy-config-file-input"')
    expect(source).toContain('accept=".json,.yaml,.yml,.txt,.conf,')
    expect(source).toContain('@change="handleConfigFileChange"')
    expect(source).toContain('createForm.import_content = content')
    expect(source).toContain("appStore.showError(t('admin.proxies.configFileReadFailed'))")
  })

  it('removes the proxy promotion component and every account-form reference', () => {
    expect(existsSync(resolve(process.cwd(), 'src/components/common/ProxyAdBanner.vue'))).toBe(false)
    expect(source).not.toContain('ProxyAdBanner')
    expect(createAccountSource).not.toContain('ProxyAdBanner')
    expect(editAccountSource).not.toContain('ProxyAdBanner')
    expect(source).not.toContain('sub2api.io/proxyip')
  })

  it('routes imports and subscription sources through admin-only APIs', () => {
    expect(source).toContain('adminAPI.proxies.importNodes({')
    expect(source).toContain('adminAPI.proxies.sources.create({')
    expect(source).toContain('adminAPI.proxies.sources.sync(source.id)')
    expect(source).not.toContain('/my/')
    expect(apiSource).toContain("'/admin/proxies/import'")
    expect(apiSource).toContain("'/admin/proxies/sources'")
    expect(apiSource).not.toContain('/my/')
  })

  it('uses an explicit supported-protocol whitelist for batch share links', () => {
    expect(source).toContain('const MODERN_PROXY_URI_PATTERN = /^(https?|socks(?:5h?)?|vmess|vless|trojan|ss|hysteria|hy2|hysteria2|tuic|anytls|naive(?:\\+https|\\+quic)?|wireguard|wg):\\/\\/\\S+$/i')
    expect(source).not.toContain('[a-z][a-z0-9+.-]*')
  })

  it('provides a responsive administrator subscription-source manager', () => {
    expect(source).toContain('data-test="admin-proxy-source-manager-button"')
    expect(source).toContain('data-test="admin-proxy-source-manager"')
    expect(source).toContain('width="wide"')
    expect(source).toContain('data-test="admin-proxy-source-table-scroll"')
    expect(source).toContain('class="overflow-x-auto rounded-md border')
    expect(source).toContain('min-w-[1040px]')
    expect(source).toContain('data-test="admin-proxy-source-pagination"')
    expect(source).toContain(':page-size="PROXY_SOURCE_PAGE_SIZE"')
    expect(source).toContain('adminAPI.proxies.sources.update(editingProxySourceId.value, payload)')
    expect(source).toContain('adminAPI.proxies.sources.delete(source.id)')
    expect(source).toContain('adminAPI.proxies.sources.sync(source.id)')
    expect(source).toContain("t('admin.proxies.sourceCreatedSyncFailed')")
  })

  it('mirrors the My Proxies source-management controls for administrators', () => {
    // Sync every source at once, then report counts only: node payloads and
    // subscription URLs must never reach the summary strip.
    expect(source).toContain('data-test="admin-proxy-source-sync-all"')
    expect(source).toContain('@click="syncAllProxySources"')
    expect(source).toContain('adminAPI.proxies.sources.syncAll()')
    expect(source).toContain('data-test="admin-proxy-source-sync-all-summary"')
    expect(source).toContain("t('admin.proxies.sourceSyncAllSummary'")

    // Per-source node counts double as an entry point into the list filter.
    expect(source).toContain("t('admin.proxies.sourceColumnNodes')")
    expect(source).toContain("t('admin.proxies.sourceNodeCount'")
    expect(source).toContain('@click="filterProxiesBySource(source)"')
    expect(source).toContain('data-test="admin-proxy-source-filter"')
    expect(source).toContain('v-model="filters.source_id"')
    expect(source).toContain(':options="proxySourceFilterOptions"')
    expect(source).toContain('source_id: filters.source_id ? Number(filters.source_id) : undefined')

    // Pause/resume plus the next-run and airport-quota columns.
    expect(source).toContain(':data-test="`admin-proxy-source-toggle-${source.id}`"')
    expect(source).toContain('@click="toggleProxySourceSync(source)"')
    expect(source).toContain("t('admin.proxies.sourceNextSyncAt'")
    expect(source).toContain("t('admin.proxies.sourceSyncPausedHint')")
    expect(source).toContain("t('admin.proxies.sourceColumnQuota')")
    expect(source).toContain('proxySourceTrafficText(source)')
    expect(source).toContain("t('admin.proxies.sourceExpiresAt'")
    expect(source).toContain('v-model="proxySourceForm.sync_enabled"')
    expect(source).toContain('data-test="admin-proxy-source-sync-enabled"')
  })

  it('supports standard and Xray proxy modes without dropping node metadata', () => {
    expect(source).toContain("kind: 'standard' as ProxyKind")
    expect(source).toContain("{ value: 'vless', label: 'VLESS' }")
    expect(source).toContain("{ value: 'hysteria2', label: 'Hysteria2' }")
    expect(source).toContain("{ value: 'tuic', label: 'TUIC' }")
    expect(source).toContain("{ value: 'anytls', label: 'AnyTLS' }")
    expect(source).toContain("{ value: 'naive', label: 'Naive' }")
    expect(source).toContain("{ value: 'wireguard', label: 'WireGuard' }")
    expect(source).toContain("{ ...(editingProxy.value.extra || {}), raw: editForm.xray_raw.trim() }")
    expect(source).toContain("proxy.kind || 'standard'")
  })

  it('masks both proxy usernames and passwords until explicitly revealed', () => {
    expect(source).toContain("visiblePasswordIds.has(row.id) ? row.username : '\u2022\u2022\u2022\u2022\u2022\u2022'")
    expect(source).toContain("visiblePasswordIds.has(row.id) ? row.password : '\u2022\u2022\u2022\u2022\u2022\u2022'")
    expect(source).toContain("t('admin.proxies.showCredentials')")
  })

  it('gives the toolbar actions their own wrapped row instead of shrinking them into a column', () => {
    // flex-1 resolves to flex-basis: 0%, so the action group is never pushed to a new
    // flex line: it keeps shrinking into whatever space is left beside the filters and
    // stacks the buttons into a narrow column (measured 206px wide / 6 rows once the
    // source filter joined the row). w-full (basis: 100%) puts the group on its own
    // row, grow + justify-end keeps it right-aligned there, lg:w-auto lets it share the
    // filter row again when the viewport can hold both at natural width, and
    // whitespace-nowrap stops labels from breaking mid-word if it is ever squeezed.
    expect(source).toContain('class="flex w-full grow flex-wrap items-center justify-end gap-2 whitespace-nowrap lg:w-auto"')
    expect(source).not.toContain('class="flex flex-1 flex-wrap items-center justify-end gap-2"')
  })
})
