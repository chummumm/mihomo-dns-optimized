import classnames from 'classnames'
import { forwardRef } from 'react'

import './style.css'

interface CardProps extends React.HTMLAttributes<HTMLDivElement> {
    ref?: React.ForwardedRef<HTMLDivElement>
}

export const Card = forwardRef<HTMLDivElement, CardProps>((props: CardProps, ref) => {
    const { className, style, children, ...attributes } = props
    return (
        <div {...attributes} className={classnames('card', className)} style={style} ref={ref}>
            { children }
        </div>
    )
})
