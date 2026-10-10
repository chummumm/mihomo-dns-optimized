import classnames from 'classnames'
import { useEffect, useRef, useState } from 'react'
import { createRoot } from 'react-dom/client'

import { Icon } from '@components'
import { noop } from '@lib/helper'
import './style.scss'

const TYPE_ICON_MAP = {
    info: 'info',
    success: 'check',
    warning: 'info-o',
    error: 'close',
}

type NoticeType = 'success' | 'info' | 'warning' | 'error'

interface ArgsProps {
    content: string
    type: NoticeType
    duration?: number
    onClose?: typeof noop
}

interface MessageProps {
    content?: string
    type?: NoticeType
    icon?: React.ReactNode
    duration?: number
    removeComponent: typeof noop
}

export function Message (props: MessageProps) {
    const {
        removeComponent = noop,
        icon = <Icon type="info" size={16} />,
        content = '',
        type = 'info',
        duration = 1500,
    } = props

    const [visible, setVisible] = useState(false)
    const closing = useRef(false)

    useEffect(() => {
        const lifetime = Number.isFinite(duration) ? Math.max(0, duration) : 1500
        closing.current = false
        const enter = window.setTimeout(() => setVisible(true), 0)
        const leave = window.setTimeout(() => {
            closing.current = true
            setVisible(false)
        }, lifetime)
        // transitionend is not guaranteed in hidden tabs or with reduced motion.
        const fallback = window.setTimeout(removeComponent, lifetime + 250)
        return () => {
            window.clearTimeout(enter)
            window.clearTimeout(leave)
            window.clearTimeout(fallback)
        }
    }, [duration, removeComponent])

    return (
        <div
            className={classnames('message', `message-${type}`, { 'message-show': visible })}
            role={type === 'error' ? 'alert' : 'status'}
            onTransitionEnd={event => {
                if (event.target === event.currentTarget && event.propertyName === 'opacity' && closing.current) removeComponent()
            }}
        >
            <span className="message-icon">{icon}</span>
            <span className="message-content">{content}</span>
        </div>
    )
}

export function showMessage (args: ArgsProps) {
    const container = document.createElement('div')
    container.className = 'message-container'
    document.body.appendChild(container)
    const root = createRoot(container)
    let removed = false

    const removeComponent = () => {
        if (removed) return
        removed = true
        root.unmount()
        container.remove()
        args.onClose?.()
    }

    const icon = <Icon type={TYPE_ICON_MAP[args.type]} size={16}></Icon>
    const { type, content, duration } = args
    const props: MessageProps = {
        icon,
        type,
        content,
        removeComponent,
        duration,
    }

    root.render(<Message {...props} />)
    return removeComponent
}

export const info = (
    content: string,
    duration?: number,
    onClose?: typeof noop,
) => showMessage({ type: 'info', content, duration, onClose })

export const success = (
    content: string,
    duration?: number,
    onClose?: typeof noop,
) => showMessage({ type: 'success', content, duration, onClose })

export const warning = (
    content: string,
    duration?: number,
    onClose?: typeof noop,
) => showMessage({ type: 'warning', content, duration, onClose })

export const error = (
    content: string,
    duration?: number,
    onClose?: typeof noop,
) => showMessage({ type: 'error', content, duration, onClose })
