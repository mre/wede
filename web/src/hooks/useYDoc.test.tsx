// @vitest-environment jsdom
import { commit } from '../test/react'
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import type * as Y from 'yjs'
import type { Awareness } from 'y-protocols/awareness'
import { useYDoc, type UseYDocParams, type UseYDocState } from './useYDoc'

interface TestProvider {
  doc: Y.Doc
  awareness: Awareness
  destroy: () => void
  wsconnected: boolean
  synced: boolean
  emit: (event: string, value?: unknown) => void
}
const { providers, failures } = vi.hoisted(() => ({ providers: [] as TestProvider[], failures: { construct: false } }))
vi.mock('y-websocket', async () => {
  const { Awareness } = await import('y-protocols/awareness')
  return { WebsocketProvider: class {
    awareness: Awareness
    wsconnected = false
    synced = false
    listeners = new Map<string, ((value: unknown) => void)[]>()
    on(event: string, callback: (value: unknown) => void) {
      this.listeners.set(event, [...(this.listeners.get(event) || []), callback])
    }
    emit(event: string, value?: unknown) {
      for (const callback of this.listeners.get(event) || []) callback(value)
    }
    destroy = vi.fn(() => this.awareness.destroy())
    constructor(_base: string, _room: string, public doc: Y.Doc) {
      if (failures.construct) throw new Error('construction failed')
      this.awareness = new Awareness(doc)
      providers.push(this)
    }
  } }
})

let root: Root
let container: HTMLDivElement
let snapshots: UseYDocState[]
const defaults: UseYDocParams = { workspaceId: 'one', path: 'main.rs', token: 'token', username: 'user' }
function Harness({ params }: { params: UseYDocParams }) {
  snapshots.push(useYDoc(params))
  return null
}

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
  providers.length = 0
  failures.construct = false
  snapshots = []
  container = document.createElement('div')
  root = createRoot(container)
})
afterEach(async () => {
  await commit(() => root.unmount())
  expect(providers.every(p => p.doc.isDestroyed)).toBe(true)
  vi.unstubAllGlobals()
})

it.each([
  { path: 'other.rs' },
  { workspaceId: 'two' },
  { token: 'new-token' },
])('hides the previous Y.Text in the very first render of a changed identity: %j', async override => {
  await commit(() => root.render(<Harness params={defaults} />))
  const previous = snapshots.at(-1)?.ytext
  expect(previous).not.toBeNull()
  snapshots = []
  await commit(() => root.render(<Harness params={{ ...defaults, ...override }} />))
  expect(snapshots[0].ytext).toBeNull()
  expect(snapshots.every(s => s.ytext !== previous)).toBe(true)
  expect(snapshots.at(-1)?.ytext).not.toBeNull()
  expect(providers[0].destroy).toHaveBeenCalledOnce()
  expect(providers[0].doc.isDestroyed).toBe(true)
})

it('reports connection and sync lifecycle without losing unsent changes on reconnect', async () => {
  await commit(() => root.render(<Harness params={defaults} />))
  const provider = providers[0]
  const text = snapshots.at(-1)!.ytext!
  expect(snapshots.at(-1)?.status).toBe('connecting')
  await commit(() => {
    provider.wsconnected = true
    provider.emit('status', { status: 'connected' })
  })
  expect(snapshots.at(-1)?.status).toBe('syncing')
  await commit(() => { provider.synced = true; provider.emit('sync', true) })
  expect(snapshots.at(-1)?.status).toBe('live')
  text.insert(0, 'unsent edit')
  await commit(() => provider.emit('connection-error'))
  expect(snapshots.at(-1)?.status).toBe('error')
  expect(snapshots.at(-1)?.ytext).toBe(text)
  await commit(() => {
    provider.wsconnected = false
    provider.synced = false
    provider.emit('sync', false)
    provider.emit('connection-close')
  })
  expect(snapshots.at(-1)?.status).toBe('disconnected')
  await commit(() => provider.emit('status', { status: 'connecting' }))
  expect(snapshots.at(-1)?.status).toBe('connecting')
  await commit(() => {
    provider.wsconnected = true
    provider.emit('status', { status: 'connected' })
  })
  expect(snapshots.at(-1)?.status).toBe('syncing')
  await commit(() => { provider.synced = true; provider.emit('sync', true) })
  expect(snapshots.at(-1)?.status).toBe('live')
  expect(snapshots.at(-1)?.ytext).toBe(text)
  expect(text.toJSON()).toBe('unsent edit')
  expect(providers).toHaveLength(1)
  expect(provider.destroy).not.toHaveBeenCalled()
})

it('blocks failed construction and missing credentials instead of reporting off', async () => {
  failures.construct = true
  await commit(() => root.render(<Harness params={defaults} />))
  expect(snapshots.at(-1)?.status).toBe('error')
  expect(snapshots.at(-1)?.ytext).toBeNull()
  await commit(() => root.render(<Harness params={{ ...defaults, token: null }} />))
  expect(snapshots.at(-1)?.status).toBe('error')
})

it('ignores late events from a replaced session', async () => {
  await commit(() => root.render(<Harness params={defaults} />))
  const old = providers[0]
  await commit(() => root.render(<Harness params={{ ...defaults, path: 'new.rs' }} />))
  const current = snapshots.at(-1)
  await commit(() => { old.emit('connection-error'); old.emit('sync', true) })
  expect(snapshots.at(-1)).toBe(current)
})

it('keeps the provider stable for the same identity and closes it when disabled', async () => {
  await commit(() => root.render(<Harness params={defaults} />))
  const previous = snapshots.at(-1)?.ytext
  await commit(() => root.render(<Harness params={{ ...defaults }} />))
  expect(providers).toHaveLength(1)
  expect(snapshots.at(-1)?.ytext).toBe(previous)
  snapshots = []
  await commit(() => root.render(<Harness params={{ ...defaults, path: null }} />))
  expect(snapshots.every(s => s.ytext === null)).toBe(true)
  expect(providers[0].destroy).toHaveBeenCalledOnce()
})

it('updates awareness in place on rename/color change without losing unsent edits', async () => {
  await commit(() => root.render(<Harness params={defaults} />))
  const previous = snapshots.at(-1)!
  const provider = providers[0]
  previous.ytext!.insert(0, 'unsent')
  provider.awareness.setLocalStateField('cursor', { anchor: 3 })
  for (const override of [{ username: 'renamed' }, { username: 'renamed', color: '#123456' }]) {
    snapshots = []
    await commit(() => root.render(<Harness params={{ ...defaults, ...override }} />))
    expect(snapshots.every(s => s.ytext === previous.ytext && s.provider === previous.provider)).toBe(true)
    expect(provider.awareness.getLocalState()).toMatchObject({
      user: { name: 'renamed', color: 'color' in override ? override.color : '#888' }, cursor: { anchor: 3 },
    })
  }
  expect(providers).toHaveLength(1)
  expect(provider.destroy).not.toHaveBeenCalled()
  expect(provider.doc.isDestroyed).toBe(false)
  expect(previous.ytext!.toJSON()).toBe('unsent')
})
