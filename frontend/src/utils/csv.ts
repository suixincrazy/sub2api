const CSV_FORMULA_PREFIX = /^[=+\-@\t\r]/

export const escapeCSVValue = (value: unknown): string => {
  if (value == null) return ''

  const stringValue = String(value)
  const escapedValue = stringValue.replace(/"/g, '""')

  // Spreadsheet applications may execute cells beginning with these characters.
  if (CSV_FORMULA_PREFIX.test(stringValue)) return `"\'${escapedValue}"`
  if (/[,"\n\r]/.test(stringValue)) return `"${escapedValue}"`
  return stringValue
}

export const createCSVContent = (rows: readonly (readonly unknown[])[]): string => {
  const content = rows
    .map((row) => row.map(escapeCSVValue).join(','))
    .join('\n')

  return `\uFEFF${content}`
}
