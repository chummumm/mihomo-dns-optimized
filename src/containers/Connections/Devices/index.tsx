import classnames from 'classnames'
import { AnimatePresence, LayoutGroup, motion, useIsPresent } from 'motion/react'
import { forwardRef, useId, useMemo } from 'react'

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

interface SourceButtonProps {
    sourceKey: string
    label: string
    count: number
    selected: boolean
    onChange?: (key: string) => void
}

const SourceButton = forwardRef<HTMLButtonElement, SourceButtonProps>(function SourceButton (props, ref) {
    const isPresent = useIsPresent()
    const selected = isPresent && props.selected

    return (
        <motion.button
            ref={ref}
            type="button"
            data-source-key={props.sourceKey}
            layout="position"
            initial={{ opacity: 0, scale: 0.9 }}
            animate={{ opacity: 1, scale: 1 }}
            exit={{ opacity: 0, scale: 0.9 }}
            transition={springs.reflow}
            aria-hidden={!isPresent || undefined}
            aria-pressed={selected}
            disabled={!isPresent}
            tabIndex={isPresent ? undefined : -1}
            className={classnames('connections-devices-item', { selected })}
            onClick={() => { if (isPresent) props.onChange?.(props.sourceKey) }}>
            {selected && <motion.span className="device-highlight" layoutId="device-highlight" transition={springs.snap} />}
            <span className="device-label">{ props.label }</span> <span className="device-count">({ props.count })</span>
        </motion.button>
    )
})

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
                <AnimatePresence initial={false} mode="popLayout">
                    {props.internalCount > 0 && <SourceButton key={INTERNAL_CONNECTIONS} sourceKey={INTERNAL_CONNECTIONS} label="mihomo" count={props.internalCount} selected={props.selected === INTERNAL_CONNECTIONS} onChange={props.onChange} />}
                    {
                        props.devices.map(
                            device => device.key !== INTERNAL_CONNECTIONS && (
                                <SourceButton key={device.key} sourceKey={device.key} label={label(device)} count={device.number} selected={props.selected === device.key} onChange={props.onChange} />
                            ),
                        )
                    }
                </AnimatePresence>
            </div>
        </LayoutGroup>
    )
}
