import { useCallback, useRef, useState } from 'react'

import { Button, Card, Header } from '@components'
import type { Client } from '@lib/request'
import { useClient } from '@stores'

import { DNSOverview } from './Overview'
import { DNSQueriesPage } from './Queries'
import { DNSUpstreamPage } from './Upstreams'
import { formatDNSNumber, formatDNSTime } from './helpers'
import { useDNSI18n, useDNSResource } from './hooks'
import './style.scss'

type Tab = 'overview' | 'queries' | 'upstreams'
const tabs: Tab[] = ['overview', 'queries', 'upstreams']

function DNSControllerPage ({ client }: { client: Client }) {
    const { t } = useDNSI18n()
    const [tab, setTab] = useState<Tab>('overview')
    const fetcher = useCallback(async (signal: AbortSignal) => (await client.getDNSObservability(signal)).data, [client])
    const resource = useDNSResource(fetcher, 10000)
    const capability = resource.data
    const hardError = resource.error === 'unsupported' || resource.error === 'unauthorized'
    const compatible = capability?.version === 1 && capability.storage === 'memory'

    return <div className="dns-page page">
        <Header title={t('title')} />
        {hardError || !capability
            ? <Card className="dns-state-card">
                <h2>{resource.error ? t(`errors.${resource.error}`) : t('loading')}</h2>
                {resource.error === 'unsupported' && <p>{t('unsupportedHint')}</p>}
                {!resource.loading && <Button onClick={resource.refresh}>{t('retry')}</Button>}
            </Card>
            : !compatible
                ? <Card className="dns-state-card"><h2>{t('versionMismatch')}</h2></Card>
                : !capability.enabled
                    ? <Card className="dns-state-card"><h2>{t('disabled')}</h2><p>{t('disabledHint')}</p><Button onClick={resource.refresh}>{t('refresh')}</Button></Card>
                    : <>
                        <div className="dns-storage-note">
                            <span>{t('memoryOnly')} · {t('retention')}: {formatDNSNumber(capability.retention_seconds / 3600)} h · {t('capacity')}: {formatDNSNumber(capability.capacity)} · {t('memoryLimit')}: {formatDNSNumber(capability.accounted_bytes / 1024 / 1024, 2)} / {formatDNSNumber(capability.memory_limit_bytes / 1024 / 1024, 1)} MiB</span>
                            <span>{t('startedAt')}: {formatDNSTime(capability.started_at, true)}</span>
                        </div>
                        {resource.error && <p className="dns-notice" role="status">{t(`errors.${resource.error}`)}</p>}
                        <div className="dns-tabs" role="tablist" aria-label={t('title')}>
                            {tabs.map(item => <button key={item} id={`dns-tab-${item}`} type="button" role="tab" aria-selected={tab === item} aria-controls="dns-panel" className={tab === item ? 'dns-tab active' : 'dns-tab'} onClick={() => setTab(item)}>{t(`tabs.${item}`)}</button>)}
                        </div>
                        <div id="dns-panel" role="tabpanel" aria-labelledby={`dns-tab-${tab}`} key={`${capability.instance_id}:${tab}`}>
                            {tab === 'overview' && <DNSOverview client={client} instanceID={capability.instance_id} instanceChanged={resource.refresh} />}
                            {tab === 'queries' && <DNSQueriesPage client={client} instanceID={capability.instance_id} instanceChanged={resource.refresh} />}
                            {tab === 'upstreams' && <DNSUpstreamPage client={client} instanceID={capability.instance_id} instanceChanged={resource.refresh} />}
                        </div>
                    </>}
    </div>
}

export default function DNS () {
    const client = useClient()
    const controller = useRef({ client, generation: 0 })
    if (controller.current.client !== client) {
        controller.current = { client, generation: controller.current.generation + 1 }
    }
    return <DNSControllerPage key={controller.current.generation} client={client} />
}
