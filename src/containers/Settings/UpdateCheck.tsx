import classnames from 'classnames'
import { useSetAtom } from 'jotai'
import { motion } from 'motion/react'
import { useEffect, useState } from 'react'

import { Button, Card, Icon, springs } from '@components'
import type { Client } from '@lib/request'
import { useI18n, version as versionAtom } from '@stores'

import { currentUpdateSchedules, formatUpdateTime, nextUpdateTime, updateTaskActive, type UpdateItem, type UpdateItemStatus } from './update-state'
import { useUpdateTask } from './use-update-task'

const RELEASES = 'https://github.com/chummumm/mihomo-optimized/releases'
const DASHBOARD_SOURCE = 'https://github.com/chummumm/mihomo-optimized/commits/clash-dashboard'

const statusIcons: Record<UpdateItemStatus, string> = {
    pending: 'info-o',
    checking: 'update',
    updated: 'check-circle',
    up_to_date: 'check-circle',
    skipped: 'info-o',
    error: 'warning',
    restarting: 'update',
}

function formatClock (value: string | undefined) {
    if (!value) return ''
    const date = new Date(value)
    return Number.isFinite(date.getTime()) ? date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
}

function shortVersion (version: string | undefined) {
    return version?.startsWith('mihomo-optimized-v') === true ? version.slice('mihomo-optimized-'.length) : version ?? ''
}

interface ScheduleClock {
    requestedAt?: number
    now: number
    monotonic: number
}

function UpdateSchedule ({ item, clock, group = false, stale = false }: { item: UpdateItem, clock: ScheduleClock, group?: boolean, stale?: boolean }) {
    const { translation } = useI18n()
    const { t } = translation('Settings')
    if (!item.auto_update) return null
    const next = nextUpdateTime(item, clock.requestedAt, clock.now, clock.monotonic)
    const active = item.auto_update_state === 'checking' || item.auto_update_state === 'waiting'
    const phase = active ? t(item.auto_update_state === 'checking' ? 'update.autoChecking' : 'update.autoWaiting') : undefined
    const due = next !== undefined && next <= clock.now

    return <span className="settings-update-schedule" data-update-schedule={item.id}>
        {phase && <span>{phase}</span>}
        {next !== undefined
            ? <span>{t(item.auto_update_state === 'retrying' ? 'update.nextRetry' : group ? 'update.nextGroupCheck' : 'update.nextCheck')} <time dateTime={new Date(next).toISOString()}>{formatUpdateTime(next)}</time></span>
            : !active && <span>{t(item.auto_update_state === 'unscheduled' ? 'update.notScheduled' : 'update.scheduleUnavailable')}</span>}
        {stale ? <span>{t('update.scheduleStale')}</span> : due && !active && <span>{t('update.scheduleDue')}</span>}
    </span>
}

function UpdateRow ({ item, label, version, idle, clock, stale, uncertain = false }: { item: UpdateItem, label: string, version?: string, idle: boolean, clock: ScheduleClock, stale: boolean, uncertain?: boolean }) {
    const { translation } = useI18n()
    const { t } = translation('Settings')
    const unconfirmed = uncertain && (item.status === 'pending' || item.status === 'checking' || item.status === 'restarting')
    const state = unconfirmed ? 'skipped' : item.status
    const operationErrors = ['busy', 'timeout', 'configuration_changed', 'version_unavailable', 'update_failed'] as const
    const operationError = operationErrors.find(value => value === item.error) ?? 'update_failed'
    const text = unconfirmed
        ? t('update.unconfirmed')
        : item.status === 'error'
            ? t(`update.operationErrors.${operationError}`)
            : item.status === 'pending'
                ? t(idle ? 'update.notChecked' : 'update.pending')
                : item.status === 'skipped'
                    ? t(item.reason === 'local_provider' ? 'update.localProvider' : item.reason === 'embedded' ? 'update.embedded' : 'update.notConfigured')
                    : t(`update.states.${item.status}`)
    const policyText = item.auto_update === undefined
        ? undefined
        : item.auto_update
            ? [t('update.autoOn'), item.update_interval ? `${t('update.every')} ${item.update_interval} ${t(item.interval_unit === 'seconds' ? 'update.seconds' : 'update.hours')}` : ''].filter(Boolean).join(' · ')
            : t('update.autoOff')
    const shownVersion = version ?? item.current_version
    const busy = state === 'checking' || state === 'restarting'

    return <div className="settings-row settings-update-row" data-update-id={item.id} data-update-status={state}>
        <div className="settings-update-name">
            <span className="label">{label}</span>
            {shownVersion && <span className="settings-update-version" title={shownVersion}>{shownVersion}</span>}
            {item.latest_version && item.latest_version !== shownVersion && <span className="settings-update-version" title={item.latest_version}>{t('update.latestIs')} {shortVersion(item.latest_version)}</span>}
        </div>
        <div className="settings-update-result">
            <motion.span
                key={`${state}:${item.error ?? ''}:${unconfirmed}`}
                className={classnames('settings-update-status', `is-${state}`)}
                initial={idle ? false : { opacity: 0, y: 6 }}
                animate={{ opacity: 1, y: 0 }}
                transition={springs.snap}>
                <Icon type={statusIcons[state]} size={15} className={classnames({ 'is-busy': busy })} />
                <span>{text}</span>
            </motion.span>
            {policyText && <span className="settings-update-policy">{policyText}</span>}
            <UpdateSchedule item={item} clock={clock} stale={stale} />
        </div>
    </div>
}

