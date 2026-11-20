import { useEffect } from 'react'

/** A dialog over the window: the close button, Escape or a click outside close it. */
export function Modal({ title, onClose, children }) {
  useEffect(() => {
    const onKey = (ev) => {
      if (ev.key === 'Escape') {
        onClose()
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  return (
    <div
      className="modal"
      role="presentation"
      onMouseDown={(ev) => {
        if (ev.target === ev.currentTarget) {
          onClose()
        }
      }}
    >
      <div className="modal__box" role="dialog" aria-modal="true" aria-label={title}>
        <div className="modal__head">
          <h2 className="modal__title">{title}</h2>
          <button type="button" className="header__btn" onClick={onClose}>Close</button>
        </div>
        {children}
      </div>
    </div>
  )
}
