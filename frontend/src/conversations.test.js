import test from 'node:test'
import assert from 'node:assert/strict'
import { convOf, initialState, reduce, shownRows } from './conversations.js'

const rows = [
  { id: 'a', name: 'Family', joined: true, error: '' },
  { id: 'b', name: 'Work', joined: true, error: '' },
  { id: 'p', name: 'Conversation', joined: false, error: '' },
  { id: 'c', name: 'Old', joined: false, error: 'conversation.json: damaged' },
]
const event = (name, conv, data) => ({ type: 'event', name, payload: { conv, data } })
const run = (state, ...actions) => actions.reduce(reduce, state)

test('the list shows the conversations and the failed ones, not the join that waits, and selects the first', () => {
  assert.deepEqual(shownRows(rows).map((r) => r.id), ['a', 'b', 'c'])
  assert.deepEqual(shownRows(null), [])
  const s = run(initialState, { type: 'list', list: rows })
  assert.equal(s.selected, 'a')
  assert.equal(run(s, { type: 'select', id: 'b' }, { type: 'list', list: [...rows, { id: 'd', joined: true }] }).selected, 'b', 'a new row keeps the selection')
  assert.equal(run(initialState, { type: 'list', list: null }).selected, '')
})

test('events go to their conversation, and unread counts only the others', () => {
  let s = run(initialState, { type: 'list', list: rows })
  s = run(s,
    event('chat:msg', 'a', { from: 'x', text: 'seen' }),
    event('chat:msg', 'b', { from: 'x', text: 'one' }),
    event('chat:msg', 'b', { from: 'x', text: 'two' }),
    event('chat:peers', 'b', 3),
    event('chat:profiles', 'a', null),
    event('chat:rooms', 'b', null),
    event('chat:invite-request', 'a', { request: 7, joiner: 'cc' }),
    event('chat:invite-request', 'a', null),
  )
  assert.deepEqual(convOf(s, 'a').messages.map((m) => m.text), ['seen'])
  assert.deepEqual(convOf(s, 'b').messages.map((m) => m.text), ['one', 'two'])
  assert.equal(convOf(s, 'a').unread, 0)
  assert.equal(convOf(s, 'b').unread, 2)
  assert.equal(convOf(s, 'b').peers, 3)
  assert.equal(convOf(s, 'a').peers, 0)
  assert.deepEqual(convOf(s, 'a').members, [], 'a null list stays a list')
  assert.deepEqual(convOf(s, 'b').rooms, ['general'])
  assert.deepEqual(convOf(s, 'a').requests.map((r) => r.request), [7])
  assert.equal(new Set([...convOf(s, 'a').messages, ...convOf(s, 'b').messages].map((m) => m.id)).size, 3, 'ids stay unique')

  s = run(s, { type: 'select', id: 'b' })
  assert.equal(convOf(s, 'b').unread, 0, 'selecting reads it')
  s = run(s, event('chat:msg', 'b', { text: 'three' }), event('chat:msg', 'a', { text: 'four' }))
  assert.equal(convOf(s, 'b').unread, 0)
  assert.equal(convOf(s, 'a').unread, 1)
  assert.equal(run(s, event('chat:msg', '', { text: 'nowhere' })), s, 'an event without a conversation changes nothing')
  s = run(s, { type: 'request-done', conv: 'a', request: 7 }, { type: 'sent', conv: 'b', from: 'me', room: 'general', text: 'mine' })
  assert.deepEqual(convOf(s, 'a').requests, [])
  assert.equal(convOf(s, 'b').messages.at(-1).mine, true)
})

test('each conversation keeps its own draft, and sending clears only its own', () => {
  let s = run(initialState, { type: 'list', list: rows }, { type: 'draft', conv: 'b', text: 'reply to work' }, { type: 'select', id: 'a' })
  assert.equal(convOf(s, 'a').draft, '', 'another conversation starts empty')
  assert.equal(convOf(s, 'b').draft, 'reply to work')
  s = run(s, { type: 'draft', conv: 'a', text: 'for the family' }, { type: 'sent', conv: 'a', from: 'me', room: 'general', text: 'for the family' })
  assert.equal(convOf(s, 'a').draft, '')
  assert.equal(convOf(s, 'b').draft, 'reply to work')
})

test('chat:state reaches its conversation only, with every field in place', () => {
  let state = reduce(initialState, { type: 'list', list: [{ id: 'a', joined: true }, { id: 'b', joined: true }] })
  state = reduce(state, { type: 'event', name: 'chat:state', payload: { conv: 'a', data: { paused: 2, pendingRooms: null } } })
  assert.equal(convOf(state, 'a').state.paused, 2)
  assert.deepEqual(convOf(state, 'a').state.pendingRooms, [])
  assert.equal(convOf(state, 'b').state.paused, 0)
})