export function UpdateCheck ({ client, kernelVersion }: { client: Client, kernelVersion: string }) {
    const { translation } = useI18n()
    const { t } = translation('Settings')
    const task = useUpdateTask(client)
    const [, refreshClock] = useState(0)
    useEffect(() => {
        const refresh = () => { if (!document.hidden) refreshClock(value => value + 1) }
        const timer = window.setInterval(refresh, 30000)
        document.addEventListener('visibilitychange', refresh)
        return () => {
            window.clearInterval(timer)
            document.removeEventListener('visibilitychange', refresh)
        }
    }, [])
    const clock = { requestedAt: task.scheduleRequestedAt, now: Date.now(), monotonic: performance.now() }
    const staleSchedule = task.problem !== undefined || task.disconnected
    const setVersion = useSetAtom(versionAtom)
    const snapshot = task.snapshot
    const previous = task.restart ? task.previous : undefined
    const resultItems = currentUpdateSchedules(previous?.items ?? snapshot?.items ?? [], snapshot?.items ?? [])
    // If a POST response was lost and its task cannot be recovered, old
    // terminal results belong to an earlier run. Never label them as this run.
    const items = task.unconfirmedStart ? resultItems.map(item => ({ ...item, status: 'pending' as const, error: undefined })) : resultItems
    const running = updateTaskActive(snapshot)
    const idle = !previous && (snapshot === undefined || snapshot.state === 'idle')
    const core = items.find(item => item.kind === 'kernel') ?? { id: 'kernel', kind: 'kernel', name: 'kernel', status: 'pending' } as UpdateItem
    const ui = items.find(item => item.kind === 'ui') ?? { id: 'ui', kind: 'ui', name: 'ui', status: 'pending' } as UpdateItem
    const currentCore = snapshot?.items.find(item => item.kind === 'kernel')
    const currentUI = snapshot?.items.find(item => item.kind === 'ui')
    useEffect(() => {
        if (task.restart && currentCore?.current_version) {
            const version = currentCore.current_version
            setVersion(client, previous => previous.version === version ? previous : { ...previous, version })
        }
    }, [client, task.restart, currentCore?.current_version, setVersion])
    const completedCore: UpdateItem = task.restart === 'verified' ? { ...core, current_version: currentCore?.current_version, status: 'updated' } : core
    const geodata = items.filter(item => item.kind === 'geodata')
    const providers = items.filter(item => item.kind === 'rule-provider')
    const uiInstalled = currentUI?.current_version
    const reloadAvailable = ui.status === 'updated' && uiInstalled && uiInstalled !== __UI_VERSION__
    const retry = task.problem !== undefined && !task.disconnected
    const busy = task.loading || task.starting || task.disconnected || (running && !retry)
    const finished = snapshot?.finished_at ?? previous?.finished_at
    const footer = task.disconnected
        ? t('update.reconnecting')
        : task.problem
            ? t(`update.errors.${task.problem}`)
            : task.startRejected === true || task.unconfirmedStart === true
                ? t(task.startRejected ? 'update.requestBusy' : 'update.requestUnconfirmed')
                : task.starting || running
                    ? t(snapshot?.state === 'restarting' ? 'update.restartingHint' : 'update.checkingHint')
                    : task.restart
                        ? t(task.restart === 'verified' ? 'update.restartVerified' : 'update.restartUnconfirmed')
                        : snapshot?.state === 'partial'
                            ? t('update.summaryPartial')
                            : snapshot?.state === 'completed' ? t('update.summaryDone') : t('update.hint')

    function group (rows: UpdateItem[], label: string) {
        const enabled = rows.filter(item => item.auto_update)
        const nearest = enabled.reduce<UpdateItem | undefined>((best, item) => {
            if (!best) return item
            const next = nextUpdateTime(item, clock.requestedAt, clock.now, clock.monotonic)
            const earlier = nextUpdateTime(best, clock.requestedAt, clock.now, clock.monotonic)
            return next !== undefined && (earlier === undefined || next < earlier) ? item : best
        }, undefined)
        const active = enabled.find(item => item.auto_update_state === 'checking' || item.auto_update_state === 'waiting')
        const schedule = nearest
        return <details className="settings-update-group">
            <summary>
                <span className="settings-update-group-name">{label}</span>
                <span className="settings-update-group-meta">
                    <span className="settings-update-group-count">{rows.filter(item => item.reason !== 'not_configured').length} {t('update.items')}{rows.some(item => item.status === 'error') ? ` · ${t('update.hasErrors')}` : ''}</span>
                    {active && active !== schedule && <span className="settings-update-schedule">{t(active.auto_update_state === 'checking' ? 'update.autoChecking' : 'update.autoWaiting')}</span>}
                    {schedule && <UpdateSchedule item={schedule} clock={clock} group stale={staleSchedule} />}
                </span>
                <Icon type="caret-right" size={14} />
            </summary>
            {rows.length
                ? rows.map(item => <UpdateRow key={item.id} item={item} label={item.name} idle={idle} clock={clock} stale={staleSchedule} uncertain={task.restart !== undefined || task.unconfirmedStart} />)
                : <p className="settings-update-empty">{t(task.loading ? 'update.loading' : 'update.noResources')}</p>}
        </details>
    }

    return <Card className="settings-card settings-update" aria-busy={busy}>
        <UpdateRow item={completedCore} label={t('update.kernel')} version={currentCore?.current_version ? currentCore.current_version : task.loading ? '' : kernelVersion} idle={idle} clock={clock} stale={staleSchedule} uncertain={task.restart === 'unconfirmed' || task.unconfirmedStart} />
        <UpdateRow item={ui} label={t('update.dashboard')} version={__UI_VERSION__} idle={idle} clock={clock} stale={staleSchedule} uncertain={task.restart !== undefined || task.unconfirmedStart} />
        {uiInstalled && uiInstalled !== __UI_VERSION__ && <div className="settings-update-installed">
            <span>{t('update.installedVersion')} {uiInstalled}</span>
            {reloadAvailable && <button type="button" className="settings-update-link" onClick={() => window.location.reload()}>{t('update.reloadPage')}<Icon type="update" size={12} /></button>}
        </div>}
        {group(geodata, t('update.geodata'))}
        {group(providers, t('update.ruleProviders'))}
        <div className="settings-update-sources">
            <span>{t('update.policyHint')}</span>
            <span>{t('update.scheduleHint')}</span>
            <a className="settings-update-link" href={RELEASES} target="_blank" rel="noreferrer noopener">{t('update.coreReleases')}<Icon type="arrow-square-out" size={12} /></a>
            <a className="settings-update-link" href={DASHBOARD_SOURCE} target="_blank" rel="noreferrer noopener">{t('update.uiReleases')}<Icon type="arrow-square-out" size={12} /></a>
        </div>
        <div className="settings-row settings-row-action settings-update-footer">
            <span className="settings-update-summary" role={task.problem && !task.disconnected ? 'alert' : 'status'}>
                {footer}{finished && !running ? ` · ${formatClock(finished)}` : ''}
            </span>
            <Button icon="arrow-circle-up" loading={busy} disabled={busy || task.problem === 'unsupported'} onClick={() => { void task.start() }}>{t(busy ? 'update.checking' : retry ? 'update.retry' : 'update.check')}</Button>
        </div>
    </Card>
}
