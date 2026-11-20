import { useState } from 'react'
import { Create } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/conversations.js"
import { networkLine } from './network.js'

/** Turns whatever a rejected call threw into a string for the UI. */
const errorText = (err) => (err instanceof Error && err.message ? err.message : String(err))

/**
 * The column of conversations, the full height of the window: each with its unread count and a mark when one did not
 * start, then "New conversation" (a name, then Create) and "Join with an invite", which opens the join panel,
 * and a line that says which network the app joins.
 */
export function ConversationList({ rows, selected, convs, canCreate, network, max, count, pending, onSelect, onCreated, onJoin }) {
  const [naming, setNaming] = useState(false)
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const full = max > 0 && count >= max
  const joinFull = full && !pending // a join takes the folder of the one that waits

  const create = (ev) => {
    ev.preventDefault()
    setError('')
    Create(name)
      .then((info) => {
        setName('')
        setNaming(false)
        onCreated(info)
      })
      .catch((err) => setError(errorText(err)))
  }

  return (
    <nav className="convs" aria-label="Conversations">
      <h2 className="sidebar__title">Conversations</h2>
      {rows.length === 0 ? <p className="note convs__empty">No conversation yet. Start one, or paste an invite.</p> : null}
      <ul className="sidebar__list">
        {rows.map((r) => {
          const unread = convs[r.id]?.unread ?? 0
          return (
            <li key={r.id}>
              <button type="button" className={`conv ${r.id === selected ? 'is-active' : ''}`} aria-current={r.id === selected} onClick={() => onSelect(r.id)}>
                <span className="conv__name">{r.name}</span>
                {r.error ? <span className="conv__failed" title="This conversation could not start.">!</span> : null}
                {unread > 0 ? <span className="conv__unread">{unread} new</span> : null}
              </button>
            </li>
          )
        })}
      </ul>
      <div className="convs__actions">
        {naming ? (
          <form className="convs__new" onSubmit={create}>
            <label className="panel__label">
              Name
              <input className="field" value={name} maxLength={32} placeholder="Family, Work..." onChange={(ev) => setName(ev.target.value)} autoComplete="off" autoFocus />
            </label>
            <p className="note">Only you see this name.</p>
            <div className="panel__row">
              <button type="submit" className="btn">Create</button>
              <button type="button" className="btn btn--ghost" onClick={() => setNaming(false)}>Cancel</button>
            </div>
          </form>
        ) : (
          <button type="button" className="btn btn--ghost" disabled={!canCreate || full} onClick={() => setNaming(true)}>New conversation</button>
        )}
        <button type="button" className="btn btn--ghost" disabled={joinFull} onClick={onJoin}>Join with an invite</button>
        {!canCreate ? <p className="note">Start Kolo Chat with -pin-key to start a conversation. You can still join one with an invite.</p> : null}
        {full ? <p className="note">You have {max} conversations, the most this app keeps.</p> : null}
        {error ? <p className="error">{error}</p> : null}
        <p className="note" title={network.nodes.join(', ')}>{networkLine(network)}</p>
      </div>
    </nav>
  )
}
