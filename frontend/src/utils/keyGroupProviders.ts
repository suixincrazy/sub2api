import type { GroupPlatform } from '@/types'

export type KeyGroupProvider = 'anthropic' | 'openai' | 'domestic' | 'other'

export const KEY_GROUP_PROVIDERS = ['anthropic', 'openai', 'domestic', 'other'] as const

// Classify by the configured upstream platform, never by a group's display name.
const PROVIDER_BY_PLATFORM: Record<GroupPlatform, KeyGroupProvider> = {
  anthropic: 'anthropic',
  openai: 'openai',
  kimi: 'domestic',
  zhipu: 'domestic',
  deepseek: 'domestic',
  minimax: 'domestic',
  gemini: 'other',
  grok: 'other',
  antigravity: 'other',
  composite: 'other',
  opencode_go: 'other',
  typesafe: 'other',
  command_code: 'other',
  cline: 'other'
}

export function getKeyGroupProvider(platform: GroupPlatform): KeyGroupProvider {
  return PROVIDER_BY_PLATFORM[platform] ?? 'other'
}

// Composite groups route Claude and GPT requests alike, so they must be pickable from those tabs too.
const COMPOSITE_EXTRA_PROVIDERS: readonly KeyGroupProvider[] = ['anthropic', 'openai']

export function keyGroupMatchesProvider(platform: GroupPlatform, provider: KeyGroupProvider): boolean {
  if (getKeyGroupProvider(platform) === provider) return true
  return platform === 'composite' && COMPOSITE_EXTRA_PROVIDERS.includes(provider)
}

// Collections use representative provider marks rather than an invented brand logo.
export const KEY_GROUP_PROVIDER_ICONS: Record<KeyGroupProvider, GroupPlatform[]> = {
  anthropic: ['anthropic'],
  openai: ['openai'],
  domestic: ['deepseek', 'kimi'],
  other: ['gemini', 'grok']
}
