import type { Proxy } from '@/types'

export type AccountResourceScope = 'admin' | 'user'

export function filterAccountCompatibleProxies(
  proxies: Proxy[],
  accountOwnerUserId: number | null | undefined
): Proxy[] {
  if (accountOwnerUserId == null) {
    return proxies.filter(proxy => proxy.owner_user_id == null)
  }

  return proxies.filter(proxy =>
    proxy.owner_user_id === accountOwnerUserId || proxy.is_public === true
  )
}

export function isUserSelectableProxy(proxy: Proxy, now = Date.now()): boolean {
  if (proxy.status !== 'active') return false
  if (proxy.expires_at == null || proxy.expires_at === '') return true

  const expiresAt = new Date(proxy.expires_at).getTime()
  return Number.isFinite(expiresAt) && expiresAt > now
}

export function proxiesForAccountScope(
  proxies: Proxy[],
  scope: AccountResourceScope,
  accountOwnerUserId?: number | null
): Proxy[] {
  if (scope === 'user') {
    return filterAccountCompatibleProxies(proxies, accountOwnerUserId)
      .filter(proxy => isUserSelectableProxy(proxy))
  }

  return filterAccountCompatibleProxies(proxies, accountOwnerUserId)
}

export function toUnixSeconds(value: unknown): number | null {
  if (value == null || value === '') return null
  if (typeof value === 'number' && Number.isFinite(value)) {
    const seconds = Math.floor(value)
    return seconds > 0 ? seconds : null
  }

  const date = new Date(String(value))
  if (Number.isNaN(date.getTime())) return null
  return Math.floor(date.getTime() / 1000)
}

export function toUserAccountPayload<T extends object>(payload: T): T {
  const normalized = { ...payload } as Record<string, unknown>
  if ('expires_at' in normalized) {
    const timestamp = toUnixSeconds(normalized.expires_at)
    normalized.expires_at = timestamp == null ? null : new Date(timestamp * 1000).toISOString()
  }
  if (normalized.proxy_id === 0) normalized.proxy_id = null
  return normalized as T
}
