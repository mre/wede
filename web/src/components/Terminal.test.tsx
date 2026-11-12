// @vitest-environment jsdom
import { createRef } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { commit } from '../test/react'
import type { TerminalHandle } from '../types'
import Terminal from './Terminal'

const mocks = vi.hoisted(() => ({ terms: [] as MockTerm[], capacity: { cols: 120, rows: 30 }, propose: vi.fn(), observers: [] as (() => void)[] }))
class MockTerm {
  options: Record<string, unknown>
  rows = 24
  cols = 80
  events: string[] = []
  writes: (() => void)[] = []
  data: (data: string) => void = () => {}
  title: (title: string) => void = () => {}
  constructor(options: Record<string, unknown>) { this.options = options; mocks.terms.push(this) }
  loadAddon() {}
  open() {}
  focus() {}
  refresh() { this.events.push('refresh') }
  dispose() { this.events.push('dispose') }
  reset() { this.events.push('reset') }
  resize(cols: number, rows: number) { this.cols = cols; this.rows = rows; this.events.push(`size:${cols}x${rows}`) }
  write(data: Uint8Array, done: () => void) { this.events.push(`write:${new TextDecoder().decode(data)}`); this.writes.push(done) }
  onData(callback: (data: string) => void) { this.data = callback; return { dispose() {} } }
  onTitleChange(callback: (title: string) => void) { this.title = callback; return { dispose() {} } }
}
vi.mock('@xterm/xterm', () => ({ Terminal: class { constructor(options: Record<string, unknown>) { return new MockTerm(options) } } }))
vi.mock('@xterm/addon-fit', () => ({ FitAddon: class { proposeDimensions() { mocks.propose(); return mocks.capacity } } }))
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }))

