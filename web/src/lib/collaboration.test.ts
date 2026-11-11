import { afterEach, expect, it, vi } from 'vitest'
import { collaborationRequired, effectiveCollaboration } from './collaboration'

afterEach(() => vi.unstubAllEnvs())

it('forces collaboration over persisted false without changing the settings', () => {
  vi.stubEnv('VITE_WEDE_REQUIRE_COLLAB', 'true')
  const settings = { collab: false }
  expect(collaborationRequired()).toBe(true)
  expect(effectiveCollaboration(settings)).toBe(true)
  expect(settings.collab).toBe(false)
  expect(effectiveCollaboration({})).toBe(true)
})

it.each([undefined, 'false', '1', 'TRUE'])('preserves upstream opt-in when flag is %s', flag => {
  vi.stubEnv('VITE_WEDE_REQUIRE_COLLAB', flag)
  expect(collaborationRequired()).toBe(false)
  expect(effectiveCollaboration({ collab: false })).toBe(false)
  expect(effectiveCollaboration({})).toBe(false)
  expect(effectiveCollaboration({ collab: true })).toBe(true)
})
