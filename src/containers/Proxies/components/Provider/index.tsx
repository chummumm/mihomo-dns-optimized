import { motion } from 'motion/react'
import { useMemo, useState } from 'react'

import { Button, Card, Tag, Loading, springs } from '@components'
import { compareDesc } from '@containers/Proxies'
import { Proxy } from '@containers/Proxies/components/Proxy'
import { fromNow } from '@lib/date'
import { useVisible } from '@lib/hook'
import { type Provider as IProvider, type Proxy as IProxy } from '@lib/request'
import { useClient, useI18n, useProxyProviders } from '@stores'

import './style.css'

interface ProvidersProps {
    provider: IProvider
}

export function Provider (props: ProvidersProps) {
    const { update } = useProxyProviders()
    const { translation, lang } = useI18n()
    const client = useClient()

    const { provider } = props
    const { t } = translation('Proxies')

    const { visible, hide, show } = useVisible()
    // UI only: which action is running, so its own icon can show the work.
    const [action, setAction] = useState<'check' | 'update'>()

    function handleHealthChech () {
        setAction('check')
        show()
        client.healthCheckProvider(provider.name).then(async () => await update()).finally(() => hide())
    }

    function handleUpdate () {
        setAction('update')
        show()
        client.updateProvider(provider.name).then(async () => await update()).finally(() => hide())
    }

    const proxies = useMemo(() => {
        return (provider.proxies as IProxy[]).slice().sort((a, b) => -1 * compareDesc(a, b))
    }, [provider.proxies])

    return (
        <Card className="proxy-provider">
            <Loading visible={visible} />
            <div className="proxy-provider-head">
                <div className="proxy-provider-title">
                    <span className="proxy-provider-name">{ provider.name }</span>
                    <Tag>{ provider.vehicleType }</Tag>
                    <span className="proxy-provider-count reading">{ proxies.length }</span>
                </div>
                <div className="proxy-provider-actions">
                    {
                        provider.updatedAt &&
                        <span className="proxy-provider-updated">{ `${t('providerUpdateTime')}: ${fromNow(new Date(provider.updatedAt), lang)}`}</span>
                    }
                    <Button type="ghost" size="sm" icon="healthcheck" loading={visible && action === 'check'} disabled={visible} onClick={handleHealthChech}>{t('healthCheck')}</Button>
                    <Button type="ghost" size="sm" icon="update" loading={visible && action === 'update'} disabled={visible} onClick={handleUpdate}>{t('update')}</Button>
                </div>
            </div>
            <ul className="proxies-list">
                {
                    proxies.map((p: IProxy) => (
                        <motion.li key={p.name} layout="position" transition={springs.reflow}>
                            <Proxy className="proxy-provider-item" config={p} />
                        </motion.li>
                    ))
                }
            </ul>
        </Card>
    )
}
