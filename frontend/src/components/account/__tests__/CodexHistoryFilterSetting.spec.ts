import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CodexHistoryFilterSetting from '../CodexHistoryFilterSetting.vue'

const api = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { settings: { getCodexHistoryFilter: api.get } } }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

describe('CodexHistoryFilterSetting', () => {
  beforeEach(() => {
    api.get.mockReset().mockResolvedValue({ enabled: true })
  })

  it('shows the gateway default without creating an account override', async () => {
    const wrapper = mount(CodexHistoryFilterSetting, { props: { modelValue: null } })
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
    await wrapper.get('[role="switch"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[false]])
  })

  it('keeps an explicit disabled override and can restore inheritance', async () => {
    const wrapper = mount(CodexHistoryFilterSetting, { props: { modelValue: false } })
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('aria-checked')).toBe('false')
    await wrapper.get('[data-testid="codex-history-filter-inherit"]').trigger('click')
    expect(wrapper.emitted('update:modelValue')).toEqual([[null]])
  })

  it('does not guess or persist a default when the gateway setting cannot load', async () => {
    api.get.mockRejectedValue(new Error('unavailable'))
    const wrapper = mount(CodexHistoryFilterSetting, { props: { modelValue: null } })
    await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('admin.accounts.openai.historyFilterLoadError')
    expect(wrapper.emitted('update:modelValue')).toBeUndefined()
  })
})
