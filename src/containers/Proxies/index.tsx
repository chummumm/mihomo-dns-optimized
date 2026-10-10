import { motion } from 'motion/react'
import { useMemo } from 'react'

import { Button, Header, Checkbox, springs } from '@components'
import EE from '@lib/event'
import { useRound } from '@lib/hook'
import type * as API from '@lib/request'
import { useI18n, useConfig, useProxy, useProxyProviders, useGeneral } from '@stores'

import { useTestProgress, useTestsRunning } from './activity'
import { Proxy, Group, Provider, Topology } from './components'
import './style.css'

enum sortType {
    None,
    Asc,
    Desc,
}

const sortMap = {
    [sortType.None]: 'sort',
    [sortType.Asc]: 'sort-ascending',
    [sortType.Desc]: 'sort-descending',
}

const sortLabel = {
    [sortType.None]: 'sort.none',
    [sortType.Asc]: 'sort.asc',
    [sortType.Desc]: 'sort.desc',
} as const

export function compareDesc (a: API.Proxy, b: API.Proxy) {
    const lastDelayA = (a.history.length > 0) ? a.history.slice(-1)[0].delay : 0
    const lastDelayB = (b.history.length > 0) ? b.history.slice(-1)[0].delay : 0
    const delayA = a.alive === false ? 0 : lastDelayA
    const delayB = b.alive === false ? 0 : lastDelayB
    return (delayB || Number.MAX_SAFE_INTEGER) - (delayA || Number.MAX_SAFE_INTEGER)
}

function ProxyGroups () {
    const { groups, global } = useProxy()
    const { data: config, set: setConfig } = useConfig()
    const { general } = useGeneral()
    const { translation } = useI18n()
    const { t } = translation('Proxies')

    const list = useMemo(
        () => general.mode === 'global' ? [global, ...groups] : groups,
        [general, groups, global],
    )

    return <>
        {
            list.length !== 0 &&
            <section className="proxy-section">
                <Header level={2} title={t('groupTitle')} meta={<span className="reading">{list.length}</span>}>
                    <Checkbox
                        className="proxy-break-connections"
                        checked={config.breakConnections}
                        onChange={value => setConfig('breakConnections', value)}>
                        {t('breakConnectionsText')}
                    </Checkbox>
                </Header>
                <ul className="proxy-groups">
                    {
                        list.map(p => (
                            <li key={p.name}>
                                <Group config={p} />
                            </li>
                        ))
                    }
                </ul>
            </section>
        }
    </>
}

function ProxyProviders () {
    const { providers } = useProxyProviders()
    const { translation: useTranslation } = useI18n()
    const { t } = useTranslation('Proxies')

    return <>
        {
            providers.length !== 0 &&
            <section className="proxy-section">
                <Header level={2} title={t('providerTitle')} meta={<span className="reading">{providers.length}</span>} />
                <ul className="proxy-provider-list">
                    {
                        providers.map(p => (
                            <li key={p.name}>
                                <Provider provider={p} />
                            </li>
                        ))
                    }
                </ul>
            </section>
        }
    </>
}

function Proxies () {
    const { proxies } = useProxy()
    const { translation: useTranslation } = useI18n()
    const { t } = useTranslation('Proxies')

    const { current: sort, next } = useRound(
        [sortType.Asc, sortType.Desc, sortType.None],
    )
    const sortedProxies = useMemo(() => {
        switch (sort) {
            case sortType.Desc:
                return proxies.slice().sort((a, b) => compareDesc(a, b))
            case sortType.Asc:
                return proxies.slice().sort((a, b) => -1 * compareDesc(a, b))
            default:
                return proxies.slice()
        }
    }, [sort, proxies])
    const handleSort = next

    return <>
        {
            sortedProxies.length !== 0 &&
            <section className="proxy-section">
                <Header level={2} title={t('nodesTitle')} meta={<><span className="reading">{sortedProxies.length}</span><span className="proxy-section-hint">{t('nodesHint')}</span></>}>
                    <Button type="ghost" size="sm" icon={sortMap[sort]} onClick={handleSort} title={t(sortLabel[sort])}>{t(sortLabel[sort])}</Button>
                </Header>
                <ul className="proxies-list">
                    {
                        sortedProxies.map(p => (
                            // Re-sorting after a speed test slides tiles to their new places.
                            <motion.li key={p.name} layout="position" transition={springs.reflow}>
                                <Proxy config={p} />
                            </motion.li>
                        ))
                    }
                </ul>
            </section>
        }
    </>
}

export default function ProxyContainer () {
    const { translation: useTranslation } = useI18n()
    const { t } = useTranslation('Proxies')
    const testing = useTestsRunning()
    const progress = useTestProgress()
    const { groups, proxies } = useProxy()
    const { providers } = useProxyProviders()

    function handleNotitySpeedTest () {
        EE.notifySpeedTest()
    }

    // Summary readings under the title, from the lists already on the page.
    const summary = useMemo(() => {
        const nodes = [...proxies, ...providers.flatMap(provider => provider.proxies as API.Proxy[])]
        const available = nodes.filter(node => node.alive !== false && (node.history?.length ?? 0) > 0 && node.history[node.history.length - 1].delay > 0).length
        return { groups: groups.length, nodes: nodes.length, available }
    }, [groups, proxies, providers])

    const meta = <span className="proxies-summary">
        <span><span className="reading">{summary.groups}</span> {t('summary.groups')}</span>
        <span><span className="reading">{summary.nodes}</span> {t('summary.nodes')}</span>
        <span><span className="reading status-good">{summary.available}</span> {t('summary.available')}</span>
    </span>

    return (
        <div className="page proxies-page">
            <Header title={t('title')} meta={meta}>
                <Button type="primary" icon="speed" loading={testing} onClick={handleNotitySpeedTest}>
                    {testing ? <>{t('testing')} <span className="reading">{progress.done}/{progress.total}</span></> : t('speedTestText')}
                </Button>
            </Header>
            <Topology />
            <ProxyGroups />
            <ProxyProviders />
            <Proxies />
        </div>
    )
}
