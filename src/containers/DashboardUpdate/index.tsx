import axios from 'axios'
import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'

import { Button, Card, Icon } from '@components'
import { type Client, type DashboardUpdateStatus } from '@lib/request'
import { useAPIInfo, useClient, useI18n } from '@stores'

import { updateMessages } from './messages'
import { canInstallDashboardUpdate, compareDashboardVersions, dashboardControllerOrigin, isCoreHostedDashboard, readDashboardUpdateStatus } from './model'
import './style.css'

const dashboardVersion = DASHBOARD_VERSION
type Action = 'status' | 'check' | 'install'
type Phase = 'idle' | 'checking' | 'installing'
type Issue = 'oldCore' | 'unavailable' | 'checkError' | 'updateError' | 'busy' | 'failedRefresh'
interface ViewState {
    owner: Client
    status: DashboardUpdateStatus | null
    phase: Phase
    issue: Issue | null
    checked: boolean
    installed: boolean
}
interface Operation {
    owner: Client
    abort: AbortController
    promise: Promise<void>
}
interface UpdateContextValue {
    status: DashboardUpdateStatus | null
    phase: Phase
    issue: Issue | null
    checked: boolean
    ready: boolean
    editorOpen: boolean
    refreshing: boolean
    coreHosted: boolean
    coreUI: string
    setEditorOpen: (open: boolean) => void
    check: () => Promise<void>
    install: () => Promise<void>
    refresh: () => Promise<void>
    dismiss: () => void
}
const UpdateContext = createContext<UpdateContextValue | null>(null)

export function useDashboardUpdates () {
    const value = useContext(UpdateContext)
    if (value === null) throw new Error('DashboardUpdateProvider is missing')
    return value
}

