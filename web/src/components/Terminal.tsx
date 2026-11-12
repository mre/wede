import { useEffect, useRef, useImperativeHandle, forwardRef } from 'react'
import { Terminal as XTerminal, type ITheme } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import type { TerminalHandle } from '../types'
import { terminalQueue } from '../lib/terminalQueue'

interface TerminalProps {
  token: string | null | undefined
  workspaceId: string | null | undefined
  sessionId?: string
  visible: boolean
  terminalTheme?: ITheme
  fontSize?: number
  initialCommand?: string
  onInitialRun?: () => void
  onTitle?: (title: string) => void
}

const encoder = new TextEncoder()
const fontFamily = "'JetBrains Mono Variable', monospace"

export default forwardRef<TerminalHandle, TerminalProps>(function Terminal({ token, workspaceId, sessionId, visible, terminalTheme, fontSize = 13, initialCommand, onInitialRun, onTitle }, ref) {
  const containerRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<XTerminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const updateRef = useRef<(() => void) | null>(null)
  const latest = useRef({ visible, fontSize, initialCommand, onInitialRun, onTitle })
  useEffect(() => {
    latest.current = { visible, fontSize, initialCommand, onInitialRun, onTitle }
  }, [visible, fontSize, initialCommand, onInitialRun, onTitle])
  useEffect(() => { updateRef.current?.() }, [visible, fontSize])

  useImperativeHandle(ref, () => ({
    send: (data: string) => {
      const ws = wsRef.current
      if (ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
      termRef.current?.focus()
    },
  }), [])

  useEffect(() => {
    const container = containerRef.current
    if (!container || !token) return
    let alive = true
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined
    let initialTimer: ReturnType<typeof setTimeout> | undefined
    let frame: number | undefined
    let reconnectDelay = 1000
    let ranInitial = false
    let fontGeneration = 0
    let loadedFontSize: number | undefined
    let lastViewport = ''
    const term = new XTerminal({
      cursorBlink: true,
      fontSize: latest.current.fontSize,
      fontFamily,
      theme: terminalTheme,
      allowTransparency: true,
      scrollback: 5000,
    })
    const fitAddon = new FitAddon()
    term.loadAddon(fitAddon)
    term.loadAddon(new WebLinksAddon())
    term.open(container)
    termRef.current = term
    const textarea = container.querySelector('textarea')
    for (const [key, value] of Object.entries({ autocomplete: 'off', autocorrect: 'off', autocapitalize: 'off', spellcheck: 'false', 'data-gramm': 'false' })) {
      textarea?.setAttribute(key, value)
    }
    const titleSubscription = term.onTitleChange(title => {
      if (alive) latest.current.onTitle?.(title)
    })
    const queue = terminalQueue(() => {
      // Never drop part of a live output stream and continue rendering it.
      // Reconnect instead, using the server's bounded scrollback replay.
      const ws = wsRef.current
      if (ws) disconnect(ws)
    })

    function reportViewport() {
      if (!alive) return
      const ws = wsRef.current
      if (ws?.readyState !== WebSocket.OPEN) return
      const rect = container!.getBoundingClientRect()
      let viewport: { type: 'viewport'; visible: boolean; cols?: number; rows?: number } = { type: 'viewport', visible: false }
      if (latest.current.visible && document.visibilityState !== 'hidden' && rect.width > 0 && rect.height > 0 && container!.getClientRects().length > 0) {
        // FitAddon is a capacity probe only. It must never resize the emulator.
        const size = fitAddon.proposeDimensions()
        if (size && Number.isInteger(size.cols) && Number.isInteger(size.rows) && size.cols > 0 && size.rows > 0) {
          viewport = { type: 'viewport', ...size, visible: true }
        }
      }
      const message = JSON.stringify(viewport)
      if (message !== lastViewport) {
        ws.send(message)
        lastViewport = message
      }
    }

    function measureSoon() {
      if (!alive || frame !== undefined) return
      frame = requestAnimationFrame(() => {
        frame = undefined
        if (alive) reportViewport()
      })
    }

    function refreshFontMetrics() {
      if (!alive) return
      const rect = container!.getBoundingClientRect()
      if (latest.current.visible && document.visibilityState !== 'hidden' && rect.width > 0 && rect.height > 0 && container!.getClientRects().length > 0) {
        // xterm ignores identical option assignments. Equivalent CSS quoting
        // triggers its public font-option measurement path after font loading.
        term.options.fontFamily = term.options.fontFamily === fontFamily
          ? '"JetBrains Mono Variable", monospace'
          : fontFamily
      }
      measureSoon()
    }

    function update() {
      if (!alive) return
      const size = latest.current.fontSize
      term.options.fontSize = size
      reportViewport() // Withdraw immediately, even when animation frames are paused.
      refreshFontMetrics()
      if (loadedFontSize !== size) {
        loadedFontSize = size
        const generation = ++fontGeneration
        const fonts = document.fonts
        if (fonts) {
          void fonts.load(`${size}px 'JetBrains Mono Variable'`).then(() => fonts.ready).then(() => {
            if (!alive || generation !== fontGeneration) return
            refreshFontMetrics()
          }).catch(() => { /* Keep the fallback font usable if loading fails. */ })
        }
      }
      if (latest.current.visible && document.visibilityState !== 'hidden') term.focus()
    }
    updateRef.current = update

    function disconnect(ws: WebSocket) {
      if (!alive || wsRef.current !== ws) return
      wsRef.current = null
      ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null
      ws.close()
      clearTimeout(initialTimer)
      queue.clear()
      reconnectTimer = setTimeout(() => {
        if (alive) connect(true)
      }, reconnectDelay)
      reconnectDelay = Math.min(reconnectDelay * 1.5, 10000)
    }

    function connect(isReconnect: boolean) {
      if (!alive) return
      const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
      const port = window.location.port
      const host = (port === '5173' || port === '5174') ? window.location.hostname + ':9090' : window.location.host
      const path = workspaceId ? `/api/workspaces/${encodeURIComponent(workspaceId)}/terminal` : '/api/terminal'
      const ws = new WebSocket(`${protocol}//${host}${path}?session=${encodeURIComponent(sessionId || token!)}&protocol=2`, [`auth.${token}`])
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws
      let revision = -1
      const active = () => alive && wsRef.current === ws
      ws.onopen = () => {
        if (!active()) return
        reconnectDelay = 1000
        if (isReconnect) queue.push(done => { term.reset(); done() })
        lastViewport = ''
        reportViewport()
        if (!ranInitial && latest.current.initialCommand) {
          const command = latest.current.initialCommand
          initialTimer = setTimeout(() => {
            if (!active() || ws.readyState !== WebSocket.OPEN || ranInitial) return
            ws.send(encoder.encode(command + '\r'))
            ranInitial = true
            latest.current.onInitialRun?.()
          }, 350)
        }
      }
      ws.onmessage = (event: MessageEvent) => {
        if (!active()) return
        if (event.data instanceof ArrayBuffer) {
          const data = new Uint8Array(event.data)
          queue.push(done => term.write(data, done), data.byteLength)
        } else if (typeof event.data === 'string') {
          // Text is exclusively protocol control, never terminal output.
          try {
            const message: unknown = JSON.parse(event.data)
            if (!message || typeof message !== 'object' || !('type' in message) || message.type !== 'size' || !('cols' in message) || !('rows' in message) || !('revision' in message)) return
            const { cols, rows, revision: nextRevision } = message
            if (typeof cols !== 'number' || !Number.isSafeInteger(cols) || cols <= 0 || typeof rows !== 'number' || !Number.isSafeInteger(rows) || rows <= 0 || typeof nextRevision !== 'number' || !Number.isSafeInteger(nextRevision) || nextRevision < 0 || nextRevision <= revision) return
            revision = nextRevision
            queue.push(done => { term.resize(cols, rows); done() })
          } catch { /* Ignore malformed or unknown control frames. */ }
        }
      }
      ws.onerror = () => {}
      ws.onclose = () => disconnect(ws)
    }

    const inputSubscription = term.onData(data => {
      const ws = wsRef.current
      if (alive && ws?.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
    })
    const observer = new ResizeObserver(measureSoon)
    observer.observe(container)
    document.addEventListener('visibilitychange', update)
    document.fonts?.addEventListener('loadingdone', refreshFontMetrics)
    update()
    connect(false)

    return () => {
      alive = false
      updateRef.current = null
      clearTimeout(reconnectTimer)
      clearTimeout(initialTimer)
      if (frame !== undefined) cancelAnimationFrame(frame)
      observer.disconnect()
      document.removeEventListener('visibilitychange', update)
      document.fonts?.removeEventListener('loadingdone', refreshFontMetrics)
      queue.dispose()
      const ws = wsRef.current
      wsRef.current = null
      if (ws) {
        ws.onopen = ws.onmessage = ws.onclose = ws.onerror = null
        ws.close()
      }
      inputSubscription.dispose()
      titleSubscription.dispose()
      term.dispose()
      termRef.current = null
    }
  }, [token, sessionId, workspaceId]) // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (termRef.current && terminalTheme) termRef.current.options.theme = terminalTheme
  }, [terminalTheme])

  return <div ref={containerRef} className="h-full w-full" title="Shared terminal grid: sized to the smallest visible browser viewport." style={{ display: visible ? 'block' : 'none' }} />
})
