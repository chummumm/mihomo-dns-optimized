import classnames from 'classnames'
import { atom, useAtomValue } from 'jotai'
import { useCallback } from 'react'

import { type BaseComponentProps } from '@models/BaseProps'
import { proxies } from '@stores'

import './style.css'

const LINE_COUNT = 9

// Every policy group owns one line colour, assigned in the configured group
// order already held in the proxies store. Names that are not a known group
// (single nodes, or groups before the store has loaded) stay neutral, so a
// colour never changes meaning between pages.
const groupLines = atom(get => new Map(get(proxies).groups.map((group, index) => [group.name, index % LINE_COUNT])))

export function lineColor (name?: string, assigned?: Map<string, number>) {
    if (!name) return 'var(--idle)'
    switch (name) {
        case 'GLOBAL':
            return 'var(--line-system)'
        case 'DIRECT':
            return 'var(--idle)'
        case 'REJECT':
        case 'REJECT-DROP':
            return 'var(--bad)'
        default: {
            const index = assigned?.get(name)
            return index === undefined ? 'var(--idle)' : `var(--line-${index})`
        }
    }
}

// Reads the group order already held in the store; it never requests data.
export function useLineColor () {
    const assigned = useAtomValue(groupLines)
    return useCallback((name?: string) => lineColor(name, assigned), [assigned])
}

export type DelayStatus = 'idle' | 'good' | 'warn' | 'bad'

// Thresholds match the original tag colours: up to 260 ms good, up to 600 ms slow.
export function delayStatus (delay: number): DelayStatus {
    if (!delay || delay <= 0) return 'idle'
    if (delay <= 260) return 'good'
    if (delay <= 600) return 'warn'
    return 'bad'
}

interface LineBulletProps extends BaseComponentProps {
    name?: string
}

export function LineBullet ({ name, className, style }: LineBulletProps) {
    const color = useLineColor()
    return <span className={classnames('line-bullet', className)} style={{ '--line': color(name), ...style } as React.CSSProperties} aria-hidden="true" />
}
