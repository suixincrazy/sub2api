import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

import en from '@/i18n/locales/en'
import zh from '@/i18n/locales/zh'

const source = readFileSync(resolve(process.cwd(), 'src/views/user/MyResourcesView.vue'), 'utf8')
const proxyEditorSource = readFileSync(resolve(process.cwd(), 'src/components/user/MyProxyEditorDialog.vue'), 'utf8')
const globalStyles = readFileSync(resolve(process.cwd(), 'src/style.css'), 'utf8')

describe('MyResourcesView responsive localization', () => {
  it('delegates responsive tables and mobile cards to the shared DataTable', () => {
    expect(source).toContain('<DataTable')
    expect(source).toContain(':columns="alignedColumns"')
    expect(source).not.toContain('sm:sticky sm:right-0')
    expect(source).not.toContain('<div v-else class="space-y-4">')
  })

  it('aligns every user resource with the administrator table system', () => {
    expect(source).toContain('<TablePageLayout>')
    expect(source).toContain('<DataTable')
    expect(source).toContain(':columns="alignedColumns"')
    expect(source).toContain("resource === 'proxies' ? 'flex min-h-0 min-w-0 flex-1 flex-col overflow-hidden'")
    expect(source).toContain("'flex min-h-0 min-w-0 flex-1 flex-col space-y-4'")
    expect(source).not.toContain('adminAlignedResource')
    expect(source).not.toContain('<div v-else class="space-y-4">')
    for (const resource of ['groups', 'accounts', 'proxies', 'assigned-subscriptions', 'redeem-codes', 'account-logs', 'upstream-errors']) {
      expect(source).toContain(`'${resource}'`)
    }
    expect(source).toContain("mr('columns.visibility')")
    expect(source).toContain("mr('columns.proxyMode')")
    expect(source).toContain("mr('columns.location')")
    expect(source).toContain("mr('columns.latency')")
    expect(source).toContain("mr('columns.usage')")
    expect(source).toContain("mr('actions.userRates')")
    expect(source).toContain('max-w-[70%] break-all text-xs sm:max-w-none sm:whitespace-nowrap sm:break-normal')
    expect(source).toContain("mr('states.publicDetailsHidden')")
    expect(source).not.toContain("class: 'min-w-44'")
    expect(source).not.toContain("resource.value === 'proxies' ? 'min-w-64'")
    expect(source).not.toContain('class="code whitespace-nowrap text-xs"')
    expect(source).toContain("t('admin.groups.accountsCount', { count: Number(value || 0) })")
    expect(source).toContain("t('admin.proxies.qualityInline'")

    const testAction = source.indexOf("mr('actions.testConnection')")
    const qualityAction = source.indexOf("mr('actions.quality')")
    const proxyEditAction = source.indexOf("resource === 'proxies' && canMutateItem(row)")
    expect(testAction).toBeGreaterThan(-1)
    expect(testAction).toBeLessThan(qualityAction)
    expect(qualityAction).toBeLessThan(proxyEditAction)
  })

  it('constrains every resource dialog to the viewport with an internal scroll area', () => {
    const compactConstraints = source.match(/max-h-\[calc\(100dvh-1rem\)\]/g) || []
    const wideConstraints = source.match(/sm:max-h-\[calc\(100dvh-2rem\)\]/g) || []
    expect(compactConstraints.length).toBeGreaterThanOrEqual(9)
    expect(wideConstraints).toHaveLength(compactConstraints.length)
    expect((source.match(/overscroll-contain/g) || []).length).toBeGreaterThanOrEqual(compactConstraints.length)
    expect(source).not.toContain('max-h-[90vh]')
  })

  it('uses localized labels for the toolbar actions found during UI review', () => {
    for (const text of ['>Columns<', '>Export<', '>Import<', '>Test<', '>Refresh<', '>Sources<', '>Stats<']) {
      expect(source).not.toContain(text)
    }
  })

  it('promotes the proxy source manager to a labeled toolbar button', () => {
    expect(source).toContain(":title=\"mr('actions.sources')\"")
    expect(source).toContain(":aria-label=\"mr('actions.sources')\"")
    expect(source).toContain('<Icon name="link" size="md" class="md:mr-2" />')
    expect(source).toContain("<span class=\"hidden md:inline\">{{ mr('actions.sources') }}</span>")
    expect(source).toContain('@click="openProxySources"')

    // The remaining dropdown trigger is the column picker for every resource.
    expect(source).toContain(":title=\"mr('actions.columns')\"")
    expect(source).toContain('<Icon name="grid" size="md" class="md:mr-2" />')
    expect(source).toContain("mr('actions.visibleColumns')")
    expect(source).not.toContain("resource === 'proxies' ? t('admin.accounts.moreActions') : mr('actions.columns')")
    expect(source).not.toContain("<Icon :name=\"resource === 'proxies' ? 'more' : 'grid'\"")
    expect(source).not.toContain('showColumnSettings = false; openProxySources()')

    // Toolbar order from the approved review: refresh, test, quality, sources, columns.
    const qualityAction = source.indexOf("mr('actions.batchQuality')")
    const sourcesAction = source.indexOf("mr('actions.sources')")
    const columnsAction = source.indexOf("mr('actions.columns')")
    expect(qualityAction).toBeGreaterThan(-1)
    expect(qualityAction).toBeLessThan(sourcesAction)
    expect(sourcesAction).toBeLessThan(columnsAction)

    expect(source).toContain("document.addEventListener('click', closeColumnSettingsOnOutsideClick)")
    expect(source).toContain("document.removeEventListener('click', closeColumnSettingsOnOutsideClick)")
  })

  it('gives the proxy toolbar actions their own wrapped row instead of shrinking them into a column', () => {
    // flex-1 resolves to flex-basis: 0%, so the action group is never pushed to a new
    // flex line: it keeps shrinking into whatever space is left beside the filters and
    // stacks the nine buttons into a narrow column (measured 206px wide / 6 rows once
    // the source filter joined the row, and narrow enough at 1200px to break labels
    // mid-word). w-full (basis: 100%) puts the group on its own row, grow with
    // justify-end keeps it right-aligned there, lg:w-auto lets it share the filter row
    // again when the viewport can hold both at natural width, and whitespace-nowrap
    // stops labels from breaking mid-word if it is ever squeezed again.
    expect(source).toContain(
      "resource === 'proxies' ? 'flex w-full grow flex-wrap items-center justify-end gap-2 whitespace-nowrap lg:w-auto'"
    )
    expect(source).not.toContain("'flex flex-1 flex-wrap items-center justify-end gap-2'")
    // The non-proxy resources keep the official toolbar contract untouched.
    expect(source).toContain("'flex w-full flex-shrink-0 flex-wrap items-center justify-end gap-2 lg:w-auto'")
  })

  it('manages proxy subscription sources from one modal', () => {
    // Sync every source, then report counts only — never node payloads or URLs.
    expect(source).toContain("mr('actions.syncAllSources')")
    expect(source).toContain('@click="syncAllProxySourceItems"')
    expect(source).toContain('myResourcesApi.proxies.sources.syncAll()')
    expect(source).toContain("mr('sources.syncAllSummary'")

    // Per-source node counts double as a filter entry point.
    expect(source).toContain("mr('sources.nodeCount'")
    expect(source).toContain('@click="filterProxiesBySource(source)"')
    expect(source).toContain('v-model="filters.source_id"')
    expect(source).toContain(':options="proxySourceFilterOptions"')
    expect(source).toContain('source_id: filters.source_id ? Number(filters.source_id) : undefined')

    // Pause/resume plus the next-run and airport-quota columns.
    expect(source).toContain('@click="toggleProxySourceSync(source)"')
    expect(source).toContain("mr('actions.pauseSync')")
    expect(source).toContain("mr('actions.resumeSync')")
    expect(source).toContain("mr('sources.nextSyncAt'")
    expect(source).toContain("mr('sources.syncPaused')")
    expect(source).toContain('proxySourceTrafficText(source)')
    expect(source).toContain("mr('sources.expiresAt'")
    expect(source).toContain('v-model="proxySourceForm.sync_enabled"')

    // The widened modal table scrolls locally instead of clipping columns.
    expect(source).toContain('class="min-w-0 overflow-x-auto rounded-lg border border-gray-200 dark:border-dark-700"')
    expect(source).toContain('colspan="7"')
    expect(source).not.toContain('colspan="5"')
  })

  it('uses project Select controls and proxy-specific filters', () => {
    expect((source.match(/<Select/g) || []).length).toBeGreaterThan(10)
    expect((source.match(/<select[^>]+multiple/g) || []).length).toBeGreaterThanOrEqual(3)
    expect(source).toContain('<MyProxyEditorDialog')
    expect(proxyEditorSource).toContain('<BaseDialog')
    expect(proxyEditorSource).toContain('width="normal"')
    expect(proxyEditorSource).toContain("type CreateMode = 'standard' | 'batch'")
    expect(globalStyles).toContain('max-h-[calc(100dvh-1rem)] sm:max-h-[calc(100dvh-2rem)]')
    expect(globalStyles).toContain('@apply flex-1 overflow-y-auto')
    expect(source).toContain('v-model="filters.type"')
    expect(source).toContain('v-model="filters.protocol"')
    expect(source).toContain("mr(`filters.searchByResource.${resource.value}`)")
    expect(source).not.toContain(':placeholder="mr(\'filters.search\')"')
  })

  it('does not submit redacted proxy credentials unless the user edits them', () => {
    expect(proxyEditorSource).toContain('usernameDirty.value')
    expect(proxyEditorSource).toContain('passwordDirty.value')
    expect(proxyEditorSource).toContain('nodeContentDirty.value')
    expect(proxyEditorSource).toContain('if (!props.proxy || usernameDirty.value)')
    expect(proxyEditorSource).toContain('if (!props.proxy || passwordDirty.value)')
    expect(proxyEditorSource).toContain("if (inputMode.value === 'xray' && nodeContentDirty.value)")
  })

  it('supports repeatable redeem codes through owner-scoped usage details', () => {
    expect(source).toContain('v-model="editorForm.redeem.repeatable"')
    expect(source).toContain('v-model.number="editorForm.redeem.max_uses"')
    expect(source).toContain('myResourcesApi.redeemCodes.usages')
    expect(source).toContain('#cell-usage_count')
  })

  it('renders scoped usage and upstream-error details without a second unscoped request', () => {
    expect(source).toContain("resource.value === 'upstream-errors'")
    expect(source).toContain("mr('details.accountLogTitle')")
    expect(source).toContain("mr('details.upstreamErrorTitle')")
    expect(source).toContain('@click="openRecordDetails(row)"')
    expect(source).not.toContain('usage.recordDetails')
    expect(source).not.toMatch(/[\p{Script=Han}]/u)
  })

  it('defines the user-resource message namespace in both locales', () => {
    for (const messages of [zh, en]) {
      expect(messages.myResources.actions.columns).toEqual(expect.any(String))
      expect(messages.myResources.actions.exportCsv).toEqual(expect.any(String))
      expect(messages.myResources.actions.userRates).toEqual(expect.any(String))
      expect(messages.myResources.actions.testConnection).toEqual(expect.any(String))
      expect(messages.myResources.states.healthy).toEqual(expect.any(String))
      expect(messages.myResources.fields.subscriptionUrl).toEqual(expect.any(String))
      expect(messages.myResources.table.actions).toEqual(expect.any(String))
      expect(messages.myResources.table.qualityInline).toEqual(expect.any(String))
      expect(messages.myResources.messages.oauthUrlMissing).toEqual(expect.any(String))
      expect(messages.myResources.overrides.title).toEqual(expect.any(String))
    }
  })
})
