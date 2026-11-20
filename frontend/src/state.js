/** value when it is a list, else fallback: Go sends a nil slice as null. */
const list = (value, fallback = []) => (Array.isArray(value) ? value : fallback)

/** The payload of "chat:state" with every field in place. */
export const stateStatus = (data) => ({
  singleSettler: data?.singleSettler === true,
  settler: data?.settler === true,
  pending: Number(data?.pending) || 0,
  pendingRooms: list(data?.pendingRooms),
  paused: Number(data?.paused) || 0,
  halted: data?.halted === true,
  settlementHalted: data?.settlementHalted === true,
  diverged: data?.diverged === true,
  unconfirmed: data?.unconfirmed === true,
  strandLost: data?.strandLost === true,
  noAnswer: data?.noAnswer === true,
  otherGroup: data?.otherGroup === true,
  notListed: data?.notListed === true,
})

/** What removing a member does and does not do, shown before an admin confirms it. */
export const removeConfirm = (name) =>
  `Remove ${name} from the member list? Each member's app stops talking to them once it learns of this, and an app ` +
  'that is offline learns it later. They keep the conversation key and everything they received, and an app that has ' +
  'not learned of this still gives them what it has. To keep them out for good, start a new conversation without them.'

/** Whether me is an admin of the member list: who invites, approves, removes and adds rooms. */
export const isAdmin = (members, me) => list(members).some((m) => m.key === me && m.admin)

/** Whether the window offers Remove on m: to admins only, never on the settler, whom nobody can remove, nor on yourself. */
export const canRemove = (members, me, m) => isAdmin(members, me) && !m.seat && m.key !== me

/** The marks next to a member. */
export function memberMarks(m) {
  const marks = []
  if (m.seat) {
    marks.push('settles changes')
  }
  if (m.admin) {
    marks.push('admin')
  }
  if (m.pending) {
    marks.push('pending')
  }
  return marks
}

/** The sentences "chat:state" asks for, most important first. */
export function stateBanners(s) {
  const out = []
  const add = (on, text) => on && out.push(text)
  add(s.diverged, "This app's member and room lists differ from the others'. Join again with an invite.")
  add(s.halted, 'A newer Kolo Chat changed the member or room list: update Kolo Chat to see it. Until then this app does not learn of later changes to the member list, removals included.')
  add(s.settlementHalted, 'Changes to members and rooms can no longer be settled in this conversation. The chat works; start a new conversation to change them again.')
  add(s.strandLost, 'This copy of the conversation can no longer write: ask an admin to invite you again.')
  add(s.otherGroup, 'This computer has another member list for this conversation: join with an invite.')
  add(s.notListed, 'Waiting for a member to come online with the member list. Until then this conversation shows nothing, not even from the pin.')
  add(s.noAnswer, 'Members are online but none answers. You may have been removed from the member list.')
  add(s.paused > 0, `Members and rooms are not up to date with ${s.paused} member(s) right now. The chat works.`)
  add(s.unconfirmed && !s.strandLost, 'Waiting for another member to come online.')
  return out
}

/**
 * The rooms the sidebar lists: the room list, then the rooms that messages name and the list does not have yet, which
 * the window dims. Each is { name, known, pending }.
 */
export function shownRooms(rooms, messages, pendingRooms = []) {
  const known = list(rooms, ['general'])
  const out = known.map((name) => ({ name, known: true, pending: list(pendingRooms).includes(name) }))
  const unknown = [...new Set(list(messages).map((m) => m.room).filter((r) => r && !known.includes(r)))].sort()
  return [...out, ...unknown.map((name) => ({ name, known: false, pending: false }))]
}
