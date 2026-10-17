// matrix/hardened: render guest/app budgets with a visible qualifier, including zero usage.
import { h } from 'preact'
import { render, screen } from '@testing-library/preact'
import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('../../../internal/web/static/app/dataModel.js', () => ({
  menuModelSignal: { value: { sessions: [] } },
}))
vi.mock('../../../internal/web/static/app/uiState.js', () => ({
  profileSignal: { value: '' },
}))
vi.mock('../../../internal/web/static/app/state.js', () => ({
  connectionSignal: { value: 'connected' },
  systemStatsSignal: { value: null },
}))

import { Footer } from '../../../internal/web/static/app/Footer.js'
import { systemStatsSignal } from '../../../internal/web/static/app/state.js'

afterEach(() => { systemStatsSignal.value = null })

describe('Footer memory scope', () => {
  it('identifies the guest budget', () => {
    systemStatsSignal.value = { memory: { scope: 'guest', usage_percent: 0 } }
    render(h(Footer))
    expect(screen.getByText('mem (guest) · 0%')).toBeVisible()
  })

  it('preserves the legacy label when no scope is reported', () => {
    systemStatsSignal.value = { memory: { usage_percent: 25 } }
    render(h(Footer))
    expect(screen.getByText('mem · 25%')).toBeVisible()
    expect(screen.queryByText(/guest/)).toBeNull()
  })

  it('does not invent an unavailable reading', () => {
    systemStatsSignal.value = {}
    render(h(Footer))
    expect(screen.queryByText(/^mem/)).toBeNull()
  })
})
