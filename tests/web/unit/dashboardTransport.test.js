// matrix/hardened: one push-only dashboard transport, bounded JSON probes and
// generation-safe lifecycle stops, including native error/close ordering.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

const mocked = vi.hoisted(() => ({
  sessionsSignal: { value: [] }, sessionsLoadedSignal: { value: false },
  connectionSignal: { value: 'connecting' }, authTokenSignal: { value: '' },
  commandCenterSignal: { value: null }, selectedGroupSignal: { value: null },
  loadArchivedSessions: vi.fn(), addToast: vi.fn(),
}))
vi.mock('../../../internal/web/static/app/state.js', () => mocked)
vi.mock('../../../internal/web/static/app/auth.js', () => ({ authTokenSignal: mocked.authTokenSignal }))
vi.mock('../../../internal/web/static/app/App.js', () => ({ App: () => null, applyPath: vi.fn() }))
vi.mock('../../../internal/web/static/app/Toast.js', () => ({ addToast: mocked.addToast }))
vi.mock('../../../internal/web/static/app/push.js', () => ({ initPush: vi.fn() }))

class FakeWebSocket {
  static CONNECTING = 0
  static OPEN = 1
  static CLOSED = 3
  static instances = []
  constructor(url) {
    this.url = url
    this.readyState = FakeWebSocket.CONNECTING
    this.listeners = new Map()
    this.close = vi.fn(() => { this.readyState = FakeWebSocket.CLOSED; this.emit('close') })
    this.send = vi.fn()
    FakeWebSocket.instances.push(this)
  }
  addEventListener(type, listener) {
    const list = this.listeners.get(type) || []
    list.push(listener)
    this.listeners.set(type, list)
  }
  emit(type, event) { for (const listener of this.listeners.get(type) || []) listener(event) }
  open() { this.readyState = FakeWebSocket.OPEN; this.emit('open') }
  fail() { this.readyState = FakeWebSocket.CLOSED; this.emit('error'); this.emit('close') }
  frame(type, data) { this.emit('message', { data: JSON.stringify({ type, data }) }) }
}
const response = (status = 200, data = { items: [] }, contentType = 'application/json; charset=utf-8') => ({
  status, ok: status === 200, headers: new Headers({ 'Content-Type': contentType }),
  json: vi.fn().mockResolvedValue(data), body: { cancel: vi.fn().mockResolvedValue(undefined) },
})

let app, fetchMock, listeners
beforeEach(async () => {
  vi.resetModules()
  vi.useFakeTimers()
  FakeWebSocket.instances = []
  vi.stubGlobal('WebSocket', FakeWebSocket)
  fetchMock = vi.fn().mockResolvedValue(response())
  vi.stubGlobal('fetch', fetchMock)
  mocked.sessionsSignal.value = []
  mocked.sessionsLoadedSignal.value = false
  mocked.commandCenterSignal.value = null
  mocked.selectedGroupSignal.value = null
  mocked.connectionSignal.value = 'connecting'
  mocked.authTokenSignal.value = ''
  mocked.addToast.mockClear()
  mocked.loadArchivedSessions.mockClear()
  document.body.innerHTML = ''
  history.replaceState(null, '', '/')
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  listeners = []
  for (const target of [window, document]) {
    const original = target.addEventListener.bind(target)
    vi.spyOn(target, 'addEventListener').mockImplementation((type, listener, options) => {
      listeners.push([target, type, listener])
      original(type, listener, options)
    })
  }
  app = await import('../../../internal/web/static/app/main.js')
})
afterEach(() => {
  app?.stopSSE()
  for (const [target, type, listener] of listeners) target.removeEventListener(type, listener)
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  vi.useRealTimers()
})
const sources = () => FakeWebSocket.instances
const start = () => { app.startSSE(); return sources()[0] }
const wake = () => window.dispatchEvent(new Event('online'))

