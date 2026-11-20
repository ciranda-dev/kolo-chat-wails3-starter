import { useState, useEffect, useReducer, useRef } from 'react'
import { Start, Send, Members, Rooms } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/conversations.js"
import { Events } from "@wailsio/runtime"
import logo from './assets/kolo-mark.svg'
import { ConversationList } from './Conversations.jsx'
import { JoinPanel, InvitePanel, Requests } from './Invite.jsx'
import { Modal } from './Modal.jsx'
import { ProfilePanel, Sidebar, labelFor } from './Profile.jsx'
import { convOf, initialState, reduce, shownRows } from './conversations.js'
import './chat.css'

/** The events of the conversations, each { conv, data }; "chat:join" and "chat:invite" go to their panels. */
const CHAT_EVENTS = ['chat:msg', 'chat:peers', 'chat:pin', 'chat:profiles', 'chat:rooms', 'chat:invite-request']

/** First 8 characters of a kolo-id, enough to tell peers apart. */
const shortId = (id) => (id ? id.slice(0, 8) : '')

/** Turns whatever a rejected call threw into a string for the UI. */
function errorText(err) {
  if (err instanceof Error && err.message) {
    return err.message
  }
  if (typeof err === 'string') {
    return err
  }
  return String(err)
}

function App() {
  const [state, dispatch] = useReducer(reduce, initialState)
  const [app, setApp] = useState({ maxConversations: 0, canCreate: false })
  const [startError, setStartError] = useState('')
  const [error, setError] = useState('')
  const [room, setRoom] = useState('general')
  const [joining, setJoining] = useState(false)
  const [inviting, setInviting] = useState(false)
  const [editing, setEditing] = useState(false)
  const bottomRef = useRef(null)

  const rows = shownRows(state.list)
  const row = rows.find((r) => r.id === state.selected)
  const conv = convOf(state, state.selected)
  const joined = row?.joined === true && !row.error
  const pending = state.list.find((r) => !r.joined && !r.error)

  /** The members and the rooms of a conversation, which its engine told before the window listened. */
  const loadDirectory = (id) => {
    Promise.all([Members(id), Rooms(id)])
      .then(([members, rooms]) => dispatch({ type: 'directory', conv: id, members, rooms }))
      .catch((err) => setError(errorText(err)))
  }

  useEffect(() => {
    const offs = CHAT_EVENTS.map((name) => Events.On(name, (e) => dispatch({ type: 'event', name, payload: e.data })))
    offs.push(Events.On('chat:conversations', (e) => dispatch({ type: 'list', list: e.data })))
    Start()
      .then((info) => {
        setApp({ maxConversations: info.maxConversations, canCreate: info.canCreate })
        dispatch({ type: 'list', list: info.conversations })
        for (const c of shownRows(info.conversations)) {
          if (c.joined && !c.error) {
            loadDirectory(c.id)
          }
        }
      })
      .catch((err) => setStartError(errorText(err)))
    return () => offs.forEach((off) => off())
  }, [])

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: 'end' })
  }, [conv.messages])

  const select = (id) => {
    dispatch({ type: 'select', id })
    setRoom('general')
    setJoining(false)
    setError('')
  }

  /** A conversation made or joined here: select it and read its members and rooms. */
  const added = (info) => {
    select(info.id)
    loadDirectory(info.id)
  }

  const send = () => {
    const text = conv.draft.trim()
    if (!text || !row) {
      return
    }
    const id = row.id
    Send(id, room, text)
      .then(() => {
        setError('')
        dispatch({ type: 'sent', conv: id, from: row.me, room, text })
      })
      .catch((err) => setError(errorText(err)))
  }

  const mine = conv.members.find((m) => m.key === row?.me)
  const shown = conv.messages.filter((m) => m.room === room)
  const status = startError || row?.error ? 'error' : conv.peers > 0 ? 'live' : 'waiting'
  const badge = status === 'live' ? `${conv.peers} ${conv.peers === 1 ? 'peer' : 'peers'}` : status === 'error' ? 'Error' : 'Waiting for peer'
  // The main column: the join panel, also when there is no conversation, the reason one did not start, or the chat.
  const view = startError ? 'start-error' : joining || rows.length === 0 ? 'join' : row?.error ? 'failed' : 'chat'

  return (
    <div className="app">
      <header className="header">
        <img className="header__logo" src={logo} alt="" width={32} height={32} />
        <span className="header__name">kolo</span>
        <span className="header__spacer" />
        {joined ? (
          <span className="header__id" title={row.me}>
            {row.name} · you · {mine?.name || shortId(row.me)}
          </span>
        ) : null}
        {joined ? (
          <button type="button" className="header__btn" onClick={() => setEditing((open) => !open)}>
            Profile
          </button>
        ) : null}
        {joined ? (
          <button type="button" className="header__btn" onClick={() => setInviting((open) => !open)}>
            Invite{conv.requests.length > 0 ? ` (${conv.requests.length})` : ''}
          </button>
        ) : null}
        <span className={`header__badge is-${status}`}>{badge}</span>
      </header>

      {joined && inviting ? (
        <Modal title={`Invite someone to ${row.name}`} onClose={() => setInviting(false)}>
          <InvitePanel conv={row.id} />
        </Modal>
      ) : null}
      {joined && editing ? (
        <Modal title={`Your profile in ${row.name}`} onClose={() => setEditing(false)}>
          <ProfilePanel conv={row.id} mine={mine} onDone={() => setEditing(false)} />
        </Modal>
      ) : null}

      <div className="body">
        {startError ? null : (
          <ConversationList
            rows={rows}
            selected={view === 'join' ? '' : state.selected}
            convs={state.convs}
            canCreate={app.canCreate}
            max={app.maxConversations}
            count={state.list.length}
            pending={Boolean(pending)}
            onSelect={select}
            onCreated={added}
            onJoin={() => setJoining(true)}
          />
        )}
        {joined && view === 'chat' ? (
          <Sidebar conv={row.id} rooms={conv.rooms} room={room} onRoom={setRoom} members={conv.members} isOwner={row.owner} />
        ) : null}
        <div className="main">
          {view === 'join' ? <JoinPanel empty={rows.length === 0} onJoined={added} /> : null}
          {view === 'start-error' ? (
            <div className="panel panel--center">
              <h2 className="panel__title">Kolo Chat could not start</h2>
              <p className="error">{startError}</p>
            </div>
          ) : null}
          {view === 'failed' ? (
            <div className="panel panel--center">
              <h2 className="panel__title">This conversation could not start.</h2>
              <p className="error">{row.error}</p>
              {row.folder ? <p className="note">Its folder: <code>{row.folder}</code></p> : null}
            </div>
          ) : null}
          {view === 'chat' ? (
            <div className="messages" role="log" aria-live="polite">
              {shown.length === 0 ? (
                <p className="empty">No messages in # {room} yet. Open a second window and say hi.</p>
              ) : (
                shown.map((msg) => (
                  <div key={msg.id} className={`row ${msg.mine ? 'is-mine' : 'is-theirs'}`}>
                    <div className="msg">
                      <span className="msg__from" title={msg.from}>
                        {msg.mine ? 'you' : labelFor(conv.members, msg.from) || 'peer'}
                        {msg.via === 'pin' ? <span className="msg__via"> · synced from pin</span> : null}
                      </span>
                      <div className="bubble">{msg.text}</div>
                    </div>
                  </div>
                ))
              )}
              <div ref={bottomRef} />
            </div>
          ) : null}

          {joined ? (
            <Requests conv={row.id} requests={conv.requests} onDone={(request) => dispatch({ type: 'request-done', conv: row.id, request })} />
          ) : null}
          {error ? <p className="error">{error}</p> : null}
          {joined && conv.pin.error ? <p className="error">{conv.pin.error}</p> : null}
          {joined && !conv.pin.error && conv.pin.locked > 0 ? (
            <p className="note">
              {conv.pin.locked} pinned {conv.pin.locked === 1 ? 'message waits' : 'messages wait'} for the sender to come online.
            </p>
          ) : null}

          {view === 'chat' && joined ? (
            <form
              className="composer"
              onSubmit={(ev) => {
                ev.preventDefault()
                send()
              }}
            >
              <input
                className="composer__input"
                aria-label="Message"
                value={conv.draft}
                onChange={(ev) => dispatch({ type: 'draft', conv: row.id, text: ev.target.value })}
                type="text"
                placeholder={`Message # ${room}`}
                autoComplete="off"
              />
              <button type="submit" className="composer__send">Send</button>
            </form>
          ) : null}
        </div>
      </div>
    </div>
  )
}

export default App
