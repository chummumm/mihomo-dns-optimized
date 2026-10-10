import classnames from 'classnames'

import { Button, Tag } from '@components'
import { fromNow } from '@lib/date'
import { useVisible } from '@lib/hook'
import { type RuleProvider } from '@lib/request'
import { useClient, useI18n, useRuleProviders } from '@stores'
import './style.css'

interface ProvidersProps {
    provider: RuleProvider
}

export function Provider (props: ProvidersProps) {
    const { update } = useRuleProviders()
    const { translation, lang } = useI18n()
    const client = useClient()

    const { provider } = props
    const { t } = translation('Rules')

    const { visible, hide, show } = useVisible()

    function handleUpdate () {
        show()
        client.updateRuleProvider(provider.name).then(async () => await update()).finally(() => hide())
    }

    return (
        <div className={classnames('rule-provider', { 'is-updating': visible })}>
            <div className="rule-provider-header-part">
                <span className="rule-provider-name" title={provider.name}>{ provider.name }</span>
                <Tag>{ provider.vehicleType }</Tag>
                <Tag tone="ink">{ provider.behavior }</Tag>
            </div>
            <div className="rule-provider-header-part rule-provider-meta">
                <span className="rule-provider-update">{ t('ruleCount') }: <b className="reading">{ provider.ruleCount }</b></span>
                {
                    provider.updatedAt &&
                    <span className="rule-provider-update">{ `${t('providerUpdateTime')}: ${fromNow(new Date(provider.updatedAt), lang)}`}</span>
                }
                <Button type="ghost" size="sm" icon="update" loading={visible} disabled={visible} onClick={handleUpdate}>{t('update')}</Button>
            </div>
        </div>
    )
}
