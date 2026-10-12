import type { DNSQueries } from './types'

// A bounded index of page boundaries supplied atomically with the visible rows.
// The browser never scans or retains the contents/IDs of other result pages.
export interface PageIndex {
    total: number
    anchor: string
    cursors: string[]
}

export function pageCount (total: number, limit: number) {
    return Math.max(1, Math.ceil(total / limit))
}

function queryID (value: unknown): value is string {
    return typeof value === 'string' && /^[1-9][0-9]{0,19}$/.test(value) && BigInt(value) <= BigInt('18446744073709551615')
}

// Missing metadata means an older core: retain cursor navigation without
// inventing a filtered total or assuming that retained IDs are consecutive.
export function readPageIndex (data: DNSQueries, limit: number): PageIndex | undefined {
    const { total, anchor_id: anchor, page_cursors: cursors } = data
    if (total === undefined || !Number.isSafeInteger(total) || total < 0 || total > data.retained) return undefined
    if (typeof anchor !== 'string' || (anchor !== '' && !queryID(anchor)) || (total > 0 && anchor === '')) return undefined
    if (!Array.isArray(cursors) || cursors.length !== pageCount(total, limit) || cursors[0] !== '') return undefined
    let previous = anchor === '' ? BigInt(0) : BigInt(anchor) + BigInt(1)
    for (const cursor of cursors.slice(1)) {
        if (!queryID(cursor) || BigInt(cursor) >= previous) return undefined
        previous = BigInt(cursor)
    }
    return { total, anchor, cursors }
}

export function cursorForPage (index: PageIndex | undefined, page: number) {
    return Number.isSafeInteger(page) && page >= 1 ? index?.cursors[page - 1] : undefined
}
