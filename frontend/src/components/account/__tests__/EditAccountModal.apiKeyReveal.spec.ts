import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, nextTick } from 'vue'
import { mount } from '@vue/test-utils'

const { updateAccountMock, getCredentialsMock } = vi.hoisted(() => ({
  updateAccountMock: vi.fn(),
  getCredentialsMock: vi.fn()
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    get isSimpleMode() {
      return true
    }
  })
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      update: updateAccountMock,
      getCredentials: getCredentialsMock,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: {
      list: vi.fn().mockResolvedValue([])
    }
  }
}))

vi.mock('@/api/admin/accounts', () => ({
  getAntigravityDefaultModelMapping: vi.fn()
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({
  name: 'BaseDialog',
  props: {
    show: {
      type: Boolean,
      default: false
    }
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>'
})

function buildApiKeyAccount(overrides: Record<string, unknown> = {}) {
  return {
    id: 11,
    name: 'Anthropic Key',
    notes: '',
    platform: 'anthropic',
    type: 'apikey',
    // 后端脱敏后 credentials 里没有 api_key，只有 credentials_status 标记它存在
    credentials: {
      base_url: 'https://api.anthropic.com'
    },
    credentials_status: {
      has_api_key: true
    },
    extra: {},
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    rate_multiplier: 1,
    status: 'active',
    group_ids: [],
    expires_at: null,
    auto_pause_on_expired: false,
    ...overrides
  } as any
}

function mountModal(account: Record<string, unknown>) {
  return mount(EditAccountModal, {
    props: {
      show: true,
      account,
      proxies: [],
      groups: []
    },
    global: {
      stubs: {
        BaseDialog: BaseDialogStub,
        Select: true,
        Icon: true,
        ProxySelector: true,
        GroupSelector: true,
        ModelWhitelistSelector: true
      }
    }
  })
}

function apiKeyInput(wrapper: ReturnType<typeof mountModal>) {
  return wrapper.get('input[autocomplete="new-password"]')
}

describe('EditAccountModal api_key 回填', () => {
  beforeEach(() => {
    getCredentialsMock.mockReset()
    updateAccountMock.mockReset()
    updateAccountMock.mockResolvedValue({})
  })

  it('账号已存 api_key 时拉取原文并回填编辑框', async () => {
    getCredentialsMock.mockResolvedValue({
      id: 11,
      type: 'apikey',
      platform: 'anthropic',
      credentials: { api_key: 'sk-ant-plaintext' }
    })

    const wrapper = mountModal(buildApiKeyAccount())
    await nextTick()
    await nextTick()

    expect(getCredentialsMock).toHaveBeenCalledWith(11)
    expect((apiKeyInput(wrapper).element as HTMLInputElement).value).toBe('sk-ant-plaintext')
  })

  it('账号没有 api_key 时不请求原文，编辑框留空', async () => {
    const wrapper = mountModal(
      buildApiKeyAccount({ credentials_status: { has_api_key: false } })
    )
    await nextTick()
    await nextTick()

    expect(getCredentialsMock).not.toHaveBeenCalled()
    expect((apiKeyInput(wrapper).element as HTMLInputElement).value).toBe('')
  })

  it('取原文失败时保持留空，不阻塞其它字段编辑', async () => {
    getCredentialsMock.mockRejectedValue(new Error('step-up required'))

    const wrapper = mountModal(buildApiKeyAccount())
    await nextTick()
    await nextTick()

    expect((apiKeyInput(wrapper).element as HTMLInputElement).value).toBe('')
    expect(wrapper.find('form#edit-account-form').exists()).toBe(true)
  })

  it('回填后的值原样提交，不会把已存密钥改掉', async () => {
    getCredentialsMock.mockResolvedValue({
      id: 11,
      type: 'apikey',
      platform: 'anthropic',
      credentials: { api_key: 'sk-ant-plaintext' }
    })

    const wrapper = mountModal(buildApiKeyAccount())
    await nextTick()
    await nextTick()

    await wrapper.get('form#edit-account-form').trigger('submit.prevent')
    await nextTick()

    expect(updateAccountMock).toHaveBeenCalledTimes(1)
    expect(updateAccountMock.mock.calls[0]?.[1]?.credentials?.api_key).toBe('sk-ant-plaintext')
  })

  it('默认掩码显示，点击眼睛图标切明文', async () => {
    getCredentialsMock.mockResolvedValue({
      id: 11,
      type: 'apikey',
      platform: 'anthropic',
      credentials: { api_key: 'sk-ant-plaintext' }
    })

    const wrapper = mountModal(buildApiKeyAccount())
    await nextTick()
    await nextTick()

    const input = wrapper.get('input.font-mono[type="password"]')
    await input.element.parentElement!.querySelector('button')!.click()
    await nextTick()

    expect(wrapper.find('input.font-mono[type="password"]').exists()).toBe(false)
    expect(wrapper.find('input.font-mono[type="text"]').exists()).toBe(true)
  })
})
