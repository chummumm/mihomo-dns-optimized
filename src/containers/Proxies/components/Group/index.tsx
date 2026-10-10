import classnames from 'classnames'
import { useAtomValue } from 'jotai'
import { useMemo, useRef } from 'react'

import { Card, Ring, SlideText, Tags, Tag, TweenNumber, latencyQuality, useLineColor, delayStatus } from '@components'
import { type Group as IGroup } from '@lib/request'
import { useProxy, useConfig, proxyMapping, proxyGroupMapping, useClient, useI18n } from '@stores'

import { resolveProxyExit } from '../../resolve'
import './style.css'

interface GroupProps {
    config: IGroup
}

export function Group (props: GroupProps) {
    const { markProxySelected } = useProxy()
    const { translation } = useI18n()
    const { t } = translation('Proxies')
    const proxyMap = useAtomValue(proxyMapping)
    const groups = useAtomValue(proxyGroupMapping)
    const { data: Config } = useConfig()
    const client = useClient()
    const color = useLineColor()
    const { config } = props

    // UI only: once the selection moves after mount, the summary animates its change.
    const nowSeen = useRef({ name: config.now, moved: false })
    if (nowSeen.current.name !== config.now) nowSeen.current = { name: config.now, moved: true }

    async function handleChangeProxySelected (name: string) {
        await client.changeProxySelected(props.config.name, name)
        markProxySelected(props.config.name, name)
        if (Config.breakConnections) {
            const list: string[] = []
            const snapshot = await client.getConnections()
            for (const connection of snapshot.data.connections ?? []) {
                if (connection.chains.includes(props.config.name)) {
                    list.push(connection.id)
                }
            }

            await Promise.all(list.map(id => client.closeConnection(id)))
        }
    }

    const errSet = useMemo(() => {
        const set = new Set<string>()
        for (const proxy of config.all) {
            const history = proxyMap.get(proxy)?.history
            const alive = proxyMap.get(proxy)?.alive
            if (alive === false || (history?.length && history.slice(-1)[0].delay === 0)) {
                set.add(proxy)
            }
        }

        return set
    }, [config.all, proxyMap])

    // Latest reading per station, read from the same mapping as errSet.
    const readings = useMemo(() => {
        const map = new Map<string, number>()
        for (const proxy of config.all) {
            const history = proxyMap.get(proxy)?.history
            if (history?.length) map.set(proxy, history.slice(-1)[0].delay)
        }
        return map
    }, [config.all, proxyMap])

    const line = color(config.name)

    // Follow nested groups to the node that actually carries the traffic.
    const exit = useMemo(() => resolveProxyExit(config.now, groups), [config.now, groups])
    const exitProxy = exit === undefined ? undefined : proxyMap.get(exit)
    const exitHistory = exitProxy?.history
    const nowDelay = exitHistory?.length ? exitHistory.slice(-1)[0].delay : 0
    const nowFailed = exitProxy?.alive === false || (exitHistory?.length ? nowDelay === 0 : false)
    const nowStatus = nowFailed ? 'bad' : delayStatus(nowDelay)
    const tone = nowStatus === 'good' ? 'flow' : nowStatus

    let via = ''
    if (exit === 'DIRECT') via = t('directExit')
    else if (exit === 'REJECT' || exit === 'REJECT-DROP') via = t('reject')
    else if (exit !== undefined && exit !== config.now) via = `${t('via')} ${config.now} → ${exit}`
    else if (exitProxy != null) via = `${exitProxy.type}${exitProxy.udp ? ' · UDP' : ''}`
    if (config.type !== 'Selector' && via) via += ` · ${t('autoSelect')}`

    const canClick = config.type === 'Selector'
    return (
        <Card className="proxy-group" data-group={config.name} style={{ '--line': line } as React.CSSProperties}>
            <div className="proxy-group-head">
                <span className="proxy-group-hue" aria-hidden="true" />
                <span className="proxy-group-name" title={config.name}>{ config.name }</span>
                <Tag>{ config.type }</Tag>
                <span className="proxy-group-count"><span className="reading">{config.all.length}</span> {t('exits')}</span>
            </div>
            <div className="proxy-group-now">
                <Ring value={latencyQuality(nowDelay, nowFailed)} tone={tone} size={64}>
                    {nowDelay > 0 && !nowFailed
                        ? <TweenNumber className={classnames('proxy-group-ms', `status-${nowStatus}`)} value={nowDelay} />
                        : <span className={classnames('proxy-group-ms', { 'status-bad': nowFailed })}>{nowFailed ? '✕' : '—'}</span>}
                    <small>ms</small>
                </Ring>
                <div className="proxy-group-now-text">
                    {config.now
                        ? <SlideText className={classnames('proxy-group-now-name', { 'is-new': nowSeen.current.moved })} text={config.now} />
                        : <span className="proxy-group-now-name">—</span>}
                    {via && <small className="proxy-group-via" title={via}>{via}</small>}
                </div>
            </div>
            <Tags
                id={config.name}
                className="proxy-group-chips"
                data={config.all}
                onClick={handleChangeProxySelected}
                errSet={errSet}
                readings={readings}
                line={line}
                select={config.now}
                canClick={canClick}
                rowHeight={36} />
        </Card>
    )
}
