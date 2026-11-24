#!/usr/bin/env python3
"""Describe the exact tested Docker archive for governed delivery."""

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess


def command(*args):
    return subprocess.check_output(args, text=True).strip()


def main():
    root = Path("target/governed-image")
    archive = root / "image.tar.gz"
    reviewed = os.environ["OUF_REVIEWED_SOURCE_COMMIT"]
    tested = command("git", "rev-parse", "HEAD")
    if not all(re.fullmatch("[a-f0-9]{40}", sha) for sha in (reviewed, tested)):
        raise SystemExit("invalid source commit")
    tag = "ouf-vertical-mock:" + os.environ["GITHUB_SHA"]
    label = command("docker", "image", "inspect", tag, "--format",
                    '{{index .Config.Labels "org.opencontainers.image.revision"}}')
    if label != tested:
        raise SystemExit("image revision differs from tested commit")
    digest = hashlib.sha256()
    with archive.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    checksum = digest.hexdigest()
    manifest = {
        "schema": "ouf.vertical-mock-tested-image.v1",
        "contract": "ouf.vertical-mock.v1",
        "journalFormat": "ouf.vertical-mock-journal.v1",
        "repository": os.environ["GITHUB_REPOSITORY"],
        "reviewedSourceCommit": reviewed,
        "testedCommit": tested,
        "testedTree": command("git", "rev-parse", "HEAD^{tree}"),
        "workflowRunId": os.environ["GITHUB_RUN_ID"],
        "workflowRunAttempt": os.environ["GITHUB_RUN_ATTEMPT"],
        "imageTag": tag,
        "imageId": command("docker", "image", "inspect", tag, "--format", "{{.Id}}"),
        "imageRevisionLabel": label,
        "archiveName": archive.name,
        "archiveBytes": archive.stat().st_size,
        "archiveSha256": checksum,
        "qualifiedInCiOnly": True,
        "targetApplyAuthorized": False,
        "humanBudgetGranted": False,
        "releaseAcceptance": False,
    }
    (root / "manifest.json").write_text(
        json.dumps(manifest, sort_keys=True, separators=(",", ":")) + "\n", encoding="utf-8")
    (root / "SHA256SUMS").write_text(f"{checksum}  {archive.name}\n", encoding="ascii")


if __name__ == "__main__":
    main()
