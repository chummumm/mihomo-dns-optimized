import { Button } from '@components'

import { formatDNSTime } from './helpers'
import { type DNSError, useDNSI18n } from './hooks'

interface StatusProps {
    error?: DNSError
    loading: boolean
    updatedAt?: number
    refresh: () => void
}

export function DNSStatus ({ error, loading, updatedAt, refresh }: StatusProps) {
    const { t } = useDNSI18n()
    return <div className="dns-resource-status">
        <span className={error ? 'dns-error' : 'dns-muted'} role="status">
            {error ? t(`errors.${error}`) : updatedAt ? `${t('updated')}: ${formatDNSTime(updatedAt)}` : t('loading')}
        </span>
        <Button type="ghost" size="sm" icon="update" loading={loading} disabled={loading} onClick={refresh}>{t('refresh')}</Button>
    </div>
}
