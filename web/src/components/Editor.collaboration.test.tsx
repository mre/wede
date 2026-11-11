// @vitest-environment jsdom
import { createRoot, type Root } from 'react-dom/client'
import { EditorView } from '@codemirror/view'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { commit } from '../test/react'
import Editor from './Editor'
import CollaborationStatusIndicator from './CollaborationStatusIndicator'
import type { CollaborationStatus } from '../hooks/useYDoc'

vi.mock('../hooks/useTheme', () => ({ useTheme: () => ({ isDark: true }) }))
let root: Root
let container: HTMLDivElement
let doc: Y.Doc
let provider: WebsocketProvider
const onChange = vi.fn()
const onSave = vi.fn()
const file = { path: 'one:test.txt', rel: 'test.txt', name: 'test.txt', workspaceId: 'one' }

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
  container = document.createElement('div')
  root = createRoot(container)
  doc = new Y.Doc()
  provider = new WebsocketProvider('ws://localhost', 'test', doc, { connect: false, disableBc: true })
  onChange.mockClear()
  onSave.mockClear()
})
afterEach(async () => {
  await commit(() => root.unmount())
  provider.destroy()
  doc.destroy()
  vi.unstubAllGlobals()
})
async function render(status: CollaborationStatus) {
  await commit(() => root.render(<>
    <Editor file={file} content="stale REST snapshot" onChange={onChange} onSave={onSave}
      collab={{ ytext: doc.getText('content'), awareness: provider.awareness, provider, status }} />
    <CollaborationStatusIndicator status={status} />
  </>))
  return EditorView.findFromDOM(container.querySelector('.cm-editor') as HTMLElement)!
}

it('blocks pre-sync and disconnected edits, accepts sync, and reconnects without replacing the editor', async () => {
  const view = await render('connecting')
  expect(view.state.readOnly).toBe(true)
  view.dispatch({ changes: { from: 0, insert: 'must not insert' } })
  expect(doc.getText('content').toJSON()).toBe('')
  provider.wsconnected = true
  await render('syncing')
  await commit(() => doc.getText('content').insert(0, 'server text'))
  expect(view.state.doc.toString()).toBe('server text')
  provider.synced = true
  expect(await render('live')).toBe(view)
  expect(view.state.readOnly).toBe(false)
  view.dispatch({ changes: { from: 0, insert: 'local ' } })
  expect(doc.getText('content').toJSON()).toBe('local server text')
  provider.wsconnected = false
  provider.synced = false
  // Even before React processes the disconnect, reject stale LSP/local edits.
  view.dispatch({ changes: { from: 0, insert: 'blocked ' } })
  await render('disconnected')
  expect(view.state.readOnly).toBe(true)
  const undo = new InputEvent('beforeinput', { inputType: 'historyUndo', bubbles: true, cancelable: true })
  view.contentDOM.dispatchEvent(undo)
  expect(undo.defaultPrevented).toBe(true)
  expect(doc.getText('content').toJSON()).toBe('local server text')
  await render('error')
  expect(view.state.doc.toString()).toBe('local server text')
  provider.wsconnected = true
  provider.synced = true
  expect(await render('live')).toBe(view)
  expect(view.state.doc.toString()).toBe('local server text')
  expect(onChange).not.toHaveBeenCalled()
  expect(onSave).not.toHaveBeenCalled()
})

it.each(['off', 'connecting', 'syncing', 'live', 'disconnected', 'error'] as const)('shows separate %s collaboration status', async status => {
  await render(status)
  expect(container.querySelector('[role="status"]')?.textContent).toBe(`Collaboration: ${status}`)
  if (status === 'live') expect(container.querySelector('[role="status"]')?.getAttribute('title')).toContain('not a disk-save acknowledgement')
})

it('shows viewers a read-only REST snapshot, not a rejected empty document', async () => {
  await commit(() => root.render(<>
    <Editor file={file} content="REST viewer content" editable={false} onChange={onChange} onSave={onSave} />
    <CollaborationStatusIndicator status="off" viewer />
  </>))
  const view = EditorView.findFromDOM(container.querySelector('.cm-editor') as HTMLElement)!
  expect(view.state.doc.toString()).toBe('REST viewer content')
  expect(view.state.readOnly).toBe(true)
  expect(container.querySelector('[role="status"]')?.textContent).toContain('read-only snapshot')
})
