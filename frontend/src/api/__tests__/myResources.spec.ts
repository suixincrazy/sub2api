import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { apiClient } from '@/api/client'
import { myResourcesApi, unsubscribeSubscription } from '@/api/myResources'

describe('myResourcesApi', () => {
  const originalAdapter = apiClient.defaults.adapter

  beforeEach(() => {
    vi.restoreAllMocks()
  })

  afterEach(() => {
    apiClient.defaults.adapter = originalAdapter
  })

  it('uses only user-scoped resource endpoints for ordinary user resource operations', async () => {
    const adapter = vi.fn().mockResolvedValue({
      status: 200,
      data: { code: 0, data: { items: [], total: 0, page: 1, page_size: 20, pages: 0 } },
      headers: {},
      config: {},
      statusText: 'OK',
    })
    apiClient.defaults.adapter = adapter

    await myResourcesApi.groups.list()
    await myResourcesApi.groups.liveCapability()
    await myResourcesApi.groups.usageSummary('Asia/Shanghai')
    await myResourcesApi.groups.capacitySummary()
    await myResourcesApi.groups.get(1)
    await myResourcesApi.groups.create({ name: 'group' })
    await myResourcesApi.groups.update(1, { name: 'group' })
    await myResourcesApi.groups.poolHealth(1)
    await myResourcesApi.groups.modelCandidates(1, 'openai')
    await myResourcesApi.groups.userOverrides(1)
    await myResourcesApi.groups.setRateMultipliers(1, [{ user_id: 2, rate_multiplier: 1.2 }])
    await myResourcesApi.groups.clearRateMultipliers(1)
    await myResourcesApi.groups.setRPMOverrides(1, [{ user_id: 2, rpm_override: 10 }])
    await myResourcesApi.groups.clearRPMOverrides(1)
    await myResourcesApi.accounts.list()
    await myResourcesApi.accounts.export({ ids: [1], include_proxies: true })
    await myResourcesApi.accounts.import({ accounts: [] })
    await myResourcesApi.accounts.importCodexSessions({ content: '{}' })
    await myResourcesApi.accounts.importCodexPAT({ access_token: 'at-test' })
    await myResourcesApi.accounts.batchUpdate([1], { status: 'active' })
    await myResourcesApi.accounts.oauth.authURL({ platform: 'openai' })
    await myResourcesApi.accounts.oauth.exchange({ platform: 'openai', session_id: 'session', code: 'code' })
    await myResourcesApi.accounts.oauth.cookie({ session_key: 'cookie' })
    await myResourcesApi.accounts.oauth.geminiCapabilities()
    await myResourcesApi.accounts.syncUpstreamModels(1)
    await myResourcesApi.accounts.syncUpstreamModelsPreview({ platform: 'openai', type: 'apikey', api_key: 'temporary-key' })
    await myResourcesApi.accounts.getAntigravityDefaultModelMapping()
    await myResourcesApi.accounts.test(1)
    await myResourcesApi.accounts.refresh(1)
    await myResourcesApi.accounts.clearError(1)
    await myResourcesApi.accounts.setSchedulable(1, true)
    await myResourcesApi.proxies.list()
    await myResourcesApi.proxies.export({ ids: [1] })
    await myResourcesApi.proxies.importNodes({ content: 'http://127.0.0.1:8080' })
    await myResourcesApi.proxies.sources.list()
    await myResourcesApi.proxies.sources.sync(1)
    await myResourcesApi.assignedSubscriptions.list()
    await myResourcesApi.assignedSubscriptions.assign({ email: 'user@example.com', group_id: 1 })
    await myResourcesApi.assignedSubscriptions.bulkAssign({ emails: ['user@example.com'], group_id: 1 })
    await myResourcesApi.assignedSubscriptions.extend(1, 30)
    await myResourcesApi.assignedSubscriptions.revoke(1)
    await myResourcesApi.assignedSubscriptions.restore(1)
    await myResourcesApi.assignedSubscriptions.resetUsage(1)
    await myResourcesApi.redeemCodes.list()
    await myResourcesApi.redeemCodes.usages(1)
    await myResourcesApi.redeemCodes.generate({ group_id: 1, count: 1 })
    await myResourcesApi.redeemCodes.stats()
    await myResourcesApi.redeemCodes.export({ ids: [1] })
    await myResourcesApi.redeemCodes.batchUpdate([1], { status: 'expired' })
    await myResourcesApi.redeemCodes.expire(1)
    await myResourcesApi.redeemCodes.batchDelete([1])
    await myResourcesApi.redeemCodes.batchExpire([1])
    await myResourcesApi.usage.accountLogs()
    await myResourcesApi.usage.accountStats()
    await myResourcesApi.usage.exportAccountLogs()
    await myResourcesApi.usage.upstreamErrors()
    await unsubscribeSubscription(1)

    const urls = adapter.mock.calls.map(([config]) => String(config.url))

    expect(urls.length).toBeGreaterThan(0)
    expect(urls.some(url => url.includes('/admin/'))).toBe(false)
    expect(urls.filter(url => url.startsWith('/my/')).length).toBe(urls.length - 1)
    expect(urls).toContain('/subscriptions/1/unsubscribe')
    expect(urls).toContain('/my/accounts/1/models/sync-upstream')
    expect(urls).toContain('/my/accounts/models/sync-upstream-preview')
    expect(urls).toContain('/my/accounts/oauth/gemini/capabilities')
    expect(urls).toContain('/my/accounts/antigravity/default-model-mapping')
    expect(urls).toContain('/my/groups/live-capability')
  })
})
