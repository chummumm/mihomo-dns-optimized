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
    CGO_ENABLED=0 go test -tags with_gvisor -count=1 -timeout=5m \
      ./adapter ./component/dnsmessage/... ./component/resolver/... ./context/... ./dns/... ./listener/sing_tun/... ./transport/socks4/... ./transport/socks5/... \
      ./tunnel/... ./rules/logic/... ./rules/provider/... ./config/... \
      ./hub/route/... ./hub/executor/...
    CGO_ENABLED=1 go test -race -tags with_gvisor -count=1 -timeout=5m \
      ./adapter ./component/dnsmessage/... ./component/resolver/... ./context/... ./dns/... \
      ./listener/sing_tun/... ./tunnel/... ./rules/logic/... ./rules/provider/... \
      ./config/... ./hub/route/... ./hub/executor/... \
      -run 'DNSRouting|DNSRuleRouting|DNSProxy|DNSDrop|DNSDirectProbe|DNSOptimization|SpeedCheck|CacheControl'
    ;;
  build)
    arch=${2:?architecture required: amd64 or arm64}
    output_dir=${3:?absolute output directory required}
    version=${4:?build version required}
    build_time=${5:?build time required}
    [[ "$arch" == amd64 || "$arch" == arm64 ]]
    [[ "$output_dir" == /* ]]
    [[ "$version" =~ ^[0-9A-Za-z._+-]+$ ]]
    [[ "$build_time" =~ ^[0-9TZ:+.-]+$ ]]
    mkdir -p "$output_dir"
    binary="mihomo-dns-linux-${arch}-${version}"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOAMD64=v1 \
      go build -tags with_gvisor -trimpath \
      -ldflags "-s -w -buildid= -X github.com/metacubex/mihomo/constant.Version=$version -X github.com/metacubex/mihomo/constant.BuildTime=$build_time" \
      -o "$output_dir/$binary" .
    if [[ "$arch" == "$(go env GOHOSTARCH)" && "$(go env GOHOSTOS)" == linux ]]; then
      "$output_dir/$binary" -v
      python3 scripts/test-dns-proxy.py "$output_dir/$binary"
    fi
    gzip -n -f "$output_dir/$binary"
    (cd "$output_dir" && sha256sum "$binary.gz" > "$binary.gz.sha256")
    ;;
  *)
    printf 'Usage: %s test | build <amd64|arm64> <absolute-output-dir> <version> <ISO-time>\n' "$0" >&2
    exit 2
    ;;
esac
