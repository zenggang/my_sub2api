#!/usr/bin/env python
"""Read-only package and live migration validation; Python 2.7/3 compatible."""
from __future__ import print_function
import hashlib
import json
import os
import re
import subprocess
import sys


def validate_package(directory):
    with open(os.path.join(directory, 'manifest.json')) as f:
        m = json.load(f)
    assert m['format'] == 'fork-release-v1' and m['status'] == 'BUILT_TESTED'
    assert re.match(r'^[a-f0-9]{40}$', m['source_commit'])
    assert re.match(r'^\d+\.\d+\.\d+-fork\.[a-f0-9]{12}$', m['build_version'])
    assert m['build_version'].endswith(m['source_commit'][:12]) and m['mode'] == 'http'
    assert set(m['files']) == set(['sub2api','source.tar.gz','build.log','tests.jsonl'])
    for name, expected in m['files'].items():
        path = os.path.join(directory, name)
        assert not os.path.islink(path)
        with open(path, 'rb') as f:
            assert hashlib.sha256(f.read()).hexdigest() == expected, 'checksum mismatch: '+name
    with open(os.path.join(directory, 'tests.jsonl')) as f:
        events = [json.loads(line) for line in f if line.strip()]
    assert not any(e.get('Action') == 'fail' for e in events), 'failed tests'
    passed = set(e.get('Test') for e in events if e.get('Action') == 'pass')
    assert m['required_tests'] and all(n in passed for n in m['required_tests'])
    assert len(passed) == m['tests_passed']
    assert any(n and n.startswith('TestMappedGPT55') for n in passed)
    return m


def validate_migrations(m, applied):
    for name, rule in m['migrations'].items():
        assert name in applied, 'pending migration: '+name
        current = rule['checksum']
        accepted = rule['accepted']
        assert applied[name] == current or (current in accepted and applied[name] in accepted), 'migration checksum mismatch: '+name


if __name__ == '__main__':
    manifest = validate_package(sys.argv[1])
    raw = subprocess.check_output(['sudo','-u','postgres','/opt/postgresql/bin/psql','-h','/var/run/postgresql','-d','sub2api','-X','-At','-v','ON_ERROR_STOP=1','-c','SELECT filename,checksum FROM schema_migrations ORDER BY filename']).decode('utf-8')
    validate_migrations(manifest, dict(line.split('|',1) for line in raw.splitlines()))
    print(manifest['files']['sub2api'])
