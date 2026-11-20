import { useState, useEffect, useRef } from 'react'
import { Start, Send } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/chatservice.js"
import { Events } from "@wailsio/runtime"
import logo from './assets/kolo-mark.svg'
import './chat.css'
import { appendMessage } from './messages.js'

/** @typedef {{ id: number, from: string, text: string, mine: boolean, via: string }} Msg */

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
  const status = failed ? 'error' : peers > 0 ? 'live' : 'waiting'
  const bottomRef = useRef(null)
  const nextId = useRef(1)

  useEffect(() => {
    Start()
      .then((id) => setMe(id))
      .catch((err) => {
        setFailed(true)
        setError(errorText(err))
      })
    const offPeers = Events.On("chat:peers", (e) => setPeers(Number(e.data) || 0))
    const offPin = Events.On("chat:pin", (e) => setPin({ error: e.data?.error ?? '', locked: e.data?.locked ?? 0 }))
    const off = Events.On("chat:msg", e => {
      const data = e.data
      const from = typeof data === 'object' && data ? (data.from ?? '') : ''
      const text = typeof data === 'object' && data ? (data.text ?? '') : String(data ?? '')
      const via = typeof data === 'object' && data ? (data.via ?? '') : ''
      setMessages((prev) => appendMessage(prev, { id: nextId.current++, from, text, mine: false, via }))
    })
    return () => {
      off()
      offPeers()
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
    Send(text)
      .then(() => {
        setError('')
        setMessages((prev) => appendMessage(prev, { id: nextId.current++, from: me, text, mine: true, via: '' }))
        setDraft('')
      })
      .catch((err) => {
        setError(errorText(err))
      })
  }

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
            you · {shortId(me)}
          </span>
        ) : null}
        <span className={`header__badge is-${status}`}>{badge}</span>
      </header>

      <div className="messages" role="log" aria-live="polite">
        {messages.length === 0 ? (
          <p className="empty">No messages yet. Open a second window and say hi.</p>
        ) : (
          messages.map((msg) => (
            <div key={msg.id} className={`row ${msg.mine ? 'is-mine' : 'is-theirs'}`}>
              <div className="msg">
                <span className="msg__from" title={msg.from}>
                  {msg.mine ? 'you' : shortId(msg.from) || 'peer'}
                  {msg.via === 'pin' ? <span className="msg__via"> · synced from pin</span> : null}
                </span>
                <div className="bubble">{msg.text}</div>
              </div>
            </div>
          ))
        )}
        <div ref={bottomRef} />
      </div>

      {error ? <p className="error">{error}</p> : null}
      {pin.error ? <p className="error">{pin.error}</p> : null}
      {!pin.error && pin.locked > 0 ? (
        <p className="note">
          {pin.locked} pinned {pin.locked === 1 ? 'message waits' : 'messages wait'} for the sender to come online.
        </p>
      ) : null}

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
          placeholder="Message"
          autoComplete="off"
        />
        <button type="submit" className="composer__send">Send</button>
      </form>
    </div>
  )
}

export default App
