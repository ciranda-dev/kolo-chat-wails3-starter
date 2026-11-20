import test from 'node:test'
import assert from 'node:assert/strict'
import { appendMessage, cut, MAX_FROM_CHARS, MAX_MESSAGES, MAX_TEXT_CHARS } from '../src/messages.js'

const msg = (id, text = 'hi', from = 'abcdef0123456789') => ({ id, from, text, mine: false })

test('the list keeps the last MAX_MESSAGES messages', () => {
  let list = []
  for (let i = 1; i <= MAX_MESSAGES + 25; i++) {
    list = appendMessage(list, msg(i))
  }
  assert.equal(list.length, MAX_MESSAGES)
  assert.equal(list[0].id, 26)
  assert.equal(list.at(-1).id, MAX_MESSAGES + 25)
})

test('a long text and a long sender are cut', () => {
  const [m] = appendMessage([], msg(1, 'x'.repeat(MAX_TEXT_CHARS * 50), 'f'.repeat(1000)))
  assert.equal(Array.from(m.text).length, MAX_TEXT_CHARS + 1)
  assert.ok(m.text.endsWith('…'))
  assert.equal(Array.from(m.from).length, MAX_FROM_CHARS + 1)
})

test('cut keeps a short text and does not split a character made of two code units', () => {
  assert.equal(cut('hello', 5), 'hello')
  const emoji = '\u{1F600}'.repeat(10)
  assert.equal(cut(emoji, 4), '\u{1F600}'.repeat(4) + '…')
})

test('appendMessage does not change the list it is given', () => {
  const list = [msg(1)]
  appendMessage(list, msg(2))
  assert.equal(list.length, 1)
})
