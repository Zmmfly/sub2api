#!/usr/bin/env python3
"""Allocate immutable fork versions and safely resume GitHub release publication.

Run allocation under the repository-wide fork release concurrency lock, after
fetching all remote tags. No credentials other than the workflow GITHUB_TOKEN
(used by git/gh) are needed. VERSION remains upstream-owned metadata.
"""
import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile

TAG = re.compile(r'z(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)-([1-9]\d*)')
BASE = re.compile(r'(?:v)?((?:0|[1-9]\d*)\.(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))')
SHA = re.compile(r'[0-9a-f]{40}')


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def choose_version(base, sha, tags):
    """Pure allocator: tags is a mapping of names to peeled commit IDs."""
    if not BASE.fullmatch(base) or not SHA.fullmatch(sha):
        raise ValueError('invalid upstream version or source commit')
    base = BASE.fullmatch(base).group(1)
    existing = sorted(tag for tag, target in tags.items() if TAG.fullmatch(tag) and target == sha)
    if len(existing) > 1:
        raise ValueError('multiple fork release tags identify this source commit')
    if existing:
        if not existing[0].startswith('z' + base + '-'):
            raise ValueError('existing source tag disagrees with upstream VERSION')
        return existing[0]
    numbers = [int(tag.rsplit('-', 1)[1]) for tag in tags
               if TAG.fullmatch(tag) and tag.startswith('z' + base + '-')]
    return f'z{base}-{max(numbers, default=0) + 1}'


