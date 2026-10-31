// matrix/hardened: the sidebar's 最近 / Recent view mirrors the TUI's Ctrl+E
// list: every visited session flat, newest operator visit first.
import { beforeEach, describe, expect, it } from 'vitest'

const stateModulePath = '../../../internal/web/static/app/state.js'
const uiStateModulePath = '../../../internal/web/static/app/uiState.js'
const dataModelModulePath = '../../../internal/web/static/app/dataModel.js'

const MENU = [
  { type: 'group', level: 0, group: { name: 'work', path: 'work', expanded: false, order: 0 } },
  { type: 'session', session: { id: 's1', title: 'api', groupPath: 'work', tool: 'claude', status: 'running', lastAccessedAt: '2026-10-30T10:00:00Z' } },
  { type: 'session', session: { id: 's2', title: 'web', groupPath: 'work', tool: 'codex', status: 'idle', lastAccessedAt: '0001-01-01T00:00:00Z' } },
  { type: 'group', level: 0, group: { name: 'personal', path: 'personal', expanded: true, order: 1 } },
  { type: 'session', session: { id: 's3', title: 'scratch', groupPath: 'personal', tool: 'shell', status: 'idle', lastAccessedAt: '2026-10-31T09:00:00Z' } },
  { type: 'session', session: { id: 's4', title: 'notes', groupPath: 'personal', tool: 'claude', status: 'waiting' } },
]

describe('recent sidebar view', () => {
  beforeEach(async () => {
    const { sessionsSignal, sessionCostsSignal } = await import(stateModulePath)
    const { sidebarFilterSignal, groupExpandedSignal, statusFiltersSignal, sidebarViewSignal } = await import(uiStateModulePath)
    sessionsSignal.value = MENU
    sessionCostsSignal.value = {}
    sidebarFilterSignal.value = ''
    groupExpandedSignal.value = { work: false }
    statusFiltersSignal.value = []
    sidebarViewSignal.value = 'recent'
  })

  it('lists only visited sessions, newest first, ignoring groups and folds', async () => {
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s3', 's:s1'])
    expect(sidebarRowsSignal.value.every((r) => r.type === 'session' && r.depth === 1)).toBe(true)
  })

  it('applies the text filter and status chips', async () => {
    const { sidebarFilterSignal, statusFiltersSignal } = await import(uiStateModulePath)
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    sidebarFilterSignal.value = 'api'
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s1'])
    sidebarFilterSignal.value = ''
    statusFiltersSignal.value = ['idle']
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s3'])
  })

  it('returns to the group tree when switched back', async () => {
    const { sidebarViewSignal } = await import(uiStateModulePath)
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    sidebarViewSignal.value = 'groups'
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['g:work', 'g:personal', 's:s3', 's:s4'])
  })
})
