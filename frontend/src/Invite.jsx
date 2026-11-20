import { useState, useEffect } from 'react'
import { Join, CancelJoin, CreateInvite, Invites, Revoke, Approve, Deny } from "../bindings/github.com/ciranda-dev/kolo-chat-wails3-starter/chatservice.js"
import { Events } from "@wailsio/runtime"

/** @typedef {{ id: string, code?: string, expires: string, uses: number, used: number, requireApproval: boolean, state: string }} InviteInfo */
/** @typedef {{ request: number, invite: string, joiner: string, withdrawn: boolean }} JoinRequest */

const shortId = (id) => (id ? id.slice(0, 8) : '')

/** Turns whatever a rejected call threw into a string for the UI. */
function errorText(err) {
  if (err instanceof Error && err.message) {
    return err.message
  }
  return typeof err === 'string' ? err : String(err)
}

const EXPIRIES = [
  { label: '10 minutes', seconds: 600 },
  { label: '1 hour', seconds: 3600 },
  { label: '1 day', seconds: 86400 },
  { label: '7 days', seconds: 604800 },
]

/** Shown instead of the chat until the user has joined a conversation: paste the code of an invite. */
export function JoinPanel({ onJoined }) {
  const [code, setCode] = useState('')
  const [busy, setBusy] = useState(false)
  const [status, setStatus] = useState('')
  const [error, setError] = useState('')

  useEffect(() => {
    return Events.On("chat:join", (e) => setStatus(e.data?.status === 'failed' ? '' : (e.data?.message ?? '')))
  }, [])

  const join = (ev) => {
    ev.preventDefault()
    if (!code.trim() || busy) {
      return
    }
    setBusy(true)
    setError('')
    Join(code)
      .then(() => onJoined())
      .catch((err) => setError(errorText(err)))
      .finally(() => {
        setBusy(false)
        setStatus('')
      })
  }

  return (
    <form className="panel panel--center" onSubmit={join}>
      <h2 className="panel__title">Join a conversation</h2>
      <p className="note">Paste the invite code you received. If the owner wants to approve you, this waits for the answer.</p>
      <input
        className="field"
        aria-label="Invite code"
        value={code}
        onChange={(ev) => setCode(ev.target.value)}
        placeholder="Invite code"
        autoComplete="off"
        spellCheck={false}
        disabled={busy}
      />
      <div className="panel__row">
        <button type="submit" className="btn" disabled={busy || !code.trim()}>Join</button>
        {busy ? <button type="button" className="btn btn--ghost" onClick={() => CancelJoin()}>Cancel</button> : null}
      </div>
      {status ? <p className="note">{status}</p> : null}
      {error ? <p className="error">{error}</p> : null}
    </form>
  )
}

/** Create an invite (expiry, uses, approval) and see or revoke the ones made so far. */
export function InvitePanel() {
  const [seconds, setSeconds] = useState(86400)
  const [uses, setUses] = useState(1)
  const [approval, setApproval] = useState(true)
  const [invites, setInvites] = useState(/** @type {InviteInfo[]} */ ([]))
  const [codes, setCodes] = useState(/** @type {Record<string, string>} */ ({}))
  const [error, setError] = useState('')

  useEffect(() => {
    Invites().then((list) => setInvites(list ?? [])).catch((err) => setError(errorText(err)))
    return Events.On("chat:invite", (e) => {
      const info = e.data
      if (info?.id) {
        setInvites((prev) => prev.map((i) => (i.id === info.id ? info : i)))
      }
    })
  }, [])

  const create = (ev) => {
    ev.preventDefault()
    setError('')
    CreateInvite({ ttlSeconds: seconds, uses: Number(uses) || 1, requireApproval: approval })
      .then((info) => {
        setCodes((prev) => ({ ...prev, [info.id]: info.code }))
        setInvites((prev) => [...prev, info])
      })
      .catch((err) => setError(errorText(err)))
  }

  const revoke = (id) => {
    setError('')
    Revoke(id).catch((err) => setError(errorText(err)))
  }

  return (
    <section className="panel">
      <form className="panel__row" onSubmit={create}>
        <label className="panel__label">
          Expires in
          <select className="field" value={seconds} onChange={(ev) => setSeconds(Number(ev.target.value))}>
            {EXPIRIES.map((x) => (
              <option key={x.seconds} value={x.seconds}>{x.label}</option>
            ))}
          </select>
        </label>
        <label className="panel__label">
          Uses
          <input
            className="field field--narrow"
            type="number"
            min="1"
            max="100"
            value={uses}
            onChange={(ev) => setUses(ev.target.value)}
          />
        </label>
        <label className="check">
          <input type="checkbox" checked={approval} onChange={(ev) => setApproval(ev.target.checked)} />
          Require my approval
        </label>
        <button type="submit" className="btn">Create invite</button>
      </form>
      {error ? <p className="error">{error}</p> : null}
      <ul className="invites">
        {invites.map((i) => (
          <li key={i.id} className="invite">
            <span className="invite__meta">
              {shortId(i.id)} · {i.state} · {i.used}/{i.uses} used
              {i.requireApproval ? ' · needs approval' : ''}
              {i.expires ? ` · expires ${new Date(i.expires).toLocaleString()}` : ''}
            </span>
            {i.state === 'active' ? (
              <button type="button" className="btn btn--ghost" onClick={() => revoke(i.id)}>Revoke</button>
            ) : null}
            {codes[i.id] ? (
              <input
                className="field invite__code"
                aria-label="Invite code"
                readOnly
                value={codes[i.id]}
                onFocus={(ev) => ev.target.select()}
              />
            ) : null}
          </li>
        ))}
      </ul>
      {invites.length > 0 ? <p className="note">Give a code only to the person you invite. It cannot be shown again after the app restarts.</p> : null}
    </section>
  )
}

/** The people waiting to be let in, each with Approve and Deny. */
export function Requests({ requests }) {
  const [error, setError] = useState('')
  const answer = (call, request) => {
    setError('')
    call(request).catch((err) => setError(errorText(err)))
  }
  if (requests.length === 0) {
    return error ? <p className="error">{error}</p> : null
  }
  return (
    <div className="requests">
      {requests.map((r) => (
        <div key={r.request} className="request">
          <span className="request__who" title={r.joiner}>{shortId(r.joiner)} wants to join</span>
          <button type="button" className="btn" onClick={() => answer(Approve, r.request)}>Approve</button>
          <button type="button" className="btn btn--ghost" onClick={() => answer(Deny, r.request)}>Deny</button>
        </div>
      ))}
      {error ? <p className="error">{error}</p> : null}
    </div>
  )
}
