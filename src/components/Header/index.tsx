import classnames from 'classnames'
import { type ReactNode } from 'react'

import { type BaseComponentProps } from '@models/BaseProps'
import './style.css'

interface HeaderProps extends BaseComponentProps {
    // header title
    title: string

    // page title (h1) or section title (h2)
    level?: 1 | 2

    // short readings shown beside the title
    meta?: ReactNode
}

export function Header (props: HeaderProps) {
    const { title, children, className, style, level = 1, meta } = props
    const Title = level === 1 ? 'h1' : 'h2'

    return <header className={classnames('header', `header-level-${level}`, className)} style={style}>
        <div className="header-heading">
            <Title className="header-title">{title}</Title>
            {meta != null && <div className="header-meta">{meta}</div>}
        </div>
        {children != null && <div className="header-actions">{children}</div>}
    </header>
}
