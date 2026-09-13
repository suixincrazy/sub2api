import { shallowMount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import SharedAccountTestModal from '@/components/account/AccountTestModal.vue'
import AccountTestModal from '../AccountTestModal.vue'

describe('admin AccountTestModal', () => {
  it('delegates to the shared prompt-capable modal with admin scope', async () => {
    const account = {
      id: 42,
      name: 'Admin API account',
      platform: 'openai',
      type: 'apikey',
      status: 'active'
    } as any

    const wrapper = shallowMount(AccountTestModal, {
      props: {
        show: true,
        account
      }
    })

    const sharedModal = wrapper.findComponent(SharedAccountTestModal)
    expect(sharedModal.exists()).toBe(true)
    expect(sharedModal.props()).toMatchObject({
      show: true,
      account,
      scope: 'admin'
    })

    await sharedModal.vm.$emit('close')
    expect(wrapper.emitted('close')).toHaveLength(1)
  })
})
