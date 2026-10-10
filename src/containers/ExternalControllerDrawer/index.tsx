import { useAtom } from 'jotai'
import { useEffect } from 'react'

import { Modal, Input, Alert, Button, error } from '@components'
import { useObject } from '@lib/hook'
import { useI18n, useAPIInfo, identityAtom } from '@stores'
import { hostSelectIdxStorageAtom, hostsStorageAtom } from '@stores/request'
import './style.css'

export default function ExternalController () {
    const { translation } = useI18n()
    const { t } = translation('Settings')
    const { hostname, port, secret } = useAPIInfo()
    const [identity, setIdentity] = useAtom(identityAtom)
    const [value, set] = useObject({
        hostname: '',
        port: '',
        secret: '',
    })

    useEffect(() => {
        set({ hostname, port, secret })
    }, [hostname, port, secret, set])

    const [hosts, setter] = useAtom(hostsStorageAtom)
    const [hostSelectIdx, setHostSelectIdx] = useAtom(hostSelectIdxStorageAtom)

    function handleOk () {
        const { hostname, port, secret } = value
        setter([{ hostname, port, secret }])
    }

    function handleAdd () {
        const { hostname, port, secret } = value
        const nextHosts = [...hosts, { hostname, port, secret }]
        setter(nextHosts)
        setHostSelectIdx(nextHosts.length - 1)
    }

    function handleDelete () {
        const { hostname, port } = value
        const idx = hosts.findIndex(h => h.hostname === hostname && h.port === port)
        if (idx === -1) {
            error(t('externalControllerSetting.deleteErrorText'))
            return
        }

        const nextHosts = [...hosts.slice(0, idx), ...hosts.slice(idx + 1)]
        setter(nextHosts)
        if (hostSelectIdx >= idx) {
            setHostSelectIdx(0)
        }
    }

    const footerExtra = (
        <div className="external-controller-extra">
            <Button icon="plus" onClick={() => handleAdd()}>{ t('externalControllerSetting.addText') }</Button>
            <Button type="ghost" icon="trash" disabled={hosts.length < 2} onClick={() => handleDelete()}>{ t('externalControllerSetting.deleteText') }</Button>
        </div>
    )

    return (
        <Modal
            className="external-controller-modal"
            show={!identity}
            title={t('externalControllerSetting.title')}
            bodyClassName="external-controller"
            footerExtra={footerExtra}
            onClose={() => setIdentity(true)}
            onOk={handleOk}
        >
            <Alert type="info" inside={true}>
                <p>{t('externalControllerSetting.note')}</p>
            </Alert>
            <div className="external-controller-fields">
                <label className="external-controller-field external-controller-host" htmlFor="controller-host">
                    <span>{t('externalControllerSetting.host')}</span>
                    <Input
                        id="controller-host"
                        mono
                        value={value.hostname}
                        onChange={hostname => set('hostname', hostname)}
                        onEnter={handleOk}
                    />
                </label>
                <label className="external-controller-field" htmlFor="controller-port">
                    <span>{t('externalControllerSetting.port')}</span>
                    <Input
                        id="controller-port"
                        mono
                        inputMode="numeric"
                        value={value.port}
                        onChange={port => set('port', port)}
                        onEnter={handleOk}
                    />
                </label>
                <label className="external-controller-field external-controller-secret" htmlFor="controller-secret">
                    <span>{t('externalControllerSetting.secret')}</span>
                    <Input
                        id="controller-secret"
                        mono
                        value={value.secret}
                        onChange={secret => set('secret', secret)}
                        onEnter={handleOk}
                    />
                </label>
            </div>
        </Modal>
    )
}
