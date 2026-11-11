// @vitest-environment jsdom
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'
import type { LspStatus } from '../lib/lsp'
import LspStatusIndicator from './LspStatusIndicator'

describe('LspStatusIndicator', () => {
  it('renders nothing without a session', () => {
    expect(renderToStaticMarkup(<LspStatusIndicator status={null} />)).toBe('')
  })

  it.each<[LspStatus['state'], string]>([
    ['connecting', 'LSP connecting…'],
    ['connected', 'LSP connected'],
    ['indexing', 'LSP indexing…'],
    ['warning', 'LSP warning'],
    ['unavailable', 'LSP unavailable'],
  ])('labels %s accurately and keeps the server message in the tooltip', (state, label) => {
    const container = document.createElement('div')
    container.innerHTML = renderToStaticMarkup(<LspStatusIndicator status={{ state, message: 'Workspace <problem>' }} />)
    const indicator = container.querySelector('[role="status"]')!
    expect(indicator.textContent).toBe(label)
    expect(indicator.getAttribute('aria-live')).toBe('polite')
    expect(indicator.getAttribute('title')).toContain('Workspace <problem>')
    expect(indicator.getAttribute('title')?.includes('No automatic retry')).toBe(state === 'unavailable')
    expect(indicator.classList.contains('text-yellow')).toBe(state === 'warning' || state === 'unavailable')
    expect(container.querySelector('problem')).toBeNull()
  })
})