describe('dashboard WebSocket transport', () => {
  it('uses one same-origin authenticated connection for both entry points and never sends input', () => {
    mocked.authTokenSignal.value = 'a+/ ?&=中'
    const source = start()
    app.startSSE()
    app.startCommandCenterSSE()
    expect(sources()).toHaveLength(1)
    const url = new URL(source.url)
    expect(url.protocol).toBe(location.protocol === 'https:' ? 'wss:' : 'ws:')
    expect(url.host).toBe(location.host)
    expect(url.pathname).toBe('/ws/dashboard')
    expect(url.searchParams.get('token')).toBe(mocked.authTokenSignal.value)
    source.open()
    source.frame('heartbeat', {})
    expect(source.send).not.toHaveBeenCalled()
    expect(fetchMock).not.toHaveBeenCalled()
  })
  it('supports cookie authentication without adding a token query', () => {
    expect(new URL(start().url).search).toBe('')
  })
  it('uses wss for an HTTPS page and preserves its host and port', () => {
    vi.stubGlobal('window', { location: new URL('https://deck.example:9443/s/session') })
    expect(start().url).toBe('wss://deck.example:9443/ws/dashboard')
  })
  it('strips the initial token from navigation while retaining it for the handshake', async () => {
    history.replaceState(null, '', '/?token=a%2B%2F%26%E4%B8%AD&view=fleet#detail')
    vi.resetModules()
    app = await import('../../../internal/web/static/app/main.js')
    expect(location.search).toBe('?view=fleet')
    expect(location.hash).toBe('#detail')
    expect(document.querySelector('meta[name="referrer"]').content).toBe('no-referrer')
    expect(new URL(start().url).searchParams.get('token')).toBe('a+/&中')
  })
  it('applies both snapshots with archived refresh and completion de-duplication', () => {
    const source = start()
    source.open()
    mocked.selectedGroupSignal.value = 'project'
    const items = [{ type: 'session', session: { id: 's1', title: 'Worker' } }]
    source.frame('menu', { items })
    expect(mocked.sessionsSignal.value).toEqual(items)
    expect(mocked.sessionsLoadedSignal.value).toBe(true)
    expect(mocked.loadArchivedSessions).toHaveBeenCalledTimes(1)
    const cc = { totals: { running: 0 }, recentlyCompleted: [{ id: 's1', at: 'now', title: 'Worker' }] }
    source.frame('command-center', cc)
    source.frame('command-center', cc)
    expect(mocked.commandCenterSignal.value).toEqual(cc)
    expect(mocked.addToast).toHaveBeenCalledExactlyOnceWith('✅ Worker just finished', 'success')
    source.frame('menu', { items: [] })
    expect(mocked.sessionsSignal.value).toEqual([])
    expect(mocked.connectionSignal.value).toBe('connected')
  })
  it('ignores malformed frames and keeps the socket through source errors until a successful snapshot', () => {
    const source = start()
    source.open()
    source.emit('message', { data: '{' })
    source.emit('message', { data: 'null' })
    source.frame('menu', { error: { code: 'BAD' } })
    source.frame('command-center', [])
    source.frame('unknown', { items: ['bad'] })
    source.frame('error', { error: { code: 'MENU_UNAVAILABLE' } })
    source.frame('heartbeat', {})
    expect(mocked.sessionsLoadedSignal.value).toBe(false)
    expect(mocked.commandCenterSignal.value).toBeNull()
    expect(mocked.connectionSignal.value).toBe('disconnected')
    expect(source.close).not.toHaveBeenCalled()
    source.frame('menu', { items: [] })
    expect(mocked.connectionSignal.value).toBe('connected')
  })
  it('coalesces CONNECTING error and close into one authenticated JSON probe', async () => {
    mocked.authTokenSignal.value = 'bearer-token'
    const source = start()
    source.emit('error')
    expect(source.close).not.toHaveBeenCalled()
    source.readyState = FakeWebSocket.CLOSED
    source.emit('close')
    await vi.advanceTimersByTimeAsync(999)
    expect(fetchMock).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][0]).toBe('/api/menu')
    const options = fetchMock.mock.calls[0][1]
    expect(options.method).toBe('GET')
    expect(options.cache).toBe('no-store')
    expect(options.headers.Authorization).toBe('Bearer bearer-token')
    expect(options.headers.Accept).toBe('application/json')
    expect(options.signal).toBeInstanceOf(AbortSignal)
    expect(sources()).toHaveLength(2)
  })
  it.each([408, 425, 429, 500, 503])('retries transient %i with backoff', async (status) => {
    fetchMock.mockResolvedValueOnce(response(status)).mockResolvedValueOnce(response())
    start().fail()
    await vi.advanceTimersByTimeAsync(1000)
    expect(sources()).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1999)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(sources()).toHaveLength(2)
  })
  it.each([
    [401, 'application/json', { error: { code: 'UNAUTHORIZED' } }],
    [400, 'application/json', {}], [403, 'application/json', {}], [404, 'application/json', {}], [204, 'application/json', {}],
    [200, 'text/html', '<html>offline</html>'],
    [200, 'application/json', { error: { code: 'UNAVAILABLE' }, items: [] }],
    [200, 'application/json', { sessions: [] }],
  ])('waits for wake after terminal response %i %s', async (status, contentType, data) => {
    fetchMock.mockResolvedValueOnce(response(status, data, contentType)).mockResolvedValueOnce(response())
    start().fail()
    await vi.advanceTimersByTimeAsync(31000)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(sources()).toHaveLength(1)
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(0)
    expect(fetchMock).toHaveBeenCalledTimes(2)
    expect(sources()).toHaveLength(2)
  })
  it('requires valid JSON rather than reconnecting on successful headers alone', async () => {
    const malformed = response()
    malformed.json.mockRejectedValue(new SyntaxError('invalid JSON'))
    fetchMock.mockResolvedValue(malformed)
    start().fail()
    await vi.advanceTimersByTimeAsync(31000)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(sources()).toHaveLength(1)
  })
  it('times out a stalled probe and retries', async () => {
    fetchMock.mockImplementationOnce((_path, { signal }) => new Promise((_resolve, reject) => {
      signal.addEventListener('abort', () => reject(new DOMException('aborted', 'AbortError')))
    }))
    start().fail()
    await vi.advanceTimersByTimeAsync(5999)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(fetchMock.mock.calls[0][1].signal.aborted).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    expect(fetchMock.mock.calls[0][1].signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(2000)
    expect(sources()).toHaveLength(2)
  })
  it('resync closes immediately and schedules a probe without a network error', async () => {
    const source = start()
    source.open()
    source.frame('resync', { error: { code: 'RESYNC_REQUIRED' } })
    expect(source.close).toHaveBeenCalledTimes(1)
    expect(mocked.connectionSignal.value).toBe('disconnected')
    await vi.advanceTimersByTimeAsync(1000)
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(sources()).toHaveLength(2)
  })
  it('ignores retired socket events and de-duplicates completions across reconnect', async () => {
    const source = start()
    source.open()
    const cc = { recentlyCompleted: [{ id: 's1', at: 'now', title: 'Worker' }] }
    source.frame('command-center', cc)
    source.fail()
    await vi.advanceTimersByTimeAsync(1000)
    const replacement = sources()[1]
    replacement.open()
    replacement.frame('menu', { items: ['current'] })
    replacement.frame('command-center', cc)
    source.frame('menu', { items: ['stale'] })
    source.frame('command-center', { totals: { running: 99 } })
    source.emit('error')
    source.emit('close')
    await vi.advanceTimersByTimeAsync(31000)
    expect(mocked.sessionsSignal.value).toEqual(['current'])
    expect(mocked.commandCenterSignal.value).toEqual(cc)
    expect(mocked.addToast).toHaveBeenCalledTimes(1)
    expect(mocked.connectionSignal.value).toBe('connected')
    expect(sources()).toHaveLength(2)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })
  it('idempotent stop cancels retry and wake cannot revive it', async () => {
    const source = start()
    source.fail()
    app.stopSSE()
    app.stopSSE()
    wake()
    document.dispatchEvent(new Event('visibilitychange'))
    await vi.advanceTimersByTimeAsync(31000)
    expect(sources()).toHaveLength(1)
    expect(fetchMock).not.toHaveBeenCalled()
    expect(source.close).toHaveBeenCalledTimes(1)
  })
  it('stop aborts a probe and rejects late success after explicit restart', async () => {
    let resolveProbe
    fetchMock.mockImplementationOnce(() => new Promise(resolve => { resolveProbe = resolve }))
    start().fail()
    await vi.advanceTimersByTimeAsync(1000)
    const signal = fetchMock.mock.calls[0][1].signal
    app.stopSSE()
    expect(signal.aborted).toBe(true)
    app.startCommandCenterSSE()
    sources()[1].open()
    resolveProbe(response())
    await vi.advanceTimersByTimeAsync(31000)
    expect(sources()).toHaveLength(2)
    expect(mocked.connectionSignal.value).toBe('connected')
  })
  it('wake invalidates an in-flight probe before immediate replacement probing', async () => {
    let resolveProbe
    fetchMock.mockImplementationOnce(() => new Promise(resolve => { resolveProbe = resolve }))
    start().fail()
    await vi.advanceTimersByTimeAsync(1000)
    wake()
    await vi.advanceTimersByTimeAsync(0)
    expect(fetchMock.mock.calls[0][1].signal.aborted).toBe(true)
    expect(sources()).toHaveLength(2)
    resolveProbe(response())
    await vi.advanceTimersByTimeAsync(0)
    expect(sources()).toHaveLength(2)
  })
  it('initial REST starts one socket and a stopped pending boot cannot revive it', async () => {
    await app.loadMenu()
    expect(mocked.sessionsLoadedSignal.value).toBe(true)
    expect(sources()).toHaveLength(1)
    app.stopSSE()
    let resolveBoot
    fetchMock.mockImplementationOnce(() => new Promise(resolve => { resolveBoot = resolve }))
    const loading = app.loadMenu()
    app.stopSSE()
    resolveBoot(response(200, { items: ['stale boot'] }))
    await loading
    expect(mocked.sessionsSignal.value).toEqual([])
    expect(sources()).toHaveLength(1)
  })
  it('initial REST failure still starts dashboard recovery', async () => {
    fetchMock.mockResolvedValueOnce(response(503, { error: { message: 'offline' } }))
    await app.loadMenu()
    expect(mocked.sessionsLoadedSignal.value).toBe(false)
    expect(mocked.connectionSignal.value).toBe('disconnected')
    expect(sources()).toHaveLength(1)
  })
})
