// useYDoc — opens a Yjs document synced to the server's CRDT doc for one file.
//
// Connects a y-websocket WebsocketProvider to the ygo doc endpoint
// /api/workspaces/{id}/doc/{workspace} where {workspace} is base64url(relative path) (matching
// the Go backend's decodeRoom). The provider speaks the y-protocols sync +
// awareness wire format that ygo's provider/websocket implements. Exposes the
// shared Y.Text 'content' plus the provider/awareness so the editor can bind a
// yCollab extension and render remote cursors.
//
// Connection failures never discard the document: the provider reconnects with
// its existing Y.Doc, including edits not yet delivered to the server.

import { useEffect, useState } from 'react'
import * as Y from 'yjs'
import { WebsocketProvider } from 'y-websocket'
import type { Awareness } from 'y-protocols/awareness'

export interface UseYDocParams {
  workspaceId: string | null | undefined
  path: string | null | undefined
  token: string | null | undefined
  username?: string
  color?: string
}

export type CollaborationStatus = 'off' | 'connecting' | 'syncing' | 'live' | 'disconnected' | 'error'

export interface UseYDocState {
  status: CollaborationStatus
  ytext: Y.Text | null
  provider: WebsocketProvider | null
  awareness: Awareness | null
}

const INACTIVE: UseYDocState = { ytext: null, provider: null, awareness: null, status: 'off' }

// b64urlPath encodes a (possibly UTF-8) path as base64url without padding —
// byte-for-byte compatible with Go's base64.RawURLEncoding used by decodeRoom.
export function b64urlPath(path: string): string {
  const b64 = btoa(unescape(encodeURIComponent(path)))
  return b64.replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

function docBaseUrl(workspaceId: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  const port = window.location.port
  const host = (port === '5173' || port === '5174')
    ? window.location.hostname + ':9090'
    : window.location.host
  return `${proto}//${host}/api/workspaces/${encodeURIComponent(workspaceId)}/doc`
}

export function useYDoc({ workspaceId, path, token, username, color }: UseYDocParams): UseYDocState {
  const identity = JSON.stringify([workspaceId, path, token])
  const [state, setState] = useState<{ identity: string; value: UseYDocState } | null>(null)

  /* eslint-disable react-hooks/set-state-in-effect */
  useEffect(() => {
    if (!workspaceId || !path || !token) {
      setState(null)
      return undefined
    }

    let active = true
    let ydoc: Y.Doc | undefined
    let provider: WebsocketProvider | undefined
    try {
      ydoc = new Y.Doc()
      // y-websocket builds the socket URL as `${base}/${workspace}?${params}`, which
      // matches our route /api/workspaces/{id}/doc/{workspace...} + ?token= (the auth
      // middleware authenticates WS upgrades via ?token=).
      provider = new WebsocketProvider(docBaseUrl(workspaceId), b64urlPath(path), ydoc, {
        params: { token },
        connect: true,
      })
      const ytext = ydoc.getText('content')
      const session = provider
      const publish = (status: CollaborationStatus) => {
        if (active) setState({ identity, value: { ytext, provider: session, awareness: session.awareness, status } })
      }
      session.on('status', ({ status }: { status: string }) => {
        publish(status === 'connected' ? (session.synced ? 'live' : 'syncing')
          : status === 'connecting' ? 'connecting' : 'disconnected')
      })
      session.on('sync', (synced: boolean) => {
        publish(session.wsconnected ? (synced ? 'live' : 'syncing') : 'disconnected')
      })
      session.on('connection-error', () => publish('error'))
      session.on('connection-close', () => publish('disconnected'))
      publish(session.wsconnected ? (session.synced ? 'live' : 'syncing') : 'connecting')
    } catch {
      // Surface failure so the caller can block editing rather than REST fallback.
      try { if (provider) provider.destroy() } catch { /* ignore */ }
      try { if (ydoc) ydoc.destroy() } catch { /* ignore */ }
      setState({ identity, value: { ...INACTIVE, status: 'error' } })
      return undefined
    }

    return () => {
      active = false
      try { provider?.destroy() } catch { /* ignore */ }
      try { ydoc?.destroy() } catch { /* ignore */ }
    }
  }, [workspaceId, path, token, identity])
  /* eslint-enable react-hooks/set-state-in-effect */

  const awareness = state?.identity === identity ? state.value.awareness : null
  useEffect(() => {
    awareness?.setLocalStateField('user', { name: username || 'anon', color: color || '#888' })
  }, [awareness, username, color])

  // Effects replace the provider after commit. Never let that intervening render
  // bind the new file's editor to the previous file's (possibly destroyed) Y.Text.
  return state?.identity === identity ? state.value
      : { ...INACTIVE, status: path ? (workspaceId && token ? 'connecting' : 'error') : 'off' }
}
