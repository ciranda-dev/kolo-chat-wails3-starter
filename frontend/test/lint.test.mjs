import test from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { check } from '../scripts/lint.mjs'

const real = join(fileURLToPath(import.meta.url), '..', '..')

test('the page of the starter passes the lint', () => {
  assert.deepEqual(check(real), [])
})

test('the page enforces a strict content security policy', () => {
  const html = readFileSync(join(real, 'index.html'), 'utf8')
  assert.match(html, /default-src 'self'/)
  assert.match(html, /script-src 'self'/)
  assert.match(html, /object-src 'none'/)
})

/** A frontend folder with the given index.html and one source file. */
function fixture(html, source) {
  const root = mkdtempSync(join(tmpdir(), 'kolo-lint-'))
  for (const dir of ['src', 'test', 'scripts']) {
    mkdirSync(join(root, dir))
  }
  writeFileSync(join(root, 'index.html'), html)
  writeFileSync(join(root, 'src', 'x.js'), source)
  return root
}

test('the lint refuses a page without the policy, inline script, dashes and remote addresses', () => {
  const html = fixture('<html><script>1</script></html>', `// ${String.fromCharCode(0x2014)} ${String.fromCharCode(0x2013)}\nfetch('https://example.org')\n`)
  try {
    const out = check(html).join('\n')
    assert.match(out, /no content security policy/)
    assert.match(out, /inline script/)
    assert.match(out, /em or en dash/)
    assert.match(out, /an address outside the app/)
  } finally {
    rmSync(html, { recursive: true })
  }
})
