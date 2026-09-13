import { describe, expect, it } from 'vitest'

import {
  USER_ACCOUNT_STATUS_OPTIONS,
  USER_ACCOUNT_TYPE_OPTIONS,
  USER_GROUP_STATUS_OPTIONS,
  USER_GROUP_SUBSCRIPTION_TYPE_OPTIONS,
  getUserAccountTypeOptions,
} from '@/utils/userResourceOptions'

describe('user resource account options', () => {
  it('uses only canonical backend account types', () => {
    expect(USER_ACCOUNT_TYPE_OPTIONS.map(option => option.value)).toEqual([
      'oauth',
      'apikey',
      'setup-token',
      'service_account',
    ])
  })

  it('uses only account statuses accepted by the service', () => {
    expect(USER_ACCOUNT_STATUS_OPTIONS.map(option => option.value)).toEqual(['active', 'disabled', 'error'])
  })

  it('limits account types to combinations supported by each platform', () => {
    expect(getUserAccountTypeOptions('anthropic').map(option => option.value)).toEqual(['oauth', 'apikey', 'setup-token', 'service_account'])
    expect(getUserAccountTypeOptions('gemini').map(option => option.value)).toEqual(['oauth', 'apikey', 'service_account'])
    expect(getUserAccountTypeOptions('openai').map(option => option.value)).toEqual(['oauth', 'apikey'])
    expect(getUserAccountTypeOptions('kimi').map(option => option.value)).toEqual(['apikey'])
    expect(getUserAccountTypeOptions('zhipu').map(option => option.value)).toEqual(['apikey'])
    expect(getUserAccountTypeOptions('deepseek').map(option => option.value)).toEqual(['apikey'])
  })

  it('uses only group values accepted by the service', () => {
    expect(USER_GROUP_SUBSCRIPTION_TYPE_OPTIONS.map(option => option.value)).toEqual(['standard', 'subscription'])
    expect(USER_GROUP_STATUS_OPTIONS.map(option => option.value)).toEqual(['active', 'disabled'])
  })
})
