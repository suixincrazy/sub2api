import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { describe, expect, it } from 'vitest'

const source = readFileSync(resolve(process.cwd(), 'src/views/user/UserAccountsView.vue'), 'utf8')
const accountTestModalSource = readFileSync(resolve(process.cwd(), 'src/components/account/AccountTestModal.vue'), 'utf8')

describe('UserAccountsView user scope and admin-equivalent experience', () => {
  it('uses the account management table experience without admin APIs', () => {
    expect(source).toContain('<TablePageLayout>')
    expect(source).toContain('<DataTable')
    expect(source).toContain("t('admin.accounts.columns.todayStats')")
    expect(source).toContain("t('admin.accounts.columns.usageWindows')")
    expect(source).toContain('myResourcesApi.accounts.list')
    expect(source).not.toContain('adminAPI')
    expect(source).not.toContain("'/admin/")
  })

  it('reuses the original account dialogs in user scope', () => {
    expect(source).toContain('<CreateAccountModal')
    expect(source).toContain('<EditAccountModal')
    expect(source).toContain('<AccountTestModal')
    expect(source.match(/scope="user"/g)).toHaveLength(3)
    expect(source).toContain('<BaseDialog')
  })

  it('opens the original test dialog from the row action without admin endpoints', () => {
    expect(source).toContain('@test="testAccount"')
    expect(source).toContain('accountTestTarget.value = account')
    expect(source).toContain('accountTestOpen.value = true')
    expect(accountTestModalSource).toContain('myResourcesApi.accounts.getAvailableModels')
    expect(accountTestModalSource).toContain("`/my/accounts/${props.account.id}/test/stream`")
  })

  it('keeps account filters full width on mobile', () => {
    expect(source).toContain('w-full min-w-0 basis-full')
    expect(source).toContain('lg:w-auto lg:basis-auto lg:flex-1')
  })

  it('supports the expected user account data and batch operations', () => {
    for (const operation of ['export', 'import', 'importCodexSessions', 'importCodexPAT', 'batchUpdate', 'refresh', 'clearError', 'setSchedulable']) {
      expect(source).toContain(`myResourcesApi.accounts.${operation}`)
    }
  })

  it('loads full account details before opening the shared editor', () => {
    expect(source).toContain('myResourcesApi.accounts.get(Number(row.id))')
    expect(source).toContain('credentials_status: item.credentials_status || {}')
    expect(source).toContain('editingAccount.value = toAccount')
  })

  it('keeps visible copy and operation messages localized', () => {
    expect(source).not.toMatch(/[\p{Script=Han}]/u)
    expect(source).toContain("mr('actions.clearError')")
    expect(source).toContain("t('common.more')")
  })
})
