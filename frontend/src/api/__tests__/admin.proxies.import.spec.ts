import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get, post, put, del } = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  put: vi.fn(),
  del: vi.fn(),
}))

vi.mock('@/api/client', () => ({
  apiClient: {
    get,
    post,
    put,
    delete: del,
  },
}))

import {
  createSource,
  deleteSource,
  getAdminProxyImportCount,
  importNodes,
  listSources,
  syncSource,
  updateSource,
} from '@/api/admin/proxies'

describe('admin proxy import and source APIs', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    get.mockResolvedValue({ data: { items: [], total: 0, page: 1, page_size: 20 } })
    post.mockResolvedValue({ data: { id: 7, created_count: 1 } })
    put.mockResolvedValue({ data: { id: 7 } })
    del.mockResolvedValue({ data: { message: 'success' } })
  })

  it('imports nodes through the administrator endpoint', async () => {
    const payload = {
      name_prefix: 'edge',
      content: 'vless://node',
      is_public: true,
    }

    await importNodes(payload)

    expect(post).toHaveBeenCalledWith('/admin/proxies/import', payload)
  })

  it('uses administrator-only endpoints for subscription source CRUD and sync', async () => {
    const payload = {
      name: 'provider',
      subscription_url: 'https://example.com/subscription',
      refresh_interval_minutes: 60,
      is_public: false,
    }

    await listSources(2, 50)
    await createSource(payload)
    await updateSource(7, payload)
    await syncSource(7)
    await deleteSource(7)

    expect(get).toHaveBeenCalledWith('/admin/proxies/sources', {
      params: { page: 2, page_size: 50 },
    })
    expect(post).toHaveBeenNthCalledWith(1, '/admin/proxies/sources', payload)
    expect(put).toHaveBeenCalledWith('/admin/proxies/sources/7', payload)
    expect(post).toHaveBeenNthCalledWith(2, '/admin/proxies/sources/7/sync')
    expect(del).toHaveBeenCalledWith('/admin/proxies/sources/7')

    const requestedPaths = [
      ...get.mock.calls.map(([path]) => path),
      ...post.mock.calls.map(([path]) => path),
      ...put.mock.calls.map(([path]) => path),
      ...del.mock.calls.map(([path]) => path),
    ]
    expect(requestedPaths).toHaveLength(5)
    expect(requestedPaths.every(path => String(path).startsWith('/admin/'))).toBe(true)
    expect(requestedPaths.some(path => String(path).startsWith('/my/'))).toBe(false)
  })

  it('normalizes subscription sync and legacy import counts', () => {
    expect(getAdminProxyImportCount({
      imported_count: 1,
      created_count: 0,
      updated_count: 9,
    })).toBe(1)
    expect(getAdminProxyImportCount({ created_count: 2, updated_count: 3 })).toBe(5)
    expect(getAdminProxyImportCount({ created: [{ id: 1 }, { id: 2 }] as never[] })).toBe(2)
    expect(getAdminProxyImportCount({
      created: [{ id: 1 }] as never[],
      updated: [{ id: 2 }, { id: 3 }] as never[],
    })).toBe(3)
  })
})
