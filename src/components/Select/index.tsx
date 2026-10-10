import classnames from 'classnames'
import { useEffect, useId, useRef, useState, useMemo, useLayoutEffect, type ReactNode, type KeyboardEvent } from 'react'
import { createPortal } from 'react-dom'

import { type BaseComponentProps } from '@models'

import { Icon } from '../Icon'
import './style.css'

export interface SelectOptions<T extends string | number> {
    label: ReactNode
    value: T
    disabled?: boolean
    key?: React.Key
}

interface SelectProps<T extends string | number> extends BaseComponentProps {
    /**
     * selected value
     * must match one of options
     */
    value: T

    options: Array<SelectOptions<T>>

    disabled?: boolean

    ariaLabel?: string

    onSelect?: (value: T, e: React.MouseEvent<HTMLElement> | React.KeyboardEvent<HTMLElement>) => void
}

export function Select<T extends string | number> (props: SelectProps<T>) {
    const { value, options, onSelect, disabled, ariaLabel, className: cn, style } = props

    const portalRef = useRef(document.createElement('div'))
    const targetRef = useRef<HTMLButtonElement>(null)
    const listRef = useRef<HTMLUListElement>(null)
    const listID = useId()

    const [open, setOpen] = useState(false)
    const [active, setActive] = useState(0)
    const [position, setPosition] = useState<React.CSSProperties>({})

    useLayoutEffect(() => {
        const current = portalRef.current
        document.body.appendChild(current)
        return () => {
            document.body.removeChild(current)
        }
    }, [])

    const selectedIndex = useMemo(
        () => options.findIndex(o => o.value === value),
        [value, options],
    )
    const matchChild = options[selectedIndex]

    function place () {
        const rect = targetRef.current!.getBoundingClientRect()
        const spaceBelow = window.innerHeight - rect.bottom
        const above = spaceBelow < 220 && rect.top > spaceBelow
        setPosition({
            left: Math.max(8, Math.min(rect.left, window.innerWidth - Math.max(rect.width, 160) - 8)),
            minWidth: rect.width,
            ...(above ? { bottom: window.innerHeight - rect.top + 6 } : { top: rect.bottom + 6 }),
        })
    }

    function toggle () {
        if (disabled) return
        if (!open) {
            place()
            setActive(Math.max(0, selectedIndex))
        }
        setOpen(!open)
    }

    useEffect(() => {
        if (!open) return
        function onPointer (event: PointerEvent) {
            const target = event.target as Node
            if (!targetRef.current?.contains(target) && !listRef.current?.contains(target)) setOpen(false)
        }
        function onViewportChange () { setOpen(false) }
        function onScroll (event: Event) {
            // Only scrolling an ancestor of the trigger moves its anchor. Log
            // updates and scrolling inside this list must not dismiss it.
            const target = event.target
            if (target === document || (target instanceof Element && target.contains(targetRef.current))) setOpen(false)
        }
        document.addEventListener('pointerdown', onPointer)
        window.addEventListener('resize', onViewportChange)
        window.addEventListener('scroll', onScroll, true)
        return () => {
            document.removeEventListener('pointerdown', onPointer)
            window.removeEventListener('resize', onViewportChange)
            window.removeEventListener('scroll', onScroll, true)
        }
    }, [open])

    function choose (option: SelectOptions<T>, event: React.MouseEvent<HTMLElement> | React.KeyboardEvent<HTMLElement>) {
        if (option.disabled) return
        onSelect?.(option.value, event)
        setOpen(false)
        targetRef.current?.focus()
    }

    function handleKeyDown (event: KeyboardEvent<HTMLButtonElement>) {
        if (disabled) return
        if (!open && ['ArrowDown', 'ArrowUp', 'Enter', ' '].includes(event.key)) {
            event.preventDefault()
            toggle()
            return
        }
        if (!open) return
        if (event.key === 'Escape') {
            event.preventDefault()
            setOpen(false)
        } else if (event.key === 'ArrowDown') {
            event.preventDefault()
            setActive(index => Math.min(options.length - 1, index + 1))
        } else if (event.key === 'ArrowUp') {
            event.preventDefault()
            setActive(index => Math.max(0, index - 1))
        } else if (event.key === 'Enter' || event.key === ' ') {
            event.preventDefault()
            const option = options[active]
            if (option) choose(option, event)
        } else if (event.key === 'Tab') {
            setOpen(false)
        }
    }

    const dropDownList = (
        <ul
            ref={listRef}
            id={listID}
            role="listbox"
            className={classnames('select-list', { 'select-list-show': open })}
            style={position}
        >
            {
                options.map((option, index) => (
                    <li
                        key={option.key ?? option.value}
                        role="option"
                        aria-selected={option.value === value}
                        aria-disabled={option.disabled}
                        className={classnames('option', { selected: option.value === value, active: index === active, disabled: option.disabled })}
                        onMouseEnter={() => setActive(index)}
                        onClick={e => choose(option, e)}
                    >
                        <span className="option-label">{option.label}</span>
                        {option.value === value && <Icon type="check" size={14} className="option-check" />}
                    </li>
                ))
            }
        </ul>
    )

    return (
        <>
            <button
                type="button"
                className={classnames('select', { disabled, open }, cn)}
                style={style}
                ref={targetRef}
                disabled={disabled}
                aria-haspopup="listbox"
                aria-expanded={open}
                aria-controls={listID}
                aria-label={ariaLabel}
                onClick={toggle}
                onKeyDown={handleKeyDown}
            >
                <span className="select-value">{matchChild?.label}</span>
                <Icon type="triangle-down" size={14} className="select-caret" />
            </button>
            {createPortal(dropDownList, portalRef.current)}
        </>
    )
}
