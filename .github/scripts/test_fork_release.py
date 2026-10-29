import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('fork_release', Path(__file__).with_name('fork-release.py'))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
SHA = 'a' * 40
OTHER = 'b' * 40
TAG = 'z0.2.19-1'


class AllocationTests(unittest.TestCase):
    def test_first_and_upstream_reset(self):
        self.assertEqual(release.choose_version('0.2.19', SHA, {'z0.2.18-90': OTHER}), TAG)

    def test_numeric_sequence_not_lexical(self):
        self.assertEqual(release.choose_version('0.2.19', SHA, {'z0.2.19-9': OTHER, 'z0.2.19-10': OTHER}), 'z0.2.19-11')

    def test_same_sha_idempotent(self):
        self.assertEqual(release.choose_version('0.2.19', SHA, {TAG: SHA, 'z0.2.19-2': OTHER}), TAG)

    def test_ignore_other_tags(self):
        self.assertEqual(release.choose_version('0.2.19', SHA, {'v0.2.19': SHA, 'z0.2.19-foo': SHA}), TAG)

    def test_duplicate_source_rejected(self):
        with self.assertRaises(ValueError):
            release.choose_version('0.2.19', SHA, {TAG: SHA, 'z0.2.19-2': SHA})

    def test_base_disagreement_rejected(self):
        with self.assertRaises(ValueError):
            release.choose_version('0.2.20', SHA, {TAG: SHA})

    def test_invalid_versions(self):
        for base in ['traffic-stat-deadbeef', '0.2', '0.2.19-1', 'z0.2.19-1', '01.2.3']:
            with self.subTest(base=base), self.assertRaises(ValueError):
                release.choose_version(base, SHA, {})

    def test_git_annotated_and_lightweight_tags_without_mutation(self):
        with tempfile.TemporaryDirectory() as temp:
            previous = os.getcwd()
            try:
                os.chdir(temp)
                subprocess.run(['git', 'init', '-q'], check=True)
                subprocess.run(['git', 'config', 'user.name', 'Test'], check=True)
                subprocess.run(['git', 'config', 'user.email', 'test@example.invalid'], check=True)
                path = Path('backend/cmd/server/VERSION')
                path.parent.mkdir(parents=True)
                path.write_text('0.2.19\n')
                subprocess.run(['git', 'add', '.'], check=True)
                subprocess.run(['git', 'commit', '-qm', 'fixture'], check=True)
                sha = release.run('git', 'rev-parse', 'HEAD')
                subprocess.run(['git', 'tag', '-a', TAG, '-m', 'fixture'], check=True)
                with patch.dict(os.environ, {'GITHUB_OUTPUT': str(Path(temp) / 'output')}):
                    release.allocate(argparse.Namespace(sha=sha, push=False))
                self.assertIn('version=' + TAG, Path('output').read_text())
                self.assertEqual(release.run('git', 'tag'), TAG)
            finally:
                os.chdir(previous)


class ReleaseLookupTests(unittest.TestCase):
    view = {'tagName': TAG, 'isDraft': True, 'isPrerelease': False, 'assets': []}

    def test_actual_cli_draft_schema_normalized(self):
        response = subprocess.CompletedProcess([], 0, json.dumps(self.view), '')
        with patch.object(release.subprocess, 'run', return_value=response) as run:
            self.assertEqual(release.release_info('example/sub2api', TAG),
                             {'tag_name': TAG, 'draft': True, 'prerelease': False, 'assets': []})
        self.assertEqual(run.call_args.args[0], ['gh', 'release', 'view', TAG, '--repo',
                         'example/sub2api', '--json', 'tagName,isDraft,isPrerelease,assets'])

    def test_only_exact_cli_notfound_means_absent(self):
        response = subprocess.CompletedProcess([], 1, '', 'release not found\n')
        with patch.object(release.subprocess, 'run', return_value=response):
            self.assertIsNone(release.release_info('example/sub2api', TAG))

    def test_access_network_and_server_errors_never_mean_absent(self):
        for error in ['gh: Not Found (HTTP 404)', 'gh: Bad credentials (HTTP 401)',
                      'gh: Forbidden (HTTP 403)', 'dial tcp: network unreachable', 'HTTP 503',
                      'release not found: HTTP 403']:
            response = subprocess.CompletedProcess([], 1, '', error)
            with self.subTest(error=error), patch.object(release.subprocess, 'run', return_value=response):
                with self.assertRaisesRegex(RuntimeError, 'cannot read release'):
                    release.release_info('example/sub2api', TAG)

    def test_malformed_or_wrong_tag_rejected(self):
        for data in [{}, [], dict(self.view, tagName='z0.2.19-2'),
                     dict(self.view, assets=[{'name': 'checksums.txt'}])]:
            response = subprocess.CompletedProcess([], 0, json.dumps(data), '')
            with self.subTest(data=data), patch.object(release.subprocess, 'run', return_value=response):
                with self.assertRaises(ValueError):
                    release.release_info('example/sub2api', TAG)


class PublishTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.archive = self.directory / f'sub2api_{TAG}_linux_amd64.tar.gz'
        for name in release.expected_archives(TAG):
            (self.directory / name).write_bytes(b'archive fixture ' + name.encode())
        self.now = 0
        def sleep(seconds):
            self.now += seconds
        for target, replacement in [('monotonic', lambda: self.now), ('sleep', sleep)]:
            clock = patch.object(release.time, target, side_effect=replacement)
            clock.start()
            self.addCleanup(clock.stop)
        self.args = argparse.Namespace(tag=TAG, sha=SHA, input=self.temp.name, latest=False)
        self.env = patch.dict(os.environ, {'GITHUB_REPOSITORY': 'example/sub2api'})
        self.env.start()
        self.addCleanup(self.env.stop)

    def info(self, draft=True, assets=None):
        return {'tag_name': TAG, 'draft': draft, 'prerelease': False, 'assets': assets or []}

    def test_checksums_deterministic(self):
        first = release.prepare_assets(self.directory, TAG, SHA)
        before = first['checksums.txt'].read_bytes()
        release.prepare_assets(self.directory, TAG, SHA)
        self.assertEqual(before, first['checksums.txt'].read_bytes())

    def test_provenance_mismatch(self):
        (self.directory / 'manifest-linux-amd64.json').write_text(json.dumps({'sha': OTHER}))
        with self.assertRaisesRegex(ValueError, 'provenance'):
            release.prepare_assets(self.directory, TAG, SHA)

    def test_existing_checksums_mismatch(self):
        (self.directory / 'checksums.txt').write_text('bogus')
        with self.assertRaisesRegex(ValueError, 'checksums'):
            release.prepare_assets(self.directory, TAG, SHA)

    def test_publish_new_draft_without_clobber(self):
        final = self.info(False, [{'name': name} for name in release.expected_archives(TAG) | {'checksums.txt'}])
        responses = [subprocess.CompletedProcess([], 1, '', 'release not found\n')]
        for info in [self.info(), final]:
            data = {'tagName': info['tag_name'], 'isDraft': info['draft'],
                    'isPrerelease': info['prerelease'],
                    'assets': [dict(asset, size=1) for asset in info['assets']]}
            responses.append(subprocess.CompletedProcess([], 0, json.dumps(data), ''))
        with patch.object(release.subprocess, 'run', side_effect=responses), patch.object(release, 'run', return_value=SHA) as run:
            release.publish(self.args)
        calls = [call.args for call in run.call_args_list]
        self.assertTrue(any(call[1:3] == ('release', 'create') and '--draft' in call for call in calls))
        self.assertTrue(any('--latest=false' in call and '--prerelease=false' in call for call in calls))
        self.assertFalse(any('--clobber' in call or 'delete' in call for call in calls))

    def test_draft_visibility_failure_is_explicit_and_does_not_upload(self):
        with patch.object(release, 'release_info', return_value=None), patch.object(release, 'run', return_value=SHA) as run:
            with self.assertRaisesRegex(RuntimeError, 'created draft metadata did not converge'):
                release.publish(self.args)
        calls = [call.args for call in run.call_args_list]
        self.assertEqual(sum(call[1:3] == ('release', 'create') for call in calls), 1)
        self.assertFalse(any(call[1:3] in [('release', 'upload'), ('release', 'edit')] for call in calls))

    def test_publish_verifies_final_visibility_without_none_crash(self):
        with patch.object(release, 'release_info', side_effect=[self.info()] + [None] * 30), patch.object(release, 'run', return_value=SHA):
            with self.assertRaisesRegex(RuntimeError, 'published release metadata did not converge'):
                release.publish(self.args)

    def test_create_and_publish_metadata_propagation_polls_reads_only(self):
        names = [{'name': name} for name in release.expected_archives(TAG) | {'checksums.txt'}]
        final = self.info(False, names)
        observations = [None, None, None, None, self.info(),  # initial missing, then 3 stale reads
                        None, self.info(True, names), self.info(False), final]
        with patch.object(release, 'release_info', side_effect=observations), patch.object(release, 'run', return_value=SHA) as run:
            release.publish(self.args)
        calls = [call.args for call in run.call_args_list]
        self.assertEqual(sum(call[1:3] == ('release', 'create') for call in calls), 1)
        self.assertEqual(sum(call[1:3] == ('release', 'edit') for call in calls), 1)
        self.assertEqual(sum(call[1:3] == ('release', 'upload') for call in calls), 6)
        self.assertGreater(self.now, 0)

    def test_initial_auth_failure_never_creates_release(self):
        with patch.object(release, 'release_info', side_effect=RuntimeError('HTTP 403')), patch.object(release, 'run', return_value=SHA) as run:
            with self.assertRaisesRegex(RuntimeError, '403'):
                release.publish(self.args)
        self.assertEqual(len(run.call_args_list), 1)  # read-only git tag verification

    def test_poll_auth_failure_does_not_recreate(self):
        with patch.object(release, 'release_info', side_effect=[None, RuntimeError('HTTP 403')]), patch.object(release, 'run', return_value=SHA) as run:
            with self.assertRaisesRegex(RuntimeError, '403'):
                release.publish(self.args)
        self.assertEqual(sum(call.args[1:3] == ('release', 'create') for call in run.call_args_list), 1)

    def test_visibility_timeout_polls_for_full_budget(self):
        with patch.object(release, 'release_info', return_value=None), patch.object(release, 'run', return_value=SHA):
            with self.assertRaisesRegex(RuntimeError, '180s'):
                release.publish(self.args)
        self.assertEqual(self.now, 180)

    def test_published_missing_asset_is_not_modified(self):
        with patch.object(release, 'release_info', return_value=self.info(False)), patch.object(release, 'run', return_value=SHA) as run:
            with self.assertRaisesRegex(ValueError, 'missing asset'):
                release.publish(self.args)
        self.assertEqual(len(run.call_args_list), 1)

    def test_draft_existing_asset_mismatch_not_overwritten(self):
        def execute(*args):
            if args[0] == 'git':
                return SHA
            if args[1:3] == ('release', 'download'):
                directory = Path(args[args.index('--dir') + 1])
                name = args[args.index('--pattern') + 1]
                (directory / name).write_bytes(b'wrong')
                return ''
            self.fail('unexpected mutation: ' + repr(args))
        info = self.info(True, [{'name': 'checksums.txt'}])
        with patch.object(release, 'release_info', return_value=info), patch.object(release, 'run', side_effect=execute):
            with self.assertRaisesRegex(ValueError, 'refusing replacement'):
                release.publish(self.args)

    def test_draft_retry_reuses_identical_assets(self):
        assets = release.prepare_assets(self.directory, TAG, SHA)
        def execute(*args):
            if args[0] == 'git':
                return SHA
            if args[1:3] == ('release', 'download'):
                directory = Path(args[args.index('--dir') + 1])
                name = args[args.index('--pattern') + 1]
                (directory / name).write_bytes(assets[name].read_bytes())
                return ''
            if args[1:3] == ('release', 'edit'):
                return ''
            self.fail('unexpected mutation: ' + repr(args))
        names = [{'name': name} for name in assets]
        with patch.object(release, 'release_info', side_effect=[self.info(True, names), self.info(False, names)]), patch.object(release, 'run', side_effect=execute):
            release.publish(self.args)

    def test_published_retry_is_read_only(self):
        assets = release.prepare_assets(self.directory, TAG, SHA)
        def execute(*args):
            if args[0] == 'git':
                return SHA
            if args[1:3] == ('release', 'download'):
                directory = Path(args[args.index('--dir') + 1])
                name = args[args.index('--pattern') + 1]
                (directory / name).write_bytes(assets[name].read_bytes())
                return ''
            self.fail('unexpected mutation: ' + repr(args))
        names = [{'name': name} for name in assets]
        with patch.object(release, 'release_info', return_value=self.info(False, names)), patch.object(release, 'run', side_effect=execute):
            release.publish(self.args)

    def test_completed_release_status(self):
        assets = [{'name': 'checksums.txt', 'size': 1}]
        for system, arch in [('linux', 'amd64'), ('linux', 'arm64'), ('darwin', 'amd64'), ('darwin', 'arm64'), ('windows', 'amd64')]:
            assets.append({'name': f'sub2api_{TAG}_{system}_{arch}.' + ('zip' if system == 'windows' else 'tar.gz'), 'size': 1})
        self.args.output = str(self.directory / 'restored')
        local_assets = release.prepare_assets(self.directory, TAG, SHA)
        def execute(*args):
            if args[0] == 'git':
                return SHA
            directory = Path(args[args.index('--dir') + 1])
            name = args[args.index('--pattern') + 1]
            (directory / name).write_bytes(local_assets[name].read_bytes())
            return ''
        with patch.object(release, 'release_info', return_value=self.info(False, assets)), patch.object(release, 'run', side_effect=execute), patch.dict(os.environ, {'GITHUB_OUTPUT': str(self.directory / 'output')}):
            release.status(self.args)
        self.assertEqual((self.directory / 'output').read_text(), 'published=true\n')
        restored = Path(self.args.output)
        self.assertEqual(len(list(restored.iterdir())), 10)
        self.assertFalse((restored / 'checksums.txt').exists())
        for name in release.expected_archives(TAG):
            self.assertEqual((restored / name).read_bytes(), local_assets[name].read_bytes())
        manifest = json.loads((restored / 'manifest-linux-amd64.json').read_text())
        self.assertEqual(manifest, {'sha': SHA, 'version': TAG, 'target': {'goos': 'linux', 'goarch': 'amd64'}, 'archive': self.archive.name, 'sha256': release.digest(self.archive)})

    def test_publish_requires_full_matrix(self):
        self.archive.unlink()
        with self.assertRaisesRegex(ValueError, 'five-target'):
            release.prepare_assets(self.directory, TAG, SHA)

    def test_downloaded_archive_checksum_mismatch(self):
        release.prepare_assets(self.directory, TAG, SHA)
        self.archive.write_bytes(b'tampered')
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            release.verify_downloaded_assets(self.directory, TAG)

    def test_downloaded_checksums_require_exact_entries(self):
        release.prepare_assets(self.directory, TAG, SHA)
        path = self.directory / 'checksums.txt'
        lines = path.read_text().splitlines()
        for content in ['\n'.join(lines[:-1]), '\n'.join(lines + [lines[0]])]:
            path.write_text(content)
            with self.assertRaises(ValueError):
                release.verify_downloaded_assets(self.directory, TAG)

    def test_incomplete_published_status_fails_closed(self):
        with patch.object(release, 'release_info', return_value=self.info(False)), patch.object(release, 'run', return_value=SHA):
            with self.assertRaisesRegex(ValueError, 'incomplete'):
                release.status(self.args)


if __name__ == '__main__':
    unittest.main()
