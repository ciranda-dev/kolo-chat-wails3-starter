/** @typedef {{ id: number, from: string, room: string, text: string, mine: boolean, via: string }} Msg */

/** value when it is a list, else fallback: Go sends a nil slice as null. */
const list = (value, fallback = []) => (Array.isArray(value) ? value : fallback)

/** The state of one conversation in the window, before any of its events. */
export const emptyConv = () => ({
  messages: [],
  peers: 0,
  pin: { error: '', locked: 0 },
  members: [],
  rooms: ['general'],
  requests: [],
  unread: 0,
  draft: '',
})

/** The window's state: the list from Go, the selected conversation and the state of each one. */
export const initialState = { list: [], selected: '', convs: {}, nextId: 1 }

/** The rows the window lists: the conversations, and those that did not start, but not the join that waits. */
export const shownRows = (rows) => list(rows).filter((c) => c?.joined || c?.error)

/** The state of the conversation id, empty until it has events. */
export const convOf = (state, id) => state.convs[id] ?? emptyConv()

/** The payload of "chat:msg" as a Msg with the given id. */
function toMsg(data, id) {
  const item = typeof data === 'object' && data ? data : { text: String(data ?? '') }
  return { id, from: item.from ?? '', room: item.room ?? 'general', text: item.text ?? '', mine: false, via: item.via ?? '' }
}

/**
 * The window's reducer. Each event of a conversation comes as { conv, data }, and goes to that conversation only. A
 * message counts as unread unless its conversation is the selected one. Each conversation keeps its own draft, so a
 * reply typed in one is never sent to another.
 */
export function reduce(state, action) {
  switch (action.type) {
    case 'event':
      return onEvent(state, action.name, action.payload)
    case 'list':
      return onList(state, list(action.list))
    case 'select':
      return { ...state, selected: action.id, convs: { ...state.convs, [action.id]: { ...convOf(state, action.id), unread: 0 } } }
    case 'draft': {
      const c = convOf(state, action.conv)
      return { ...state, convs: { ...state.convs, [action.conv]: { ...c, draft: String(action.text ?? '') } } }
    }
    case 'sent': {
      const c = convOf(state, action.conv)
      const msg = { id: state.nextId, from: action.from, room: action.room, text: action.text, mine: true, via: '' }
      return { ...state, nextId: state.nextId + 1, convs: { ...state.convs, [action.conv]: { ...c, messages: [...c.messages, msg], draft: '' } } }
    }
    case 'directory': {
      const c = convOf(state, action.conv)
      return { ...state, convs: { ...state.convs, [action.conv]: { ...c, members: list(action.members), rooms: list(action.rooms, ['general']) } } }
    }
    case 'request-done': {
      const c = convOf(state, action.conv)
      return { ...state, convs: { ...state.convs, [action.conv]: { ...c, requests: c.requests.filter((r) => r.request !== action.request) } } }
    }
  }
  return state
}

function onEvent(state, name, payload) {
  const conv = typeof payload?.conv === 'string' ? payload.conv : ''
  const data = payload?.data
  if (conv === '') {
    return state
  }
  const c = convOf(state, conv)
  let nextId = state.nextId
  let next
  switch (name) {
    case 'chat:msg':
      next = { ...c, messages: [...c.messages, toMsg(data, nextId++)], unread: conv === state.selected ? 0 : c.unread + 1 }
      break
    case 'chat:peers':
      next = { ...c, peers: Number(data) || 0 }
      break
    case 'chat:pin':
      next = { ...c, pin: { error: data?.error ?? '', locked: data?.locked ?? 0 } }
      break
    case 'chat:profiles':
      next = { ...c, members: list(data) }
      break
    case 'chat:rooms':
      next = { ...c, rooms: list(data, ['general']) }
      break
    case 'chat:invite-request': {
      if (!data) {
        return state
      }
      const rest = c.requests.filter((r) => r.request !== data.request)
      next = { ...c, requests: data.withdrawn ? rest : [...rest, data] }
      break
    }
    default:
      return state
  }
  return { ...state, nextId, convs: { ...state.convs, [conv]: next } }
}

/** A new list from Go. The selection stays, or goes to the first row when it is not listed. */
function onList(state, rows) {
  const shown = shownRows(rows)
  const selected = shown.some((r) => r.id === state.selected) ? state.selected : (shown[0]?.id ?? '')
  return { ...state, list: rows, selected }
}
