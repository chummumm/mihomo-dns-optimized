import classnames from 'classnames'

import { type BaseComponentProps } from '@models/BaseProps'

import './style.css'

interface TagProps extends BaseComponentProps {
    color?: string
    tone?: 'neutral' | 'ink' | 'good' | 'warn' | 'bad'
    title?: string
}

export function Tag (props: TagProps) {
    const { color, tone = 'neutral', title, className: cn, style: s, children } = props
    const className = classnames('tag', `tag-${tone}`, cn)
    const style: React.CSSProperties = { color, ...s }

    return <span className={className} style={style} title={title}>{ children }</span>
}
