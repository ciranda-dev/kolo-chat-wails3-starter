// Checks the sources without any dependency: no em or en dash anywhere; the page keeps its content security policy
// and has no inline script, style or handler (the policy would block them); and no HTML or code from strings, no
// address outside the app, no camera or microphone of the browser. Run it with `npm run lint`.
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'

const policy = /<meta http-equiv="Content-Security-Policy" content="default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'"/

// Words that must not be in the window, built from pieces so this file passes itself.
const banned = [
  ['HTML from a string', new RegExp(['inner' + 'HTML', 'outer' + 'HTML', 'dangerously' + 'Set', 'insertAdjacent' + 'HTML', 'document\\.' + 'write'].join('|'))],
  ['code from a string', new RegExp(['\\bev' + 'al\\(', 'new ' + 'Function\\('].join('|'))],
  ['the camera or microphone of the browser', new RegExp(['getUser' + 'Media', 'media' + 'Devices', 'getDisplay' + 'Media'].join('|'))],
  ['an address outside the app', /https?:\/\/(?!127\.0\.0\.1[:/])/],
]

/** Lists the problems of the frontend at root; an empty list is a pass. */
export function check(root) {
  const files = []
  const walk = (dir) => {
    for (const name of readdirSync(dir)) {
      const path = join(dir, name)
      if (statSync(path).isDirectory()) {
        walk(path)
      } else if (/\.(jsx?|mjs|css|html|json)$/.test(name)) {
        files.push(path)
      }
    }
  }
  for (const dir of ['src', 'test', 'scripts']) {
    walk(join(root, dir))
  }
  files.push(join(root, 'index.html'))

  const problems = []
  for (const file of files) {
    const text = readFileSync(file, 'utf8')
    const name = relative(root, file)
    const shipped = name.startsWith('src/') || name === 'index.html'
    text.split('\n').forEach((line, i) => {
      if (line.includes(String.fromCharCode(0x2014)) || line.includes(String.fromCharCode(0x2013))) {
        problems.push(`${name}:${i + 1}: em or en dash`)
      }
      if (shipped) {
        for (const [what, pattern] of banned) {
          if (pattern.test(line)) {
            problems.push(`${name}:${i + 1}: ${what}`)
          }
        }
      }
    })
    if (name.endsWith('.html') && !policy.test(text)) {
      problems.push(`${name}: no content security policy`)
    }
    if (name.endsWith('.html') && /<script(?![^>]*\bsrc=)|<style|\sstyle=|\son\w+=/.test(text)) {
      problems.push(`${name}: inline script, style or handler (a content security policy would block it)`)
    }
  }
  return problems
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const problems = check(join(fileURLToPath(import.meta.url), '..', '..'))
  if (problems.length > 0) {
    console.error(problems.join('\n'))
    process.exit(1)
  }
  console.log('lint: no problems')
}
