import { apiClient } from './client'
import type { ClaudeModel, ProxyQualityCheckResult, ProxySourceSyncAllResult } from '@/types'

export interface ResourcePage<T = Record<string, any>> {
  items: T[]
  total: number
  page: number
  page_size: number
  pages: number
}

export interface ResourceListParams {
  page?: number
  page_size?: number
  search?: string
  status?: string
  platform?: string
  type?: string
  protocol?: string
  group_id?: number | string
  user_id?: number | string
  api_key_id?: number | string
  account_id?: number | string
  source_id?: number | string
  start_date?: string
  end_date?: string
  timezone?: string
  sort_by?: string
  sort_order?: 'asc' | 'desc' | string
  owned_only?: boolean
}

export interface AssignSubscriptionPayload {
  user_id?: number
  email?: string
  group_id: number
  validity_days?: number
  notes?: string
}

export interface BulkAssignSubscriptionPayload {
  user_ids?: number[]
  emails?: string[]
  group_id: number
  validity_days?: number
  notes?: string
}

export interface UserOAuthAuthURLPayload {
  platform: string
  proxy_id?: number
  setup_token?: boolean
  redirect_uri?: string
  project_id?: string
  oauth_type?: string
  tier_id?: string
}

export interface UserOAuthExchangePayload extends UserOAuthAuthURLPayload {
  session_id: string
  code: string
  state?: string
}

export interface UserOAuthCredentialsResult {
  credentials: Record<string, any>
  extra?: Record<string, any>
  suggested_name?: string
}

export interface SyncUpstreamModelsResult {
  models: string[]
  metadata?: Record<string, UpstreamModelMetadata>
  warnings?: UpstreamModelSyncWarning[]
}

export interface UpstreamModelSyncWarning {
  code: string
  message: string
}

export interface UpstreamModelMetadata {
  id: string
  display_name?: string
  description?: string
  reasoning?: boolean
  default_reasoning_level?: string
  supported_reasoning_levels?: string[]
  input_modalities?: string[]
  context_window?: number
  max_output_tokens?: number
}

export interface SyncUpstreamPreviewParams {
  platform: string
  type: string
  base_url?: string
  api_key: string
  account_mode?: string
  api_protocol?: string
}

export interface GeminiOAuthCapabilities {
  ai_studio_oauth_enabled: boolean
  required_redirect_uris: string[]
}

export type ResourceItem = Record<string, any>

export interface UserProxyTestResult {
  success: boolean
  message: string
  latency_ms?: number
  ip_address?: string
  city?: string
  region?: string
  country?: string
  country_code?: string
}

const getPage = async (url: string, params?: ResourceListParams): Promise<ResourcePage> => {
  const { data } = await apiClient.get<ResourcePage>(url, { params })
  return data
}

