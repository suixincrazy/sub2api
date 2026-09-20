import { beforeEach, describe, expect, it, vi } from 'vitest'
const client = vi.hoisted(() => ({ get: vi.fn(), put: vi.fn() }))
vi.mock('@/api/client', () => ({ default: client, apiClient: client }))
import { getCodexHistoryFilter, updateCodexHistoryFilter } from '@/api/admin/settings'

describe('Codex history filter API', () => {
  beforeEach(() => { vi.clearAllMocks() })
  it('reads status from the authenticated settings endpoint', async () => {
    const value = { enabled: true, filter_version: 2, stats: { filtered_requests: 4 } }
    client.get.mockResolvedValue({ data: value })
    expect(await getCodexHistoryFilter()).toBe(value)
    expect(client.get).toHaveBeenCalledWith('/admin/settings/codex-history-filter')
  })
  it('preserves an explicit false value when disabling', async () => {
    const value = { enabled: false }
    client.put.mockResolvedValue({ data: value })
    expect(await updateCodexHistoryFilter(false)).toBe(value)
    expect(client.put).toHaveBeenCalledWith('/admin/settings/codex-history-filter', { enabled: false })
  })
})
