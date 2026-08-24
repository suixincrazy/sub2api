import { shallowMount } from '@vue/test-utils'
import type { VueWrapper } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import PricingEntryCard from '../PricingEntryCard.vue'
import { derivedMTokPrice, isValidNonNegativeMultiplier } from '../types'
import type { PricingFormEntry } from '../types'

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

function createEntry(overrides: Partial<PricingFormEntry> = {}): PricingFormEntry {
  return {
    models: [],
    billing_mode: 'token',
    input_price: null,
    output_price: null,
    cache_write_price: null,
    cache_read_price: null,
    completion_multiplier: null,
    cache_creation_multiplier: null,
    cache_read_multiplier: null,
    fast_multiplier: null,
    flex_multiplier: null,
    image_input_price: null,
    image_output_price: null,
    per_request_price: null,
    intervals: [],
    time_pricing: { timezone: 'Asia/Shanghai', periods: [] },
    ...overrides,
  }
}

// 派生价预览是那段 $/MTok 说明文字；没有任何档位派生时整段不渲染，此时返回 null。
function previewText(wrapper: VueWrapper): string | null {
  const p = wrapper.findAll('p').find(el => el.text().includes('$/MTok'))
  return p ? p.text().replace(/\s+/g, ' ').trim() : null
}

// 运营口径：提示价 $4/MTok + 补全 5 / 缓存创建 1.25 / 缓存读取 0.2
// => 补全 $20、缓存创建 $5、缓存读取 $0.80
const ratioEntry = () => createEntry({
  input_price: 4,
  completion_multiplier: 5,
  cache_creation_multiplier: 1.25,
  cache_read_multiplier: 0.2,
})

describe('derivedMTokPrice', () => {
  it('multiplies the prompt price by the ratio', () => {
    expect(derivedMTokPrice(4, null, 5)).toBe(20)
    expect(derivedMTokPrice(4, null, 1.25)).toBe(5)
    expect(derivedMTokPrice(4, null, 0.2)).toBe(0.8)
  })

  it('yields to an absolute price on that tier', () => {
    expect(derivedMTokPrice(4, 30, 5)).toBeNull()
    // 显式填 0 也算配置过，不能被倍率顶掉。
    expect(derivedMTokPrice(4, 0, 5)).toBeNull()
  })

  it('does not derive without a ratio or a usable prompt price', () => {
    expect(derivedMTokPrice(4, null, null)).toBeNull()
    expect(derivedMTokPrice(null, null, 5)).toBeNull()
    expect(derivedMTokPrice(0, null, 5)).toBeNull()
    expect(derivedMTokPrice('', null, 5)).toBeNull()
  })

  it('keeps ratios of 0 (giving a tier away for free)', () => {
    expect(derivedMTokPrice(4, null, 0)).toBe(0)
  })
})

describe('isValidNonNegativeMultiplier', () => {
  it('accepts empty, 0 and positives, rejects negatives', () => {
    expect(isValidNonNegativeMultiplier(null)).toBe(true)
    expect(isValidNonNegativeMultiplier('')).toBe(true)
    expect(isValidNonNegativeMultiplier(0)).toBe(true)
    expect(isValidNonNegativeMultiplier(1.25)).toBe(true)
    expect(isValidNonNegativeMultiplier(-0.1)).toBe(false)
  })
})

describe('PricingEntryCard derived multipliers', () => {
  it('shows the ratio inputs only when channel pricing is being edited', () => {
    // 账号成本统计那条路径后端会丢掉倍率（allowChannelMultipliers=false），
    // 所以那里的卡片不能出现这些输入框，否则"存了却不生效"。
    const hidden = shallowMount(PricingEntryCard, { props: { entry: ratioEntry() } })
    expect(hidden.text()).not.toContain('admin.channels.form.derivedMultipliers')

    const shown = shallowMount(PricingEntryCard, {
      props: { entry: ratioEntry(), enableTierMultipliers: true },
    })
    expect(shown.text()).toContain('admin.channels.form.derivedCompletionMultiplier')
    expect(shown.text()).toContain('admin.channels.form.derivedCacheCreationMultiplier')
    expect(shown.text()).toContain('admin.channels.form.derivedCacheReadMultiplier')
  })

  it('previews the derived prices', () => {
    const wrapper = shallowMount(PricingEntryCard, {
      props: { entry: ratioEntry(), enableTierMultipliers: true },
    })

    expect(previewText(wrapper)).toBe(
      'admin.channels.form.outputPrice $20 · ' +
      'admin.channels.form.cacheWritePrice $5 · ' +
      'admin.channels.form.cacheReadPrice $0.8 $/MTok',
    )
  })

  it('omits tiers that carry an absolute price from the preview', () => {
    const wrapper = shallowMount(PricingEntryCard, {
      props: {
        entry: createEntry({
          input_price: 4,
          output_price: 30,
          completion_multiplier: 5,
          cache_creation_multiplier: 1.25,
        }),
        enableTierMultipliers: true,
      },
    })

    expect(previewText(wrapper)).toBe('admin.channels.form.cacheWritePrice $5 $/MTok')
  })

  it('hides the preview entirely when nothing derives', () => {
    const wrapper = shallowMount(PricingEntryCard, {
      props: { entry: createEntry({ completion_multiplier: 5 }), enableTierMultipliers: true },
    })

    expect(previewText(wrapper)).toBeNull()
  })

  it('emits the ratio field on input', async () => {
    const entry = createEntry()
    const wrapper = shallowMount(PricingEntryCard, {
      props: { entry, enableTierMultipliers: true },
    })

    const input = wrapper.findAll('input[type="number"]').find(
      el => el.attributes('placeholder') === 'admin.channels.form.multiplierPlaceholder',
    )
    expect(input).toBeDefined()
    await input!.setValue('5')

    expect(wrapper.emitted('update')?.[0]?.[0]).toMatchObject({ completion_multiplier: '5' })
  })
})
