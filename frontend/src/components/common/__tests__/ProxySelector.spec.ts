import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import ProxySelector from '../ProxySelector.vue'
import type { Proxy } from '@/types'

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key === 'myResources.states.publicDetailsHidden'
        ? 'Public proxy hidden'
        : key
    })
  }
})

const hiddenPublicProxy = {
  id: 2,
  owner_user_id: null,
  is_public: true,
  is_owned: false,
  details_hidden: true,
  name: 'Shared proxy',
  kind: 'standard',
  protocol: 'http',
  status: 'active',
  expires_at: null,
  fallback_mode: 'none',
  expiry_warn_days: 7,
  created_at: '2026-07-20T00:00:00Z',
  updated_at: '2026-07-20T00:00:00Z'
} as unknown as Proxy

const mountSelector = (props: Record<string, unknown>) => mount(ProxySelector, {
  props: props as any,
  global: {
    stubs: {
      Icon: true,
      Transition: false
    }
  }
})

describe('ProxySelector', () => {
  it('uses a privacy label instead of rendering an empty public endpoint', async () => {
    const wrapper = mountSelector({
      modelValue: hiddenPublicProxy.id,
      proxies: [hiddenPublicProxy]
    })

    expect(wrapper.get('.select-trigger').text()).toContain('Public proxy hidden')
    expect(wrapper.get('.select-trigger').text()).not.toContain('http://:')

    await wrapper.get('.select-trigger').trigger('click')

    expect(wrapper.get('.select-options').text()).toContain('Public proxy hidden')
    expect(wrapper.get('.select-options').text()).not.toContain('http://:')
  })

  it('searches a redacted public proxy with endpoint and auth fields omitted', async () => {
    const wrapper = mountSelector({
      modelValue: null,
      proxies: [hiddenPublicProxy]
    })

    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('.select-search-input').setValue('shared')

    const options = wrapper.get('.select-options').text()
    expect(options).toContain('Shared proxy')
    expect(options).toContain('Public proxy hidden')
    expect(options).not.toContain('undefined')
  })

  it('limits batch proxy tests to four concurrent requests', async () => {
    const proxies = Array.from({ length: 11 }, (_, index) => ({
      ...hiddenPublicProxy,
      id: index + 1,
      name: `Proxy ${index + 1}`
    }))
    let active = 0
    let maxActive = 0
    let releaseFirstWave!: () => void
    const firstWave = new Promise<void>(resolve => {
      releaseFirstWave = resolve
    })
    const testProxy = vi.fn(async () => {
      active += 1
      maxActive = Math.max(maxActive, active)
      if (testProxy.mock.calls.length <= 4) {
        await firstWave
      }
      active -= 1
      return { success: true, message: 'ok' }
    })
    const wrapper = mountSelector({ modelValue: null, proxies, testProxy })

    await wrapper.get('.select-trigger').trigger('click')
    await wrapper.get('.batch-test-btn').trigger('click')

    try {
      await vi.waitFor(() => expect(testProxy).toHaveBeenCalledTimes(4))
      expect(maxActive).toBe(4)
    } finally {
      releaseFirstWave()
    }

    await vi.waitFor(() => expect(testProxy).toHaveBeenCalledTimes(proxies.length))
    await vi.waitFor(() => expect(active).toBe(0))
    expect(maxActive).toBeLessThanOrEqual(4)
  })
})
