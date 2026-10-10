import { isAxiosError } from 'axios'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'

import { useI18n } from '@stores'

export type DNSError = 'unsupported' | 'unauthorized' | 'network'

const isPageVisible = () => document.visibilityState !== 'hidden'

interface Resource<T> {
    data?: T
    error?: DNSError
    loading: boolean
    updatedAt?: number
    source: (signal: AbortSignal) => Promise<T>
}

export function useDNSI18n () {
    const { translation, lang } = useI18n()
    const t = useMemo(() => translation('DNS').t, [translation])
    return { t, lang }
}

// Each mounted page owns its data. Requests never enter a shared or persisted cache.
export function useDNSResource<T> (fetcher: (signal: AbortSignal) => Promise<T>, interval = 0) {
    const [revision, setRevision] = useState(0)
    const [state, setState] = useState<Resource<T>>({ loading: true, source: fetcher })
    const refresh = useCallback(() => setRevision(value => value + 1), [])
    const intervalRef = useRef(interval)
    intervalRef.current = interval
    const pollingControl = useRef<(() => void) | undefined>()
    const previousPolling = useRef({ fetcher, interval })

    useEffect(() => {
        let disposed = false
        let unsupported = false
        let attempted = false
        let timer: ReturnType<typeof setTimeout> | undefined
        let request: AbortController | undefined

        function stopRequest () {
            clearTimeout(timer)
            request?.abort()
        }

        async function load () {
            if (disposed || !isPageVisible() || unsupported) return
            stopRequest()
            const controller = new AbortController()
            request = controller
            setState(previous => ({
                ...(previous.source === fetcher ? previous : { source: fetcher }),
                loading: true,
            }))
            try {
                const data = await fetcher(controller.signal)
                if (!disposed && !controller.signal.aborted) {
                    attempted = true
                    setState({ data, source: fetcher, loading: false, updatedAt: Date.now() })
                }
            } catch (error) {
                if (disposed || controller.signal.aborted) return
                const status = isAxiosError(error) ? error.response?.status : undefined
                attempted = true
                const kind: DNSError = status === 404 ? 'unsupported' : status === 401 || status === 403 ? 'unauthorized' : 'network'
                unsupported = kind === 'unsupported'
                setState(previous => ({
                    ...(previous.source === fetcher ? previous : { source: fetcher }),
                    error: kind,
                    loading: false,
                }))
            } finally {
                if (!disposed && !controller.signal.aborted && !unsupported && intervalRef.current > 0 && isPageVisible()) {
                    timer = setTimeout(load, intervalRef.current)
                }
            }
        }

        function visibilityChanged () {
            if (!isPageVisible()) stopRequest()
            else if (intervalRef.current > 0 || !attempted) load()
        }

        pollingControl.current = () => {
            stopRequest()
            setState(previous => ({ ...previous, loading: false }))
            if (intervalRef.current > 0 && isPageVisible()) {
                timer = setTimeout(load, intervalRef.current)
            }
        }

        load()
        document.addEventListener('visibilitychange', visibilityChanged)
        return () => {
            disposed = true
            stopRequest()
            pollingControl.current = undefined
            document.removeEventListener('visibilitychange', visibilityChanged)
        }
    }, [fetcher, revision])

    useEffect(() => {
        const previous = previousPolling.current
        if (previous.fetcher === fetcher && previous.interval !== interval) pollingControl.current?.()
        previousPolling.current = { fetcher, interval }
    }, [fetcher, interval])

    const current: Omit<Resource<T>, 'source'> = state.source === fetcher ? state : { loading: true }
    return { ...current, refresh }
}
