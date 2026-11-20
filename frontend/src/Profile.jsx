import { useState } from 'react'
import { SetProfile, AddRoom } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/conversations.js"

/** @typedef {{ key: string, name: string, avatar: string, owner: boolean }} Member */

const shortId = (id) => (id ? id.slice(0, 8) : '')

/** Turns whatever a rejected call threw into a string for the UI. */
function errorText(err) {
  if (err instanceof Error && err.message) {
    return err.message
  }
  return typeof err === 'string' ? err : String(err)
}

/**
 * The name to show for a member: its profile name, or the short key until its profile has been read. A name is
 * not an identity, so two members with the same name get the start of their keys too.
 * @param {Member[]} members
 * @param {string} key
 */
export function labelFor(members, key) {
  const m = members.find((x) => x.key === key)
  if (!m || !m.name) {
    return shortId(key)
  }
  const twin = members.some((x) => x.key !== key && x.name === m.name)
  return twin ? `${m.name} #${key.slice(0, 4)}` : m.name
}

/** A round badge with the member's avatar reference, or the first letter of its name. */
function Avatar({ member }) {
  return <span className="avatar" aria-hidden="true">{member.avatar || (member.name || '?').slice(0, 1).toUpperCase()}</span>
}

/** Change your name and avatar in the conversation conv. The avatar is a short reference, such as an emoji, not an image. */
export function ProfilePanel({ conv, mine, onDone }) {
  const [name, setName] = useState(mine?.name ?? '')
  const [avatar, setAvatar] = useState(mine?.avatar ?? '')
  const [error, setError] = useState('')

  const save = (ev) => {
    ev.preventDefault()
    setError('')
    SetProfile(conv, name, avatar)
      .then(() => onDone())
      .catch((err) => setError(errorText(err)))
  }

  return (
    <section className="panel">
      <form className="panel__row" onSubmit={save}>
        <label className="panel__label">
          Name
          <input className="field" value={name} maxLength={40} onChange={(ev) => setName(ev.target.value)} autoComplete="off" />
        </label>
        <label className="panel__label">
          Avatar
          <input className="field field--narrow" value={avatar} maxLength={16} onChange={(ev) => setAvatar(ev.target.value)} placeholder="🦊" autoComplete="off" />
        </label>
        <button type="submit" className="btn" disabled={!name.trim()}>Save</button>
      </form>
      {error ? <p className="error">{error}</p> : null}
    </section>
  )
}

/** The rooms (the owner can add one) and the members of the conversation conv. */
export function Sidebar({ conv, rooms, room, onRoom, members, isOwner }) {
  const [adding, setAdding] = useState('')
  const [error, setError] = useState('')

  const add = (ev) => {
    ev.preventDefault()
    if (!adding.trim()) {
      return
    }
    setError('')
    AddRoom(conv, adding)
      .then(() => setAdding(''))
      .catch((err) => setError(errorText(err)))
  }

  return (
    <aside className="sidebar">
      <h2 className="sidebar__title">Rooms</h2>
      <ul className="sidebar__list">
        {rooms.map((r) => (
          <li key={r}>
            <button type="button" className={`room ${r === room ? 'is-active' : ''}`} onClick={() => onRoom(r)}>
              # {r}
            </button>
          </li>
        ))}
      </ul>
      {isOwner ? (
        <form className="sidebar__add" onSubmit={add}>
          <input className="field" aria-label="New room" placeholder="New room" value={adding} maxLength={32} onChange={(ev) => setAdding(ev.target.value)} autoComplete="off" />
        </form>
      ) : null}
      {error ? <p className="error">{error}</p> : null}
      <h2 className="sidebar__title">Members</h2>
      <ul className="sidebar__list">
        {members.map((m) => (
          <li key={m.key} className="member" title={m.key}>
            <Avatar member={m} />
            <span className="member__name">{labelFor(members, m.key)}</span>
            {m.owner ? <span className="member__tag">owner</span> : null}
          </li>
        ))}
      </ul>
    </aside>
  )
}
