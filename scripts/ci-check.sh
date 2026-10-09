#!/usr/bin/env bash
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"
export GOFLAGS="${GOFLAGS:--mod=readonly}"
export SKIP_INTEROP_TEST=1
export SKIP_CONCURRENT_TEST=1

case "${1:-test}" in
  test)
    python3 scripts/test-upstream-sync.py
    python3 scripts/test-release-build.py
    CGO_ENABLED=0 go test -tags with_gvisor -count=1 -timeout=5m \
      ./adapter ./common/arc/... ./component/dnsmessage/... ./component/resolver/... ./component/updater/... ./context/... ./dns/... ./listener/sing_tun/... ./transport/socks4/... ./transport/socks5/... \
      ./tunnel/... ./rules/logic/... ./rules/provider/... ./config/... \
      ./hub/route/... ./hub/executor/...
    CGO_ENABLED=1 go test -race -tags with_gvisor -count=1 -timeout=5m \
      ./adapter ./common/arc/... ./component/dnsmessage/... ./component/resolver/... ./component/updater/... ./context/... ./dns/... \
      ./listener/sing_tun/... ./tunnel/... ./rules/logic/... ./rules/provider/... \
      ./config/... ./hub/route/... ./hub/executor/... \
      -run 'DNSRouting|DNSRuleRouting|DNSProxy|DNSDrop|DNSDirectProbe|DNSOptimization|SpeedCheck|DualStack|CacheControl|ARCDelete|DNSPerf|DNSCandidate|DNSCachedTTL|DNSAnswerLifetime|DNSResolverDoesNotRestartTTL|DNSFallback|DNSIndependentDirectPool|CoreUpdater'
    # A single green rerun cannot validate detached HTTP/2 dial shutdown.
    # Repeat the lifecycle barriers with different Go scheduler parallelism.
    for procs in 1 4; do
      GOMAXPROCS="$procs" CGO_ENABLED=1 go test -race -tags with_gvisor \
        -count=50 -timeout=5m ./dns ./tunnel \
        -run 'DNSRuleRoutingNative(CloseDuringEncryptedConstruction|H2CloseBeforeLateDialReturns|DashboardCloseDoesNotRetry)$|DNSRoutingNativeCanceledBootstrapDoesNotDial$|DNSProxyCanceledBootstrapDoesNotExchange$|DNSProxyCanceledLateSocketNeverWrites$'
    done
    ;;
  build)
    target=${2:?release target required (amd64/arm64 aliases remain supported)}
    output_dir=${3:?absolute output directory required}
    version=${4:?build version required}
    build_time=${5:?build time required}
    python3 scripts/release-build.py build "$target" "$output_dir" "$version" "$build_time"
    ;;
  *)
    printf 'Usage: %s test | build <target> <absolute-output-dir> <version> <ISO-time>\n' "$0" >&2
    exit 2
    ;;
esac
