"""Release checks must run once, on the exact published source, and fail closed."""
import copy
from pathlib import Path
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / '.github/workflows'
SOURCE = '${{ inputs.source_sha }}'
CHECK_SOURCE = '${{ inputs.source_sha || github.sha }}'


def read(name):
    # BaseLoader keeps the YAML 1.1 boolean word `on` as the workflow event key.
    return yaml.load((WORKFLOWS / name).read_text(), Loader=yaml.BaseLoader)


def validate_gate(build, ci, security):
    jobs = build['jobs']
    assert set(jobs['release']['needs']) == {'ci', 'security'}
    # A job condition without a status function retains Actions' implicit success().
    assert jobs['release']['if'] == "github.repository == 'Zmmfly/sub2api'"
    for job, filename, checks in [('ci', 'backend-ci.yml', ci),
                                  ('security', 'security-scan.yml', security)]:
        gate = jobs[job]
        assert gate['uses'] == './.github/workflows/' + filename
        assert gate['with']['source_sha'] == SOURCE
        assert 'if' not in gate and 'continue-on-error' not in gate
        assert gate['permissions'] == {'contents': 'read'}
        assert checks['on']['workflow_call']['inputs']['source_sha'] == {
            'description': 'Exact commit validated for the calling release',
            'type': 'string', 'required': 'true'}
        assert 'push' not in checks['on']
        assert checks['on']['pull_request']['branches'] == ['main']
        for check in checks['jobs'].values():
            assert 'if' not in check and 'continue-on-error' not in check
            checkout, verify = check['steps'][:2]
            assert checkout['uses'] == 'actions/checkout@v6'
            assert checkout['with']['ref'] == CHECK_SOURCE
            assert checkout['with']['persist-credentials'] == 'false'
            assert verify['env']['SOURCE_SHA'] == CHECK_SOURCE
            assert '[[ "$SOURCE_SHA" =~ ^[0-9a-f]{40}$ ]]' in verify['run']
            assert 'test "$(git rev-parse HEAD)" = "$SOURCE_SHA"' in verify['run']
            for step in check['steps']:
                assert 'continue-on-error' not in step
    assert security['on']['schedule'] == [{'cron': '0 3 * * 1'}]


class ForkWorkflowTest(unittest.TestCase):
    def setUp(self):
        self.build = read('fork-build.yml')
        self.ci = read('backend-ci.yml')
        self.security = read('security-scan.yml')

    def test_release_is_gated_by_same_commit_ci_and_security(self):
        validate_gate(self.build, self.ci, self.security)

    def test_both_entry_points_use_gated_publisher(self):
        for filename, job, sha in [
            ('fork-latest.yml', 'release', '${{ needs.resolve.outputs.sha }}'),
            ('fork-sync.yml', 'build', '${{ needs.sync.outputs.sha }}'),
        ]:
            workflow = read(filename)
            self.assertEqual(workflow['jobs'][job]['uses'], './.github/workflows/fork-build.yml')
            self.assertEqual(workflow['jobs'][job]['with']['source_sha'], sha)
        self.assertEqual(read('fork-latest.yml')['on']['push']['branches'], ['main'])
        self.assertEqual(read('fork-sync.yml')['on']['schedule'], [{'cron': '17 */12 * * *'}])

    def test_checks_retain_required_coverage(self):
        self.assertEqual(set(self.ci['jobs']), {'shell', 'test', 'frontend', 'golangci-lint', 'release-helpers'})
        self.assertEqual(set(self.security['jobs']), {'backend-security', 'frontend-security'})
        source = (WORKFLOWS / 'backend-ci.yml').read_text()
        for command in ['make test-unit', 'make test-integration', 'make test-frontend', "-p 'test_fork*.py'"]:
            self.assertIn(command, source)
        self.assertIn('govulncheck ./...', (WORKFLOWS / 'security-scan.yml').read_text())
        self.assertIn('check_pnpm_audit_exceptions.py', (WORKFLOWS / 'security-scan.yml').read_text())

    def test_only_required_workflows_remain(self):
        self.assertEqual({p.name for p in WORKFLOWS.glob('*.yml')}, {
            'backend-ci.yml', 'security-scan.yml', 'fork-build.yml', 'fork-latest.yml', 'fork-sync.yml'})
        self.assertTrue((ROOT / '.github/release-tools/release_matrix.py').is_file())

    def test_contract_rejects_bypassing_or_mismatching_checks(self):
        mutations = [
            lambda b, c, s: b['jobs']['release'].update(needs=['ci']),
            lambda b, c, s: b['jobs']['release'].update({'if': '${{ always() }}'}),
            lambda b, c, s: b['jobs']['ci'].update({'continue-on-error': 'true'}),
            lambda b, c, s: b['jobs']['security']['with'].update(source_sha='main'),
            lambda b, c, s: c['jobs']['test']['steps'][0]['with'].update(ref='main'),
            lambda b, c, s: s['jobs']['backend-security'].update({'if': 'false'}),
            lambda b, c, s: c['on'].update(push={}),
        ]
        for mutate in mutations:
            with self.subTest(mutation=mutate):
                args = copy.deepcopy([self.build, self.ci, self.security])
                mutate(*args)
                with self.assertRaises(AssertionError):
                    validate_gate(*args)


if __name__ == '__main__':
    unittest.main()
