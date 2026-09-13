export const USER_ACCOUNT_TYPE_OPTIONS = [
  { value: 'oauth', label: 'oauth' },
  { value: 'apikey', label: 'api credential' },
  { value: 'setup-token', label: 'setup credential' },
  { value: 'service_account', label: 'service_account' },
] as const

export function getUserAccountTypeOptions(platform: string) {
  const allowed = ['kimi', 'zhipu', 'deepseek'].includes(platform)
    ? ['apikey']
    : platform === 'anthropic'
    ? ['oauth', 'apikey', 'setup-token', 'service_account']
    : platform === 'gemini'
      ? ['oauth', 'apikey', 'service_account']
      : ['oauth', 'apikey']
  return USER_ACCOUNT_TYPE_OPTIONS.filter(option => allowed.includes(option.value))
}

export const USER_ACCOUNT_STATUS_OPTIONS = [
  { value: 'active', label: 'active' },
  { value: 'disabled', label: 'disabled' },
  { value: 'error', label: 'error' },
] as const

export const USER_GROUP_SUBSCRIPTION_TYPE_OPTIONS = [
  { value: 'standard', label: 'standard' },
  { value: 'subscription', label: 'subscription' },
] as const

export const USER_GROUP_STATUS_OPTIONS = [
  { value: 'active', label: 'active' },
  { value: 'disabled', label: 'disabled' },
] as const
