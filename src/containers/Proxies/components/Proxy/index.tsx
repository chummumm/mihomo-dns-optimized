import type { AxiosError } from 'axios'
import classnames from 'classnames'
import { motion } from 'motion/react'
import { ResultAsync } from 'neverthrow'
import { useMemo, useLayoutEffect, useCallback, useRef, useState } from 'react'

import { delayStatus, springs } from '@components'
import EE, { Action } from '@lib/event'
import { isClashX, jsBridge } from '@lib/jsBridge'
import { supportsProxyDelay } from '@lib/probe'
import { type Proxy as IProxy } from '@lib/request'
import { type BaseComponentProps } from '@models'
import { useClient, useConfig, useProxy } from '@stores'

import { testActivity } from '../../activity'
import './style.css'

interface ProxyProps extends BaseComponentProps {
    config: IProxy
}

export function Proxy (props: ProxyProps) {
    const { config, className } = props
    const { set } = useProxy()
    const { data: settings } = useConfig()
    const client = useClient()
    const activeRequest = useRef<AbortController | null>(null)
    const canSpeedTest = supportsProxyDelay(config)

    useLayoutEffect(() => () => {
        activeRequest.current?.abort()
        activeRequest.current = null
    }, [client])

    const getDelay = useCallback(async (name: string, signal: AbortSignal) => {
        if (isClashX()) {
            const delay = await jsBridge?.getProxyDelay(name) ?? 0
            return delay
        }

        const { data: { delay } } = await client.getProxyDelay(name, settings.probeURL, signal)
        return delay
    }, [client, settings.probeURL])

    const speedTest = useCallback(async function () {
        if (!canSpeedTest || activeRequest.current) return
        const request = new AbortController()
        activeRequest.current = request
        try {
            const result = await ResultAsync.fromPromise(getDelay(config.name, request.signal), e => e as AxiosError)
            if (request.signal.aborted || activeRequest.current !== request) return
            const validDelay = result.isErr() ? 0 : result.value
            set(draft => {
                const proxy = draft.proxies.find(p => p.name === config.name)
                if (proxy != null) {
                    proxy.history.push({ time: Date.now().toString(), delay: validDelay })
                    if (proxy.history.length > 10) proxy.history.splice(0, proxy.history.length - 10)
                }
            })
        } finally {
            if (activeRequest.current === request) activeRequest.current = null
        }
    }, [canSpeedTest, config.name, getDelay, set])

    // UI-only state: shows the running probe without touching the request itself.
    const [testing, setTesting] = useState(false)
    const runTest = useCallback(async function () {
        if (!canSpeedTest || activeRequest.current) return
        setTesting(true)
        testActivity.begin()
        try {
            await speedTest()
        } finally {
            setTesting(false)
            testActivity.end()
        }
    }, [canSpeedTest, speedTest])

    const delay = config.history?.length ? config.history.slice(-1)[0].delay : 0
    const meanDelay = config.history?.length ? config.history.slice(-1)[0].meanDelay : undefined

    const delayText = delay === 0 ? '-' : `${delay}ms`
    const meanDelayText = !meanDelay ? '' : `(${meanDelay}ms)`

    useLayoutEffect(() => {
        const handler = () => { runTest() }
        EE.subscribe(Action.SPEED_NOTIFY, handler)
        return () => EE.unsubscribe(Action.SPEED_NOTIFY, handler)
    }, [runTest])

    const hasError = useMemo(() => delay === 0, [delay])
    const status = useMemo(
        // eslint-disable-next-line @typescript-eslint/prefer-nullish-coalescing
        () => hasError ? 'idle' : delayStatus(meanDelay || delay),
        [delay, meanDelay, hasError],
    )

    const body = <>
        <span className="proxy-item-head">
            <span className="proxy-type">{config.type}</span>
            { config.udp && <span className="proxy-udp">UDP</span> }
        </span>
        <span className="proxy-name" title={config.name}>{config.name}</span>
        <span className="proxy-item-foot">
            <span className={classnames('proxy-status', `status-${status}`)} aria-hidden="true" />
            <span className={classnames('proxy-delay', `status-${status}`)}>
                <span className="proxy-delay-value" key={`${delay}-${meanDelay ?? ''}`}>{delayText}</span>
                {meanDelayText && <span className="proxy-delay-mean">{meanDelayText}</span>}
            </span>
        </span>
        {testing && <span className="proxy-probe" aria-hidden="true" />}
    </>

    const classname = classnames('proxy-item', `is-${status}`, { 'is-error': hasError, 'is-testing': testing }, className)
    return canSpeedTest
        ? <motion.button type="button" className={classname} whileTap={{ scale: 0.97 }} transition={springs.snap} onClick={() => { runTest() }} aria-busy={testing || undefined}>{body}</motion.button>
        : <div className={classname}>{body}</div>
}