def allocate(args):
    if not SHA.fullmatch(args.sha):
        raise ValueError('source must be a full commit SHA')
    sha = run('git', 'rev-parse', '--verify', args.sha + '^{commit}')
    base = run('git', 'show', f'{sha}:backend/cmd/server/VERSION')
    tags = {tag: run('git', 'rev-parse', '--verify', f'refs/tags/{tag}^{{commit}}')
            for tag in run('git', 'tag', '--list', 'z*').splitlines() if TAG.fullmatch(tag)}
    tag = choose_version(base, sha, tags)
    if args.push:
        if tag not in tags:
            run('git', 'tag', '-a', tag, sha, '-m', f'Sub2API {tag}\n\nUpstream version: {base}\nSource: {sha}')
        # No --force: a conflicting allocation is an error, never rewritten.
        run('git', 'push', 'origin', f'refs/tags/{tag}:refs/tags/{tag}')
    timestamp = int(run('git', 'show', '-s', '--format=%ct', sha))
    result = {'tag': tag, 'version': tag, 'sha': sha,
              'date': datetime.fromtimestamp(timestamp, timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')}
    if os.environ.get('GITHUB_OUTPUT'):
        with Path(os.environ['GITHUB_OUTPUT']).open('a') as stream:
            stream.write(''.join(f'{key}={value}\n' for key, value in result.items()))
    print(json.dumps(result, sort_keys=True))


def digest(path):
    with Path(path).open('rb') as stream:
        return hashlib.file_digest(stream, 'sha256').hexdigest()


def expected_archives(tag):
    return {
        f'sub2api_{tag}_{system}_{arch}.' + ('zip' if system == 'windows' else 'tar.gz')
        for system, arch in [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')]}


def verify_downloaded_assets(directory, tag):
    directory = Path(directory)
    entries = {}
    for line in (directory / 'checksums.txt').read_text().splitlines():
        parts = line.split()
        if len(parts) != 2 or not re.fullmatch(r'[0-9a-f]{64}', parts[0]):
            raise ValueError('invalid published checksum entry')
        name = parts[1].removeprefix('*')
        if name in entries:
            raise ValueError('duplicate published checksum entry')
        entries[name] = parts[0]
    if set(entries) != expected_archives(tag):
        raise ValueError('published checksum entries do not match the complete release matrix')
    for name, checksum in entries.items():
        if digest(directory / name) != checksum:
            raise ValueError('published archive checksum mismatch: ' + name)


def prepare_assets(directory, tag, sha):
    directory = Path(directory)
    pattern = re.compile(r'sub2api_' + re.escape(tag) + r'_(linux|darwin|windows)_(amd64|arm64)\.(tar\.gz|zip)')
    assets = {}
    for path in directory.rglob('*'):
        if not path.is_file() or not pattern.fullmatch(path.name):
            continue
        if path.name in assets:
            raise ValueError('duplicate archive: ' + path.name)
        assets[path.name] = path
        manifest = path.with_name('manifest-' + pattern.fullmatch(path.name).group(1) + '-' + pattern.fullmatch(path.name).group(2) + '.json')
        if manifest.exists():
            data = json.loads(manifest.read_text())
            if data.get('sha') != sha or data.get('version') != tag or data.get('archive') != path.name or data.get('sha256') != digest(path):
                raise ValueError('archive provenance mismatch: ' + path.name)
    if set(assets) != expected_archives(tag):
        raise ValueError('release archives do not match the complete five-target matrix')
    content = ''.join(f'{digest(path)}  {name}\n' for name, path in sorted(assets.items()))
    checksums = directory / 'checksums.txt'
    if checksums.exists() and checksums.read_text() != content:
        raise ValueError('provided checksums disagree with release archives')
    checksums.write_text(content)
    assets['checksums.txt'] = checksums
    return assets


def release_info(repo, tag):
    result = subprocess.run(['gh', 'api', f'repos/{repo}/releases/tags/{tag}'], text=True, capture_output=True)
    if result.returncode:
        if 'HTTP 404' in result.stderr:
            return None
        raise RuntimeError('cannot read release: ' + result.stderr.strip())
    return json.loads(result.stdout)


def publish(args):
    if not TAG.fullmatch(args.tag) or not SHA.fullmatch(args.sha):
        raise ValueError('invalid fork tag or commit')
    if run('git', 'rev-parse', '--verify', f'refs/tags/{args.tag}^{{commit}}') != args.sha:
        raise ValueError('release tag does not identify the selected commit')
    repo = os.environ['GITHUB_REPOSITORY']
    assets = prepare_assets(args.input, args.tag, args.sha)
    info = release_info(repo, args.tag)
    if info is None:
        run('gh', 'release', 'create', args.tag, '--repo', repo, '--verify-tag', '--draft',
            '--title', f'Sub2API {args.tag}', '--notes',
            f'Fork release of https://github.com/{repo}/commit/{args.sha}.\n\nBased on Wei-Shaw/sub2api; original source attribution and license are retained.')
        info = release_info(repo, args.tag)
    if info['tag_name'] != args.tag:
        raise ValueError('release tag mismatch')
    existing = {asset['name'] for asset in info['assets']}
    if len(existing) != len(info['assets']):
        raise ValueError('release contains duplicate asset names')
    for name, path in sorted(assets.items()):
        if name in existing:
            with tempfile.TemporaryDirectory() as temp:
                run('gh', 'release', 'download', args.tag, '--repo', repo, '--pattern', name, '--dir', temp)
                if digest(Path(temp) / name) != digest(path):
                    raise ValueError('existing release asset differs; refusing replacement: ' + name)
        elif not info['draft']:
            raise ValueError('published release is missing asset; refusing modification: ' + name)
        else:
            run('gh', 'release', 'upload', args.tag, str(path), '--repo', repo)
    if not info['draft']:
        if info['prerelease']:
            raise ValueError('existing fork release is incorrectly marked prerelease')
        print('Published release verified; no changes made.')
        return
    run('gh', 'release', 'edit', args.tag, '--repo', repo, '--draft=false', '--prerelease=false',
        '--latest=' + str(args.latest).lower())
    final = release_info(repo, args.tag)
    if final['draft'] or final['prerelease'] or not set(assets).issubset({a['name'] for a in final['assets']}):
        raise ValueError('release publication verification failed')
    print('Published stable fork release ' + args.tag)



def status(args):
    if not TAG.fullmatch(args.tag) or not SHA.fullmatch(args.sha):
        raise ValueError('invalid fork tag or commit')
    if run('git', 'rev-parse', '--verify', f'refs/tags/{args.tag}^{{commit}}') != args.sha:
        raise ValueError('release tag does not identify the selected commit')
    info = release_info(os.environ['GITHUB_REPOSITORY'], args.tag)
    published = False
    if info and not info['draft']:
        expected = {'checksums.txt'} | expected_archives(args.tag)
        assets = info['assets']
        if info['prerelease'] or info['tag_name'] != args.tag:
            raise ValueError('existing fork release has invalid stability or tag metadata')
        if len({a['name'] for a in assets}) != len(assets):
            raise ValueError('release contains duplicate asset names')
        if not expected.issubset({a['name'] for a in assets if a['size'] > 0}):
            raise ValueError('published release is incomplete; refusing to overwrite it')
        with tempfile.TemporaryDirectory() as temp:
            for name in sorted(expected):
                run('gh', 'release', 'download', args.tag, '--repo', os.environ['GITHUB_REPOSITORY'], '--pattern', name, '--dir', temp)
            verify_downloaded_assets(temp, args.tag)
            if getattr(args, 'output', None):
                output = Path(args.output)
                output.mkdir(parents=True, exist_ok=True)
                if any(output.iterdir()):
                    raise ValueError('verified release output directory must be empty')
                for name in sorted(expected_archives(args.tag)):
                    source = Path(temp) / name
                    shutil.copy2(source, output / name)
                    target = re.fullmatch(r'sub2api_' + re.escape(args.tag) + r'_(linux|darwin|windows)_(amd64|arm64)\.(?:tar\.gz|zip)', name)
                    system, arch = target.groups()
                    manifest = {'sha': args.sha, 'version': args.tag, 'target': {'goos': system, 'goarch': arch}, 'archive': name, 'sha256': digest(source)}
                    (output / f'manifest-{system}-{arch}.json').write_text(json.dumps(manifest) + '\n')
        published = True
    value = str(published).lower()
    if os.environ.get('GITHUB_OUTPUT'):
        with Path(os.environ['GITHUB_OUTPUT']).open('a') as stream:
            stream.write('published=' + value + '\n')
    print('published=' + value)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(required=True)
    command = commands.add_parser('allocate')
    command.add_argument('--sha', required=True)
    command.add_argument('--push', action='store_true')
    command.set_defaults(handler=allocate)
    command = commands.add_parser('publish')
    command.add_argument('--tag', required=True)
    command.add_argument('--sha', required=True)
    command.add_argument('--input', required=True)
    command.add_argument('--latest', action='store_true')
    command.set_defaults(handler=publish)
    command = commands.add_parser('status')
    command.add_argument('--tag', required=True)
    command.add_argument('--sha', required=True)
    command.add_argument('--output', help='Restore verified archives and build manifests into an empty directory')
    command.set_defaults(handler=status)
    args = parser.parse_args()
    args.handler(args)


if __name__ == '__main__':
    main()