export function DashboardUpdateProvider ({ children }: { children: ReactNode }) {
    const client = useClient()
    const api = useAPIInfo()
    const currentClient = useRef(client)
    currentClient.current = client
    const active = useRef<Operation | null>(null)
    const [state, setState] = useState<ViewState>({ owner: client, status: null, phase: 'idle', issue: null, checked: false, installed: false })
    const [editorOpen, setEditorOpen] = useState(false)
    const editorOpenRef = useRef(editorOpen)
    editorOpenRef.current = editorOpen
    const [refreshing, setRefreshing] = useState(false)
    const refreshOperation = useRef<object | null>(null)
    const [dismissed, setDismissed] = useState('')
    const controllerOrigin = dashboardControllerOrigin(api.protocol, api.hostname, api.port)
    const coreUI = controllerOrigin !== null ? new URL('/ui/', controllerOrigin).href : ''
    const coreHosted = controllerOrigin !== null && isCoreHostedDashboard(window.location, controllerOrigin)
    const view = state.owner === client ? state : { owner: client, status: null, phase: 'idle' as const, issue: null, checked: false, installed: false }
    const readyVersion = view.status?.current_version ?? ''
    const ready = view.installed || (coreHosted && compareDashboardVersions(readyVersion, dashboardVersion) === 1)
    const readyKey = view.status?.current_digest ?? (readyVersion + ':' + (view.status?.current_commit ?? ''))

    const run = useCallback((action: Action): Promise<void> => {
        if (active.current !== null) return active.current.promise
        const operation: Operation = { owner: client, abort: new AbortController(), promise: Promise.resolve() }
        active.current = operation
        const current = () => active.current === operation && !operation.abort.signal.aborted && currentClient.current === operation.owner
        setState(previous => ({
            ...(previous.owner === client ? previous : { owner: client, status: null, checked: false, installed: false }),
            phase: 'checking',
            issue: null,
        }))
        operation.promise = (async () => {
            let installing = false
            try {
                const response = await client.getDashboardUpdate(action !== 'status', operation.abort.signal)
                if (!current()) return
                const status = readDashboardUpdateStatus(response.data)
                if (status === null) {
                    setState(previous => ({ ...previous, status: null, issue: 'oldCore' }))
                    return
                }
                setState(previous => ({
                    ...previous,
                    status,
                    checked: action !== 'status',
                    installed: previous.installed || Boolean(previous.status?.current_digest && status.current_digest && previous.status.current_digest !== status.current_digest),
                    issue: status.check_error ? 'checkError' : null,
                }))
                if (action !== 'install' || !canInstallDashboardUpdate(status)) return
                installing = true
                setState(previous => ({ ...previous, phase: 'installing' }))
                await client.updateDashboard(operation.abort.signal)
                if (!current()) return
                // A lost status response must not turn a completed installation into
                // an "installation failed" claim. The running UI remains untouched.
                setState(previous => ({ ...previous, installed: true, checked: true, issue: null }))
                const installed = await client.getDashboardUpdate(false, operation.abort.signal).catch(() => null)
                if (current() && installed !== null) {
                    const next = readDashboardUpdateStatus(installed.data)
                    if (next !== null) setState(previous => ({ ...previous, status: next }))
                }
            } catch (reason) {
                if (!current()) return
                const code = axios.isAxiosError(reason) ? reason.response?.status : undefined
                const issue: Issue = code === 404 || code === 405
                    ? 'oldCore'
                    : code === 409 ? 'busy' : installing ? 'updateError' : action === 'status' ? 'unavailable' : 'checkError'
                setState(previous => ({ ...previous, issue }))
            } finally {
                if (current()) setState(previous => ({ ...previous, phase: 'idle' }))
                if (active.current === operation) active.current = null
            }
        })()
        return operation.promise
    }, [client])

    useEffect(() => {
        setEditorOpen(false)
        setDismissed('')
        setRefreshing(false)
        refreshOperation.current = null
        return () => {
            const operation = active.current
            if (operation?.owner === client) {
                operation.abort.abort()
                active.current = null
            }
        }
    }, [client])

    useEffect(() => {
        // Read the core's local status once per controller. Only the core's
        // YAML configuration controls scheduled checks and installations.
        void run('status')
    }, [run])

    const refresh = useCallback(async () => {
        if (editorOpenRef.current || refreshOperation.current !== null || !coreHosted) return
        const operation = {}
        refreshOperation.current = operation
        setRefreshing(true)
        try {
            // external-ui-url may switch to a dashboard without our sw.js.
            // Unregister only the current /ui/ worker before this explicit
            // refresh, so its old precached index cannot mask the new files.
            // The new page registers its own worker if it has one.
            if ('serviceWorker' in navigator) {
                const registration = await navigator.serviceWorker.getRegistration()
                if (currentClient.current !== client || refreshOperation.current !== operation || editorOpenRef.current) return
                if (registration !== undefined) {
                    const scope = new URL(registration.scope)
                    if (scope.origin === window.location.origin && scope.pathname.startsWith('/ui/')) await registration.unregister()
                }
            }
            if (currentClient.current !== client || refreshOperation.current !== operation || editorOpenRef.current) return
            window.location.reload()
        } catch {
            if (currentClient.current === client) {
                setState(previous => ({ ...previous, issue: 'failedRefresh' }))
            }
        } finally {
            if (refreshOperation.current === operation) {
                refreshOperation.current = null
                setRefreshing(false)
            }
        }
    }, [client, coreHosted])

    const check = useCallback(async () => { await run('check') }, [run])
    const install = useCallback(async () => { await run('install') }, [run])
    const dismiss = useCallback(() => { setDismissed(readyKey) }, [readyKey])
    const context = useMemo<UpdateContextValue>(() => ({
        status: view.status,
        phase: view.phase,
        issue: view.issue,
        checked: view.checked,
        ready,
        editorOpen,
        refreshing,
        coreHosted,
        coreUI,
        setEditorOpen,
        check,
        install,
        refresh,
        dismiss,
    }), [view.status, view.phase, view.issue, view.checked, ready, editorOpen, refreshing, coreHosted, coreUI, check, install, refresh, dismiss])

    return (
        <UpdateContext.Provider value={context}>
            {children}
            {ready && readyKey !== dismissed && <DashboardUpdateNotice />}
        </UpdateContext.Provider>
    )
}

function useCopy () {
    const { lang } = useI18n()
    return updateMessages[lang]
}

