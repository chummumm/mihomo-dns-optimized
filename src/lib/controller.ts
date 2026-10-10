export function controllerProtocol (override: string | null, hostname: string, configured: string | undefined, pageProtocol: string): 'http:' | 'https:' {
    if (override === 'http:' || override === 'https:') return override
    if (hostname === '127.0.0.1') return 'http:'
    if (configured === 'http:' || configured === 'https:') return configured
    return pageProtocol === 'http:' ? 'http:' : 'https:'
}
