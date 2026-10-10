#!/usr/bin/env python3
"""Package the built dashboard for Mihomo's external UI updater."""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import zipfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-sha', required=True)
    parser.add_argument('--repository', required=True)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    dist = root / 'dist'
    if not (dist / 'index.html').is_file() or not (dist / 'assets').is_dir():
        raise SystemExit('Run pnpm build before packaging the UI.')

    package = json.loads((root / 'package.json').read_text())
    info = {
        'name': package['name'],
        'version': package['version'],
        'source_commit': args.source_sha,
        'source_url': f'https://github.com/{args.repository}/tree/{args.source_sha}',
        'upstream_commit': '9a32d9d163ad233141c384ba365c6ef18c58cb94',
        'display_host_order': ['host', 'sniffHost', 'destinationIP'],
        'dns_observability_api': 1,
        'dns_storage': 'memory',
        'dns_cache_ratio': '(cache_fresh + cache_stale) / queries',
        'closed_connection_limit': 5000,
        'virtualized_connections': True,
    }
    (dist / 'build-info.json').write_text(json.dumps(info, indent=2) + '\n')
    shutil.copyfile(root / 'LICENSE', dist / 'LICENSE')
    (dist / '.nojekyll').touch()

    files = sorted(p for p in dist.rglob('*') if p.is_file() and p.name != 'SHA256SUMS')
    sums = [f'{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.relative_to(dist).as_posix()}' for p in files]
    (dist / 'SHA256SUMS').write_text('\n'.join(sums) + '\n')

    artifacts = root / 'artifacts'
    artifacts.mkdir(exist_ok=True)
    archive = artifacts / 'clash-dashboard-ui.zip'
    with zipfile.ZipFile(archive, 'w', compression=zipfile.ZIP_DEFLATED) as output:
        for file in sorted(p for p in dist.rglob('*') if p.is_file()):
            output.write(file, file.relative_to(dist).as_posix())
    (artifacts / 'SHA256SUMS').write_text(f'{hashlib.sha256(archive.read_bytes()).hexdigest()}  {archive.name}\n')
    print(f'Packaged {len(files) + 1} UI files for {args.source_sha}: {archive.name}')


if __name__ == '__main__':
    main()
