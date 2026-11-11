import type { EditorSettings } from '../types'

export function collaborationRequired(): boolean {
  return import.meta.env.VITE_WEDE_REQUIRE_COLLAB === 'true'
}

export function effectiveCollaboration(settings: Pick<EditorSettings, 'collab'>): boolean {
  return collaborationRequired() || (settings.collab ?? false)
}
