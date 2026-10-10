import classnames from 'classnames'
import { useMemo } from 'react'

import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { ALL_CONNECTIONS, INTERNAL_CONNECTIONS, UNKNOWN_CONNECTIONS } from '../source'
import type { ConnectionDevice } from '../store'
import './style.scss'

interface DevicesProps extends BaseComponentProps {
    devices: ConnectionDevice[]
    selected: string
    selectedMissing: boolean
    total: number
    onChange?: (label: string) => void
}

export function Devices (props: DevicesProps) {
    const { translation } = useI18n()
    const t = useMemo(() => translation('Connections').t, [translation])

    const { className, style } = props
    const classname = classnames('connections-source-filters flex flex-wrap px-1', className)
    const devices = props.selectedMissing
        ? [...props.devices, { key: props.selected, label: props.selected.slice(7), number: 0 }]
        : props.devices
    function label (device: ConnectionDevice) {
        if (device.key === INTERNAL_CONNECTIONS) return 'mihomo'
        if (device.key === UNKNOWN_CONNECTIONS) return t('filter.unknown')
        return device.label
    }

    return (
        <div className={classname} style={style} role="group" aria-label={t('filter.sources')}>
            <button type="button" aria-pressed={props.selected === ALL_CONNECTIONS} className={classnames('connections-devices-item mb-2 pt-2', { selected: props.selected === ALL_CONNECTIONS })} onClick={() => props.onChange?.(ALL_CONNECTIONS)}>
                { t('filter.all') } ({ props.total })
            </button>
            {
                devices.map(
                    device => (
                        <button
                            type="button"
                            key={device.key}
                            aria-pressed={props.selected === device.key}
                            className={classnames('connections-devices-item mb-2 pt-2', { selected: props.selected === device.key })}
                            onClick={() => props.onChange?.(device.key)}>
                            { label(device) } ({ device.number })
                        </button>
                    ),
                )
            }
        </div>
    )
}
