import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest


SPEC = importlib.util.spec_from_file_location(
    "cleanup_local", Path(__file__).with_name("cleanup-local.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class CleanupLocalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.repo = self.root / "repository"
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        subprocess.run(["git", "-C", str(self.repo), "-c", "user.name=Test",
                        "-c", "user.email=test@example.com", "commit", "-qm",
                        "fixture", "--allow-empty"], check=True)
        self.commit = subprocess.check_output(
            ["git", "-C", str(self.repo), "rev-parse", "HEAD"], text=True).strip()
        name = "0.2.22-" + self.commit[:12]
        self.package = self.root / "cache/releases" / ("fork-" + self.commit[:12] + "-fixture")
        self.source = self.root / "cache/build-sources" / name
        self.delivery = self.root / "output" / ("sub2api-" + name)
        for p in (self.package, self.delivery, self.source.parent):
            p.mkdir(parents=True)
        subprocess.run(["git", "clone", "-q", str(self.repo), str(self.source)], check=True)
        (self.package / "manifest.json").write_text(json.dumps({
            "source_commit": self.commit, "files": {"sub2api": "binary-sha"}}))
        (self.package / "build.log").write_text("build succeeded\n")
        (self.package / "sub2api").write_bytes(b"binary")
        (self.delivery / "release-linux-amd64.tar.gz").write_bytes(b"archive")
        self.shared = self.root / "cache/go/toolchain"
        self.shared.parent.mkdir()
        self.shared.write_text("keep")
        self.run = self.repo / "docs-local/run"
        self.run.mkdir(parents=True)
        self.provenance = self.run / "build-provenance.json"
        self.deployment = self.run / "deployment-result.json"
        self.provenance.write_text(json.dumps({
            "source_commit": self.commit, "binary_sha256": "binary-sha",
            "package_directory": str(self.package), "build_clone": str(self.source),
            "delivery_directory": str(self.delivery), "source_repository": str(self.repo),
            "build_version": "0.2.22-fork." + self.commit[:12],
        }))
        self.deployment.write_text(json.dumps({
            "status": "deployed_and_standard_smoke_verified",
            "source_commit": self.commit, "binary_sha256": "binary-sha",
        }))

    def test_preview_then_cleanup_preserves_records_source_and_shared_tools(self):
        preview = MODULE.cleanup(self.provenance, self.deployment)
        self.assertEqual(preview["status"], "preview")
        self.assertTrue(self.package.exists())
        result = MODULE.cleanup(self.provenance, self.deployment, apply=True)
        self.assertEqual(result["status"], "cleaned")
        for p in (self.package, self.source, self.delivery):
            self.assertFalse(p.exists())
        record = self.run / "local-cleanup"
        self.assertEqual((record / "build.log").read_text(), "build succeeded\n")
        self.assertTrue((record / "manifest.json").is_file())
        self.assertTrue((record / "result.json").is_file())
        self.assertEqual(self.shared.read_text(), "keep")
        self.assertTrue((self.repo / ".git").is_dir())

    def test_unaccepted_deployment_keeps_artifacts(self):
        data = json.loads(self.deployment.read_text())
        data["status"] = "deployed_pending_acceptance"
        self.deployment.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "acceptance"):
            MODULE.cleanup(self.provenance, self.deployment, apply=True)
        self.assertTrue(self.package.exists())

    def test_mismatched_binary_keeps_artifacts(self):
        data = json.loads(self.deployment.read_text())
        data["binary_sha256"] = "another-binary"
        self.deployment.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "acceptance"):
            MODULE.cleanup(self.provenance, self.deployment, apply=True)
        self.assertTrue(self.package.exists())

    def test_unexpected_path_keeps_repository(self):
        data = json.loads(self.provenance.read_text())
        data["delivery_directory"] = str(self.repo)
        self.provenance.write_text(json.dumps(data))
        with self.assertRaisesRegex(ValueError, "artifact path"):
            MODULE.cleanup(self.provenance, self.deployment, apply=True)
        self.assertTrue((self.repo / ".git").is_dir())

    def test_changed_temporary_clone_is_not_deleted(self):
        (self.source / "new-work.txt").write_text("uncommitted work")
        with self.assertRaisesRegex(ValueError, "clone changed"):
            MODULE.cleanup(self.provenance, self.deployment, apply=True)
        self.assertTrue((self.source / "new-work.txt").exists())
        self.assertTrue(self.package.exists())
