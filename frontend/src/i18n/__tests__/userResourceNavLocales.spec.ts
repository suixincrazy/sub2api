import { describe, expect, it } from 'vitest'

import en from '../locales/en'
import zh from '../locales/zh'

describe('user resource navigation locales', () => {
  it('defines nav labels for every user-resource menu item in zh and en', () => {
    const expected = [
      'myGroups',
      'myAccounts',
      'myProxies',
      'assignSubscriptions',
      'myRedeemCodes',
      'myAccountUsage',
      'myUpstreamErrors',
    ]

    for (const messages of [zh, en]) {
      for (const key of expected) {
        expect(messages.nav[key]).toEqual(expect.any(String))
        expect(messages.nav[key].trim()).not.toBe('')
      }
    }
  })
})
