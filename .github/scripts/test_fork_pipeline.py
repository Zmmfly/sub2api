import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('image', Path(__file__).with_name('verify-fork-image.py'))
image = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image)


class ForkPipelineTest(unittest.TestCase):
    def test_image_provenance_and_platforms(self):
        def config(arch):
            return {'architecture': arch, 'os': 'linux', 'config': {'Labels': {
                'org.opencontainers.image.version': 'z0.2.19-1',
                'org.opencontainers.image.revision': 'a' * 40,
                'org.opencontainers.image.source': 'https://github.com/Zmmfly/sub2api'}}}
        configs = {arch: config(arch) for arch in ['amd64', 'arm64']}
        image.verify(configs, 'z0.2.19-1', 'a' * 40, 'Zmmfly/sub2api', ['amd64', 'arm64'])
        for bad, sha in [(configs, 'b' * 40), ({'amd64': configs['amd64']}, 'a' * 40)]:
            with self.assertRaises(ValueError):
                image.verify(bad, 'z0.2.19-1', sha, 'Zmmfly/sub2api', ['amd64', 'arm64'])
        image.verify(config('amd64'), 'z0.2.19-1', 'a' * 40, 'Zmmfly/sub2api', ['amd64'])

    def test_stale_promotion_does_not_touch_registry_or_release(self):
        with tempfile.TemporaryDirectory() as tmp:
            directory = Path(tmp)
            for command, body in [('git', 'printf "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\\trefs/heads/main\\n"'),
                                  ('docker', 'exit 91'), ('gh', 'exit 92')]:
                p = directory / command
                p.write_text('#!/bin/sh\n' + body + '\n')
                p.chmod(0o755)
            env = dict(os.environ, PATH=tmp + ':' + os.environ['PATH'], RELEASE_VERSION='z0.2.19-1',
                       RELEASE_SHA='a' * 40, GITHUB_REPOSITORY='Zmmfly/sub2api', GITHUB_STEP_SUMMARY=str(directory / 'summary'))
            subprocess.run(['bash', str(ROOT / '.github/scripts/fork-images.sh'), 'promote'], env=env, check=True)
            self.assertIn('skipped', (directory / 'summary').read_text())

    def test_resolver_prefers_canonical_fork_tag_and_rejects_ambiguity(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / 'backend/scripts').mkdir(parents=True)
            (root / 'backend/cmd/server').mkdir(parents=True)
            shutil.copy(ROOT / 'backend/scripts/resolve-version.sh', root / 'backend/scripts/')
            (root / 'backend/cmd/server/VERSION').write_text('0.2.19\n')
            def git(*args):
                return subprocess.check_output(['git', '-C', tmp, *args], stderr=subprocess.DEVNULL, text=True)
            git('init'); git('config', 'user.name', 'test'); git('config', 'user.email', 'test@example.com')
            git('add', '.'); git('commit', '-m', 'fixture'); git('tag', 'v0.2.19')
            resolve = ['sh', str(root / 'backend/scripts/resolve-version.sh')]
            self.assertEqual(subprocess.check_output(resolve, text=True).strip(), '0.2.19')
            git('tag', 'z0.2.19-10')
            self.assertEqual(subprocess.check_output(resolve, text=True).strip(), 'z0.2.19-10')
            git('tag', 'z0.2.19-11')
            self.assertNotEqual(subprocess.run(resolve, capture_output=True).returncode, 0)

    def test_workflow_contract(self):
        import yaml
        build = yaml.safe_load((ROOT / '.github/workflows/fork-build.yml').read_text())
        self.assertEqual(build['concurrency'], {'group': 'fork-main-release', 'cancel-in-progress': False})
        latest = (ROOT / '.github/workflows/fork-latest.yml').read_text()
        self.assertIn('branches: [main]', latest)
        self.assertNotIn('dev/traffic-stat', latest)
        sync = (ROOT / '.github/scripts/fork-sync.sh').read_text()
        self.assertNotIn('refs/heads/dev', sync)
        self.assertNotIn('--force', sync)
        source = (ROOT / '.github/workflows/fork-build.yml').read_text()
        self.assertLess(source.index('Source is no longer main tip'), source.index(' allocate --sha'))
        self.assertNotIn('secrets.DOCKERHUB', source)


if __name__ == '__main__':
    unittest.main()
