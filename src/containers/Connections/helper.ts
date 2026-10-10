import type { Connections } from '@lib/request'

export function getConnectionHost (metadata?: Connections['metadata']) {
    if (metadata?.host) return metadata.host
    if (metadata?.sniffHost) return metadata.sniffHost
    return metadata?.destinationIP ?? ''
}
