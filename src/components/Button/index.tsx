import classnames from 'classnames'
import { type MouseEventHandler } from 'react'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models'

import { Icon, playIconGesture } from '../Icon'
import './style.css'

// Icons that can show their own work while the control is busy; others give way to the spinner.
const busyIcons = new Set(['update', 'healthcheck', 'speed'])

interface ButtonProps extends BaseComponentProps {
    type?: 'primary' | 'normal' | 'danger' | 'success' | 'warning' | 'ghost'
    size?: 'sm' | 'md'
    htmlType?: 'button' | 'submit' | 'reset'
    icon?: string
    loading?: boolean
    title?: string
    ariaLabel?: string
    pressed?: boolean
    onClick?: MouseEventHandler<HTMLButtonElement>
    disabled?: boolean
}

export function Button (props: ButtonProps) {
    const {
        type = 'normal',
        size = 'md',
        htmlType = 'button',
        icon,
        loading = false,
        title,
        ariaLabel,
        pressed,
        onClick = noop,
        children,
        className,
        style,
        disabled,
    } = props
    const busyIcon = loading && icon != null && busyIcons.has(icon)
    const classname = classnames('button', `button-${type}`, `button-${size}`, className, {
        'button-disabled': disabled,
        'button-loading': loading,
        'button-icon-only': (icon != null || loading) && children == null,
    })

    function handleClick (event: React.MouseEvent<HTMLButtonElement>) {
        playIconGesture(event.currentTarget)
        onClick(event)
    }

    return (
        <button
            type={htmlType}
            className={classname}
            style={style}
            onClick={handleClick}
            disabled={disabled}
            title={title}
            aria-label={ariaLabel}
            aria-pressed={pressed}
            aria-busy={loading || undefined}
        >
            {loading && !busyIcon
                ? <span className="button-spinner" aria-hidden="true" />
                : icon && <Icon type={icon} size={size === 'sm' ? 14 : 16} className={classnames({ 'is-busy': busyIcon })} />}
            {children != null && <span className="button-label">{children}</span>}
        </button>
    )
}
