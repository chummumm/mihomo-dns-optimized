import classnames from 'classnames'
import dayjs from 'dayjs'
import { AnimatePresence, motion } from 'motion/react'
import { useLayoutEffect, useEffect, useRef, useState } from 'react'

import { Button, Select, Card, Header, springs } from '@components'
import type { Client } from '@lib/request'
import { LOG_HISTORY_LIMIT, type Log } from '@models/Log'
import { useClient, useConfig, useI18n, useLogsStreamReader } from '@stores'

import './style.css'

const logLevelOptions = [
    { label: 'Default', value: '' },
    { label: 'Debug', value: 'debug' },
    { label: 'Info', value: 'info' },
    { label: 'Warn', value: 'warning' },
    { label: 'Error', value: 'error' },
    { label: 'Silent', value: 'silent' },
]
const logMap = new Map([
    ['debug', 'log-debug'],
    ['info', 'log-info'],
    ['warning', 'log-warning'],
    ['error', 'log-error'],
])

// One in-memory cursor is enough: changing controller resets the stream buffer.
// The entry is recorded at the click, so a remount cannot resurrect cleared lines.
// Neither the subscription nor its bounded buffer is changed by clearing the view.
let clearedThrough: { client: Client, entry: Log } | undefined

function ControllerLogs ({ client }: { client: Client }) {
    const listRef = useRef<HTMLUListElement>(null)
    const logsRef = useRef<Log[]>([])
    const [logs, setLogs] = useState<Log[]>([])
    const { translation } = useI18n()
    const { data: { logLevel }, set: setConfig } = useConfig()
    const { t } = translation('Logs')
    const logsStreamReader = useLogsStreamReader()
    const scrollHeightRef = useRef(listRef.current?.scrollHeight ?? 0)

    // Pause holds new lines aside (bounded like the view) until the person resumes.
    const pausedRef = useRef(false)
    const heldRef = useRef<Log[]>([])
    const [paused, setPaused] = useState(false)
    const [held, setHeld] = useState(0)
    const [clearing, setClearing] = useState(false)
    const clearingRef = useRef(false)
    const clearTimer = useRef<number>()
    const followRef = useRef(false)

    useEffect(() => () => { window.clearTimeout(clearTimer.current) }, [])

    useLayoutEffect(() => {
        const ul = listRef.current
        if (ul != null && (followRef.current || scrollHeightRef.current === (ul.scrollTop + ul.clientHeight))) {
            ul.scrollTop = ul.scrollHeight - ul.clientHeight
        }
        followRef.current = false
        scrollHeightRef.current = ul?.scrollHeight ?? 0
    })

    useEffect(() => {
        function handleLog (newLogs: Log[]) {
            const incoming = newLogs.slice(-LOG_HISTORY_LIMIT).map(d => ({ ...d, time: new Date() }))
            if (pausedRef.current) {
                heldRef.current = heldRef.current.concat(incoming).slice(-LOG_HISTORY_LIMIT)
                setHeld(heldRef.current.length)
                return
            }
            logsRef.current = logsRef.current.concat(incoming).slice(-LOG_HISTORY_LIMIT)
            if (!clearingRef.current) setLogs(logsRef.current)
        }

        if (logsStreamReader != null) {
            logsStreamReader.subscribe('data', handleLog)
            const buffer = logsStreamReader.buffer()
            const cleared = clearedThrough?.client === client ? buffer.indexOf(clearedThrough.entry) : -1
            logsRef.current = cleared >= 0 ? buffer.slice(cleared + 1) : buffer
            setLogs(logsRef.current)
        }
        return () => logsStreamReader?.unsubscribe('data', handleLog)
    }, [client, logsStreamReader])

    function togglePause () {
        const next = !pausedRef.current
        pausedRef.current = next
        setPaused(next)
        if (!next && heldRef.current.length > 0) {
            logsRef.current = logsRef.current.concat(heldRef.current).slice(-LOG_HISTORY_LIMIT)
            heldRef.current = []
            setHeld(0)
            followRef.current = true
            if (!clearingRef.current) setLogs(logsRef.current)
        }
    }

    function clear () {
        if (clearingRef.current) return
        const buffer = logsStreamReader.buffer()
        const entry = buffer[buffer.length - 1]
        clearedThrough = entry === undefined ? undefined : { client, entry }
        logsRef.current = []
        heldRef.current = []
        setHeld(0)
        const finish = () => {
            clearTimer.current = undefined
            clearingRef.current = false
            setLogs(logsRef.current)
            setClearing(false)
        }
        if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
            finish()
            return
        }
        // Keep the old rendered rows for their exit. New lines enter the bounded
        // logical list and appear after the animation; they are never cleared late.
        clearingRef.current = true
        setClearing(true)
        clearTimer.current = window.setTimeout(finish, 220)
    }

    return (
        <div className="page logs-page">
            <Header title={ t('title') } meta={<span className="logs-count"><span className="reading">{logs.length}</span> {t('lines')}</span>}>
                <span className="logs-level-label">{t('levelLabel')}</span>
                <Select
                    className="logs-level-select"
                    ariaLabel={t('levelLabel')}
                    options={logLevelOptions}
                    value={logLevel}
                    onSelect={level => setConfig(c => { c.logLevel = level })}
                />
                <Button className="logs-pause" icon={paused ? 'play' : 'pause'} pressed={paused} onClick={togglePause}>{paused ? t('resume') : t('pause')}</Button>
                <Button className="logs-clear" icon="trash" disabled={clearing || (logs.length === 0 && held === 0)} onClick={clear}>{t('clear')}</Button>
            </Header>

            <Card className={classnames('logs-card', { 'is-paused': paused, 'is-clearing': clearing })}>
                <ul className="logs-panel" ref={listRef}>
                    {
                        logs.map(
                            (log, index) => (
                                <li className={classnames('log-line', logMap.get(log.type))} key={index}>
                                    <time className="log-time">{ dayjs(log.time).format('YYYY-MM-DD HH:mm:ss') }</time>
                                    <span className="log-level">{ log.type.toUpperCase() }</span>
                                    <span className="log-payload">{ log.payload }</span>
                                </li>
                            ),
                        )
                    }
                </ul>
                {logs.length === 0 && !paused && <div className="logs-empty" role="status">
                    <span className="logs-empty-pulse" aria-hidden="true" />
                    {t('empty')}
                </div>}
                <AnimatePresence>
                    {paused && <motion.div
                        key="paused"
                        className="logs-paused"
                        role="status"
                        initial={{ opacity: 0, y: 12, scale: 0.98 }}
                        animate={{ opacity: 1, y: 0, scale: 1 }}
                        exit={{ opacity: 0, y: 12, scale: 0.98 }}
                        transition={springs.snap}>
                        <span className="logs-paused-lamp" aria-hidden="true" />
                        <span className="logs-paused-text">{t('paused')}</span>
                        {held > 0 && <span className="logs-paused-count"><b key={held}>{held}</b> {t('pending')}</span>}
                        <button type="button" className="logs-paused-resume" onClick={togglePause}>{t('resume')}</button>
                    </motion.div>}
                </AnimatePresence>
            </Card>
        </div>
    )
}

export default function Logs () {
    const client = useClient()
    const controller = useRef({ client, generation: 0 })
    if (controller.current.client !== client) {
        controller.current = { client, generation: controller.current.generation + 1 }
    }
    return <ControllerLogs key={controller.current.generation} client={client} />
}
