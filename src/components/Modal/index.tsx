import classnames from 'classnames'
import { motion } from 'motion/react'
import { useEffect, useRef, useLayoutEffect, type MouseEvent } from 'react'
import { createPortal } from 'react-dom'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { Button } from '../Button'
import { springs, useIsPhone } from '../Motion'
import './style.css'

interface ModalProps extends BaseComponentProps {
    // show modal
    show?: boolean

    // modal title
    title: string

    // size
    size?: 'small' | 'big'

    // body className
    bodyClassName?: string

    // body style
    bodyStyle?: React.CSSProperties

    // show footer
    footer?: boolean

    // footer extra
    footerExtra?: React.ReactNode

    // ok button style and label
    okType?: 'primary' | 'danger'
    okText?: string

    // on click ok
    onOk?: typeof noop

    // on click close
    onClose?: typeof noop
}

export function Modal (props: ModalProps) {
    const {
        show = true,
        title = 'Modal',
        size = 'small',
        footer = true,
        okType = 'primary',
        okText,
        onOk = noop,
        onClose = noop,
        bodyClassName,
        bodyStyle,
        className,
        footerExtra,
        style,
        children,
    } = props

    const { translation } = useI18n()
    const { t } = translation('Modal')
    const phone = useIsPhone()
    const hidden = { opacity: 0, scale: phone ? 1 : 0.96, y: phone ? 48 : 10 }

    const portalRef = useRef<HTMLDivElement>(document.createElement('div'))
    const maskRef = useRef<HTMLDivElement>(null)
    const onCloseRef = useRef(onClose)
    onCloseRef.current = onClose

    useLayoutEffect(() => {
        const current = portalRef.current
        document.body.appendChild(current)
        return () => { document.body.removeChild(current) }
    }, [])

    useEffect(() => {
        if (!show) return
        function keydown (event: KeyboardEvent) {
            if (event.key === 'Escape') onCloseRef.current()
        }
        document.addEventListener('keydown', keydown)
        return () => document.removeEventListener('keydown', keydown)
    }, [show])

    function handleMaskMouseDown (e: MouseEvent) {
        if (e.target === maskRef.current) {
            onClose()
        }
    }

    const modal = (
        <div
            className={classnames('modal-mask', { 'modal-show': show })}
            ref={maskRef}
            onMouseDown={handleMaskMouseDown}
            aria-hidden={!show}
        >
            <motion.div
                className={classnames('modal', `modal-${size}`, className)}
                style={style}
                initial={hidden}
                animate={show ? { opacity: 1, scale: 1, y: 0 } : hidden}
                transition={springs.sheet}
            >
                <div className="modal-title">{title}</div>
                <div
                    className={classnames('modal-body', bodyClassName)}
                    style={bodyStyle}
                >{children}</div>
                {
                    footer && (
                        <div className="modal-footer">
                            {footerExtra}
                            <div className="modal-footer-actions">
                                <Button onClick={() => onClose()}>{ t('cancel') }</Button>
                                <Button type={okType} onClick={() => onOk()}>{ okText ?? t('ok') }</Button>
                            </div>
                        </div>
                    )
                }
            </motion.div>
        </div>
    )

    return createPortal(modal, portalRef.current)
}
