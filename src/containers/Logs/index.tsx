import classnames from 'classnames'
import dayjs from 'dayjs'
import { useLayoutEffect, useEffect, useRef, useState } from 'react'

import { Select, Card, Header } from '@components'
import { LOG_HISTORY_LIMIT, type Log } from '@models/Log'
import { useConfig, useI18n, useLogsStreamReader } from '@stores'

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

export default function Logs () {
    const listRef = useRef<HTMLUListElement>(null)
    const logsRef = useRef<Log[]>([])
    const [logs, setLogs] = useState<Log[]>([])
    const { translation } = useI18n()
    const { data: { logLevel }, set: setConfig } = useConfig()
    const { t } = translation('Logs')
    const logsStreamReader = useLogsStreamReader()
    const scrollHeightRef = useRef(listRef.current?.scrollHeight ?? 0)

    useLayoutEffect(() => {
        const ul = listRef.current
        if (ul != null && scrollHeightRef.current === (ul.scrollTop + ul.clientHeight)) {
            ul.scrollTop = ul.scrollHeight - ul.clientHeight
        }
        scrollHeightRef.current = ul?.scrollHeight ?? 0
    })

    useEffect(() => {
        function handleLog (newLogs: Log[]) {
            const incoming = newLogs.slice(-LOG_HISTORY_LIMIT).map(d => ({ ...d, time: new Date() }))
            logsRef.current = logsRef.current.concat(incoming).slice(-LOG_HISTORY_LIMIT)
            setLogs(logsRef.current)
        }

        if (logsStreamReader != null) {
            logsStreamReader.subscribe('data', handleLog)
            logsRef.current = logsStreamReader.buffer()
            setLogs(logsRef.current)
        }
        return () => logsStreamReader?.unsubscribe('data', handleLog)
    }, [logsStreamReader])

    return (
        <div className="page logs-page">
            <Header title={ t('title') } meta={<span className="reading">{logs.length}</span>}>
                <span className="logs-level-label">{t('levelLabel')}</span>
                <Select
                    className="logs-level-select"
                    ariaLabel={t('levelLabel')}
                    options={logLevelOptions}
                    value={logLevel}
                    onSelect={level => setConfig(c => { c.logLevel = level })}
                />
            </Header>

            <Card className="logs-card">
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
                {logs.length === 0 && <div className="logs-empty" role="status">
                    <span className="logs-empty-pulse" aria-hidden="true" />
                    {t('empty')}
                </div>}
            </Card>
        </div>
    )
}
