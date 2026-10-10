import classnames from 'classnames'
import { motion } from 'motion/react'
import { NavLink, useLocation } from 'react-router-dom'

import logo from '@assets/logo.png'
import { Icon, springs } from '@components'
import { type Lang, type Language } from '@i18n'
import { useI18n, useVersion, useClashXData } from '@stores'
import './style.css'

// Matches --item-h in style.css: the marker travels one station per item.
const ITEM_HEIGHT = 44

interface SidebarProps {
    routes: Array<{
        path: string
        name: string
        icon?: string
        noMobile?: boolean
    }>
}

export default function Sidebar (props: SidebarProps) {
    const { routes } = props
    const { translation } = useI18n()
    const { version, premium } = useVersion()
    const { data } = useClashXData()
    const { t } = translation('SideBar')
    const location = useLocation()
    const isActive = (path: string) => location.pathname === path || location.pathname.startsWith(path + '/')
    const activeIndex = routes.findIndex(route => isActive(route.path))
    const mobileRoutes = routes.filter(route => !route.noMobile)
    const mobileIndex = mobileRoutes.findIndex(route => isActive(route.path))

    const navlinks = routes.map(
        ({ path, name, icon, noMobile }) => (
            <li className={classnames('item', { 'no-mobile': noMobile })} key={name}>
                <NavLink to={{ pathname: path, search: location.search }} className={({ isActive }) => classnames('item-link', { active: isActive })}>
                    <span className="item-station" aria-hidden="true" />
                    {icon && <Icon className="item-icon" type={icon} size={18} />}
                    <span className="item-label">{ t(name as keyof typeof Language[Lang]['SideBar']) }</span>
                </NavLink>
            </li>
        ),
    )

    return (
        <nav className="sidebar">
            <div className="sidebar-brand">
                <img src={logo} alt="" className="sidebar-logo" />
                <span className="sidebar-name">Clash{ data?.isClashX && 'X' }</span>
            </div>
            <div className="sidebar-route" style={{ '--count-m': mobileRoutes.length } as React.CSSProperties}>
                {activeIndex !== -1 && <motion.span className="sidebar-marker" aria-hidden="true" initial={false} animate={{ y: activeIndex * ITEM_HEIGHT }} transition={springs.glide} />}
                {mobileIndex !== -1 && <motion.span className="sidebar-marker-mobile" aria-hidden="true" initial={false} animate={{ x: `${mobileIndex * 100}%` }} transition={springs.glide} />}
                <ul className="sidebar-menu">
                    { navlinks }
                </ul>
            </div>
            <div className="sidebar-version">
                <span className="sidebar-version-label">Clash{ data?.isClashX && 'X' } { t('Version') }</span>
                <span className="sidebar-version-text" title={version}>{ version || '—' }</span>
                { premium && <span className="sidebar-version-premium">Premium</span> }
            </div>
        </nav>
    )
}