class Socket {
  static OPEN = 1
  static all: Socket[] = []
  readyState = 0
  binaryType = ''
  sent: (string | Uint8Array)[] = []
  onopen: (() => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null
  onmessage: ((event: { data: unknown }) => void) | null = null
  constructor(public url: string) { Socket.all.push(this) }
  send(data: string | Uint8Array) { this.sent.push(data) }
  open() { this.readyState = 1; this.onopen?.() }
  close() { this.readyState = 3 }
  drop() { this.close(); this.onclose?.() }
  size(cols: number, rows: number, revision: number) { this.onmessage?.({ data: JSON.stringify({ type: 'size', cols, rows, revision }) }) }
  output(text: string) {
    const encoded = new TextEncoder().encode(text)
    const buffer = new ArrayBuffer(encoded.length)
    new Uint8Array(buffer).set(encoded)
    this.onmessage?.({ data: buffer })
  }
  controls() { return this.sent.filter((x): x is string => typeof x === 'string').map(x => JSON.parse(x) as { type: string; visible: boolean; cols?: number; rows?: number }) }
}
let root: Root
let width: number
let height: number
let fontResolve: () => void
let fontLoad: ReturnType<typeof vi.fn>
const handle = createRef<TerminalHandle>()
async function render(props: Partial<React.ComponentProps<typeof Terminal>> = {}) {
  await commit(() => root.render(<Terminal ref={handle} token="token" workspaceId="workspace" visible {...props} />))
}
async function tick(ms = 20) { await commit(() => vi.advanceTimersByTime(ms)) }

beforeEach(() => {
  vi.useFakeTimers()
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
  vi.stubGlobal('WebSocket', Socket)
  vi.stubGlobal('ResizeObserver', class { constructor(callback: () => void) { mocks.observers.push(callback) } observe() {} disconnect() {} })
  vi.stubGlobal('requestAnimationFrame', (callback: () => void) => setTimeout(callback, 16))
  vi.stubGlobal('cancelAnimationFrame', clearTimeout)
  width = 1200; height = 600
  vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockImplementation(() => ({ width, height }) as DOMRect)
  vi.spyOn(HTMLElement.prototype, 'getClientRects').mockImplementation(() => (width && height ? [{}] : []) as unknown as DOMRectList)
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  fontLoad = vi.fn(() => new Promise<void>(resolve => { fontResolve = resolve }))
  Object.defineProperty(document, 'fonts', { configurable: true, value: { load: fontLoad, ready: Promise.resolve(), addEventListener() {}, removeEventListener() {} } })
  Socket.all = []; mocks.terms = []; mocks.observers = []; mocks.propose.mockClear(); mocks.capacity = { cols: 120, rows: 30 }
  root = createRoot(document.createElement('div'))
})
afterEach(async () => { await commit(() => root.unmount()); vi.useRealTimers(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('shared terminal protocol 2', () => {
  it('reports capacity but only applies canonical sizes, with no resize feedback or JSON output', async () => {
    await render()
    const ws = Socket.all[0]; const term = mocks.terms[0]
    ws.open()
    expect(ws.url).toContain('protocol=2')
    expect(ws.controls()).toEqual([{ type: 'viewport', cols: 120, rows: 30, visible: true }])
    expect(term.cols).toBe(80)
    ws.size(25, 10, 1)
    expect(term.events).toEqual(['size:25x10'])
    mocks.observers[0](); await tick()
    expect(ws.controls()).toHaveLength(1)
    ws.size(120, 30, 2); ws.size(25, 10, 1)
    ws.onmessage?.({ data: '{broken' }); ws.onmessage?.({ data: '{"type":"other"}' })
    expect(term.events).toEqual(['size:25x10', 'size:120x30'])
  })

  it('encodes keyboard, imperative and initial task input as binary exactly once across reconnects', async () => {
    const onInitialRun = vi.fn()
    await render({ initialCommand: 'echo café', onInitialRun })
    const ws = Socket.all[0]; ws.open()
    mocks.terms[0].data('é'); handle.current!.send('\u0003')
    await tick(350)
    expect(ws.sent.filter((x): x is Uint8Array => ArrayBuffer.isView(x)).map(x => new TextDecoder().decode(x))).toEqual(['é', '\u0003', 'echo café\r'])
    expect(onInitialRun).toHaveBeenCalledOnce()
    ws.drop(); await tick(1000); Socket.all[1].open(); await tick(350)
    expect(Socket.all[1].sent.every(x => typeof x === 'string')).toBe(true)
  })

  it('keeps callbacks fresh without focusing, resizing or reporting viewport on callback-only rerenders', async () => {
    const oldTitle = vi.fn()
    const oldInitialRun = vi.fn()
    const newTitle = vi.fn()
    const newInitialRun = vi.fn()
    await render({ initialCommand: 'task', onTitle: oldTitle, onInitialRun: oldInitialRun })
    const ws = Socket.all[0]; const term = mocks.terms[0]
    ws.open()
    // Settle mount/font measurement before isolating callback-only updates.
    await commit(() => fontResolve()); await tick()
    const focus = vi.spyOn(term, 'focus')
    const resize = vi.spyOn(term, 'resize')
    mocks.propose.mockClear()
    const controls = ws.controls()
    const family = term.options.fontFamily
    // A stray measurement must not be masked by viewport message deduplication.
    mocks.capacity = { cols: 90, rows: 20 }

    await render({ initialCommand: 'task', onTitle: newTitle, onInitialRun: oldInitialRun })
    await tick()
    await render({ initialCommand: 'task', onTitle: newTitle, onInitialRun: newInitialRun })
    await tick(350)
    term.title('current title')

    expect(focus).not.toHaveBeenCalled()
    expect(resize).not.toHaveBeenCalled()
    expect(mocks.propose).not.toHaveBeenCalled()
    expect(ws.controls()).toEqual(controls)
    expect(term.options.fontFamily).toBe(family)
    expect(mocks.terms).toHaveLength(1)
    expect(Socket.all).toHaveLength(1)
    expect(oldTitle).not.toHaveBeenCalled()
    expect(newTitle).toHaveBeenCalledExactlyOnceWith('current title')
    expect(oldInitialRun).not.toHaveBeenCalled()
    expect(newInitialRun).toHaveBeenCalledOnce()
    expect(ws.sent.filter((x): x is Uint8Array => ArrayBuffer.isView(x)).map(x => new TextDecoder().decode(x))).toEqual(['task\r'])
  })

  it('withdraws hidden panes/documents and zero-sized containers without measuring them', async () => {
    await render({ visible: false }); const ws = Socket.all[0]; ws.open()
    expect(ws.controls().at(-1)).toEqual({ type: 'viewport', visible: false })
    expect(mocks.propose).not.toHaveBeenCalled()
    await render(); expect(ws.controls().at(-1)?.cols).toBe(120)
    mocks.propose.mockClear()
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    document.dispatchEvent(new Event('visibilitychange'))
    expect(ws.controls().at(-1)).toEqual({ type: 'viewport', visible: false })
    await tick(); expect(mocks.propose).not.toHaveBeenCalled()
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
    width = 0; document.dispatchEvent(new Event('visibilitychange')); await tick()
    expect(mocks.propose).not.toHaveBeenCalled()
    width = 1200; mocks.observers[0](); await tick()
    expect(ws.controls().at(-1)?.visible).toBe(true)
  })

  it('preserves async output/size order and resets behind old writes before replay', async () => {
    await render(); const ws = Socket.all[0]; const term = mocks.terms[0]; ws.open()
    ws.output('before'); ws.size(25, 10, 1); ws.output('after')
    expect(term.events).toEqual(['write:before'])
    term.writes.shift()!()
    expect(term.events).toEqual(['write:before', 'size:25x10', 'write:after'])
    ws.size(99, 99, 2)
    const lateMessage = ws.onmessage!
    ws.drop(); await tick(1000); const next = Socket.all[1]; next.open(); next.size(40, 20, 0); next.output('replay')
    lateMessage({ data: JSON.stringify({ type: 'size', cols: 9, rows: 9, revision: 100 }) })
    expect(term.events.at(-1)).toBe('write:after')
    term.writes.shift()!()
    expect(term.events.slice(-3)).toEqual(['reset', 'size:40x20', 'write:replay'])
    expect(term.events).not.toContain('size:99x99')
  })

  it('updates the bundled font and remeasures after loading; ignores old session callbacks/timers', async () => {
    await render({ initialCommand: 'old' }); const ws = Socket.all[0]; ws.open()
    const oldFontResolve = fontResolve
    const oldOpen = ws.onopen!
    await render({ sessionId: 'new', fontSize: 18, initialCommand: 'new' })
    expect(mocks.terms[0].events.at(-1)).toBe('dispose')
    oldOpen(); oldFontResolve(); await tick(400)
    expect(ws.sent.filter(x => ArrayBuffer.isView(x))).toHaveLength(0)
    const term = mocks.terms[1]; const next = Socket.all[1]; next.open()
    expect(term.options.fontFamily).toContain('JetBrains Mono Variable')
    expect(fontLoad).toHaveBeenLastCalledWith("18px 'JetBrains Mono Variable'")
    mocks.capacity = { cols: 90, rows: 20 }
    const previousFamily = term.options.fontFamily
    await commit(() => fontResolve()); await tick()
    expect(term.options.fontFamily).not.toBe(previousFamily)
    expect(term.options.fontFamily).toContain('JetBrains Mono Variable')
    expect(next.controls().at(-1)?.cols).toBe(90)
    await render({ sessionId: 'new', fontSize: 20 })
    expect(term.options.fontSize).toBe(20)
    expect(fontLoad).toHaveBeenLastCalledWith("20px 'JetBrains Mono Variable'")
  })

  it('retries an initial task interrupted before sending and bounds queued output', async () => {
    const onInitialRun = vi.fn()
    await render({ initialCommand: 'task', onInitialRun }); const ws = Socket.all[0]; ws.open()
    ws.drop(); await tick(1000); const next = Socket.all[1]; next.open(); await tick(350)
    expect(onInitialRun).toHaveBeenCalledOnce()
    next.output('blocked')
    next.onmessage?.({ data: new ArrayBuffer(4 * 1024 * 1024) })
    expect(next.readyState).toBe(3)
    await tick(1000); expect(Socket.all).toHaveLength(3)
  })
})
