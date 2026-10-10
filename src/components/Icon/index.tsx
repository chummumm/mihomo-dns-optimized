import classnames from 'classnames'
import { useRef } from 'react'

import { type BaseComponentProps } from '@models/BaseProps'

import { icons, strokeIcons, type IconType } from './icons'
import './style.css'

export { type IconType } from './icons'
export { playIconGesture } from './gestures'

interface IconProps extends BaseComponentProps {
    // icon type
    type: IconType | string

    // icon size
    size?: number

    // accessible label; the icon is decorative when omitted
    label?: string

    // when this value changes the glyph replays its entrance, e.g. a reading that moved
    replay?: string | number

    onClick?: React.MouseEventHandler<HTMLElement>
}

export function Icon (props: IconProps) {
    const { type, size = 16, className, style, label, replay, onClick } = props
    const stroke = (strokeIcons as Record<string, string>)[type]
    const glyph = stroke ?? (icons as Record<string, string>)[type] ?? ''

    // Count changes of glyph or replay value; each one remounts the svg so its CSS entrance runs again.
    // The first render never animates, so a page full of icons arrives still.
    const signature = `${type}\u0000${replay ?? ''}`
    const previous = useRef(signature)
    const changes = useRef(0)
    if (previous.current !== signature) {
        previous.current = signature
        changes.current += 1
    }

    return (
        <i
            className={classnames('icon', `icon-${type}`, className)}
            style={{ width: size, height: size, ...style }}
            data-icon={type}
            role={label ? 'img' : undefined}
            aria-label={label}
            aria-hidden={label ? undefined : true}
            onClick={onClick}
        >
            <svg
                key={changes.current}
                className={classnames({ 'icon-replay': changes.current > 0, 'icon-stroke': stroke != null })}
                viewBox="0 0 256 256"
                width={size}
                height={size}
                fill={stroke != null ? 'none' : 'currentColor'}
                focusable="false"
                dangerouslySetInnerHTML={{ __html: glyph }} />
        </i>
    )
}
