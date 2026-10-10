interface SelectedGroup {
    now?: string
}

// Follow only the selected path. Map lookups avoid scanning every group at each
// hop; the visited set handles cycles without imposing an arbitrary depth cap.
export function resolveProxyExit (name: string, groups: ReadonlyMap<string, SelectedGroup>): string | undefined {
    const visited = new Set<string>()
    let current = name
    while (current !== '') {
        if (visited.has(current)) return undefined
        const group = groups.get(current)
        if (group === undefined) return current
        visited.add(current)
        current = group.now ?? ''
    }
    return undefined
}
