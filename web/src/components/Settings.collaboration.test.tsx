// @vitest-environment jsdom
import { createRoot, type Root } from 'react-dom/client'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'
import { commit } from '../test/react'
import Settings from './Settings'

vi.mock('../hooks/useTheme', () => ({ useTheme: () => ({ isDark: true, setTheme: vi.fn() }) }))
let root: Root
let container: HTMLDivElement
beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
  container = document.createElement('div')
  root = createRoot(container)
})
afterEach(async () => {
  await commit(() => root.unmount())
  vi.unstubAllGlobals()
  vi.unstubAllEnvs()
})

it.each([true, false])('shows the effective setting and only permits opt-out when not required: %s', async required => {
  vi.stubEnv('VITE_WEDE_REQUIRE_COLLAB', required ? 'true' : undefined)
  const change = vi.fn()
  await commit(() => root.render(<Settings visible onClose={() => {}} onOpenFolder={() => {}}
    workspace={null} editorSettings={{ collab: false, fontSize: 13, tabWidth: 2, wordWrap: false, autoSave: true, minimap: false, lsp: true, formatOnSave: false }} onEditorSettingsChange={change}
    lspAvailable={null} authFetch={vi.fn()} role="viewer" workspaceId={null} username="viewer"
    onUsernameChange={name => Promise.resolve(name)} onTrustChange={() => {}} />))
  const label = [...container.querySelectorAll('label')].find(el => el.textContent?.includes('Collaborative editing'))!
  if (required) {
    expect(label.textContent).toContain('Required by this shared deployment')
    expect(label.querySelector('button')).toBeNull()
    expect(container.textContent).toContain('REST auto-save is inactive')
    expect(container.textContent).toContain('Save formatting is inactive')
  } else {
    expect(label.textContent).not.toContain('Required')
    expect(label.querySelector('button')).not.toBeNull()
  }
  expect(change).not.toHaveBeenCalled()
})


it.each([true, false])('shows saving then the actual rename outcome (success=%s)', async success => {
  let resolve!: (name: string) => void
  let reject!: (error: Error) => void
  const change = vi.fn(() => new Promise<string>((yes, no) => { resolve = yes; reject = no }))
  await commit(() => root.render(<Settings visible onClose={() => {}} onOpenFolder={() => {}}
    workspace={null} editorSettings={{ collab: false, fontSize: 13, tabWidth: 2, wordWrap: false, autoSave: true, minimap: false, lsp: true, formatOnSave: false }} onEditorSettingsChange={() => {}}
    lspAvailable={null} authFetch={vi.fn()} role="viewer" workspaceId={null} username="viewer"
    onUsernameChange={change} onTrustChange={() => {}} />))
  const input = container.querySelector<HTMLInputElement>('input[placeholder="Your name"]')!
  await commit(() => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, 'requested')
    input.dispatchEvent(new Event('input', { bubbles: true }))
  })
  const save = [...container.querySelectorAll('button')].find(el => el.textContent === 'Save')!
  await commit(() => save.click())
  expect(save.textContent).toBe('Saving…')
  expect(save.disabled).toBe(true)
  expect(input.disabled).toBe(true)
  expect(container.textContent).not.toContain('Saved')
  await commit(() => input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })))
  expect(change).toHaveBeenCalledOnce()
  await commit(() => { if (success) resolve('canonical'); else reject(new Error('Server rejected name')) })
  expect(input.disabled).toBe(false)
  if (success) {
    expect(save.textContent).toBe('Saved')
    expect(input.value).toBe('canonical')
    expect(container.querySelector('[role="alert"]')).toBeNull()
  } else {
    expect(save.textContent).toBe('Save')
    expect(input.value).toBe('requested')
    expect(container.querySelector('[role="alert"]')?.textContent).toBe('Server rejected name')
    expect(save.disabled).toBe(false)
  }
})
