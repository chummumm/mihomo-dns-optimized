export interface DNSObservability {
    version: number
    enabled: boolean
    storage: 'memory'
    instance_id: string
    started_at: string
    retention_seconds: number
    capacity: number
    memory_limit_bytes: number
    accounted_bytes: number
    memory_budget_kind: 'bounded_accounting'
    latency_percentile_kind: 'logarithmic_upper_bound'
    retained: number
    evicted: number
    oldest_id: string
    newest_id: string
    upstream_limit: number
}

export type DNSOutcome = 'cache_fresh' | 'cache_stale' | 'hosts' | 'fake_ip' | 'upstream' | 'reject' | 'drop' | 'error' | 'local'

export interface DNSAnswer {
    type: string
    value: string
    ttl: number
}

export interface DNSQuery {
    id: string
    time: string
    qname: string
    client: string
    qtype: string
    protocol: string
    source: string
    outcome: DNSOutcome
    cache: 'fresh' | 'stale' | 'miss' | 'none'
    rcode: string
    elapsed_ms: number
    answers: DNSAnswer[]
    answers_truncated: boolean
    error: string
    upstream: string
}

export interface DNSQueryParams {
    limit: 50 | 100
    cursor?: string
    qname?: string
    client?: string
    outcome?: DNSOutcome
    qtype?: string
}

export interface DNSQueries {
    items: DNSQuery[]
    next_cursor: string
    has_more: boolean
    retained: number
    evicted: number
    scope: 'retained'
    instance_id: string
}

export interface DNSCounts {
    queries: number
    cache_fresh: number
    cache_stale: number
    hosts: number
    fake_ip: number
    upstream: number
    reject: number
    drop: number
    errors: number
    local: number
    elapsed_ms_avg: number
    elapsed_ms_p95: number
}

export interface DNSBucket {
    time: string
    queries: number
    cache_hits: number
    errors: number
    elapsed_ms_avg: number
}

export interface DNSTopEntry {
    name: string
    count: number
}

export interface DNSStats {
    instance_id: string
    started_at: string
    totals: DNSCounts
    last_24h: DNSCounts
    series: DNSBucket[]
    top_domains: DNSTopEntry[]
    top_clients: DNSTopEntry[]
    top_scope: 'retained'
    retained: number
    evicted: number
}

export interface DNSUpstream {
    id: string
    address: string
    attempts: number
    successes: number
    errors: number
    canceled: number
    timeouts: number
    rcode_errors: number
    elapsed_ms_avg: number
}

export interface DNSUpstreams {
    items: DNSUpstream[]
    scope: 'process'
    attempt_scope: 'resolver_exchange'
    limit: number
    overflowed: number
    instance_id: string
}
