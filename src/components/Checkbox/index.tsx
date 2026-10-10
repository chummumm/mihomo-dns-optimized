import classnames from 'classnames'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models/BaseProps'

import { Icon } from '../Icon'
import './style.css'

interface CheckboxProps extends BaseComponentProps {
    checked: boolean
    onChange?: (checked: boolean) => void
}

export function Checkbox (props: CheckboxProps) {
    const { className, style, checked = false, onChange = noop } = props
    const classname = classnames('checkbox', { checked }, className)

    function handleChange () {
        onChange(!checked)
    }

    return (
        <label className={classname} style={style}>
            <input className="checkbox-input" type="checkbox" checked={checked} onChange={handleChange} />
            <span className="checkbox-box" aria-hidden="true">
                <Icon className="checkbox-icon" type="check" size={12} />
            </span>
            <span className="checkbox-label">{ props.children }</span>
        </label>
    )
}
