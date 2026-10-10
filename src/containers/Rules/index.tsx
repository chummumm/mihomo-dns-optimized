import AutoSizer from 'react-virtualized-auto-sizer'
import { FixedSizeList as List } from 'react-window'
import useSWR from 'swr'

import { Header, Card, LineBullet } from '@components'
import { useI18n, useRule, useRuleProviders } from '@stores'

import { Provider } from './Provider'
import './style.css'

function RuleProviders () {
    const { providers } = useRuleProviders()
    const { translation } = useI18n()
    const { t } = translation('Rules')

    return <>
        {
            providers.length !== 0 &&
            <section className="rules-section">
                <Header level={2} title={t('providerTitle')} meta={<span className="reading">{providers.length}</span>} />
                <Card className="rule-providers">
                    {
                        providers.map(p => (
                            <Provider key={p.name} provider={p} />
                        ))
                    }
                </Card>
            </section>
        }
    </>
}

export default function Rules () {
    const { rules, update } = useRule()
    const { translation } = useI18n()
    const { t } = translation('Rules')

    useSWR('rules', update)

    function renderRuleItem ({ index, style }: { index: number, style: React.CSSProperties }) {
        const rule = rules[index]
        return (
            <li className="rule-item" style={style}>
                <span className="rule-index">{ index + 1 }</span>
                <span className="rule-type"><span className="rule-type-tag">{ rule.type }</span></span>
                <span className="payload" title={rule.payload}>{ rule.payload || '*' }</span>
                <span className="rule-proxy" title={rule.proxy}><LineBullet name={rule.proxy} />{ rule.proxy }</span>
            </li>
        )
    }

    return (
        <div className="page rules-page">
            <Header title={t('title')} meta={<span className="reading">{rules.length}</span>} />
            <RuleProviders />
            <Card className="rules-card">
                <div className="rule-head" aria-hidden="true">
                    <span className="rule-index">#</span>
                    <span className="rule-type">{t('columns.type')}</span>
                    <span className="payload">{t('columns.payload')}</span>
                    <span className="rule-proxy">{t('columns.proxy')}</span>
                </div>
                <div className="rules-list">
                    <AutoSizer>
                        {
                            ({ height, width }) => (
                                <List
                                    height={height ?? 0}
                                    width={width ?? 0}
                                    itemCount={rules.length}
                                    itemSize={40}
                                    innerElementType="ul"
                                >
                                    { renderRuleItem }
                                </List>
                            )
                        }
                    </AutoSizer>
                </div>
            </Card>
        </div>
    )
}
