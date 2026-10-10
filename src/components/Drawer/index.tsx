import classnames from 'classnames'
import { motion } from 'motion/react'
import { useLayoutEffect, useRef, type RefObject } from 'react'
import { createPortal } from 'react-dom'

import { type BaseComponentProps } from '@models/BaseProps'

import '../Card/style.css'
import { springs } from '../Motion'
import './style.css'

interface DrawerProps extends BaseComponentProps {
    visible?: boolean
    width?: number
    bodyClassName?: string
    containerRef?: RefObject<HTMLElement>
    label?: string
}

export function Drawer (props: DrawerProps) {
    const portalRef = useRef<HTMLElement>(document.createElement('div'))

    useLayoutEffect(() => {
        const current = portalRef.current
        document.body.appendChild(current)
        return () => { document.body.removeChild(current) }
    }, [])

    const container = (
        <div className={classnames(props.className, 'drawer-layer')}>
            <motion.div
                className={classnames(
                    'card drawer',
                    props.bodyClassName,
                    { 'translate-x-0': props.visible, 'translate-x-full': !props.visible },
                )}
                style={{ width: props.width ?? 400, maxWidth: '100%' }}
                initial={false}
                animate={props.visible
                    ? { x: 0, visibility: 'visible' }
                    : { x: '110%', transitionEnd: { visibility: 'hidden' } }}
                transition={springs.sheet}
            >
                <section className="drawer-inner" aria-label={props.label} aria-hidden={!props.visible}>
                    {props.children}
                </section>
            </motion.div>
        </div>
    )

    return createPortal(container, props.containerRef?.current ?? portalRef.current)
}
