import { describe, expect, it } from 'vitest'

import type { Proxy } from '@/types'
import {
  filterAccountCompatibleProxies,
  isUserSelectableProxy,
  proxiesForAccountScope,
  toUnixSeconds,
  toUserAccountPayload,
} from '@/utils/accountProxyScope'

const proxy = (
  id: number,
  owner_user_id: number | null,
  overrides: Partial<Proxy> = {}
) => ({
  id,
  owner_user_id,
  is_public: false,
  name: `Proxy ${id}`,
  kind: 'standard',
  protocol: 'http',
  host: `proxy-${id}.example.com`,
  port: 8080,
  username: null,
  status: 'active',
  expires_at: null,
  fallback_mode: 'none',
  expiry_warn_days: 7,
  created_at: '2026-07-20T00:00:00Z',
  updated_at: '2026-07-20T00:00:00Z',
  ...overrides,
} as Proxy)

describe('account proxy scope', () => {
  const proxies = [
    proxy(1, null),
    proxy(2, null, { is_public: true }),
    proxy(3, 7),
    proxy(4, 8),
    proxy(5, 8, { is_public: true }),
  ]

  it('only exposes system proxies to system accounts', () => {
    expect(filterAccountCompatibleProxies(proxies, null).map(item => item.id)).toEqual([1, 2])
  })

  it('only exposes owned and public proxies to user accounts', () => {
    expect(filterAccountCompatibleProxies(proxies, 7).map(item => item.id)).toEqual([2, 3, 5])
  })

  it('only exposes system proxies when an administrator creates a system account', () => {
    expect(proxiesForAccountScope(proxies, 'admin', null).map(item => item.id)).toEqual([1, 2])
  })

  it('uses the account owner when an administrator edits a user account', () => {
    expect(proxiesForAccountScope(proxies, 'admin', 7).map(item => item.id)).toEqual([2, 3, 5])
  })

  it('uses the current user owner when selecting user proxies', () => {
    expect(proxiesForAccountScope(proxies, 'user', 7).map(item => item.id)).toEqual([2, 3, 5])
  })

  it('excludes disabled and expired proxies from the user scope', () => {
    const candidates = [
      proxy(10, 7),
      proxy(11, 7, { status: 'disabled' }),
      proxy(12, 7, { expires_at: '2000-01-01T00:00:00Z' }),
      proxy(13, null, { is_public: true, status: 'inactive' }),
      proxy(14, null, { is_public: true, expires_at: '2099-01-01T00:00:00Z' }),
    ]

    expect(proxiesForAccountScope(candidates, 'user', 7).map(item => item.id)).toEqual([10, 14])
    expect(isUserSelectableProxy(candidates[1])).toBe(false)
    expect(isUserSelectableProxy(candidates[2])).toBe(false)
  })

  it('normalizes account timestamps for the user API', () => {
    expect(toUnixSeconds('2026-07-19T12:00:00.000Z')).toBe(1784462400)
    expect(toUserAccountPayload({ expires_at: 1784462400, proxy_id: 0 })).toEqual({
      expires_at: '2026-07-19T12:00:00.000Z',
      proxy_id: null,
    })
  })
})
