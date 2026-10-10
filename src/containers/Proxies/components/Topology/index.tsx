import classnames from 'classnames'
import { useAtomValue } from 'jotai'
import { AnimatePresence, motion, useIsPresent } from 'motion/react'
import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'

import { Card, Icon, formatRate, delayStatus, springs, useLineColor } from '@components'
import { INTERNAL_CONNECTIONS, UNKNOWN_CONNECTIONS } from '@containers/Connections/source'
import { useConnections } from '@containers/Connections/store'
import type * as API from '@lib/request'
import { proxyGroupMapping, proxyMapping, useConnectionStreamReader, useI18n, useProxy } from '@stores'

import { type TopologyLink, type TopologyModel, type TopologyNode, TopologyRenderer } from './draw'
import { resolveProxyExit } from '../../resolve'
import './style.css'

const PREFERENCE = 'ui.topology'
const LIMITS = [5, 6, 6] as const

function readOpen () {
    try {
        return localStorage.getItem(PREFERENCE) !== 'hidden'
    } catch {
        return true
    }
}

function writeOpen (open: boolean) {
    try {
        localStorage.setItem(PREFERENCE, open ? 'shown' : 'hidden')
    } catch {
        // storage unavailable; the map simply opens next time
    }
}

// Scrolls to the card or tile a map box stands for and lights it once.
function reveal (target: { type: 'group' | 'node', name: string }) {
    const selector = target.type === 'group' ? '.proxy-group[data-group]' : '.proxy-item[data-node]'
    const attribute = target.type === 'group' ? 'data-group' : 'data-node'
    const element = Array.from(document.querySelectorAll<HTMLElement>(selector)).find(item => item.getAttribute(attribute) === target.name)
    if (element === undefined) return
    const smooth = !window.matchMedia('(prefers-reduced-motion: reduce)').matches
    element.scrollIntoView({ behavior: smooth ? 'smooth' : 'auto', block: 'center' })
    element.classList.remove('is-flash')
    void element.offsetWidth
    element.classList.add('is-flash')
    window.setTimeout(() => element.classList.remove('is-flash'), 1400)
}

// The traffic map card. The live view, and its subscription to the controller's
// connection stream, only exist while the map is open.
export function Topology () {
    const { translation } = useI18n()
    const { t } = translation('Proxies')
    const [open, setOpen] = useState(readOpen)

    function toggle () {
        setOpen(!open)
        writeOpen(!open)
    }

    return (
        <Card className={classnames('topology-card', { 'is-open': open })}>
            <div className="topology-head">
                <h2 className="topology-title">{t('topology.title')}</h2>
                <span className="topology-flow">{t('topology.flow')}</span>
                <button type="button" className="topology-toggle" aria-expanded={open} onClick={toggle}>
                    <span>{open ? t('topology.hide') : t('topology.show')}</span>
                    <motion.span className="topology-caret" initial={false} animate={{ rotate: open ? 180 : 0 }} transition={springs.snap}>
                        <Icon type="triangle-down" size={12} />
                    </motion.span>
                </button>
            </div>
            <AnimatePresence initial={false}>
                {open && <motion.div
                    key="live"
                    className="topology-body"
                    initial={{ height: 0, opacity: 0 }}
                    animate={{ height: 'auto', opacity: 1 }}
                    exit={{ height: 0, opacity: 0 }}
                    transition={springs.sheet}>
                    <TopologyLive />
                </motion.div>}
            </AnimatePresence>
        </Card>
    )
}

