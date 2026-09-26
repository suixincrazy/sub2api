import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ScheduledTestsPanel from '../ScheduledTestsPanel.vue'
import type { ScheduledTestPlan } from '@/types'

const mocks = vi.hoisted(() => ({
  listByAccount: vi.fn(), listResults: vi.fn(), create: vi.fn(), update: vi.fn(), remove: vi.fn(),
  showError: vi.fn(), showSuccess: vi.fn()
}))
vi.mock('@/api/admin', () => ({ adminAPI: { scheduledTests: mocks } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => mocks }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const plan: ScheduledTestPlan = {
  id: 1, account_id: 8, model_id: 'gpt-6-astra', cron_expression: '', enabled: true,
  probe_interval_seconds: 2, keepalive_interval_seconds: 60, keepalive_max_interval_seconds: 90,
  max_results: 100, auto_recover: false, last_status: 'success', consecutive_failures: 0,
  last_run_at: null, next_run_at: null, created_at: '', updated_at: ''
}
const wrappers: ReturnType<typeof mount>[] = []
async function open() {
  const wrapper = mount(ScheduledTestsPanel, {
    props: { show: true, accountId: 8, modelOptions: [{ value: 'gpt-6-astra', label: 'gpt-6-astra' }] },
    global: { stubs: {
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' },
      ConfirmDialog: true,
      HelpTooltip: { template: '<div />' },
      Select: { props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option value="" /><option v-for="option in options" :value="option.value">{{ option.label }}</option></select>' },
      Icon: true
    } }
  })
  wrappers.push(wrapper)
  await flushPromises()
  return wrapper
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.clearAllMocks()
  mocks.listByAccount.mockResolvedValue([])
  mocks.listResults.mockResolvedValue([])
  mocks.create.mockResolvedValue(plan)
  mocks.update.mockResolvedValue({ ...plan, enabled: false })
})
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()); vi.useRealTimers() })

describe('Scheduled keepalive panel', () => {
  it('creates an immediate keepalive plan without a cron expression', async () => {
    const wrapper = await open()
    await wrapper.findAll('button').find(button => button.text() === 'admin.scheduledTests.addPlan')!.trigger('click')
    await wrapper.find('select').setValue('gpt-6-astra')
    expect(wrapper.findAll('input[type=number]').map(input => (input.element as HTMLInputElement).value)).toEqual(['2', '60', '90', '100'])
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    await flushPromises()
    expect(mocks.create).toHaveBeenCalledWith({
      account_id: 8, model_id: 'gpt-6-astra', enabled: true, auto_recover: false, max_results: 100,
      probe_interval_seconds: 2, keepalive_interval_seconds: 60, keepalive_max_interval_seconds: 90
    })
  })

  it('rejects invalid interval ranges before calling the API', async () => {
    const wrapper = await open()
    await wrapper.findAll('button').find(button => button.text() === 'admin.scheduledTests.addPlan')!.trigger('click')
    await wrapper.find('select').setValue('gpt-6-astra')
    await wrapper.findAll('input[type=number]')[2].setValue('10')
    const save = wrapper.findAll('button').find(button => button.text() === 'common.save')!
    expect(save.attributes('disabled')).toBeDefined()
    await save.trigger('click')
    expect(mocks.create).not.toHaveBeenCalled()
  })

  it('shows keepalive state and pauses the selected plan', async () => {
    mocks.listByAccount.mockResolvedValue([plan])
    const wrapper = await open()
    expect(wrapper.text()).toContain('admin.scheduledTests.keepingAlive')
    await wrapper.find('[role=switch]').trigger('click')
    await flushPromises()
    expect(mocks.update).toHaveBeenCalledWith(1, { enabled: false })
    expect(wrapper.text()).toContain('admin.scheduledTests.paused')
  })

  it('refreshes while open and stops polling when closed', async () => {
    const wrapper = await open()
    expect(mocks.listByAccount).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(3000)
    expect(mocks.listByAccount).toHaveBeenCalledTimes(2)
    await wrapper.setProps({ show: false })
    await vi.advanceTimersByTimeAsync(9000)
    expect(mocks.listByAccount).toHaveBeenCalledTimes(2)
  })

  it('does not display results from an account that was closed while loading', async () => {
    let resolve: (plans: ScheduledTestPlan[]) => void = () => {}
    mocks.listByAccount.mockImplementationOnce(() => new Promise< ScheduledTestPlan[]>(done => { resolve = done }))
    const wrapper = await open()
    await wrapper.setProps({ show: false, accountId: null })
    resolve([plan])
    await flushPromises()
    mocks.listByAccount.mockResolvedValue([])
    await wrapper.setProps({ show: true, accountId: 9 })
    await flushPromises()
    expect(wrapper.text()).not.toContain('gpt-6-astra')
  })
})
