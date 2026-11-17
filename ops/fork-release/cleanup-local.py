#!/usr/bin/env python3
"""Remove one accepted release's local artifacts while retaining small records."""
import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import shutil
import subprocess


def cleanup(provenance_path, deployment_path, apply=False):
    provenance_path = Path(provenance_path).resolve()
    deployment_path = Path(deployment_path).resolve()
    provenance = json.loads(provenance_path.read_text())
    deployment = json.loads(deployment_path.read_text())
    commit = provenance["source_commit"]
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("invalid source commit")
    if (deployment["status"] != "deployed_and_standard_smoke_verified"
            or deployment["source_commit"] != commit
            or deployment["binary_sha256"] != provenance["binary_sha256"]):
        raise ValueError("deployment acceptance must match this build")

    package = Path(provenance["package_directory"]).absolute()
    source = Path(provenance["build_clone"]).absolute()
    delivery = Path(provenance["delivery_directory"]).absolute()
    version = provenance["build_version"]
    release_name = version.split("-fork.", 1)[0] + "-" + commit[:12]
    cache = package.parent.parent
    if (package.parent.name != "releases"
            or not package.name.startswith("fork-" + commit[:12] + "-")
            or source != cache / "build-sources" / release_name
            or delivery.parent.name != "output"
            or delivery.name != "sub2api-" + release_name):
        raise ValueError("unexpected local artifact path")
    targets = [package, source, delivery]
    record = provenance_path.parent / "local-cleanup"
    for target in targets:
        if target.resolve() != target or record == target or target in record.parents:
            raise ValueError("cleanup paths must not be symlinks or contain records")

    # The temporary clone may contain work added after packaging. Do not lose it.
    if source.exists():
        head = subprocess.check_output(
            ["git", "-C", str(source), "rev-parse", "HEAD"], text=True).strip()
        dirty = subprocess.check_output(
            ["git", "-C", str(source), "status", "--porcelain"], text=True).strip()
        if head != commit or dirty:
            raise ValueError("temporary source clone changed after packaging")
    repository = Path(provenance["source_repository"])
    subprocess.run(["git", "-C", str(repository), "cat-file", "-e", commit + "^{commit}"],
                   check=True, stdout=subprocess.DEVNULL)
    if package.exists():
        manifest = json.loads((package / "manifest.json").read_text())
        if (manifest["source_commit"] != commit
                or manifest["files"]["sub2api"] != provenance["binary_sha256"]):
            raise ValueError("package manifest does not match accepted build")
    elif apply and not (record / "manifest.json").is_file():
        raise ValueError("package is missing and its manifest has not been preserved")

    result = {
        "status": "preview", "source_commit": commit,
        "targets": [str(p) for p in targets], "preserved_records": str(record),
    }
    if not apply:
        return result
    record.mkdir(parents=True, exist_ok=True)
    for origin in [provenance_path, deployment_path] + [
            package / name for name in
            ("manifest.json", "SHA256SUMS", "build.log", "tests.jsonl")]:
        if origin.is_file():
            shutil.copy2(origin, record / origin.name)
    sizes = {}
    for target in targets:
        if target.exists():
            kib = int(subprocess.check_output(["du", "-sk", str(target)], text=True).split()[0])
            sizes[str(target)] = kib * 1024
            shutil.rmtree(target)
        if target.exists():
            raise RuntimeError("cleanup did not remove " + str(target))
    result.update(status="cleaned", allocated_bytes_removed=sizes,
                  completed_at=datetime.now(timezone.utc).isoformat())
    (record / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--provenance", required=True)
    parser.add_argument("--deployment-result", required=True)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    print(json.dumps(cleanup(args.provenance, args.deployment_result, args.apply), indent=2))