function StatusText () {
    const update = useDashboardUpdates()
    const copy = useCopy()
    if (update.phase === 'installing' || update.status?.updating) return <>{copy.updating}</>
    if (update.phase === 'checking') return <>{copy.checking}</>
    if (update.issue !== null) return <>{copy[update.issue]}</>
    if (update.status?.supported && update.status.reason === 'version-unavailable') return <>{copy.versionUnavailable}</>
    if (update.ready) return <>{copy.ready}</>
    if (update.status !== null && !update.status.supported) {
        const reason = update.status.reason
        return <>{reason === 'invalid-source' ? copy.invalidSource : reason === 'ui-not-configured' ? copy.noUI : copy.unsupported}</>
    }
    if (update.status?.update_available) return <>{copy.available}</>
    return <>{update.checked ? copy.upToDate : copy.checkFirst}</>
}

function RefreshAction () {
    const update = useDashboardUpdates()
    const copy = useCopy()
    return update.coreHosted
        ? <Button icon="update" size="sm" disabled={update.editorOpen || update.refreshing} loading={update.refreshing} title={update.editorOpen ? copy.editing : copy.refreshHint} onClick={() => { void update.refresh() }}>{update.refreshing ? copy.refreshing : copy.refresh}</Button>
        : update.coreUI
            ? <a className="dashboard-update-open" href={update.coreUI} target="_blank" rel="noopener noreferrer">{copy.open}<Icon type="arrow-up" size={12} /></a>
            : <Button size="sm" disabled>{copy.open}</Button>
}

function DashboardUpdateNotice () {
    const update = useDashboardUpdates()
    const copy = useCopy()
    return (
        <aside className="dashboard-update-notice" aria-label={copy.title} role="status">
            <Icon type="update" size={19} />
            <div className="dashboard-update-notice-copy">
                <strong>{copy.ready}</strong>
                <span>{update.editorOpen ? copy.editing : copy.localOnly}</span>
            </div>
            <RefreshAction />
            <button type="button" className="dashboard-update-dismiss" onClick={update.dismiss} aria-label={copy.later}><Icon type="cross" size={13} /></button>
        </aside>
    )
}

export function DashboardUpdateSettings () {
    const update = useDashboardUpdates()
    const copy = useCopy()
    const { lang } = useI18n()
    const busy = update.phase !== 'idle' || update.status?.updating === true
    return (
        <section className="settings-section dashboard-update-settings" aria-label={copy.title}>
            <h2 className="settings-section-title">{copy.title}</h2>
            <Card className="settings-card">
                <div className="settings-row">
                    <span className="label">{copy.version}</span>
                    <span className="reading">{dashboardVersion}</span>
                </div>
                {update.status?.current_version && <div className="settings-row">
                    <span className="label">{copy.installed}</span>
                    <span className="reading">{update.status.current_version}</span>
                </div>}
                {update.status?.latest_version && update.status.update_available && <div className="settings-row">
                    <span className="label">{copy.latest}</span>
                    <span className="reading">{update.status.latest_version}</span>
                </div>}
                {update.status?.auto_update !== undefined && <div className="settings-row">
                    <span className="label">{copy.automatic}</span>
                    <span className="reading">{update.status.auto_update ? copy.enabled : copy.disabled}</span>
                </div>}
                {update.status?.update_interval !== undefined && <div className="settings-row">
                    <span className="label">{copy.interval}</span>
                    <span className="reading">{update.status.update_interval} {copy.hours}</span>
                </div>}
                {update.status?.next_check && <div className="settings-row">
                    <span className="label">{copy.nextCheck}</span>
                    <time className="reading" dateTime={update.status.next_check}>{new Date(update.status.next_check).toLocaleString(lang.replace('_', '-'))}</time>
                </div>}
                <div className="dashboard-update-description">
                    <p className="settings-hint">{copy.automaticHint}</p>
                    <p className="settings-hint">{copy.localOnly}</p>
                    <p className="dashboard-update-status" role="status"><StatusText /></p>
                </div>
                <div className="settings-row settings-row-action">
                    <Button icon="update" loading={update.phase === 'checking'} disabled={busy} onClick={() => { void update.check() }}>{copy.check}</Button>
                    {canInstallDashboardUpdate(update.status) && <Button type="primary" disabled={busy} loading={update.phase === 'installing'} onClick={() => { void update.install() }}>{update.status?.reason === 'version-unavailable' ? copy.manualInstall : copy.update}</Button>}
                    {update.ready && <RefreshAction />}
                </div>
            </Card>
        </section>
    )
}
