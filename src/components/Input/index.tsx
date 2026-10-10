import classnames from 'classnames'
import { type KeyboardEvent, type FocusEvent, type ChangeEvent } from 'react'

import { noop } from '@lib/helper'
import { type BaseComponentProps } from '@models/BaseProps'
import './style.css'

interface InputProps extends BaseComponentProps {
    value?: string | number
    align?: 'left' | 'center' | 'right'
    inside?: boolean
    autoFocus?: boolean
    type?: string
    disabled?: boolean
    id?: string
    mono?: boolean
    inputMode?: 'text' | 'numeric' | 'url'
    ariaLabel?: string
    onChange?: (value: string, event?: ChangeEvent<HTMLInputElement>) => void
    onEnter?: (event?: KeyboardEvent<HTMLInputElement>) => void
    onBlur?: (event?: FocusEvent<HTMLInputElement>) => void
}

export function Input (props: InputProps) {
    const {
        className,
        style,
        value = '',
        align = 'left',
        autoFocus = false,
        type = 'text',
        disabled = false,
        id,
        mono = false,
        inputMode,
        ariaLabel,
        onChange = noop,
        onBlur = noop,
        onEnter = noop,
    } = props
    const classname = classnames('input', `input-align-${align}`, { mono }, className)

    function handleKeyDown (e: KeyboardEvent<HTMLInputElement>) {
        if (e.code === 'Enter') {
            onEnter(e)
        }
    }

    return (
        <input
            id={id}
            disabled={disabled}
            className={classname}
            style={style}
            value={value}
            autoFocus={autoFocus}
            type={type}
            inputMode={inputMode}
            aria-label={ariaLabel}
            spellCheck={false}
            onChange={event => onChange(event.target.value, event)}
            onBlur={onBlur}
            onKeyDown={handleKeyDown}
        />
    )
}
