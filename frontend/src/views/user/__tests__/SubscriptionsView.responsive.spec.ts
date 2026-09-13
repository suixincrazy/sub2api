import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(process.cwd(), 'src/views/user/SubscriptionsView.vue'), 'utf8')

describe('SubscriptionsView responsive subscription cards', () => {
  it('wraps header actions on narrow screens and keeps cards within the viewport', () => {
    expect(source).toContain('flex flex-col gap-3 border-b')
    expect(source).toContain('sm:flex-row sm:items-start sm:justify-between')
    expect(source).toContain('flex-wrap')
    expect(source).toContain('rounded-lg')
    expect(source).not.toContain('min-w-[')
  })

  it('localizes creator, pool health, and unsubscribe interactions', () => {
    for (const key of [
      'userSubscriptions.creator',
      'userSubscriptions.poolHealth',
      'userSubscriptions.unsubscribe',
      'userSubscriptions.unsubscribeConfirm',
      'userSubscriptions.unsubscribeSuccess',
      'userSubscriptions.unsubscribeFailed',
    ]) {
      expect(source).toContain(`'${key}'`)
    }
    expect(source).not.toMatch(/[\p{Script=Han}]/u)
  })
})
