import test from 'node:test'
import assert from 'node:assert/strict'
import { canRemove, isAdmin, memberMarks, removeConfirm, shownRooms, stateBanners, stateStatus } from './state.js'

const founder = { key: 'a', admin: true, seat: true }
const admin = { key: 'b', admin: true }
const member = { key: 'c', pending: true }
const members = [founder, admin, member]

test('Remove is for admins only, never on the settler, never on yourself', () => {
  assert.ok(isAdmin(members, 'b') && !isAdmin(members, 'c') && !isAdmin(null, 'a'))
  assert.ok(canRemove(members, 'b', member))
  assert.ok(!canRemove(members, 'b', founder), 'the settler cannot be removed')
  assert.ok(!canRemove(members, 'c', admin), 'a member removes nobody')
  assert.ok(!canRemove(members, 'b', admin), 'not yourself')
  assert.ok(canRemove(members, 'a', admin), 'the founder removes another admin')
})

test('marks for the settler, admins and pending members', () => {
  assert.deepEqual(memberMarks(founder), ['settles changes', 'admin'])
  assert.deepEqual(memberMarks(member), ['pending'])
  assert.deepEqual(memberMarks({ key: 'd' }), [])
})

test('each state has its sentence, and a missing payload has none', () => {
  assert.deepEqual(stateBanners(stateStatus(null)), [])
  const all = stateStatus({ halted: true, settlementHalted: true, diverged: true, unconfirmed: true, noAnswer: true, otherGroup: true, notListed: true, paused: 2 })
  assert.equal(stateBanners(all).length, 8)
  assert.ok(stateBanners(all).some((s) => s.includes('2 member(s)')))
  assert.equal(stateBanners(stateStatus({ strandLost: true, unconfirmed: true })).length, 1)
  assert.deepEqual(stateStatus({ pendingRooms: null }).pendingRooms, [])
})

test('a room that messages name before the list has it shows, marked unknown', () => {
  const rooms = shownRooms(['general', 'dev'], [{ room: 'design' }, { room: 'dev' }, { room: 'design' }], ['dev'])
  assert.deepEqual(rooms, [
    { name: 'general', known: true, pending: false },
    { name: 'dev', known: true, pending: true },
    { name: 'design', known: false, pending: false },
  ])
  assert.deepEqual(shownRooms(null, null), [{ name: 'general', known: true, pending: false }])
})

test('the removal sentence says they keep the key, and never claims more', () => {
  const text = removeConfirm('Carol')
  assert.match(text, /keep the conversation key/)
  for (const word of ['revoke', 'ban', 'kick', 'everyone']) {
    assert.ok(!text.toLowerCase().includes(word), word)
  }
})
