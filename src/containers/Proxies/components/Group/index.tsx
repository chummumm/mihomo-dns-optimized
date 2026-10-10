import classnames from 'classnames'
import { useAtom } from 'jotai'
import { useMemo, useRef } from 'react'

import { Tags, Tag, Icon, useLineColor, delayStatus } from '@components'
import { type Group as IGroup } from '@lib/request'
import { useProxy, useConfig, proxyMapping, useClient } from '@stores'

import './style.css'

interface GroupProps {
    config: IGroup
}

export function Group (props: GroupProps) {
    const { markProxySelected } = useProxy()
    const [proxyMap] = useAtom(proxyMapping)
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
    const nowDelay = config.now ? readings.get(config.now) ?? 0 : 0
    const nowFailed = config.now ? errSet.has(config.now) : false
    const nowStatus = nowFailed ? 'bad' : delayStatus(nowDelay)

    const canClick = config.type === 'Selector'
    return (
        <div className="proxy-group" style={{ '--line': line } as React.CSSProperties}>
            <div className="proxy-group-head">
                <div className="proxy-group-title">
                    <span className="proxy-group-badge" title={config.name}>{ config.name }</span>
                    <Tag>{ config.type }</Tag>
                </div>
                {config.now && <div className="proxy-group-now" title={config.now}>
                    <Icon type="arrow-right" size={12} className="proxy-group-now-arrow" replay={config.now} />
                    <span className={classnames('proxy-group-now-name', { 'is-new': nowSeen.current.moved })} key={config.now}>{config.now}</span>
                    {nowDelay > 0 && !nowFailed && <span className={classnames('reading', `status-${nowStatus}`)} key={nowDelay}>{nowDelay}ms</span>}
                </div>}
            </div>
            <Tags
                id={config.name}
                className="proxy-group-stations"
                data={config.all}
                onClick={handleChangeProxySelected}
                errSet={errSet}
                readings={readings}
                line={line}
                select={config.now}
                canClick={canClick}
                rowHeight={40} />
        </div>
    )
}
