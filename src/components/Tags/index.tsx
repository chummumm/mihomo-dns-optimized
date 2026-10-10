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
    // identifies the line, so its "you are here" marker can travel between stations
    id?: string
    data: string[]
    onClick: (name: string) => void | Promise<void>
    errSet?: Set<string>
    readings?: Map<string, number>
    line?: string
    select: string
    rowHeight: number
    canClick: boolean
}

// A policy group drawn as a transit line: every member is a station on it and
// the selected node carries the "you are here" marker.
export function Tags (props: TagsProps) {
    const { id, className, data, onClick, select, canClick, errSet, readings, line, rowHeight } = props

    const { translation } = useI18n()
    const { t } = translation('Proxies')
    const [expand, setExpand] = useState(false)
    const [fullHeight, setFullHeight] = useState(rowHeight)
    const [currentRow, setCurrentRow] = useState(0)
    // UI only: the station just chosen, until the controller confirms it.
    const [pending, setPending] = useState<string>()

    const ulRef = useRef<HTMLUListElement>(null)
    useLayoutEffect(() => {
        const ul = ulRef.current
        if (ul == null) return
        const measure = () => {
            setFullHeight(ul.offsetHeight)
            const current = ul.querySelector<HTMLElement>('.station.is-current')
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

    const showExtend = fullHeight > rowHeight + 2
    const collapsed = showExtend && !expand
    const height = collapsed ? rowHeight : fullHeight
    // Collapsed, the line shows the row that holds "you are here".
    const shift = collapsed ? currentRow * rowHeight : 0
    const handleClick = canClick ? onClick : noop

    function choose (name: string) {
        if (name !== select) setPending(name)
        Promise.resolve(handleClick(name)).finally(() => setPending(value => value === name ? undefined : value))
    }

    function toggleExtend () {
        setExpand(!expand)
    }

    const stations = data.map(name => {
        const current = select === name
        const failed = errSet?.has(name) ?? false
        const delay = readings?.get(name) ?? 0
        const status = failed ? 'bad' : delayStatus(delay)
        const content = <>
            <span className="station-dot" aria-hidden="true">
                {current && <motion.span className="station-here" layoutId={id ? `here-${id}` : undefined} transition={springs.glide} />}
            </span>
            <span className="station-name">{name}</span>
            {delay > 0 && !failed && <span className={classnames('station-reading num', `status-${status}`)} key={delay}>{delay}</span>}
        </>
        return (
            <li className={classnames('station', { 'is-current': current, 'is-failed': failed, 'is-pending': pending === name })} key={name}>
                {canClick
                    ? <motion.button
                        type="button"
                        className="station-body"
                        aria-pressed={current}
                        aria-busy={pending === name || undefined}
                        title={name}
                        whileTap={{ scale: 0.95 }}
                        transition={springs.snap}
                        onClick={() => choose(name)}>{content}</motion.button>
                    : <span className="station-body" aria-current={current ? 'true' : undefined} title={name}>{content}</span>}
            </li>
        )
    })

    return (
        <div className={classnames('stations-wrap', className)} style={{ '--line': line ?? 'var(--idle)', '--row': `${rowHeight}px` } as React.CSSProperties}>
            <motion.div className="stations-viewport" initial={false} animate={{ height }} transition={springs.snap}>
                <motion.ul ref={ulRef} className={classnames('stations', { expand, 'is-static': !canClick })} initial={false} animate={{ y: -shift }} transition={springs.glide}>
                    { stations }
                </motion.ul>
            </motion.div>
            {
                showExtend &&
                <button type="button" className="stations-toggle" aria-expanded={expand} onClick={toggleExtend}>
                    <span>{ expand ? t('collapseText') : t('expandText') }</span>
                    <span className="stations-count num">{ data.length }</span>
                    <motion.span className="stations-caret" animate={{ rotate: expand ? 180 : 0 }} transition={springs.snap}>
                        <Icon type="triangle-down" size={12} />
                    </motion.span>
                </button>
            }
        </div>
    )
}
