import { beforeEach, describe, expect, it, vi } from 'vitest'

const {
  adminGenerateMock,
  adminExchangeMock,
  adminCapabilitiesMock,
  userGenerateMock,
  userExchangeMock,
  userCapabilitiesMock
} = vi.hoisted(() => ({
  adminGenerateMock: vi.fn(),
  adminExchangeMock: vi.fn(),
  adminCapabilitiesMock: vi.fn(),
  userGenerateMock: vi.fn(),
  userExchangeMock: vi.fn(),
  userCapabilitiesMock: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    gemini: {
      generateAuthUrl: adminGenerateMock,
      exchangeCode: adminExchangeMock,
      getCapabilities: adminCapabilitiesMock
    }
  }
}))

vi.mock('@/api/myResources', () => ({
  myResourcesApi: {
    accounts: {
      oauth: {
        authURL: userGenerateMock,
        exchange: userExchangeMock,
        geminiCapabilities: userCapabilitiesMock
      }
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn() })
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

import { useGeminiOAuth } from '../useGeminiOAuth'

describe('useGeminiOAuth scoped client', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    adminGenerateMock.mockResolvedValue({ auth_url: 'https://admin-auth', session_id: 'admin-session', state: 'admin-state' })
    adminExchangeMock.mockResolvedValue({ access_token: 'admin-token' })
    adminCapabilitiesMock.mockResolvedValue({ ai_studio_oauth_enabled: false, required_redirect_uris: [] })
    userGenerateMock.mockResolvedValue({ auth_url: 'https://user-auth', session_id: 'user-session', state: 'user-state' })
    userExchangeMock.mockResolvedValue({ credentials: { access_token: 'user-token' }, extra: { tier: 'free' } })
    userCapabilitiesMock.mockResolvedValue({ ai_studio_oauth_enabled: true, required_redirect_uris: ['http://localhost'] })
  })

  it('keeps admin behavior as the default', async () => {
    const oauth = useGeminiOAuth()

    await expect(oauth.getCapabilities()).resolves.toEqual({ ai_studio_oauth_enabled: false, required_redirect_uris: [] })
    expect(adminCapabilitiesMock).toHaveBeenCalledTimes(1)
    expect(userCapabilitiesMock).not.toHaveBeenCalled()
  })

  it('routes auth, exchange, and capabilities through /my adapters in user scope', async () => {
    const oauth = useGeminiOAuth({ scope: 'user' })

    await expect(oauth.generateAuthUrl(12, 'project-1', 'ai_studio', 'free')).resolves.toBe(true)
    const token = await oauth.exchangeAuthCode({
      code: 'code-1',
      sessionId: 'user-session',
      state: 'user-state',
      proxyId: 12,
      oauthType: 'ai_studio',
      tierId: 'free'
    })
    await expect(oauth.getCapabilities()).resolves.toEqual({
      ai_studio_oauth_enabled: true,
      required_redirect_uris: ['http://localhost']
    })

    expect(userGenerateMock).toHaveBeenCalledWith(expect.objectContaining({ platform: 'gemini', proxy_id: 12 }))
    expect(userExchangeMock).toHaveBeenCalledWith(expect.objectContaining({ platform: 'gemini', session_id: 'user-session' }))
    expect(token).toEqual({ access_token: 'user-token', extra: { tier: 'free' } })
    expect(adminGenerateMock).not.toHaveBeenCalled()
    expect(adminExchangeMock).not.toHaveBeenCalled()
    expect(adminCapabilitiesMock).not.toHaveBeenCalled()
  })

  it('accepts an explicitly scoped client callback set', async () => {
    const client = {
      generateAuthUrl: vi.fn().mockResolvedValue({ auth_url: 'https://custom', session_id: 'custom-session', state: 'custom-state' }),
      exchangeCode: vi.fn().mockResolvedValue({ access_token: 'custom-token' }),
      getCapabilities: vi.fn().mockResolvedValue({ ai_studio_oauth_enabled: true, required_redirect_uris: [] })
    }
    const oauth = useGeminiOAuth({ client })

    await oauth.generateAuthUrl(undefined)
    await oauth.getCapabilities()

    expect(client.generateAuthUrl).toHaveBeenCalledTimes(1)
    expect(client.getCapabilities).toHaveBeenCalledTimes(1)
    expect(adminGenerateMock).not.toHaveBeenCalled()
  })
})
