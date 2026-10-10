import classnames from 'classnames'
import { motion } from 'motion/react'
import { useEffect, useState, useRef, useLayoutEffect } from 'react'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { Icon } from '../Icon'
import { springs } from '../Motion'
import { delayStatus } from '../Route'
import './style.css'

interface TagsProps extends BaseComponentProps {
    // identifies the group, so its lit outline can glide between members
    id?: string
    data: string[]
    onClick: (name: string) => void | Promise<void>
    errSet?: Set<string>
    readings?: Map<string, number>
    line?: string
    select: string
    rowHeight: number
    canClick: boolean
    // rows shown while collapsed
    rows?: number
}

// A policy group's members as chips. The current member carries a lit gradient
// outline that glides to whichever member is chosen next.
export function Tags (props: TagsProps) {
    const { id, className, data, onClick, select, canClick, errSet, readings, line, rowHeight, rows = 2 } = props

    const { translation } = useI18n()
    const { t } = translation('Proxies')
    const [expand, setExpand] = useState(false)
    const [fullHeight, setFullHeight] = useState(rowHeight)
    const [currentRow, setCurrentRow] = useState(0)
    // UI only: the member just chosen, until the controller confirms it.
    const [pending, setPending] = useState<string>()

    const ulRef = useRef<HTMLUListElement>(null)
    useLayoutEffect(() => {
        const ul = ulRef.current
        if (ul == null) return
        const measure = () => {
            setFullHeight(ul.offsetHeight)
            const current = ul.querySelector<HTMLElement>('.chip.is-current')
            setCurrentRow(current == null ? 0 : Math.round(current.offsetTop / rowHeight))
        }
        measure()
        const observer = typeof ResizeObserver === 'function' ? new ResizeObserver(measure) : undefined
        observer?.observe(ul)
        return () => observer?.disconnect()
    }, [select, data, rowHeight])

    useEffect(() => {
        if (pending !== undefined && pending === select) setPending(undefined)
    }, [pending, select])

    const collapsedHeight = rows * rowHeight
    const showExtend = fullHeight > collapsedHeight + 2
    const collapsed = showExtend && !expand
    const height = collapsed ? collapsedHeight : fullHeight
    // Collapsed, the window keeps the row that holds the current member in view.
    const shift = collapsed ? Math.max(0, currentRow - rows + 1) * rowHeight : 0
    const handleClick = canClick ? onClick : noop

    function choose (name: string) {
        if (name !== select) setPending(name)
        Promise.resolve(handleClick(name)).finally(() => setPending(value => value === name ? undefined : value))
    }

    const chips = data.map(name => {
        const current = select === name
        const failed = errSet?.has(name) ?? false
        const delay = readings?.get(name) ?? 0
        const status = failed ? 'bad' : delayStatus(delay)
        const content = <>
            {current && <motion.span className="chip-here" layoutId={id ? `here-${id}` : undefined} transition={springs.glide} aria-hidden="true" />}
            <span className="chip-dot" aria-hidden="true" />
            <span className="chip-name">{name}</span>
            {failed
                ? <span className="chip-reading status-bad">{t('timeout')}</span>
                : delay > 0 && <span className={classnames('chip-reading num', `status-${status}`)} key={delay}>{delay}</span>}
        </>
        return (
            <li className={classnames('chip', { 'is-current': current, 'is-failed': failed, 'is-pending': pending === name })} key={name}>
                {canClick
                    ? <motion.button
                        type="button"
                        className="chip-body"
                        aria-pressed={current}
                        aria-busy={pending === name || undefined}
                        title={name}
                        whileTap={{ scale: 0.94 }}
                        transition={springs.snap}
                        onClick={() => choose(name)}>{content}</motion.button>
                    : <span className="chip-body" aria-current={current ? 'true' : undefined} title={name}>{content}</span>}
            </li>
        )
    })

    return (
        <div className={classnames('chips-wrap', className)} style={{ '--line': line ?? 'var(--accent)', '--row': `${rowHeight}px` } as React.CSSProperties}>
            <motion.div className="chips-viewport" initial={false} animate={{ height }} transition={springs.snap}>
                <motion.ul ref={ulRef} className={classnames('chips', { expand, 'is-static': !canClick })} initial={false} animate={{ y: -shift }} transition={springs.glide}>
                    { chips }
                </motion.ul>
            </motion.div>
            {
                showExtend &&
                <button type="button" className="chips-toggle" aria-expanded={expand} onClick={() => setExpand(!expand)}>
                    <span>{ expand ? t('collapseText') : t('expandText') }</span>
                    <span className="chips-count num">{ data.length }</span>
                    <motion.span className="chips-caret" animate={{ rotate: expand ? 180 : 0 }} transition={springs.snap}>
                        <Icon type="triangle-down" size={12} />
                    </motion.span>
                </button>
            }
        </div>
    )
}