export const myResourcesApi = {
  groups: {
    list: (params?: ResourceListParams) => getPage('/my/groups', params),
    liveCapability: async () => (await apiClient.get<{ supported: boolean; reason?: string }>('/my/groups/live-capability')).data,
    usageSummary: async (timezone?: string) => (await apiClient.get<ResourceItem[]>('/my/groups/usage-summary', { params: timezone ? { timezone } : undefined })).data,
    capacitySummary: async () => (await apiClient.get<ResourceItem[]>('/my/groups/capacity-summary')).data,
    get: async (id: number) => (await apiClient.get<ResourceItem>(`/my/groups/${id}`)).data,
    create: async (payload: ResourceItem) => (await apiClient.post<ResourceItem>('/my/groups', payload)).data,
    update: async (id: number, payload: ResourceItem) => (await apiClient.put<ResourceItem>(`/my/groups/${id}`, payload)).data,
    delete: async (id: number) => (await apiClient.delete(`/my/groups/${id}`)).data,
    poolHealth: async (id: number) => (await apiClient.get(`/my/groups/${id}/pool-health`)).data,
    modelCandidates: async (id: number, platform?: string) => (await apiClient.get<{ models: string[] }>(`/my/groups/${id}/models-list-candidates`, { params: platform ? { platform } : undefined })).data,
    userOverrides: async (id: number) => (await apiClient.get<ResourceItem[]>(`/my/groups/${id}/user-overrides`)).data,
    setRateMultipliers: async (id: number, entries: Array<{ user_id: number; rate_multiplier: number }>) => (await apiClient.put(`/my/groups/${id}/rate-multipliers`, { entries })).data,
    clearRateMultipliers: async (id: number) => (await apiClient.delete(`/my/groups/${id}/rate-multipliers`)).data,
    setRPMOverrides: async (id: number, entries: Array<{ user_id: number; rpm_override: number }>) => (await apiClient.put(`/my/groups/${id}/rpm-overrides`, { entries })).data,
    clearRPMOverrides: async (id: number) => (await apiClient.delete(`/my/groups/${id}/rpm-overrides`)).data,
  },
  accounts: {
    list: (params?: ResourceListParams) => getPage('/my/accounts', params),
    get: async (id: number) => (await apiClient.get<ResourceItem>(`/my/accounts/${id}`)).data,
    create: async (payload: ResourceItem) => (await apiClient.post<ResourceItem>('/my/accounts', payload)).data,
    update: async (id: number, payload: ResourceItem) => (await apiClient.put<ResourceItem>(`/my/accounts/${id}`, payload)).data,
    delete: async (id: number) => (await apiClient.delete(`/my/accounts/${id}`)).data,
    export: async (params?: { ids?: number[]; include_proxies?: boolean }) => (await apiClient.get('/my/accounts/export', { params: { ...params, ids: params?.ids?.join(',') } })).data,
    import: async (payload: ResourceItem) => (await apiClient.post('/my/accounts/import', payload)).data,
    importCodexSessions: async (payload: ResourceItem) => (await apiClient.post('/my/accounts/import/codex-session', payload)).data,
    importCodexPAT: async (payload: ResourceItem) => (await apiClient.post<ResourceItem>('/my/accounts/import/codex-pat', payload)).data,
    batchUpdate: async (ids: number[], fields: ResourceItem) => (await apiClient.post('/my/accounts/batch-update', { ids, fields })).data,
    oauth: {
      authURL: async (payload: UserOAuthAuthURLPayload) => (await apiClient.post<ResourceItem>('/my/accounts/oauth/auth-url', payload)).data,
      exchange: async (payload: UserOAuthExchangePayload) => (await apiClient.post<UserOAuthCredentialsResult>('/my/accounts/oauth/exchange', payload)).data,
      cookie: async (payload: { proxy_id?: number; setup_token?: boolean; session_key: string }) => (await apiClient.post<UserOAuthCredentialsResult>('/my/accounts/oauth/cookie', payload)).data,
      geminiCapabilities: async () => (await apiClient.get<GeminiOAuthCapabilities>('/my/accounts/oauth/gemini/capabilities')).data,
    },
    syncUpstreamModels: async (id: number): Promise<SyncUpstreamModelsResult> =>
      (await apiClient.post<SyncUpstreamModelsResult>(`/my/accounts/${id}/models/sync-upstream`)).data,
    syncUpstreamModelsPreview: async (payload: SyncUpstreamPreviewParams): Promise<SyncUpstreamModelsResult> =>
      (await apiClient.post<SyncUpstreamModelsResult>('/my/accounts/models/sync-upstream-preview', payload)).data,
    getAntigravityDefaultModelMapping: async (): Promise<Record<string, string>> =>
      (await apiClient.get<Record<string, string>>('/my/accounts/antigravity/default-model-mapping')).data,
    getAvailableModels: async (id: number) => (await apiClient.get<ClaudeModel[]>(`/my/accounts/${id}/models`)).data,
    test: async (id: number) => (await apiClient.post(`/my/accounts/${id}/test`)).data,
    refresh: async (id: number) => (await apiClient.post<ResourceItem>(`/my/accounts/${id}/refresh`)).data,
    clearError: async (id: number) => (await apiClient.post<ResourceItem>(`/my/accounts/${id}/clear-error`)).data,
    setSchedulable: async (id: number, schedulable: boolean) => (await apiClient.post<ResourceItem>(`/my/accounts/${id}/schedulable`, { schedulable })).data,
  },
  proxies: {
    list: (params?: ResourceListParams) => getPage('/my/proxies', params),
    get: async (id: number) => (await apiClient.get<ResourceItem>(`/my/proxies/${id}`)).data,
    create: async (payload: ResourceItem) => (await apiClient.post<ResourceItem>('/my/proxies', payload)).data,
    update: async (id: number, payload: ResourceItem) => (await apiClient.put<ResourceItem>(`/my/proxies/${id}`, payload)).data,
    delete: async (id: number) => (await apiClient.delete(`/my/proxies/${id}`)).data,
    test: async (id: number) => (await apiClient.post<UserProxyTestResult>(`/my/proxies/${id}/test`)).data,
    qualityCheck: async (id: number) => (await apiClient.post<ProxyQualityCheckResult>(`/my/proxies/${id}/quality-check`)).data,
    export: async (params?: { ids?: number[] }) => (await apiClient.get('/my/proxies/export', { params: { ...params, ids: params?.ids?.join(',') } })).data,
    importNodes: async (payload: { name_prefix?: string; content: string; is_public?: boolean }) => (await apiClient.post('/my/proxies/import', payload)).data,
    sources: {
      list: (params?: ResourceListParams) => getPage('/my/proxies/sources', params),
      create: async (payload: ResourceItem) => (await apiClient.post<ResourceItem>('/my/proxies/sources', payload)).data,
      update: async (id: number, payload: ResourceItem) => (await apiClient.put<ResourceItem>(`/my/proxies/sources/${id}`, payload)).data,
      delete: async (id: number) => (await apiClient.delete(`/my/proxies/sources/${id}`)).data,
      sync: async (id: number) => (await apiClient.post(`/my/proxies/sources/${id}/sync`)).data,
      syncAll: async () => (await apiClient.post<ProxySourceSyncAllResult>('/my/proxies/sources/sync-all')).data,
    },
  },
  assignedSubscriptions: {
    list: (params?: ResourceListParams) => getPage('/my/assigned-subscriptions', params),
    assign: async (payload: AssignSubscriptionPayload) => (await apiClient.post<ResourceItem>('/my/assigned-subscriptions', payload)).data,
    bulkAssign: async (payload: BulkAssignSubscriptionPayload) => (await apiClient.post('/my/assigned-subscriptions/bulk', payload)).data,
    extend: async (id: number, days: number) => (await apiClient.post<ResourceItem>(`/my/assigned-subscriptions/${id}/extend`, { days })).data,
    revoke: async (id: number) => (await apiClient.post(`/my/assigned-subscriptions/${id}/revoke`)).data,
    restore: async (id: number) => (await apiClient.post<ResourceItem>(`/my/assigned-subscriptions/${id}/restore`)).data,
    resetUsage: async (id: number) => (await apiClient.post<ResourceItem>(`/my/assigned-subscriptions/${id}/reset-usage`)).data,
  },
  redeemCodes: {
    list: (params?: ResourceListParams) => getPage('/my/redeem-codes', params),
    usages: (id: number, params?: ResourceListParams) => getPage(`/my/redeem-codes/${id}/usages`, params),
    generate: async (payload: ResourceItem) => (await apiClient.post<ResourceItem[]>('/my/redeem-codes', payload)).data,
    stats: async () => (await apiClient.get('/my/redeem-codes/stats')).data,
    export: async (params?: ResourceListParams & { ids?: number[] }) => (await apiClient.get('/my/redeem-codes/export', { params: { ...params, ids: params?.ids?.join(',') }, responseType: 'blob' })).data,
    batchUpdate: async (ids: number[], fields: ResourceItem) => (await apiClient.post('/my/redeem-codes/batch-update', { ids, fields })).data,
    delete: async (id: number) => (await apiClient.delete(`/my/redeem-codes/${id}`)).data,
    expire: async (id: number) => (await apiClient.post<ResourceItem>(`/my/redeem-codes/${id}/expire`)).data,
    batchDelete: async (ids: number[]) => (await apiClient.delete('/my/redeem-codes', { data: { ids } })).data,
    batchExpire: async (ids: number[]) => (await apiClient.post('/my/redeem-codes/batch-expire', { ids })).data,
  },
  usage: {
    accountLogs: (params?: ResourceListParams) => getPage('/my/usage/account-logs', params),
    accountStats: async (params?: ResourceListParams) => (await apiClient.get<ResourceItem>('/my/usage/account-logs/stats', { params })).data,
    exportAccountLogs: async (params?: ResourceListParams) => (await apiClient.get('/my/usage/account-logs/export', { params, responseType: 'blob' })).data,
    upstreamErrors: (params?: ResourceListParams) => getPage('/my/usage/upstream-errors', params),
  },
}

export async function unsubscribeSubscription(id: number): Promise<unknown> {
  const { data } = await apiClient.post(`/subscriptions/${id}/unsubscribe`)
  return data
}
