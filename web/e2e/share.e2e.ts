import { test, expect, seed } from './fixtures'

test('owners can open invite links directly beside browser preview', async ({ wede }) => {
  const { page } = wede
  await seed(page, 'authed')
  let createdInvites = 0
  await page.route('**/api/auth/tokens', route => {
    if (route.request().method() === 'POST') createdInvites++
    return route.fulfill({ json: { tokens: [] } })
  })
  await page.goto('/')
  const preview = page.getByRole('button', { name: 'Preview', exact: true })
  const share = page.getByRole('button', { name: 'Share', exact: true })
  await expect(preview).toBeVisible()
  await expect(share).toBeVisible()
  expect(await share.evaluate(button => button.previousElementSibling?.textContent?.trim())).toBe('Preview')
  await share.click()
  await expect(page.getByRole('heading', { name: 'Share Workspace' })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Create Invite Link' })).toBeVisible()
  expect(createdInvites).toBe(0)
})

for (const role of ['editor', 'viewer']) {
  test(`${role}s cannot create invitations from the toolbar`, async ({ wede }) => {
    const { page } = wede
    await seed(page, 'authed')
    await page.route('**/api/auth/check', route => route.fulfill({ json: { authenticated: true, role } }))
    await page.addInitScript(role => localStorage.setItem('wede_role', role), role)
    await page.goto('/')
    await expect(page.getByRole('button', { name: 'Preview', exact: true })).toBeVisible()
    await expect(page.getByRole('button', { name: 'Share', exact: true })).toHaveCount(0)
  })
}
