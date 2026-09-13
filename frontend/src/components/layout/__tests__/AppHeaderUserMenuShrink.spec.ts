import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { describe, expect, it } from 'vitest'

const dir = dirname(fileURLToPath(import.meta.url))
const source = readFileSync(resolve(dir, '../AppHeader.vue'), 'utf8')

// The header user menu used to widen the page by 1px on long admin pages: the
// menu is a flex item, but nothing in the chain allowed it to shrink. Browsers
// also resolve `width: auto` on a <button> to fit-content, so `min-w-0` alone
// leaves the button at its content width and it overflows its own container --
// `max-w-full` is what actually caps it and lets the labels truncate.
describe('AppHeader user menu shrink chain', () => {
  it('lets the user menu container shrink inside the header row', () => {
    expect(source).toContain('<div v-if="user" class="relative min-w-0" ref="dropdownRef">')
  })

  it('caps the menu button at its container width', () => {
    const button = source.match(/class="flex min-w-0 max-w-full items-center gap-2 rounded-xl p-1\.5[^"]*"/)
    expect(button).not.toBeNull()
  })

  it('truncates the display name and role instead of pushing the row wider', () => {
    expect(source).toContain('<div class="hidden min-w-0 text-left md:block">')
    expect(source).toContain('<div class="truncate text-sm font-medium text-gray-900 dark:text-white">')
    expect(source).toContain('<div class="truncate text-xs text-gray-500 dark:text-dark-400">')
  })

  it('keeps the avatar and chevron at their natural size', () => {
    expect(source).toContain('class="flex h-8 w-8 shrink-0 items-center justify-center overflow-hidden rounded-xl')
    expect(source).toContain('<Icon name="chevronDown" size="sm" class="hidden shrink-0 text-gray-400 md:block" />')
  })
})
