import classnames from 'classnames'
import { LayoutGroup, motion } from 'motion/react'
import { useId, useMemo } from 'react'

import { springs } from '@components'
import { type BaseComponentProps } from '@models'
import { useI18n } from '@stores'

import { ALL_CONNECTIONS, INTERNAL_CONNECTIONS, UNKNOWN_CONNECTIONS } from '../source'
import type { ConnectionDevice } from '../store'
import './style.css'

interface DevicesProps extends BaseComponentProps {
    devices: ConnectionDevice[]
    selected: string
    total: number
    internalCount: number
    onChange?: (label: string) => void
}

export function Devices (props: DevicesProps) {
    const { translation } = useI18n()
    const t = useMemo(() => translation('Connections').t, [translation])
    const highlight = useId()

    const { className, style } = props
    const classname = classnames('connections-source-filters', className)
    function label (device: ConnectionDevice) {
        if (device.key === UNKNOWN_CONNECTIONS) return t('filter.unknown')
        return device.label
    }

    return (
        <LayoutGroup id={highlight}>
            <div className={classname} style={style} role="group" aria-label={t('filter.sources')}>
                <button type="button" aria-pressed={props.selected === ALL_CONNECTIONS} className={classnames('connections-devices-item', { selected: props.selected === ALL_CONNECTIONS })} onClick={() => props.onChange?.(ALL_CONNECTIONS)}>
                    {props.selected === ALL_CONNECTIONS && <motion.span className="device-highlight" layoutId="device-highlight" transition={springs.snap} />}
                    <span className="device-label">{ t('filter.all') }</span> <span className="device-count">({ props.total })</span>
                </button>
                {props.internalCount > 0 && <button type="button" aria-pressed={props.selected === INTERNAL_CONNECTIONS} className={classnames('connections-devices-item', { selected: props.selected === INTERNAL_CONNECTIONS })} onClick={() => props.onChange?.(INTERNAL_CONNECTIONS)}>
                    {props.selected === INTERNAL_CONNECTIONS && <motion.span className="device-highlight" layoutId="device-highlight" transition={springs.snap} />}
                    <span className="device-label">mihomo</span> <span className="device-count">({ props.internalCount })</span>
                </button>}
                {
                    props.devices.map(
                        device => device.key !== INTERNAL_CONNECTIONS && (
                            <motion.button
                                type="button"
                                key={device.key}
                                layout="position"
                                initial={{ opacity: 0, scale: 0.9 }}
                                animate={{ opacity: 1, scale: 1 }}
                                transition={springs.reflow}
                                aria-pressed={props.selected === device.key}
                                className={classnames('connections-devices-item', { selected: props.selected === device.key })}
                                onClick={() => props.onChange?.(device.key)}>
                                {props.selected === device.key && <motion.span className="device-highlight" layoutId="device-highlight" transition={springs.snap} />}
                                <span className="device-label">{ label(device) }</span> <span className="device-count">({ device.number })</span>
                            </motion.button>
                        ),
                    )
                }
            </div>
        </LayoutGroup>
    )
}