function TopologyLive () {
    const { translation } = useI18n()
    const { t } = translation('Proxies')
    const { t: tc } = translation('Connections')
    const present = useIsPresent()
    const reader = useConnectionStreamReader(present)
    const { connections, feed } = useConnections(reader)
    const { groups } = useProxy()
    const groupByName = useAtomValue(proxyGroupMapping)
    const proxyMap = useAtomValue(proxyMapping)
    const color = useLineColor()
    const [rate, setRate] = useState({ up: 0, down: 0 })
    const totals = useRef<{ up: number, down: number, at: number }>()

    useLayoutEffect(() => {
        if (!present) return
        totals.current = undefined
        setRate({ up: 0, down: 0 })
        function handle (snapshots: API.Snapshot[]) {
            for (const snapshot of snapshots) {
                const now = performance.now()
                const previous = totals.current
                if (previous !== undefined && now > previous.at) {
                    const seconds = (now - previous.at) / 1000
                    setRate({
                        up: Math.max(0, snapshot.uploadTotal - previous.up) / seconds,
                        down: Math.max(0, snapshot.downloadTotal - previous.down) / seconds,
                    })
                }
                totals.current = { up: snapshot.uploadTotal, down: snapshot.downloadTotal, at: now }
                feed(snapshot.connections)
            }
        }
        reader.subscribe('data', handle)
        return () => reader.unsubscribe('data', handle)
    }, [reader, feed, present])

    const model = useMemo<TopologyModel>(() => {
        interface Tally { rate: number, count: number }
        const tally = (map: Map<string, Tally>, key: string, value: number) => {
            const entry = map.get(key)
            if (entry !== undefined) {
                entry.rate += value
                entry.count++
            } else {
                map.set(key, { rate: value, count: 1 })
            }
        }
        const sources = new Map<string, Tally>()
        const policy = new Map<string, Tally>()
        const exits = new Map<string, Tally>()
        const pairs = new Map<string, TopologyLink & { column: number }>()
        const pair = (from: string, to: string, value: number, column: number, kind: TopologyLink['kind']) => {
            const key = `${from}>${to}`
            const entry = pairs.get(key)
            if (entry !== undefined) entry.rate += value
            else pairs.set(key, { from, to, rate: value, kind, column })
        }
        const kindOf = (exit: string): TopologyLink['kind'] => {
            const type = proxyMap.get(exit)?.type
            if (exit === 'DIRECT' || type === 'Direct') return 'direct'
            if (exit === 'REJECT' || exit === 'REJECT-DROP' || type === 'Reject') return 'reject'
            return 'proxy'
        }

        for (const connection of connections) {
            if (connection.completed === true) continue
            const value = connection.downloadSpeed + connection.uploadSpeed
            const chains = connection.chains ?? []
            const exit = chains[0] ?? 'DIRECT'
            const outer = chains[chains.length - 1] ?? exit
            tally(sources, connection.sourceKey, value)
            tally(exits, exit, value)
            if (outer !== exit && groupByName.has(outer)) {
                tally(policy, outer, value)
                pair(`s:${connection.sourceKey}`, `g:${outer}`, value, 0, kindOf(exit))
                pair(`g:${outer}`, `x:${exit}`, value, 1, kindOf(exit))
            } else {
                pair(`s:${connection.sourceKey}`, `x:${exit}`, value, 0, kindOf(exit))
            }
        }

        const rank = (map: Map<string, Tally>) => [...map.entries()].sort((a, b) => b[1].rate - a[1].rate || b[1].count - a[1].count)
        const sourceRank = rank(sources)
        let groupRank = rank(policy)
        // With no traffic yet, still draw the configured groups and where they lead.
        if (groupRank.length === 0) groupRank = groups.slice(0, LIMITS[1]).map(group => [group.name, { rate: 0, count: 0 }])
        const shownGroups = new Set(groupRank.slice(0, LIMITS[1]).map(([name]) => name))
        for (const name of shownGroups) {
            const exit = resolveProxyExit(name, groupByName)
            if (exit === undefined) continue
            if (!exits.has(exit)) exits.set(exit, { rate: 0, count: 0 })
            const key = `g:${name}>x:${exit}`
            if (!pairs.has(key)) pairs.set(key, { from: `g:${name}`, to: `x:${exit}`, rate: 0, kind: kindOf(exit), column: 1 })
        }
        const exitRank = rank(exits)

        const nodes: TopologyNode[] = []
        const alias = new Map<string, string>()
        const place = (column: 0 | 1 | 2, prefix: string, ranked: Array<[string, Tally]>, make: (name: string, entry: Tally) => TopologyNode) => {
            const limit = LIMITS[column]
            const keep = ranked.length > limit + 1 ? ranked.slice(0, limit) : ranked
            for (const [name, entry] of keep) nodes.push(make(name, entry))
            const rest = ranked.slice(keep.length)
            if (rest.length > 0) {
                const id = `more:${column}`
                const restRate = rest.reduce((sum, [, entry]) => sum + entry.rate, 0)
                nodes.push({ id, column, kind: 'more', title: `${t('topology.others')} ${rest.length}`, sub: restRate > 0 ? formatRate(restRate) : `${rest.reduce((sum, [, entry]) => sum + entry.count, 0)} ${t('topology.connections')}`, tone: 'quiet' })
                for (const [name] of rest) alias.set(`${prefix}${name}`, id)
            }
        }

        place(0, 's:', sourceRank, (key, entry) => ({
            id: `s:${key}`,
            column: 0,
            kind: 'source',
            title: key === INTERNAL_CONNECTIONS ? 'mihomo' : key === UNKNOWN_CONNECTIONS ? tc('filter.unknown') : key.startsWith('device:') ? key.slice(7) : key,
            sub: entry.rate >= 1 ? formatRate(entry.rate) : `${entry.count} ${t('topology.connections')}`,
            tone: entry.rate >= 1 ? 'live' : 'quiet',
        }))
        place(1, 'g:', groupRank, name => ({
            id: `g:${name}`,
            column: 1,
            kind: 'group',
            title: name,
            sub: `→ ${groupByName.get(name)?.now ?? ''}`,
            tone: 'live',
            hue: color(name),
            target: { type: 'group', name },
        }))
        place(2, 'x:', exitRank, name => {
            const proxy = proxyMap.get(name)
            const kind = kindOf(name)
            const history = proxy?.history ?? []
            const delay = history.length > 0 ? history[history.length - 1].delay : 0
            const failed = proxy?.alive === false || (history.length > 0 && delay === 0)
            let sub = proxy?.type ?? ''
            if (kind === 'direct') sub = t('topology.direct')
            else if (kind === 'reject') sub = t('reject')
            else if (delay > 0) sub = `${delay} ms`
            else if (failed) sub = t('timeout')
            return {
                id: `x:${name}`,
                column: 2,
                kind: 'exit',
                title: name,
                sub,
                tone: kind === 'direct' ? 'quiet' : kind === 'reject' ? 'bad' : failed ? 'bad' : delay > 0 ? delayStatus(delay) : 'idle',
                target: proxy !== undefined && kind === 'proxy' ? { type: 'node', name } : undefined,
                stamp: history.length > 0 ? `${history[history.length - 1].time}:${delay}` : undefined,
            }
        })

        const links = new Map<string, TopologyLink>()
        for (const link of pairs.values()) {
            const from = alias.get(link.from) ?? link.from
            const to = alias.get(link.to) ?? link.to
            const key = `${from}>${to}`
            const entry = links.get(key)
            if (entry !== undefined) entry.rate += link.rate
            else links.set(key, { from, to, rate: link.rate, kind: link.kind })
        }
        const present = new Set(nodes.map(node => node.id))
        const rows = Math.max(3, ...[0, 1, 2].map(column => nodes.filter(node => node.column === column).length))
        return {
            nodes,
            links: [...links.values()].filter(link => present.has(link.from) && present.has(link.to)),
            rows,
            headings: [t('topology.sources'), t('topology.groups'), t('topology.exits')],
        }
    }, [connections, groups, groupByName, proxyMap, color, t, tc])

    const canvas = useRef<HTMLCanvasElement>(null)
    const tooltip = useRef<HTMLDivElement>(null)
    const renderer = useRef<TopologyRenderer>()
    useEffect(() => {
        if (canvas.current === null) return
        const instance = new TopologyRenderer(canvas.current, tooltip.current, { onPick: reveal })
        renderer.current = instance
        return () => {
            instance.destroy()
            renderer.current = undefined
        }
    }, [])
    useEffect(() => { renderer.current?.setModel(model) }, [model])

    const active = connections.length
    return (
        <div className="topology-live">
            <div className="topology-bar">
                <span className="topology-pill">
                    <span className={classnames('topology-live-dot', { 'is-idle': active === 0 })} aria-hidden="true" />
                    <span className="topology-pill-rate">↓ <b>{formatRate(rate.down)}</b></span>
                    <span className="topology-pill-rate">↑ <b>{formatRate(rate.up)}</b></span>
                    <span className="topology-pill-count"><b>{active}</b> {t('topology.connections')}</span>
                </span>
                <span className="topology-legend">
                    <span><i className="topology-key topology-key-proxy" />{t('topology.proxied')}</span>
                    <span><i className="topology-key topology-key-direct" />{t('topology.direct')}</span>
                </span>
                <span className="topology-hint">{t('topology.hint')}</span>
            </div>
            <div className="topology-stage" style={{ '--rows': model.rows } as React.CSSProperties}>
                <canvas ref={canvas} className="topology-canvas" role="img" aria-label={t('topology.label')} />
                <div ref={tooltip} className="topology-tip" aria-hidden="true"><b /><span /></div>
                {active === 0 && <span className="topology-empty">{t('topology.empty')}</span>}
            </div>
        </div>
    )
}
