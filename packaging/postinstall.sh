#!/bin/sh
set -eu

# Reuse an administrator-provided account without changing its UID, group,
# home or shell. Only newly created accounts receive our non-login defaults.
if ! getent passwd mihomo >/dev/null; then
    if ! getent group mihomo >/dev/null; then
        groupadd --system mihomo
    fi
    useradd --system --gid mihomo --home-dir /nonexistent \
        --no-create-home --shell /usr/sbin/nologin mihomo
fi

# Match the existing account's primary group, which need not be named mihomo.
# Keep ordinary access modes/ACLs; chown may clear set-ID bits as usual.
# -h/-R does not follow links out of the data directory.
owner="mihomo:$(id -g mihomo)"
chown -hR -- "$owner" /etc/mihomo
chown -h -- "$owner" /usr/bin/mihomo
