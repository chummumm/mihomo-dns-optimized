import axios from 'axios'
import classnames from 'classnames'
import { useAtom, useAtomValue, useSetAtom } from 'jotai'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useLocation } from 'react-router-dom'

import { Header, Card, Switch, Button, ButtonSelect, type ButtonSelectOptions, Input, Select, success, error } from '@components'
import { type Lang } from '@i18n'
import { useObject } from '@lib/hook'
import { jsBridge } from '@lib/jsBridge'
import { DEFAULT_PROBE_URL, normalizeProbeURL } from '@lib/probe'
import { useI18n, useClashXData, useGeneral, useVersion, useClient, useConfig, identityAtom, hostSelectIdxStorageAtom, hostsStorageAtom, useAPIInfo } from '@stores'
import './style.scss'

const languageOptions: ButtonSelectOptions[] = [{ label: '中文', value: 'zh_CN' }, { label: 'English', value: 'en_US' }]

export default function Settings () {
    const { premium } = useVersion()
    const { data: clashXData, update: fetchClashXData } = useClashXData()
    const { general, update: fetchGeneral } = useGeneral()
    const setIdentity = useSetAtom(identityAtom)
    const [hostSelectIdx, setHostSelectIdx] = useAtom(hostSelectIdxStorageAtom)
    const hostsStorage = useAtomValue(hostsStorageAtom)
    const apiInfo = useAPIInfo()
    const location = useLocation()
    const controllerFromURL = useMemo(() => {
        const query = new URLSearchParams(location.search)
        return ['host', 'port', 'secret', 'protocol'].some(key => query.has(key))
    }, [location.search])
    const { translation, setLang, lang } = useI18n()
    const { t } = translation('Settings')
    const client = useClient()
    const { data: settings, set: setSettings } = useConfig()
    const savedProbeURL = normalizeProbeURL(settings.probeURL) ?? DEFAULT_PROBE_URL
    const [probeURL, setProbeURL] = useState(savedProbeURL)
    const [reloading, setReloading] = useState(false)
    const reloadRequest = useRef<AbortController | null>(null)
    const [info, set] = useObject({
        socks5ProxyPort: 7891,
        httpProxyPort: 7890,
        mixedProxyPort: 0,
    })

    useEffect(() => {
        set('socks5ProxyPort', general?.socksPort ?? 0)
        set('httpProxyPort', general?.port ?? 0)
        set('mixedProxyPort', general?.mixedPort ?? 0)
    }, [general, set])

    useEffect(() => { setProbeURL(savedProbeURL) }, [savedProbeURL])

    useEffect(() => {
        setReloading(false)
        return () => {
            reloadRequest.current?.abort()
            reloadRequest.current = null
        }
    }, [client])

    function saveProbeURL () {
        const normalized = normalizeProbeURL(probeURL)
        if (normalized === undefined) {
            error(t('messages.invalidProbeURL'))
            return
        }
        setSettings('probeURL', normalized)
        setProbeURL(normalized)
        success(t('messages.probeURLSaved'))
    }

    function resetProbeURL () {
        setSettings('probeURL', DEFAULT_PROBE_URL)
        setProbeURL(DEFAULT_PROBE_URL)
    }

    async function handleReloadConfig () {
        if (reloadRequest.current) return
        const request = new AbortController()
        reloadRequest.current = request
        setReloading(true)
        try {
            await client.reloadConfig(request.signal)
            if (request.signal.aborted) return
            success(t('messages.reloadOk'))
            // Reload already succeeded; a failed read must not report that the
            // configuration was rejected. SWR keeps its previous snapshot.
            await fetchGeneral().catch(() => undefined)
        } catch (reason) {
            if (request.signal.aborted) return
            const detail = axios.isAxiosError<{ message?: string }>(reason)
                ? reason.response?.data?.message ?? reason.message
                : reason instanceof Error ? reason.message : ''
            error(`${t('messages.reloadErr')}${detail ? ': ' + String(detail).slice(0, 300) : ''}`)
        } finally {
            if (reloadRequest.current === request) {
                reloadRequest.current = null
                setReloading(false)
            }
        }
    }

    async function handleProxyModeChange (mode: string) {
        await client.updateConfig({ mode })
        await fetchGeneral()
    }

    async function handleStartAtLoginChange (state: boolean) {
        await jsBridge?.setStartAtLogin(state)
        await fetchClashXData()
    }

    async function handleSetSystemProxy (state: boolean) {
        await jsBridge?.setSystemProxy(state)
        await fetchClashXData()
    }

    function changeLanguage (language: Lang) {
        setLang(language)
    }

    async function handleHttpPortSave () {
        await client.updateConfig({ port: info.httpProxyPort })
        await fetchGeneral()
    }

    async function handleSocksPortSave () {
        await client.updateConfig({ 'socks-port': info.socks5ProxyPort })
        await fetchGeneral()
    }

    async function handleMixedPortSave () {
        await client.updateConfig({ 'mixed-port': info.mixedProxyPort })
        await fetchGeneral()
    }

    async function handleAllowLanChange (state: boolean) {
        await client.updateConfig({ 'allow-lan': state })
        await fetchGeneral()
    }

    const {
        hostname: externalControllerHost,
        port: externalControllerPort,
    } = apiInfo

    const { allowLan, mode } = general

    const startAtLogin = clashXData?.startAtLogin ?? false
    const systemProxy = clashXData?.systemProxy ?? false
    const isClashX = clashXData?.isClashX ?? false

    const proxyModeOptions = useMemo(() => {
        const options = [
            { label: t('values.global'), value: 'global' },
            { label: t('values.rules'), value: 'rule' },
            { label: t('values.direct'), value: 'direct' },
        ] as Array<{ label: string, value: string }>
        if (premium) {
            options.push({ label: t('values.script'), value: 'script' })
        }
        return options
    }, [t, premium])

    const controllerOptions = hostsStorage.map(
        (h, idx) => ({ value: idx, label: <span className="truncate text-right">{h.hostname}:{h.port}</span> }),
    )

    const controllers = isClashX || controllerFromURL
        ? <div className="min-w-0 text-right text-sm text-primary-darken">
            <span className="controller-endpoint break-all">{`${apiInfo.protocol}//${externalControllerHost}:${externalControllerPort}`}</span>
            {!isClashX && controllerFromURL && <p className="mt-1 text-xs">{t('controllerFromURL')}</p>}
        </div>
        : (
            <>
                <Select
                    disabled={hostsStorage.length < 2 && !isClashX}
                    options={controllerOptions}
                    value={hostSelectIdx}
                    onSelect={idx => setHostSelectIdx(idx)}
                />
                <span
                    className={classnames({ 'modify-btn': !isClashX }, 'external-controller')}
                    onClick={() => !isClashX && setIdentity(false)}>
                    {t('labels.edit')}
                </span>
            </>
        )

    return (
        <div className="page">
            <Header title={t('title')} />
            <Card className="settings-card">
                <div className="flex flex-wrap">
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.startAtLogin')}</span>
                        <Switch disabled={!clashXData?.isClashX} checked={startAtLogin} onChange={handleStartAtLoginChange} />
                    </div>
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.language')}</span>
                        <ButtonSelect options={languageOptions} value={lang} onSelect={(lang) => changeLanguage(lang as Lang)} />
                    </div>
                </div>
                <div className="flex flex-wrap">
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.setAsSystemProxy')}</span>
                        <Switch
                            disabled={!isClashX}
                            checked={systemProxy}
                            onChange={handleSetSystemProxy}
                        />
                    </div>
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.allowConnectFromLan')}</span>
                        <Switch checked={allowLan} onChange={handleAllowLanChange} />
                    </div>
                </div>
            </Card>

            <Card className="settings-card">
                <div className="flex flex-wrap">
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.proxyMode')}</span>
                        <ButtonSelect
                            options={proxyModeOptions}
                            value={mode}
                            onSelect={handleProxyModeChange}
                        />
                    </div>
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.socks5ProxyPort')}</span>
                        <Input
                            className="w-28"
                            disabled={isClashX}
                            value={info.socks5ProxyPort}
                            onChange={socks5ProxyPort => set('socks5ProxyPort', +socks5ProxyPort)}
                            onBlur={handleSocksPortSave}
                        />
                    </div>
                </div>
                <div className="flex flex-wrap">
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.httpProxyPort')}</span>
                        <Input
                            className="w-28"
                            disabled={isClashX}
                            value={info.httpProxyPort}
                            onChange={httpProxyPort => set('httpProxyPort', +httpProxyPort)}
                            onBlur={handleHttpPortSave}
                        />
                    </div>
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.mixedProxyPort')}</span>
                        <Input
                            className="w-28"
                            disabled={isClashX}
                            value={info.mixedProxyPort}
                            onChange={mixedProxyPort => set('mixedProxyPort', +mixedProxyPort)}
                            onBlur={handleMixedPortSave}
                        />
                    </div>
                </div>
                <div className="flex flex-wrap">
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <span className="label font-bold">{t('labels.externalController')}</span>
                        <div className="flex items-center space-x-2">
                            { controllers }
                        </div>
                    </div>
                    <div className="w-full flex items-center justify-between px-8 py-3 md:w-1/2">
                        <Button disabled={reloading} onClick={handleReloadConfig}>{t(reloading ? 'labels.reloading' : 'labels.reloadConfig')}</Button>
                    </div>
                </div>
            </Card>
            <Card className="settings-card">
                <form className="settings-probe-url" onSubmit={event => { event.preventDefault(); saveProbeURL() }}>
                    <label className="label font-bold" htmlFor="probe-url">{t('labels.probeURL')}</label>
                    <input
                        id="probe-url"
                        className="input text-left"
                        value={probeURL}
                        onChange={event => setProbeURL(event.target.value)}
                        type="url"
                        inputMode="url"
                        spellCheck={false}
                        autoCapitalize="off"
                        maxLength={2048}
                        disabled={isClashX}
                        aria-describedby="probe-url-hint" />
                    <div className="flex gap-3">
                        <button className="button button-primary" type="submit" disabled={isClashX}>{t('labels.save')}</button>
                        <button className="button button-normal" type="button" disabled={isClashX} onClick={resetProbeURL}>{t('labels.resetProbeURL')}</button>
                    </div>
                    <p id="probe-url-hint" className="text-primary-dark text-xs">{t('probeURLHint')}</p>
                </form>
            </Card>
            {/* <Card className="clash-version hidden">
                <span className="check-icon">
                    <Icon type="check" size={20} />
                </span>
                <p className="version-info">{t('versionString')}</p>
                <span className="check-update-btn">{t('checkUpdate')}</span>
            </Card> */}
        </div>
    )
}
