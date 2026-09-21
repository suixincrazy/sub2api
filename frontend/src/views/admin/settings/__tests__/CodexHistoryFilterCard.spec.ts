import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CodexHistoryFilterCard from '../CodexHistoryFilterCard.vue'
import type { CodexHistoryFilterStatus } from '@/api/admin/settings'

const api = vi.hoisted(() => ({ get: vi.fn(), update: vi.fn() }))
vi.mock('@/api/admin/settings', () => ({ getCodexHistoryFilter: api.get, updateCodexHistoryFilter: api.update }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

const state = (enabled = false, filteredRequests = 12): CodexHistoryFilterStatus => ({
  enabled,
  filter_version: 3,
  stats: { requests: filteredRequests, filtered_requests: filteredRequests,
    removed_reasoning_items: 3, removed_item_ids: 9, normalized_agent_text_parts: 7, blocked_requests: 2,
    upstream_errors: 1, upstream_http_errors: 4,
    last_filtered_at: '2026-09-20T12:00:00Z', last_upstream_status: 200 },
})

describe('CodexHistoryFilterCard', () => {
  beforeEach(() => {
    api.get.mockReset().mockResolvedValue(state())
    api.update.mockReset()
  })

  it('shows the saved setting and process counters', async () => {
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    expect(api.get).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('[role="switch"]').attributes('aria-label')).toBe('admin.settings.codexHistoryFilter.enabled')
    expect(wrapper.text()).toContain('12')
    expect(wrapper.text()).toContain('admin.settings.codexHistoryFilter.normalizedAgentText')
    expect(wrapper.text()).toContain('7')
    expect(wrapper.text()).toContain('admin.settings.codexHistoryFilter.boundaries')
    expect(wrapper.text()).toContain('admin.settings.codexHistoryFilter.statsHint')
  })

  it('enables and disables immediately using the dedicated endpoint', async () => {
    api.update.mockResolvedValueOnce(state(true)).mockResolvedValueOnce(state(false))
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await flushPromises()
    expect(api.update).toHaveBeenLastCalledWith(true)
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    await wrapper.get('[role="switch"]').trigger('click')
    await flushPromises()
    expect(api.update).toHaveBeenLastCalledWith(false)
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
  })

  it('blocks repeated writes until the current save has finished', async () => {
    let finish: ((value: CodexHistoryFilterStatus) => void) | undefined
    api.update.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[role="switch"]').trigger('click')
    expect(api.update).toHaveBeenCalledTimes(1)
    finish?.(state(true))
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeUndefined()
  })

  it('refreshes counters without submitting a setting change', async () => {
    api.get.mockResolvedValueOnce(state()).mockResolvedValueOnce(state(false, 25))
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    await wrapper.get('.btn-secondary').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('25')
    expect(api.update).not.toHaveBeenCalled()
  })

  it('does not claim a failed load means disabled', async () => {
    api.get.mockRejectedValueOnce(new Error('offline'))
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('loadFailed')
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).not.toContain('codexHistoryFilter.inactive')
    await wrapper.get('.btn-secondary').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('reconciles an uncertain save instead of assuming the old value persisted', async () => {
    api.get.mockResolvedValueOnce(state(false)).mockResolvedValueOnce(state(true))
    api.update.mockRejectedValueOnce(new Error('response lost after persistence'))
    const wrapper = mount(CodexHistoryFilterCard)
    await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await flushPromises()
    expect(api.get).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.get('[role="alert"]').text()).toContain('saveFailed')
  })
})
