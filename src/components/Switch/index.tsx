import classnames from 'classnames'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models/BaseProps'
import './style.css'

interface SwitchProps extends BaseComponentProps {
    checked: boolean
    disabled?: boolean
    label?: string
    onChange?: (checked: boolean) => void
}

export function Switch (props: SwitchProps) {
    const { className, checked = false, disabled = false, label, onChange = noop } = props
    const classname = classnames('switch', { checked, disabled }, className)

    function handleClick () {
        if (!disabled) {
            onChange(!checked)
        }
    }

    return (
        <button
            type="button"
            role="switch"
            aria-checked={checked}
            aria-label={label}
            disabled={disabled}
            className={classname}
            onClick={handleClick}
        >
            <span className="switch-thumb" aria-hidden="true" />
        </button>
    )
}
