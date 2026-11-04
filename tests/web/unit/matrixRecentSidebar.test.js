// matrix/hardened: 最近交互 mirrors the TUI's Ctrl+E list. Only confirmed
// submissions count; selecting, viewing and drafts do not change its order.
import { beforeEach, describe, expect, it } from 'vitest'
import { render } from 'preact'
import { html } from 'htm/preact'

const stateModulePath = '../../../internal/web/static/app/state.js'
const uiStateModulePath = '../../../internal/web/static/app/uiState.js'
const dataModelModulePath = '../../../internal/web/static/app/dataModel.js'
const sidebarModulePath = '../../../internal/web/static/app/Sidebar.js'

const MENU = [
  { type: 'group', level: 0, group: { name: 'work', path: 'work', expanded: false, order: 0 } },
  { type: 'session', session: { id: 's1', title: 'api', groupPath: 'work', tool: 'claude', status: 'running', lastAccessedAt: '2026-10-31T12:00:00Z', lastInteractionAt: '2026-10-30T10:00:00Z' } },
  { type: 'session', session: { id: 's2', title: 'web', groupPath: 'work', tool: 'codex', status: 'idle', lastAccessedAt: '2026-10-31T13:00:00Z', lastInteractionAt: '0001-01-01T00:00:00Z' } },
  { type: 'group', level: 0, group: { name: 'personal', path: 'personal', expanded: true, order: 1 } },
  { type: 'session', session: { id: 's3', title: 'scratch', groupPath: 'personal', tool: 'shell', status: 'idle', lastAccessedAt: '2026-10-31T09:00:00Z', lastInteractionAt: '2026-10-31T09:00:00Z' } },
  { type: 'session', session: { id: 's4', title: 'notes', groupPath: 'personal', tool: 'claude', status: 'waiting', lastAccessedAt: '2026-10-31T14:00:00Z' } },
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

  it('lists only confirmed interactions, newest first, ignoring visits, groups and folds', async () => {
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s3', 's:s1'])
    expect(sidebarRowsSignal.value.every((r) => r.type === 'session' && r.depth === 1)).toBe(true)
  })

  it('does not reorder on selection or newer visit/background timestamps', async () => {
    const { sessionsSignal, selectSession } = await import(stateModulePath)
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    selectSession('s1')
    sessionsSignal.value = MENU.map((item) => item.session
      ? { ...item, session: { ...item.session, lastAccessedAt: '2026-11-01T16:00:00Z', lastActivityAt: '2026-11-01T16:01:00Z' } }
      : item)
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s3', 's:s1'])
  })

  it('reorders when the server publishes a newly confirmed submission', async () => {
    const { sessionsSignal } = await import(stateModulePath)
    const { sidebarRowsSignal } = await import(dataModelModulePath)
    sessionsSignal.value = MENU.map((item) => item.session?.id === 's1'
      ? { ...item, session: { ...item.session, lastInteractionAt: '2026-11-01T15:00:00Z' } }
      : item)
    expect(sidebarRowsSignal.value.map((r) => r.key)).toEqual(['s:s1', 's:s3'])
  })

  it('labels and displays the interaction age rather than the newer visit age', async () => {
    const { Sidebar } = await import(sidebarModulePath)
    const { formatRelativeTime } = await import('../../../internal/web/static/app/timeFmt.js')
    const container = document.createElement('div')
    document.body.appendChild(container)
    try {
      render(html`<${Sidebar}/>`, container)
      expect(container.querySelector('.side-head .label').textContent).toBe('最近交互 RECENT')
      const clock = container.querySelector('[data-row-key="s:s1"] [title="Last interaction (orders this list)"]')
      expect(clock.textContent).toContain(formatRelativeTime(MENU[1].session.lastInteractionAt))
      expect(container.querySelector('[data-row-key="s:s2"]')).toBeNull()
    } finally {
      render(null, container)
      container.remove()
    }
  })

  it('explains that submitting, rather than viewing, adds an unknown session', async () => {
    const { sessionsSignal } = await import(stateModulePath)
    sessionsSignal.value = MENU.map((item) => item.session
      ? { ...item, session: { ...item.session, lastInteractionAt: '' } }
      : item)
    const { Sidebar } = await import(sidebarModulePath)
    const container = document.createElement('div')
    document.body.appendChild(container)
    try {
      render(html`<${Sidebar}/>`, container)
      const empty = container.querySelector('[data-testid="recent-empty"]')
      expect(empty.textContent).toContain('No recent interactions')
      expect(empty.textContent).toContain('Submit a message')
      expect(empty.textContent).not.toContain('Open a session')
    } finally {
      render(null, container)
      container.remove()
    }
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
