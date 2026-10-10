import './styles/tokens.css'
import './styles/base.css'
import { MotionConfig } from 'motion/react'
import { Suspense, StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { HashRouter } from 'react-router-dom'

import { Loading, springs } from '@components'
import App from '@containers/App'

export default function renderApp () {
    const rootEl = document.getElementById('root')
    const AppInstance = (
        <StrictMode>
            <MotionConfig reducedMotion="user" transition={springs.snap}>
                <HashRouter>
                    <Suspense fallback={<Loading visible />}>
                        <App />
                    </Suspense>
                </HashRouter>
            </MotionConfig>
        </StrictMode>
    )

    const root = createRoot(rootEl!)
    root.render(AppInstance)
}
