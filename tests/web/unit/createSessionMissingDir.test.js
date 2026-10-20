// matrix/hardened: the web create dialog asks before creating a missing
// project directory (server answers 409 DIRECTORY_NOT_FOUND), like the TUI.
import { describe, it, expect, vi, afterEach } from 'vitest'
import { createRequire } from 'node:module'
import { fireEvent, waitFor } from '@testing-library/preact'
import { render as renderPreact } from 'preact'
import { html } from 'htm/preact'

// The app modules load CJS Preact while RTL's renderer loads another
// instance (see costDashboardManualSync.test.js): mount with the same one.
const { act } = createRequire(import.meta.url)('preact/test-utils')
let mounted = null
function render(vnode) {
  const container = document.createElement('div')
  document.body.appendChild(container)
  act(() => renderPreact(vnode, container))
  mounted = container
  const byText = (t) => [...container.querySelectorAll('button')].find(b => b.textContent.trim() === t)
  return {
    container,
    getByText: byText,
    findByTestId: (id) => waitFor(() => { const el = container.querySelector(`[data-testid="${id}"]`); if (!el) throw new Error('not yet'); return el }),
  }
}
function cleanup() {
  if (!mounted) return
  act(() => renderPreact(null, mounted)); mounted.remove(); mounted = null
}

const app = '../../../internal/web/static/app/'

afterEach(() => { cleanup(); vi.unstubAllGlobals() })

function jsonResponse(status, body) {
  return { ok: status < 400, status, statusText: String(status), json: async () => body }
}

describe('CreateSessionDialog missing directory', () => {
  it('shows the missing path, creates it only on confirm, then closes', async () => {
    const state = await import(app + 'state.js')
    const { CreateSessionDialog } = await import(app + 'CreateSessionDialog.js')
    state.mutationsEnabledSignal.value = true
    state.createSessionDialogSignal.value = { groupPath: 'work', groupName: 'work', defaultPath: '/srv/new-proj', tool: 'shell', modelId: '' }

    const calls = []
    vi.stubGlobal('fetch', vi.fn(async (url, init) => {
      const body = JSON.parse(init.body)
      calls.push(body)
      if (!body.createDirectory) {
        return jsonResponse(409, { error: { code: 'DIRECTORY_NOT_FOUND', message: 'directory does not exist: /srv/new-proj' }, path: '/srv/new-proj' })
      }
      return jsonResponse(201, { sessionId: 'sess-new' })
    }))

    const { container, getByText, findByTestId } = render(html`<${CreateSessionDialog}/>`)
    const form = container.querySelector('form')
    fireEvent.input(container.querySelector('input[placeholder="/absolute/path/to/project"]'), { target: { value: '/srv/new-proj' } })
    const titleInput = [...container.querySelectorAll('input')].find(i => i.placeholder !== '/absolute/path/to/project')
    if (titleInput) fireEvent.input(titleInput, { target: { value: 'demo' } })
    fireEvent.submit(form)

    const notice = await findByTestId('create-missing-dir')
    expect(notice.textContent).toContain('/srv/new-proj')
    expect(calls).toHaveLength(1)
    expect(calls[0].createDirectory).toBeUndefined()
    expect(state.createSessionDialogSignal.value).not.toBeNull()

    fireEvent.click(getByText('Create directory and start'))
    await waitFor(() => expect(state.createSessionDialogSignal.value).toBeNull())
    expect(calls).toHaveLength(2)
    expect(calls[1].createDirectory).toBe(true)
    expect(calls[1].projectPath).toBe('/srv/new-proj')
  })
})
