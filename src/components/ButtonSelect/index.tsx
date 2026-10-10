import classnames from 'classnames'
import { motion } from 'motion/react'
import { useId } from 'react'

import { type BaseComponentProps } from '@models/BaseProps'

import { springs } from '../Motion'
import './style.css'

export interface ButtonSelectOptions<T = string> {
    label: string
    value: T
}

export interface ButtonSelectProps<T = string> extends BaseComponentProps {
    // options
    options: Array<ButtonSelectOptions<T>>

    // active value
    value: T

    // accessible name of the group
    label?: string

    // select callback
    onSelect?: (value: T) => void
}

export function ButtonSelect (props: ButtonSelectProps) {
    const { options, value, onSelect, label, className } = props
    const indicator = useId()

    return (
        <div className={classnames('button-select', className)} role="group" aria-label={label}>
            {
                options.map(option => (
                    <button
                        type="button"
                        value={option.value}
                        key={option.value}
                        aria-pressed={value === option.value}
                        className={classnames('button-select-options', { actived: value === option.value })}
                        onClick={() => onSelect?.(option.value)}>
                        {value === option.value && <motion.span className="button-select-indicator" layoutId={indicator} transition={springs.snap} aria-hidden="true" />}
                        <span className="button-select-label">{ option.label }</span>
                    </button>
                ))
            }
        </div>
    )
}
