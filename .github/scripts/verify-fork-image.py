#!/usr/bin/env python3
"""Fail closed if a reusable registry image differs from the reserved release."""
import json
import sys


def verify(data, version, sha, repository, arches):
    # Buildx .Image is a config object for one platform, or a platform->config
    # mapping for multi-platform indexes (including provenance attestations).
    configs = [data] if 'architecture' in data else list(data.values())
    platforms = set()
    for config in configs:
        if config.get('os') == 'unknown':
            continue
        arch = config.get('architecture')
        if config.get('os') != 'linux' or arch not in arches or arch in platforms:
            raise ValueError('unexpected or duplicate runtime image platform')
        labels = config.get('config', {}).get('Labels', {})
        expected = {'org.opencontainers.image.version': version,
                    'org.opencontainers.image.revision': sha,
                    'org.opencontainers.image.source': 'https://github.com/' + repository}
        if any(labels.get(k) != v for k, v in expected.items()):
            raise ValueError('registry image provenance differs from reserved release')
        platforms.add(arch)
    if platforms != set(arches):
        raise ValueError('registry image is missing a required architecture')


if __name__ == '__main__':
    verify(json.load(sys.stdin), *sys.argv[1:4], sys.argv[4].split(','))
