import { mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GroupSelector from '../GroupSelector.vue'
import type { AdminGroup } from '@/types'

const authState = { isSimpleMode: false }

vi.mock('@/stores', () => ({ useAuthStore: () => authState }))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const groups = [
  { id: 1, name: 'Basic', platform: 'anthropic', status: 'active' },
  { id: 2, name: 'Composite', platform: 'composite', status: 'active' }
] as any

const mountSelector = (modelValue: number[] = []) => mount(GroupSelector, {
  props: { modelValue, groups },
  global: { stubs: { GroupBadge: { props: ['name'], template: '<span>{{ name }}</span>' }, Icon: true } }
})

describe('GroupSelector simple-mode binding policy', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  it('hides composite groups in simple mode and preserves basic groups', () => {
    authState.isSimpleMode = true
    const wrapper = mountSelector()
    expect(wrapper.text()).toContain('Basic')
    expect(wrapper.text()).not.toContain('Composite')
  })

  it('keeps composite groups available in advanced mode', () => {
    const wrapper = mountSelector()
    expect(wrapper.text()).toContain('Composite')
  })

  it('cleans hidden historical composite IDs while preserving visible selections', () => {
    authState.isSimpleMode = true
    const wrapper = mountSelector([1, 2])
    expect(wrapper.emitted('update:modelValue')).toEqual([[[1]]])
  })
})

const group = (id: number, ownerUserId: number | null | undefined, name: string) => ({
  id,
  ...(ownerUserId === undefined ? {} : { owner_user_id: ownerUserId }),
  name,
  description: null,
  platform: 'openai',
  rate_multiplier: 1,
  status: 'active',
  subscription_type: 'standard',
  account_count: 0,
} as AdminGroup)

const ownerGroups = [
  group(1, null, 'Explicit system group'),
  group(4, undefined, 'Implicit system group'),
  group(2, 7, 'User 7 group'),
  group(3, 8, 'User 8 group'),
]

const mountOwnerSelector = (ownerUserId: number | null | undefined) => mount(GroupSelector, {
  props: {
    modelValue: [],
    groups: ownerGroups,
    platform: 'openai',
    enforceOwner: true,
    ownerUserId,
  },
  global: {
    stubs: {
      GroupBadge: {
        props: ['name'],
        template: '<span>{{ name }}</span>',
      },
      Icon: true,
    },
  },
})

describe('GroupSelector owner scope', () => {
  beforeEach(() => { authState.isSimpleMode = false })

  it('treats a missing group owner as system-owned when ownerUserId is null', () => {
    const text = mountOwnerSelector(null).text()
    expect(text).toContain('Explicit system group')
    expect(text).toContain('Implicit system group')
    expect(text).not.toContain('User 7 group')
    expect(text).not.toContain('User 8 group')
  })

  it('treats an undefined ownerUserId as the system owner', () => {
    const text = mountOwnerSelector(undefined).text()
    expect(text).toContain('Explicit system group')
    expect(text).toContain('Implicit system group')
    expect(text).not.toContain('User 7 group')
    expect(text).not.toContain('User 8 group')
  })

  it('keeps groups with a different private owner hidden', () => {
    const text = mountOwnerSelector(7).text()
    expect(text).toContain('User 7 group')
    expect(text).not.toContain('Explicit system group')
    expect(text).not.toContain('Implicit system group')
    expect(text).not.toContain('User 8 group')
  })
})
