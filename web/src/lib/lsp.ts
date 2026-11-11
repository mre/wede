import { Compartment, type Extension } from '@codemirror/state'
import { ViewPlugin } from '@codemirror/view'
import { LanguageServerClient, languageServerWithTransport, type Transport } from 'codemirror-languageserver'
import type { DocumentSaves } from './documentSaves'

export interface LspStatus {
  state: 'connecting' | 'connected' | 'indexing' | 'warning' | 'unavailable'
  message: string
}

export interface LspConfig {
  serverUri: string
  rootUri: string
  documentUri: string
  languageId: string
  persistence?: { events: DocumentSaves; workspaceId: string; path: string }
  onStatus?: (status: LspStatus | null) => void
}

// Paths are POSIX paths on the server, not URLs relative to the browser origin.
export function lspDocumentUris(root: string, relativePath: string) {
  const parts = relativePath.split('/')
  if (!root.startsWith('/') || root.includes('\0') || relativePath.includes('\0') ||
      parts.some(part => !part || part === '.' || part === '..')) return null
  const rootUri = `file://${(root.replace(/\/+$/, '') || '/').split('/').map(encodeURIComponent).join('/')}`
  return {
    rootUri,
    documentUri: `${rootUri.replace(/\/$/, '')}/${parts.map(encodeURIComponent).join('/')}`,
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

// Keep eager sysroot warmup and syntax-tree retention modest on small hosts.
// Diagnostics, cargo checks, build scripts, and proc macros keep server defaults.
const RUST_RESOURCE_SETTINGS = {
  numThreads: 1,
  cachePriming: { enable: false },
  lru: { capacity: 32 },
} as const

function rustConfiguration(section: unknown): unknown {
  if (section === undefined || section === '') return { 'rust-analyzer': RUST_RESOURCE_SETTINGS }
  if (typeof section !== 'string') return null
  const [prefix, ...parts] = section.split('.')
  if (prefix !== 'rust-analyzer') return null
  let value: unknown = RUST_RESOURCE_SETTINGS
  for (const part of parts) {
    if (!isRecord(value) || !Object.hasOwn(value, part)) return null
    value = value[part]
  }
  return value
}

// Unlike the package's WebSocketTransport, this transport is handed an already
// open socket. Closing synchronously rejects pending RPCs and silences queued
// notifications (including the package plugin's uncancellable didOpen await).
class OpenSocketTransport implements Transport {
  private closed = false
  private listeners = new AbortController()
  private closeHandlers: (() => void)[] = []

  constructor(
    private socket: WebSocket,
    private workspaceFolders: { uri: string; name: string }[],
    private onServerNotification: (method: string, params: Record<string, unknown>) => void,
    private configuration: (section: unknown) => unknown,
  ) {
    socket.addEventListener('close', () => this.close(), { signal: this.listeners.signal })
  }

  send(message: string) {
    if (this.closed) {
      // JSONRPCClient awaits readiness before registering a request. It may
      // register one after close(); reject that late request too, without a send.
      for (const callback of this.closeHandlers) callback()
      return
    }
    if (this.socket.readyState !== WebSocket.OPEN) {
      this.close()
      return
    }
    try {
      this.socket.send(message)
    } catch {
      this.close()
    }
  }

  private handleServerRequest(message: unknown): boolean {
    if (!isRecord(message) || typeof message.method !== 'string' || !('id' in message)) return false
    const { id, method, params } = message
    const reply = (response: { result: unknown } | { error: { code: number; message: string } }) => {
      this.send(JSON.stringify({ jsonrpc: '2.0', id, ...response }))
    }
    if (typeof id !== 'number' && typeof id !== 'string') {
      this.send(JSON.stringify({ jsonrpc: '2.0', id: null, error: { code: -32600, message: 'Invalid request ID' } }))
      return true
    }
    switch (method) {
      case 'workspace/configuration':
        if (!isRecord(params) || !Array.isArray(params.items) || !params.items.every(isRecord)) {
          reply({ error: { code: -32602, message: 'Expected configuration items' } })
        } else {
          // Return only explicitly configured settings; null preserves server
          // defaults for unknown sections and for every non-Rust session.
          reply({ result: params.items.map(item => this.configuration(item.section)) })
        }
        break
      case 'workspace/workspaceFolders':
        reply({ result: this.workspaceFolders })
        break
      case 'window/showMessageRequest':
        reply({ result: null }) // No action selected; no interactive LSP UI.
        break
      default:
        // Dynamic registration, workspace edits, progress, and refresh requests
        // aren't advertised. Do not pretend to implement them by returning null.
        reply({ error: { code: -32601, message: `Method not supported: ${method}` } })
    }
    return true
  }

  onMessage(callback: (message: string) => void) {
    this.socket.addEventListener('message', (event: MessageEvent<unknown>) => {
      if (typeof event.data !== 'string') return
      let message: unknown
      try {
        message = JSON.parse(event.data)
      } catch {
        return
      }
      // The package dispatches by pending ID before checking method. Server and
      // client IDs are independent: intercept requests, including ID 0, first.
      if (!this.handleServerRequest(message)) {
        if (isRecord(message) && !('id' in message) && typeof message.method === 'string' && isRecord(message.params)) {
          this.onServerNotification(message.method, message.params)
        }
        callback(event.data)
      }
    }, { signal: this.listeners.signal })
  }

  onError(callback: (error: Error) => void) {
    this.socket.addEventListener('error', () => callback(new Error('LSP connection failed')), {
      signal: this.listeners.signal,
    })
  }

  onClose(callback: () => void) {
    this.closeHandlers.push(callback)
  }

  close() {
    if (this.closed) return
    this.closed = true
    this.listeners.abort()
    this.socket.close()
    for (const callback of this.closeHandlers) callback()
  }
}

// Pure construction: no socket/client until this extension belongs to a real
// EditorView. Reusing the extension in a rebuilt view creates a fresh session.
export function lspExtension(config: LspConfig): Extension[] {
  const features = new Compartment()
  const workspaceFolders = [{ uri: config.rootUri, name: config.rootUri }]
  const rust = config.languageId === 'rust'
  return [features.of([]), ViewPlugin.define(view => {
    let stopped = false
    let socket: WebSocket | undefined
    let transport: OpenSocketTransport | undefined
    let connectTimer: ReturnType<typeof setTimeout> | undefined
    const listeners = new AbortController()
    let client: DocumentClient | undefined
    let pendingSaved: string | undefined
    let serverMessage: string | undefined
    let initialized = false
    let indexing = false
    let healthWarning: string | undefined

    function reportStatus() {
      if (stopped) return
      const warning = healthWarning || serverMessage
      // Server health is workspace-wide, not evidence that this file belongs to a crate.
      const scope = rust ? ' Rust workspace status does not confirm this file belongs to a crate.' : ''
      const status: LspStatus = warning
        ? { state: 'warning', message: warning }
        : indexing
          ? { state: 'indexing', message: 'Rust language server is indexing the workspace.' + scope }
          : initialized
            ? { state: 'connected', message: 'Language server connected.' + scope }
            : { state: 'connecting', message: 'Connecting to the language server…' }
      config.onStatus?.(status)
    }

    function serverNotification(method: string, params: Record<string, unknown>) {
      if (stopped) return
      if (method === 'window/showMessage' && (params.type === 1 || params.type === 2) && typeof params.message === 'string') {
        // These messages have no matching clear event; retain the latest warning
        // for this session rather than hide it on a later healthy/indexing update.
        serverMessage = params.message.trim().slice(0, 500) || 'Language server reported a problem.'
      } else if (rust && method === 'experimental/serverStatus' &&
        (params.health === 'ok' || params.health === 'warning' || params.health === 'error') &&
        typeof params.quiescent === 'boolean') {
        indexing = !params.quiescent
        healthWarning = params.health === 'ok' ? undefined
          : (typeof params.message === 'string' ? params.message.trim().slice(0, 500) : '') ||
            `Rust language server reported ${params.health === 'error' ? 'an error' : 'a warning'}.`
      } else return
      reportStatus()
    }
    const persistence = config.persistence
    const unsubscribe = persistence?.events.subscribe(persistence.workspaceId, persistence.path, content => {
      if (stopped) return
      pendingSaved = content
      // Flush after any queued didChange, never directly from a keystroke.
      queueMicrotask(() => client?.flushSaved())
    })

    function fail(message = 'Language server unavailable. It may be at capacity or the connection failed.') {
      if (stopped) return
      config.onStatus?.({ state: 'unavailable', message: healthWarning || serverMessage || message })
      stop()
    }

    function stop() {
      if (stopped) return
      stopped = true
      pendingSaved = undefined
      unsubscribe?.()
      clearTimeout(connectTimer)
      listeners.abort()
      if (transport) transport.close()
      else socket?.close()
    }

    class DocumentClient extends LanguageServerClient {
      private opened = false
      private version = 0

      protected override getInitializeParams() {
        const params = super.getInitializeParams()
        // The upstream client advertises dynamic registration but never applies
        // registrations. Keep static capabilities and advertise only our handlers.
        for (const capability of Object.values<unknown>({ ...params.capabilities.textDocument })) {
          if (isRecord(capability) && 'dynamicRegistration' in capability) capability.dynamicRegistration = false
        }
        params.capabilities.workspace = {
          configuration: true,
          workspaceFolders: true,
          applyEdit: false,
          didChangeConfiguration: { dynamicRegistration: false },
        }
        params.capabilities.window = { workDoneProgress: false }
        if (rust) params.capabilities.experimental = { serverStatusNotification: true }
        params.capabilities.textDocument = {
          ...params.capabilities.textDocument,
          synchronization: { ...params.capabilities.textDocument?.synchronization, didSave: true },
        }
        return params
      }

      override textDocumentDidOpen(params: Parameters<LanguageServerClient['textDocumentDidOpen']>[0]) {
        if (stopped || this.opened) return Promise.resolve()
        this.opened = true
        const opened = super.textDocumentDidOpen({
          ...params,
          textDocument: { ...params.textDocument, text: view.state.doc.toString(), version: this.version },
        })
        void opened.then(() => this.flushSaved()).catch(() => fail())
        return opened
      }

      flushSaved() {
        if (stopped || !this.opened || !this.ready || pendingSaved === undefined) return
        const text = pendingSaved
        pendingSaved = undefined
        const sync = this.capabilities.textDocumentSync
        // Respect SaveOptions, including includeText. The saved snapshot may be
        // older than the current buffer if the user typed while the write ran.
        if (typeof sync !== 'object' || !sync.save) return
        transport?.send(JSON.stringify({
          jsonrpc: '2.0', method: 'textDocument/didSave', params: {
            textDocument: { uri: config.documentUri },
            ...(typeof sync.save === 'object' && sync.save.includeText ? { text } : {}),
          },
        }))
      }

      override textDocumentDidChange(params: Parameters<LanguageServerClient['textDocumentDidChange']>[0]) {
        // The upstream plugin awaits initialization even when already ready.
        // Fold intervening edits into didOpen instead of sending them before it.
        if (stopped || !this.opened) return Promise.resolve()
        return super.textDocumentDidChange({
          ...params,
          // Ignore the plugin's counter (its first didChange also uses zero).
          // Edits folded into didOpen never consume an on-wire version.
          textDocument: { ...params.textDocument, version: ++this.version },
        })
      }
    }

    // Coalesce views constructed and destroyed in the same commit (including
    // StrictMode's effect replay); never open a socket for an abandoned view.
    queueMicrotask(() => {
      if (stopped) return
      try {
        config.onStatus?.({ state: 'connecting', message: 'Connecting to the language server…' })
        socket = new WebSocket(config.serverUri)
        connectTimer = setTimeout(() => fail('Language server connection timed out.'), 10_000)
        socket.addEventListener('close', () => fail(), { signal: listeners.signal })
        socket.addEventListener('error', () => fail(), { signal: listeners.signal })
        socket.addEventListener('open', () => {
          if (stopped || !socket) return
          clearTimeout(connectTimer)
          transport = new OpenSocketTransport(
            socket, workspaceFolders, serverNotification,
            section => rust ? rustConfiguration(section) : null,
          )
          const sessionClient = new DocumentClient({
            ...config,
            transport,
            workspaceFolders,
            initializationOptions: rust ? RUST_RESOURCE_SETTINGS : undefined,
            autoClose: false,
            onError: () => fail('Language server initialization failed or timed out.'),
            onClose: () => fail(),
          })
          client = sessionClient
          const sessionTransport = transport
          // Install only after success, taking the current text rather than a
          // snapshot from before initialization. No retries on capacity denial.
          void sessionClient.initializePromise.then(() => {
            if (stopped || !sessionClient.ready) return
            view.dispatch({ effects: features.reconfigure(languageServerWithTransport({
              ...config,
              workspaceFolders,
              client: sessionClient,
              transport: sessionTransport,
            })) })
            initialized = true
            reportStatus()
          }).catch(() => fail())
        }, { once: true, signal: listeners.signal })
      } catch {
        fail()
      }
    })

    return { destroy() {
      stop()
      config.onStatus?.(null)
    } }
  })]
}
