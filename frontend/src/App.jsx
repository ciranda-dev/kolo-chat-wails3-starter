import { useState, useEffect, useRef } from 'react'
import { Start, Send, Joined, Members, Rooms, IsOwner } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/chatservice.js"
import { Events } from "@wailsio/runtime"
import logo from './assets/kolo-mark.svg'
import { JoinPanel, InvitePanel, Requests } from './Invite.jsx'
import { ProfilePanel, Sidebar, labelFor } from './Profile.jsx'
import './chat.css'
import { appendMessage } from './messages.js'

/** @typedef {{ id: number, from: string, room: string, text: string, mine: boolean, via: string }} Msg */

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
  const [messages, setMessages] = useState(/** @type {Msg[]} */ ([]))
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')
  const [me, setMe] = useState('')
  const [peers, setPeers] = useState(0)
  const [pin, setPin] = useState({ error: '', locked: 0 })
  const [failed, setFailed] = useState(false)
  const [joined, setJoined] = useState(/** @type {boolean | null} */ (null)) // null until the app says
  const [inviting, setInviting] = useState(false)
  const [requests, setRequests] = useState([])
  const [members, setMembers] = useState([])
  const [rooms, setRooms] = useState(['general'])
  const [room, setRoom] = useState('general')
  const [isOwner, setIsOwner] = useState(false)
  const [editing, setEditing] = useState(false)
  const status = failed ? 'error' : peers > 0 ? 'live' : 'waiting'
  const bottomRef = useRef(null)
  const nextId = useRef(1)

  /** The members, the rooms and whether we own the conversation, once we are in one. */
  const loadDirectory = () => {
    Promise.all([Members(), Rooms(), IsOwner()])
      .then(([m, r, owner]) => {
        setMembers(m ?? [])
        setRooms(r ?? ['general'])
        setIsOwner(owner)
      })
      .catch((err) => setError(errorText(err)))
  }

  useEffect(() => {
    Start()
      .then((id) => {
        setMe(id)
        return Joined()
      })
      .then((yes) => {
        setJoined(yes)
        if (yes) {
          loadDirectory()
        }
      })
      .catch((err) => {
        setFailed(true)
        setError(errorText(err))
      })
    const offPeers = Events.On("chat:peers", (e) => setPeers(Number(e.data) || 0))
    const offRequest = Events.On("chat:invite-request", (e) => {
      const r = e.data
      if (!r) {
        return
      }
      setRequests((prev) => {
        const rest = prev.filter((x) => x.request !== r.request)
        return r.withdrawn ? rest : [...rest, r]
      })
    })
    const offPin = Events.On("chat:pin", (e) => setPin({ error: e.data?.error ?? '', locked: e.data?.locked ?? 0 }))
    const offProfiles = Events.On("chat:profiles", (e) => setMembers(e.data ?? []))
    const offRooms = Events.On("chat:rooms", (e) => setRooms(e.data ?? ['general']))
    const off = Events.On("chat:msg", e => {
      const data = e.data
      const from = typeof data === 'object' && data ? (data.from ?? '') : ''
      const text = typeof data === 'object' && data ? (data.text ?? '') : String(data ?? '')
      const via = typeof data === 'object' && data ? (data.via ?? '') : ''
      const where = typeof data === 'object' && data ? (data.room ?? 'general') : 'general'
      setMessages((prev) => appendMessage(prev, { id: nextId.current++, from, room: where, text, mine: false, via }))
    })
    return () => {
      off()
      offProfiles()
      offRooms()
      offPeers()
      offRequest()
      offPin()
    }
  }, [])

  useEffect(() => {
    bottomRef.current?.scrollIntoView({ block: 'end' })
  }, [messages])

  const send = () => {
    const text = draft.trim()
    if (!text) {
      return
    }
    Send(room, text)
      .then(() => {
        setError('')
        setMessages((prev) => appendMessage(prev, { id: nextId.current++, from: me, room, text, mine: true, via: '' }))
        setDraft('')
      })
      .catch((err) => {
        setError(errorText(err))
      })
  }

  const mine = members.find((m) => m.key === me)
  const shown = messages.filter((m) => m.room === room)

  const badge =
    status === 'live'
      ? `${peers} ${peers === 1 ? 'peer' : 'peers'}`
      : status === 'error'
        ? 'Error'
        : 'Waiting for peer'

  return (
    <div className="app">
      <header className="header">
        <img className="header__logo" src={logo} alt="" width={32} height={32} />
        <span className="header__name">kolo</span>
        <span className="header__spacer" />
        {me ? (
          <span className="header__id" title={me}>
            you · {mine?.name || shortId(me)}
          </span>
        ) : null}
        {joined ? (
          <button type="button" className="header__btn" onClick={() => setEditing((open) => !open)}>
            Profile
          </button>
        ) : null}
        {joined ? (
          <button type="button" className="header__btn" onClick={() => setInviting((open) => !open)}>
            Invite{requests.length > 0 ? ` (${requests.length})` : ''}
          </button>
        ) : null}
        <span className={`header__badge is-${status}`}>{badge}</span>
      </header>

      {joined && inviting ? <InvitePanel /> : null}
      {joined && editing ? <ProfilePanel mine={mine} onDone={() => setEditing(false)} /> : null}

      <div className="body">
        {joined ? <Sidebar rooms={rooms} room={room} onRoom={setRoom} members={members} isOwner={isOwner} /> : null}
        <div className="main">
          {joined === false ? (
            <JoinPanel onJoined={() => { setJoined(true); loadDirectory() }} />
          ) : (
            <div className="messages" role="log" aria-live="polite">
              {shown.length === 0 ? (
                <p className="empty">No messages in # {room} yet. Open a second window and say hi.</p>
              ) : (
                shown.map((msg) => (
                  <div key={msg.id} className={`row ${msg.mine ? 'is-mine' : 'is-theirs'}`}>
                    <div className="msg">
                      <span className="msg__from" title={msg.from}>
                        {msg.mine ? 'you' : labelFor(members, msg.from) || 'peer'}
                        {msg.via === 'pin' ? <span className="msg__via"> · synced from pin</span> : null}
                      </span>
                      <div className="bubble">{msg.text}</div>
                    </div>
                  </div>
                ))
              )}
              <div ref={bottomRef} />
            </div>
          )}

          <Requests requests={requests} />
          {error ? <p className="error">{error}</p> : null}
          {pin.error ? <p className="error">{pin.error}</p> : null}
          {!pin.error && pin.locked > 0 ? (
            <p className="note">
              {pin.locked} pinned {pin.locked === 1 ? 'message waits' : 'messages wait'} for the sender to come online.
            </p>
          ) : null}

          {joined ? (
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
                value={draft}
                onChange={(ev) => setDraft(ev.target.value)}
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
