import './app.css'
import classnames from 'classnames'
import { Route, Navigate, Routes, useLocation, Outlet } from 'react-router-dom'

// import Overview from '@containers/Overview'
import Connections from '@containers/Connections'
import DNS from '@containers/DNS'
import ExternalControllerModal from '@containers/ExternalControllerDrawer'
import Logs from '@containers/Logs'
import Proxies from '@containers/Proxies'
import Rules from '@containers/Rules'
import Settings from '@containers/Settings'
import SideBar from '@containers/Sidebar'
import { isClashX } from '@lib/jsBridge'
import { useLogsStreamReader } from '@stores'

export default function App () {
    useLogsStreamReader()

    const location = useLocation()

    const routes = [
    // { path: '/', name: 'Overview', component: Overview, exact: true },
        { path: '/proxies', name: 'Proxies', icon: 'nav-proxies', element: <Proxies /> },
        { path: '/logs', name: 'Logs', icon: 'nav-logs', element: <Logs /> },
        { path: '/rules', name: 'Rules', icon: 'nav-rules', element: <Rules />, noMobile: true },
        { path: '/connections', name: 'Connections', icon: 'nav-connections', element: <Connections /> },
        { path: '/dns', name: 'DNS', icon: 'nav-dns', element: <DNS /> },
        { path: '/settings', name: 'Settings', icon: 'nav-settings', element: <Settings /> },
    ]

    const layout = (
        <div className={classnames('app', { 'not-clashx': !isClashX() })}>
            <SideBar routes={routes} />
            <main className="page-container">
                <div className="route-view" key={location.pathname}>
                    <Outlet />
                </div>
            </main>
            <ExternalControllerModal />
        </div>
    )

    return (
        <Routes>
            <Route path="/" element={layout}>
                <Route path="/" element={<Navigate to={{ pathname: '/proxies', search: location.search }} replace />} />
                {
                    routes.map(
                        route => <Route path={route.path} key={route.path} element={route.element} />,
                    )
                }
            </Route>
        </Routes>
    )
}
