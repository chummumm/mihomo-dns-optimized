import axios from 'axios'
import { useEffect, useRef, useState } from 'react'

import type { Client } from '@lib/request'

import { readUpdateSnapshot, reconcileUpdateRestart, updateTaskActive, type RestartResult, type UpdateSnapshot } from './update-state'

export type UpdateProblem = 'network' | 'unauthorized' | 'unsupported' | 'invalid' | 'busy' | 'configuration_unavailable' | 'interrupted'

interface View {
    snapshot?: UpdateSnapshot
    previous?: UpdateSnapshot
    restart?: RestartResult
    problem?: UpdateProblem
    loading: boolean
    starting: boolean
    disconnected: boolean
    unconfirmedStart?: boolean
    startRejected?: boolean
    scheduleRequestedAt?: number
}

interface Session {
    client: Client
    abort: AbortController
    timer?: ReturnType<typeof setTimeout>
    busy: boolean
    failedSince?: number
    awaitingAcceptance?: { instance: string, request: string }
    view: View
}

function problemFor (reason: unknown): UpdateProblem {
    if (axios.isAxiosError(reason)) {
        const status = reason.response?.status
        if (status === 401 || status === 403) return 'unauthorized'
        if (status === 404 || status === 405) return 'unsupported'
        if (status === 409) return 'busy'
        if (status === 503 && reason.response?.data?.message === 'configuration_unavailable') return 'configuration_unavailable'
    }
    return reason instanceof SyntaxError ? 'invalid' : 'network'
}

const INITIAL: View = { loading: true, starting: false, disconnected: false }

export function useUpdateTask (client: Client) {
    const [state, setState] = useState(() => ({ client, view: INITIAL }))
    // A controller change must hide the old result during render, before the
    // effect gets a chance to cancel its request and clear the session.
    const view = state.client === client ? state.view : INITIAL
    const session = useRef<Session>()

    function publish (current: Session, values: Partial<View>) {
        if (current.abort.signal.aborted || session.current !== current) return
        current.view = { ...current.view, ...values }
        setState({ client: current.client, view: current.view })
    }

    function schedule (current: Session, delay = 1500) {
        clearTimeout(current.timer)
        if (current.abort.signal.aborted || document.hidden) return
        current.timer = setTimeout(() => { void read(current) }, delay)
    }

    function accept (current: Session, value: unknown, requestedAt: number, acceptedPost = false) {
        const snapshot = readUpdateSnapshot(value)
        if (snapshot === undefined) throw new SyntaxError('Invalid update task')
        const attempt = current.awaitingAcceptance
        const recovered = attempt !== undefined && snapshot.request_id === attempt.request && snapshot.instance_id === attempt.instance
        const unconfirmedStart = !acceptedPost && attempt !== undefined && !recovered
        const restart = unconfirmedStart ? undefined : reconcileUpdateRestart(current.view.snapshot, snapshot)
        current.awaitingAcceptance = undefined
        current.failedSince = undefined
        publish(current, {
            snapshot,
            previous: unconfirmedStart ? undefined : restart ? current.view.snapshot : current.view.previous,
            restart: unconfirmedStart ? undefined : restart ?? current.view.restart,
            unconfirmedStart: unconfirmedStart || current.view.unconfirmedStart,
            problem: undefined,
            loading: false,
            starting: false,
            disconnected: false,
            scheduleRequestedAt: requestedAt,
        })
        if (updateTaskActive(snapshot)) schedule(current)
        else if (snapshot.items.some(item => item.auto_update)) schedule(current, 30000)
    }

    async function read (current: Session) {
        if (current.busy || current.abort.signal.aborted) return
        current.busy = true
        const requestedAt = performance.now()
        try {
            accept(current, (await current.client.getUpdateTask(current.abort.signal)).data, requestedAt)
        } catch (reason) {
            if (current.abort.signal.aborted) return
            const problem = problemFor(reason)
            const following = updateTaskActive(current.view.snapshot) || current.view.starting || current.view.disconnected
            const retry = following && problem === 'network'
            current.failedSince ??= Date.now()
            const expired = Date.now() - current.failedSince >= 120000
            publish(current, {
                loading: false,
                starting: false,
                disconnected: retry && !expired,
                problem: retry && expired ? 'interrupted' : problem,
            })
            if (retry && !expired) schedule(current, 2000)
            else if (!following && problem === 'network' && current.view.snapshot?.items.some(item => item.auto_update)) schedule(current, 30000)
        } finally {
            current.busy = false
        }
    }

    useEffect(() => {
        const current: Session = { client, abort: new AbortController(), busy: false, view: INITIAL }
        session.current = current
        setState({ client, view: INITIAL })
        void read(current)
        const visible = () => {
            if (document.hidden) clearTimeout(current.timer)
            else void read(current)
        }
        document.addEventListener('visibilitychange', visible)
        return () => {
            current.abort.abort()
            clearTimeout(current.timer)
            document.removeEventListener('visibilitychange', visible)
            if (session.current === current) session.current = undefined
        }
        // Each controller gets a separate request lifetime and task history.
        // eslint-disable-next-line react-hooks/exhaustive-deps
    }, [client])

    async function start () {
        const current = session.current
        if (current === undefined || current.client !== client || current.busy || current.abort.signal.aborted) return
        const snapshot = current.view.snapshot
        if (snapshot === undefined || updateTaskActive(snapshot) || current.view.problem !== undefined) {
            // Reconnect to an uncertain task before allowing another POST.
            // A retry can never silently repeat an installation.
            current.failedSince = undefined
            publish(current, { loading: true })
            await read(current)
            return
        }
        clearTimeout(current.timer)
        current.busy = true
        // Chromium can replay an empty POST after a transport failure. Reuse
        // this action's identity at the HTTP layer, and reject it after a core
        // restart instead of starting another installation in the new process.
        const requestId = Array.from(crypto.getRandomValues(new Uint8Array(16)), value => value.toString(16).padStart(2, '0')).join('')
        current.awaitingAcceptance = { instance: snapshot.instance_id, request: requestId }
        publish(current, { starting: true, previous: undefined, restart: undefined, problem: undefined, unconfirmedStart: false, startRejected: false })
        const requestedAt = performance.now()
        try {
            accept(current, (await current.client.startUpdateTask(current.abort.signal, snapshot.instance_id, requestId)).data, requestedAt, true)
        } catch (reason) {
            if (current.abort.signal.aborted) return
            const problem = problemFor(reason)
            const conflict = axios.isAxiosError(reason) && reason.response?.status === 409
            if (problem !== 'network' && problem !== 'invalid' && !conflict) current.awaitingAcceptance = undefined
            publish(current, {
                starting: false,
                problem,
                disconnected: problem === 'network',
                startRejected: conflict && reason.response?.data?.message === 'busy',
            })
            // POST may have succeeded before its response was lost. Observe
            // the accepted job with GET; never retry the POST automatically.
            if (problem === 'network' || problem === 'busy') schedule(current, 300)
        } finally {
            current.busy = false
        }
    }

    return { ...view, start }
}
