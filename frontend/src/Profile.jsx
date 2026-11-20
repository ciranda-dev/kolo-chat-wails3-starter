import { useState } from 'react'
import { AddRoom, Invites, RemoveMember, Revoke, SetProfile } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/conversations.js"
import { canRemove, isAdmin, memberMarks, removeConfirm } from './state.js'

/** value when it is a list, else fallback: Go sends a nil slice as null. */
const list = (value, fallback = []) => (Array.isArray(value) ? value : fallback)

/** @typedef {{ key: string, name: string, avatar: string, admin: boolean, seat: boolean, pending: boolean }} Member */

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

/**
 * The rooms and the member list of the conversation conv, as the braid agrees them. Admins add rooms and remove
 * members (never the settler), after a confirmation that says what removal does not do. A room that a message names
 * before the room list has it is dimmed; changes the settler has not settled yet are marked pending.
 */
export function Sidebar({ conv, rooms, room, onRoom, members, me }) {
  const [adding, setAdding] = useState('')
  const [removing, setRemoving] = useState(/** @type {Member | null} */ (null))
  const [revoke, setRevoke] = useState(true)
  const [error, setError] = useState('')
  const admin = isAdmin(members, me)

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

  const remove = async () => {
    const who = removing
    setRemoving(null)
    setError('')
    try {
      await RemoveMember(conv, who.key)
      if (revoke) {
        for (const i of list(await Invites(conv))) {
          if (i.state === 'active') {
            await Revoke(conv, i.id)
          }
        }
      }
    } catch (err) {
      setError(errorText(err))
    }
  }

  return (
    <aside className="sidebar">
      <h2 className="sidebar__title">Rooms</h2>
      <ul className="sidebar__list">
        {rooms.map((r) => (
          <li key={r.name}>
            <button type="button" className={`room ${r.name === room ? 'is-active' : ''} ${r.known ? '' : 'is-unknown'}`}
              title={r.known ? undefined : 'not in your room list yet'} onClick={() => onRoom(r.name)}>
              # {r.name}
              {r.pending ? <span className="member__tag">pending</span> : null}
            </button>
          </li>
        ))}
      </ul>
      {admin ? (
        <form className="sidebar__add" onSubmit={add}>
          <input className="field" aria-label="New room" placeholder="New room" value={adding} maxLength={32} onChange={(ev) => setAdding(ev.target.value)} autoComplete="off" />
        </form>
      ) : null}
      <h2 className="sidebar__title">Members</h2>
      <ul className="sidebar__list">
        {members.map((m) => (
          <li key={m.key} className="member" title={m.key}>
            <Avatar member={m} />
            <span className="member__name">{labelFor(members, m.key)}</span>
            {memberMarks(m).map((k) => <span key={k} className="member__tag">{k}</span>)}
            {canRemove(members, me, m) ? (
              <button type="button" className="member__remove" onClick={() => setRemoving(m)}>Remove</button>
            ) : null}
          </li>
        ))}
      </ul>
      {removing ? (
        <div className="confirm" role="alert">
          <p className="note">{removeConfirm(labelFor(members, removing.key))}</p>
          <label className="check">
            <input type="checkbox" checked={revoke} onChange={(ev) => setRevoke(ev.target.checked)} />
            Also revoke your active invites for this conversation
          </label>
          <p className="note">With an invite that has uses left they can ask to join again.</p>
          <div className="panel__row">
            <button type="button" className="btn" onClick={remove}>Remove</button>
            <button type="button" className="btn btn--ghost" onClick={() => setRemoving(null)}>Cancel</button>
          </div>
        </div>
      ) : admin ? <p className="note">Removing someone from the member list is not revocation: they keep the conversation key.</p> : null}
      {error ? <p className="error">{error}</p> : null}
    </aside>
  )
}
