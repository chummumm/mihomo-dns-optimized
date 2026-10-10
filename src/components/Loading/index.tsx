import classnames from 'classnames'

import { type BaseComponentProps } from '@models/BaseProps'

import './style.css'

interface LoadingProps extends BaseComponentProps {
    visible: boolean
    spinnerClassName?: string
}

// A train running along the top edge: work is in progress, content stays readable.
export function Loading (props: LoadingProps) {
    const classname = classnames('loading', 'visible', props.className)
    return props.visible
        ? (
            <div className={classname} role="progressbar" aria-busy="true">
                <div className={classnames('loading-track', props.spinnerClassName)}>
                    <span className="loading-train" />
                </div>
            </div>
        )
        : null
}
