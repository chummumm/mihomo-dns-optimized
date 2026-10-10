import classnames from 'classnames'

import { type BaseComponentProps } from '@models'

import { Icon } from '../Icon'
import './style.css'

interface AlertProps extends BaseComponentProps {
    message?: string
    type?: 'success' | 'info' | 'warning' | 'error'
    inside?: boolean
}

const iconMap = {
    success: 'check',
    info: 'info',
    warning: 'info-o',
    error: 'error',
}

export function Alert (props: AlertProps) {
    const { message = '', type = 'info', inside = false, children, className, style } = props
    const classname = classnames('alert', `alert-${type}`, { 'alert-inside': inside }, className)
    return (
        <div className={classname} style={style} role={type === 'error' ? 'alert' : undefined}>
            <Icon className="alert-icon" type={iconMap[type]} size={18} />
            {
                message
                    ? <p className="alert-message">{message}</p>
                    : <div className="alert-message">{children}</div>
            }
        </div>
    )
}
