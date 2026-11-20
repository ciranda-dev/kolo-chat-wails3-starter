/** The most messages the window keeps, and the longest text and sender it shows: a peer chooses what it sends. */
export const MAX_MESSAGES = 500
export const MAX_TEXT_CHARS = 2000
export const MAX_FROM_CHARS = 64

/** Cuts text to max characters (by code point) and ends it with an ellipsis. */
export function cut(text, max) {
  if (text.length <= max) {
    return text
  }
  return Array.from(text).slice(0, max).join('') + '…'
}

/** The list with msg added at the end, cut to its limits; only the last MAX_MESSAGES stay. */
export function appendMessage(list, msg) {
  const next = { ...msg, from: cut(msg.from, MAX_FROM_CHARS), text: cut(msg.text, MAX_TEXT_CHARS) }
  return [...list, next].slice(-MAX_MESSAGES)
}
