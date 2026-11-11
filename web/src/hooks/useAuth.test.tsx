// @vitest-environment jsdom
import { useEffect } from 'react'
import { createRoot, type Root } from 'react-dom/client'
import { beforeEach, afterEach, expect, it, vi } from 'vitest'
import { commit } from '../test/react'
import { useAuth } from './useAuth'
import { useCollab } from './useCollab'

function deferred<T>() {
  let resolve!: (value: T) => void
  const promise = new Promise<T>(r => { resolve = r })
  return { promise, resolve }
}
const response = (data: unknown, status = 200) => new Response(JSON.stringify(data), { status })
let root: Root
let auth: ReturnType<typeof useAuth>
let check: ReturnType<typeof deferred<Response>>
let rename: ReturnType<typeof deferred<Response>>
let fetchMock: ReturnType<typeof vi.fn>
let sockets: { url: string; close: ReturnType<typeof vi.fn> }[]
function Harness() {
  const current = useAuth()
  useCollab('workspace', current.token, current.username)
  useEffect(() => { auth = current })
  return null
}
beforeEach(async () => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true)
  localStorage.clear()
  localStorage.setItem('wede_token', 'session')
  localStorage.setItem('wede_username', 'old')
  check = deferred<Response>()
  rename = deferred<Response>()
  sockets = []
  vi.stubGlobal('WebSocket', class {
    close = vi.fn()
    constructor(public url: string) { sockets.push(this) }
  })
  fetchMock = vi.fn((url: string, options?: RequestInit) => {
    if (url.endsWith('/check')) return (options?.headers as Record<string, string>).Authorization === 'session'
      ? check.promise : Promise.resolve(response({ authenticated: true, username: 'new-user' }))
    if (url.endsWith('/username')) return rename.promise
    if (url.endsWith('/login')) return Promise.resolve(response({ token: 'new-session', username: 'new-user' }))
    return Promise.resolve(response({}))
  })
  vi.stubGlobal('fetch', fetchMock)
  root = createRoot(document.createElement('div'))
  await commit(() => root.render(<Harness />))
})
afterEach(async () => {
  await commit(() => root.unmount())
  vi.unstubAllGlobals()
  localStorage.clear()
})
it('hydrates the server name and role', async () => {
  await commit(() => check.resolve(response({ authenticated: true, username: 'server', role: 'editor' })))
  expect(auth.username).toBe('server')
  expect(auth.role).toBe('editor')
  expect(localStorage.getItem('wede_username')).toBe('server')
})
it('only publishes a successful canonical name, then reconnects presence without a username query', async () => {
  let result!: Promise<string>
  await commit(() => { result = auth.updateUsername('  requested  ') })
  expect(auth.username).toBe('old')
  expect(sockets).toHaveLength(1)
  expect(fetchMock).toHaveBeenCalledWith('/api/auth/username', {
    method: 'POST', headers: { Authorization: 'session', 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: 'requested' }),
  })
  await commit(() => rename.resolve(response({ username: 'canonical' })))
  await expect(result).resolves.toBe('canonical')
  expect(auth.username).toBe('canonical')
  expect(localStorage.getItem('wede_username')).toBe('canonical')
  expect(sockets).toHaveLength(2)
  expect(sockets[0].close).toHaveBeenCalledOnce()
  expect(new URL(sockets[1].url).searchParams.has('username')).toBe(false)
  await commit(() => check.resolve(response({ authenticated: true, username: 'stale' })))
  expect(auth.username).toBe('canonical')
})
it.each([400, 401, 500])('keeps the old name and presence on HTTP %s', async status => {
  const result = auth.updateUsername('requested').catch((error: unknown) => error)
  await commit(() => rename.resolve(response({}, status)))
  expect(await result).toBeInstanceOf(Error)
  expect(auth.username).toBe('old')
  expect(localStorage.getItem('wede_username')).toBe('old')
  expect(sockets).toHaveLength(1)
})
it('ignores hydration arriving during a pending rename', async () => {
  const result = auth.updateUsername('requested')
  await commit(() => check.resolve(response({ authenticated: false })))
  expect(auth.token).toBe('session')
  await commit(() => rename.resolve(response({ username: 'canonical' })))
  await expect(result).resolves.toBe('canonical')
})
it.each(['logout', 'login'] as const)('ignores stale check and rename after %s', async action => {
  const result = auth.updateUsername('requested').catch((error: unknown) => error)
  await commit(() => { if (action === 'logout') auth.logout(); else void auth.login('password') })
  await commit(() => {
    check.resolve(response({ authenticated: false, username: 'stale' }))
    rename.resolve(response({ username: 'stale' }))
  })
  expect(await result).toBeInstanceOf(Error)
  expect(auth.token).toBe(action === 'logout' ? null : 'new-session')
  expect(auth.username).toBe(action === 'logout' ? '' : 'new-user')
})
it('rejects malformed success and network failure without changing the name', async () => {
  fetchMock.mockRejectedValueOnce(new Error('offline'))
  await expect(auth.updateUsername('requested')).rejects.toThrow('offline')
  const result = auth.updateUsername('requested').catch((error: unknown) => error)
  await commit(() => rename.resolve(response({})))
  expect(await result).toBeInstanceOf(Error)
  expect(auth.username).toBe('old')
  expect(localStorage.getItem('wede_username')).toBe('old')
})
