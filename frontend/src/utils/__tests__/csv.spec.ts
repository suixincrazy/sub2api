import { describe, expect, it } from 'vitest'

import { createCSVContent, escapeCSVValue } from '../csv'

describe('CSV utilities', () => {
  it('quotes delimiters, quotes, and newlines', () => {
    expect(escapeCSVValue('one,two')).toBe('"one,two"')
    expect(escapeCSVValue('say "hello"')).toBe('"say ""hello"""')
    expect(escapeCSVValue('line 1\nline 2')).toBe('"line 1\nline 2"')
  })

  it.each(['=1+1', '+cmd', '-2+3', '@SUM(A1:A2)', '\tformula', '\rformula'])(
    'neutralizes spreadsheet formula prefix %j',
    (value) => {
      expect(escapeCSVValue(value)).toBe(`"'${value}"`)
    },
  )

  it('creates an Excel-compatible UTF-8 CSV document', () => {
    expect(createCSVContent([
      ['Name', 'Value'],
      ['demo', 42],
    ])).toBe('\uFEFFName,Value\ndemo,42')
  })
})
