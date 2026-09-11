import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from verify import validate_migrations, validate_package


class ReleaseGates(unittest.TestCase):
    def test_pending_and_unknown_migration_rejected(self):
        m = {'migrations': {'001.sql': {'checksum': 'current', 'accepted': ['current', 'historical']}}}
        validate_migrations(m, {'001.sql': 'historical'})
        for applied in ({}, {'001.sql':'unknown'}):
            with self.assertRaises(AssertionError):
                validate_migrations(m, applied)

    def test_compatibility_requires_both_checksums(self):
        with self.assertRaises(AssertionError):
            validate_migrations({'migrations': {'x.sql': {'checksum':'changed','accepted':['old']}}}, {'x.sql':'old'})

    def test_package_tamper_and_missing_test_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)
            for name in ['sub2api','source.tar.gz','build.log']:
                (p/name).write_bytes(b'test fixture')
            (p/'tests.jsonl').write_text(json.dumps({'Action':'pass','Test':'TestMappedGPT55Fixture'})+'\n')
            m = {'format':'fork-release-v1','status':'BUILT_TESTED','source_commit':'a'*40,'build_version':'0.2.4-fork.'+'a'*12,'mode':'http','required_tests':['TestMappedGPT55Fixture'],'tests_passed':1,'files':{n:hashlib.sha256((p/n).read_bytes()).hexdigest() for n in ['sub2api','source.tar.gz','build.log','tests.jsonl']}}
            (p/'manifest.json').write_text(json.dumps(m))
            validate_package(tmp)
            (p/'sub2api').write_bytes(b'changed')
            with self.assertRaises(AssertionError):
                validate_package(tmp)
            (p/'sub2api').write_bytes(b'test fixture')
            m['required_tests'] = ['TestMissing']
            (p/'manifest.json').write_text(json.dumps(m))
            with self.assertRaises(AssertionError):
                validate_package(tmp)


if __name__ == '__main__':
    unittest.main()
