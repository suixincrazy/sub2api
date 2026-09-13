import { h, nextTick } from 'vue'
import { mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'
import BaseDialog from '../BaseDialog.vue'

describe('BaseDialog', () => {
  afterEach(() => {
    vi.restoreAllMocks()
    document.body.innerHTML = ''
    document.body.classList.remove('modal-open')
  })

  it('focuses the first form control after opening', async () => {
    const requestAnimationFrameSpy = vi.spyOn(window, 'requestAnimationFrame').mockImplementation(callback => (
      window.setTimeout(() => callback(0), 0)
    ))
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(handle => window.clearTimeout(handle))

    const wrapper = mount(BaseDialog, {
      attachTo: document.body,
      props: { show: false, title: 'Editor' },
      slots: {
        default: () => h('input', { 'data-test': 'dialog-input' })
      }
    })

    await wrapper.setProps({ show: true })
    await nextTick()
    expect(document.querySelector('[data-test="dialog-input"]')).not.toBeNull()
    await vi.waitFor(() => expect(requestAnimationFrameSpy).toHaveBeenCalled())
    await vi.waitFor(() => {
      expect(document.activeElement?.getAttribute('data-test')).toBe('dialog-input')
    })
    wrapper.unmount()
  })

  it('resets body scroll position when reopened', async () => {
    vi.spyOn(window, 'requestAnimationFrame').mockImplementation(callback => (
      window.setTimeout(() => callback(0), 0)
    ))
    vi.spyOn(window, 'cancelAnimationFrame').mockImplementation(handle => window.clearTimeout(handle))

    const wrapper = mount(BaseDialog, {
      attachTo: document.body,
      props: { show: false, title: 'Details' },
      slots: { default: '<div style="height: 2000px">content</div>' },
      global: { stubs: { Icon: true } }
    })

    await wrapper.setProps({ show: true })
    await nextTick()
    const body = document.body.querySelector<HTMLElement>('.modal-body')
    expect(body).not.toBeNull()
    body!.scrollTop = 480

    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    await nextTick()

    expect(document.body.querySelector<HTMLElement>('.modal-body')?.scrollTop).toBe(0)
    wrapper.unmount()
  })
})
