# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

import hashlib
import importlib.metadata
import io
import json
import platform
from pathlib import Path
import re
import subprocess
import sys
import tarfile

import pytest


TOOL = Path(__file__).resolve().parents[1] / "distribution_notices.py"
ROOT = TOOL.parent.parent
IMAGE_CHECKER = ROOT / ".github" / "scripts" / "check-runtime-image.py"
PRIVATE_RELEASE_DOCUMENTS = (
    "going-public.md", "release-assessment.md",
    "repository-publication-audit.md", "responsible-ai-readiness.md",
)
PRIVATE_DEMO_DOCUMENTS = (
    "docs/development-plans/tutorials/tutorial-video-script.md",
    "docs/development-plans/tutorials/tutorial-video-coverage.md",
    "tools/recording/README.md",
    "samples/trustworthiness/storyboard-v1.md",
)


def invoke(*arguments: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(TOOL), *arguments],
        capture_output=True, text=True, timeout=30,
    )


def npm_fixture(root: Path, *, legal: bool = True) -> Path:
    package = root / "node_modules" / "example"
    package.mkdir(parents=True)
    (root / "package.json").write_text('{"name":"fixture"}', encoding="utf-8")
    (root / "package-lock.json").write_text(json.dumps({
        "lockfileVersion": 3,
        "packages": {"": {"name": "fixture"}, "node_modules/example": {
            "version": "1.0.0", "license": "MIT",
        }},
    }), encoding="utf-8")
    (package / "package.json").write_text(
        '{"name":"example","version":"1.0.0"}', encoding="utf-8",
    )
    if legal:
        (package / "LICENSE").write_text("Fixture legal text\n", encoding="utf-8")
        nested = package / "bundled"
        nested.mkdir()
        (nested / "NOTICE").write_text("Nested attribution\n", encoding="utf-8")
    return package


def test_npm_retains_original_nested_text_and_reproduces_bytes(tmp_path: Path) -> None:
    package = npm_fixture(tmp_path)
    output = tmp_path / "output"
    result = invoke("npm", "--root", str(tmp_path), "--output", str(output), "--strict")
    assert result.returncode == 0, result.stderr
    notice = (output / "NOTICE").read_bytes()
    inventory = (output / "inventory.json").read_bytes()
    record = json.loads(inventory)
    assert record["noticeSHA256"] == hashlib.sha256(notice).hexdigest()
    assert record["components"][0]["license"] == "MIT"
    assert (package / "LICENSE").read_bytes() in notice
    assert (package / "bundled" / "NOTICE").read_bytes() in notice
    assert len(record["components"][0]["texts"]) == 2
    assert invoke("npm", "--root", str(tmp_path), "--output", str(output)).returncode == 0
    assert (output / "NOTICE").read_bytes() == notice
    assert (output / "inventory.json").read_bytes() == inventory


@pytest.mark.parametrize("failure", ["missing-text", "wrong-version", "missing-package"])
def test_npm_fails_explicitly_for_incomplete_installed_components(tmp_path: Path, failure: str) -> None:
    package = npm_fixture(tmp_path, legal=failure != "missing-text")
    if failure == "wrong-version":
        (package / "package.json").write_text('{"name":"example","version":"2.0.0"}', encoding="utf-8")
    if failure == "missing-package":
        (package / "package.json").unlink()
    result = invoke("npm", "--root", str(tmp_path), "--output", str(tmp_path / "output"), "--strict")
    assert result.returncode != 0
    assert result.stderr


def test_absent_optional_package_is_explicit_not_a_claim_of_legal_clearance(tmp_path: Path) -> None:
    npm_fixture(tmp_path)
    lock_path = tmp_path / "package-lock.json"
    lock = json.loads(lock_path.read_text(encoding="utf-8"))
    lock["packages"]["node_modules/optional"] = {
        "version": "1.0.0", "license": "MIT", "optional": True,
    }
    lock_path.write_text(json.dumps(lock), encoding="utf-8")
    output = tmp_path / "output"
    result = invoke("npm", "--root", str(tmp_path), "--output", str(output), "--strict")
    assert result.returncode == 0, result.stderr
    inventory = json.loads((output / "inventory.json").read_text(encoding="utf-8"))
    assert inventory["completeDistributionClearance"] is False
    assert "Not installed" in inventory["components"][1]["status"]


def test_python_uses_recorded_legal_files_and_reports_missing_texts(tmp_path: Path) -> None:
    site = tmp_path / "site"
    metadata = site / "fixture-1.0.dist-info"
    metadata.mkdir(parents=True)
    (metadata / "METADATA").write_text(
        "Metadata-Version: 2.4\nName: fixture\nVersion: 1.0\nLicense-Expression: MIT\n",
        encoding="utf-8",
    )
    (metadata / "LICENSE").write_text("Python fixture legal text\n", encoding="utf-8")
    (metadata / "RECORD").write_text(
        "fixture-1.0.dist-info/METADATA,,\nfixture-1.0.dist-info/LICENSE,,\n"
        "fixture/__pycache__/license.cpython-311.pyc,,\n",
        encoding="utf-8",
    )
    bytecode = site / "fixture" / "__pycache__"
    bytecode.mkdir(parents=True)
    (bytecode / "license.cpython-311.pyc").write_bytes(b"\xa7\r\r\n")
    output = tmp_path / "output"
    arguments = ("python", "--site-packages", str(site), "--output", str(output), "--strict")
    result = invoke(*arguments)
    assert result.returncode == 0, result.stderr
    assert "Python fixture legal text" in (output / "NOTICE").read_text(encoding="utf-8")
    (metadata / "RECORD").write_text("fixture-1.0.dist-info/METADATA,,\n", encoding="utf-8")
    result = invoke(*arguments)
    assert result.returncode == 1
    assert "legal-text or license verification" in result.stderr


def test_chart_preparation_preserves_root_legal_bytes_and_detects_drift(tmp_path: Path) -> None:
    charts = (
        "deploy/helm/kognition", "deploy/helm/kognition-knowledge-azuresearch",
        "deploy/helm/kognition-knowledge-pgvector", "samples/helm-governance",
        "samples/helm-multi-agent", "samples/helm-umbrella",
    )
    for name in ("LICENSE.TXT", "NOTICE", "PRIVACY"):
        (tmp_path / name).write_bytes(f"Fixture {name}\n".encode())
    for chart in charts:
        directory = tmp_path / chart
        directory.mkdir(parents=True)
        (directory / "Chart.yaml").write_text("name: fixture\n", encoding="utf-8")
    arguments = ("charts", "--root", str(tmp_path))
    assert invoke(*arguments, "--check").returncode != 0
    result = invoke(*arguments)
    assert result.returncode == 0, result.stderr
    assert invoke(*arguments, "--check").returncode == 0
    for chart in charts:
        for name in ("LICENSE.TXT", "NOTICE", "PRIVACY"):
            assert (tmp_path / chart / name).read_bytes() == (tmp_path / name).read_bytes()
    (tmp_path / "NOTICE").write_text("Changed legal text\n", encoding="utf-8")
    assert invoke(*arguments, "--check").returncode != 0


def test_repository_uses_only_canonical_license_and_names_project() -> None:
    license_files = sorted(
        path.name for path in ROOT.iterdir()
        if path.is_file() and
        (path.name.casefold() == "license" or path.name.casefold().startswith("license."))
    )
    assert license_files == ["LICENSE.TXT"]
    canonical = (ROOT / "LICENSE.TXT").read_bytes()
    assert canonical.index(b"Kognition") < canonical.index(b"Copyright (c) Microsoft Corporation.")


def test_publication_ignores_local_state_and_raw_acceptance_output() -> None:
    paths = [
        ".azure/deployment-plan.md", ".azure/local-samples/models.yaml",
        ".local/session.json", "mentoring/session-notes.md",
        ".vscode/PROJECT_BOOTSTRAP.md",
        ".github/instructions/postman-collections-pre-request.instructions.md",
        ".env.production", "playwright-report/index.html", "blob-report/report.zip",
        "logs/runtime.log", "runtime.pprof",
        ".demo-preparation/video-production/recording-notes.md",
        "docs/release-preparation/future-assessment.md",
        "docs/features/knowledge-provider-execution/sg07-deployed-acceptance.txt",
        "docs/features/knowledge-provider-execution/sg08-deployed-acceptance.txt",
        "docs/features/knowledge-provider-execution/sg08-deployed-cleanup-regression.txt",
        "docs/features/knowledge-provider-execution/sg08-provider-final-acceptance.txt",
        "docs/features/knowledge-provider-execution/sg08-scoped-lint.txt",
    ]
    paths.extend("docs/release-preparation/" + name for name in PRIVATE_RELEASE_DOCUMENTS)
    paths.extend(PRIVATE_DEMO_DOCUMENTS)
    result = subprocess.run(
        ["git", "check-ignore", "--no-index", "-z", "--stdin"], cwd=ROOT,
        input=("\0".join(paths) + "\0").encode(), capture_output=True, timeout=30,
    )
    assert result.returncode == 0, result.stderr
    assert set(result.stdout.decode().rstrip("\0").split("\0")) == set(paths)


def test_publication_retains_source_tests_legal_and_build_inputs() -> None:
    paths = [
        ".env.example", ".vscode/settings.json", "PROJECT.md", "BACKLOG.md",
        "LICENSE.TXT", "NOTICE", "PRIVACY", "package-lock.json",
        "third-party/npm/NOTICE", "third-party/npm/inventory.json",
        ".github/workflows/ci.yml", ".github/instructions/go.instructions.md",
        "proto/gen/kognition/v1alpha1/cpi.pb.go",
        "cmd/kognitionctl/cmd/studio_assets/resource-catalog.json",
        "samples/helm-umbrella/charts/kognition-0.1.0.tgz",
        "docs/development-plans/refactoring/repository-refactor-plan.md",
        "tools/recording/live-pilot.js", "tools/recording/live-pilot.test.js",
        "samples/trustworthiness/runbook.md",
        "samples/trustworthiness/publication_server.py",
        "samples/trustworthiness/test_publication_server.py",
        "e2e/trustworthiness-browser-rehearsal.js",
        "e2e/trustworthiness-storyboard-e2e.ps1",
        "e2e/trustworthiness-e2e.ps1",
        "e2e/workload-identity-acceptance.ps1",
        "e2e/fixtures/workload-identity-platform.yaml",
        "e2e/mint-model-session-capability.go",
        "e2e/kubernetes-runtime-tool-gateway-e2e.ps1",
    ]
    result = subprocess.run(
        ["git", "check-ignore", "--no-index", "-z", "--stdin"], cwd=ROOT,
        input=("\0".join(paths) + "\0").encode(), capture_output=True, timeout=30,
    )
    assert result.returncode == 1, result.stdout + result.stderr
    assert not result.stdout


def test_no_tracked_files_are_hidden_by_ignore_rules() -> None:
    result = subprocess.run(
        ["git", "ls-files", "--cached", "--ignored", "--exclude-standard"],
        cwd=ROOT, capture_output=True, text=True, timeout=30, check=True,
    )
    assert not result.stdout, "Tracked files still match ignore rules:\n" + result.stdout


def test_release_assessments_are_not_in_the_publishable_inventory() -> None:
    result = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=ROOT, capture_output=True, timeout=30, check=True,
    )
    published = set(result.stdout.decode().split("\0"))
    assert not any(path.startswith("docs/release-preparation/") for path in published)
    assert not published.intersection("docs/" + name for name in PRIVATE_RELEASE_DOCUMENTS)


def test_demo_preparation_documents_are_not_public_dependencies() -> None:
    result = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=ROOT, capture_output=True, timeout=30, check=True,
    )
    published = set(result.stdout.decode().split("\0"))
    assert not published.intersection(PRIVATE_DEMO_DOCUMENTS)
    for relative in published:
        path = ROOT / relative
        if not relative.endswith(".md") or not path.is_file():
            continue
        for target in re.findall(r"\]\(([^)]+)\)", path.read_text(encoding="utf-8-sig")):
            if "://" in target or target.startswith(("mailto:", "#")):
                continue
            destination = (path.parent / target.split("#", 1)[0].strip("<>")).resolve()
            if destination.is_relative_to(ROOT):
                assert destination.relative_to(ROOT).as_posix() not in PRIVATE_DEMO_DOCUMENTS, (
                    f"{relative}: {target}"
                )


def test_published_markdown_does_not_link_to_private_release_assessments() -> None:
    result = subprocess.run(
        ["git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=ROOT, capture_output=True, timeout=30, check=True,
    )
    for relative in set(result.stdout.decode().split("\0")):
        path = ROOT / relative
        if not relative.endswith(".md") or not path.is_file():
            continue
        for target in re.findall(r"\]\(([^)]+)\)", path.read_text(encoding="utf-8-sig")):
            if "://" in target or target.startswith(("mailto:", "#")):
                continue
            target = target.split("#", 1)[0]
            assert target.rsplit("/", 1)[-1] not in PRIVATE_RELEASE_DOCUMENTS, f"{relative}: {target}"
            assert "release-preparation/" not in target, f"{relative}: {target}"
    notice = (ROOT / "NOTICE").read_text(encoding="utf-8")
    assert "docs/going-public.md" not in notice
    assert "docs/release-preparation/" not in notice


def test_privacy_notice_and_documented_tracing_opt_out_are_shipped() -> None:
    notice = (ROOT / "PRIVACY").read_text(encoding="utf-8")
    assert "Your use of the software operates as your consent to these practices." in notice
    assert "KOGNITION_OTEL_TRACES_ENABLED=false" in notice
    assert "observability.openTelemetry.enabled=false" in notice
    readme = (ROOT / "README.md").read_text(encoding="utf-8")
    assert "https://opensource.microsoft.com/codeofconduct/" in readme
    assert "[PRIVACY](PRIVACY)" in readme
    assert "KOGNITION_OTEL_TRACES_ENABLED=false" in readme
    assert "observability.openTelemetry.enabled=false" in readme
    assert "trace export is disabled by default" in readme
    assert "### Data Collection Notice" not in readme
    assert "Data Collection. The software may collect" not in readme


def test_installation_state_is_excluded_from_source_and_image_contexts() -> None:
    gitignore = (ROOT / ".gitignore").read_text(encoding="utf-8").splitlines()
    dockerignore = (ROOT / ".dockerignore").read_text(encoding="utf-8").splitlines()
    assert "/.local/" in gitignore
    assert ".local/" in dockerignore
    assert ".azure/" in dockerignore


@pytest.mark.parametrize("archive", ["kognition-0.1.0.tgz", "kognition-governance-0.1.0.tgz"])
def test_bundled_chart_archives_retain_current_legal_files(archive: str) -> None:
    result = invoke("archives", str(ROOT / "samples" / "helm-umbrella" / "charts" / archive))
    assert result.returncode == 0, result.stderr


@pytest.mark.parametrize("failure", ["stale", "missing", "duplicate", "symlink"])
def test_archive_check_rejects_incomplete_legal_packaging(tmp_path: Path, failure: str) -> None:
    path = tmp_path / "fixture.tgz"
    with tarfile.open(path, "w:gz") as archive:
        for name in ("LICENSE.TXT", "NOTICE", "PRIVACY"):
            if failure == "missing" and name == "NOTICE":
                continue
            content = (ROOT / name).read_bytes()
            if failure == "stale" and name == "NOTICE":
                content = b"stale"
            member = tarfile.TarInfo(f"fixture/{name}")
            member.size = len(content)
            if failure == "symlink" and name == "NOTICE":
                member.type = tarfile.SYMTYPE
                member.linkname = "LICENSE.TXT"
                member.size = 0
                archive.addfile(member)
            else:
                archive.addfile(member, io.BytesIO(content))
            if failure == "duplicate" and name == "NOTICE":
                archive.addfile(member, io.BytesIO(content))
    result = invoke("archives", str(path))
    assert result.returncode != 0
    assert "legal file" in result.stderr


@pytest.mark.parametrize("signature", [b"MZ", b"\x7fELF", b"\0asm", b"!<arch>\n"])
def test_source_check_detects_untracked_extensionless_binaries(tmp_path: Path, signature: bytes) -> None:
    subprocess.run(["git", "init", "--quiet", str(tmp_path)], check=True)
    (tmp_path / "source.go").write_text("package fixture\n", encoding="utf-8")
    arguments = ("source", "--root", str(tmp_path))
    assert invoke(*arguments).returncode == 0
    (tmp_path / "compiled").write_bytes(signature + b"\0" * 8)
    result = invoke(*arguments)
    assert result.returncode != 0
    assert "Prebuilt code" in result.stderr


def test_source_check_ignores_local_build_outputs(tmp_path: Path) -> None:
    subprocess.run(["git", "init", "--quiet", str(tmp_path)], check=True)
    (tmp_path / ".gitignore").write_text("*.exe\n", encoding="utf-8")
    (tmp_path / "local.exe").write_bytes(b"MZ" + b"\0" * 8)
    result = invoke("source", "--root", str(tmp_path))
    assert result.returncode == 0, result.stderr


@pytest.mark.parametrize("failure", ["none", "missing-notice", "stale-hash", "package-mismatch", "unresolved", "missing-interpreter"])
def test_image_legal_verification_checks_actual_installed_components(tmp_path: Path, failure: str) -> None:
    for name in ("LICENSE.TXT", "NOTICE", "PRIVACY"):
        (tmp_path / name).write_bytes(b"Fixture legal text\n")
    directory = tmp_path / "python"
    directory.mkdir()
    notice = b"Fixture Python attribution\n"
    (directory / "NOTICE").write_bytes(notice)
    components = [
        {"component": distribution.metadata["Name"], "version": distribution.version}
        for distribution in importlib.metadata.distributions()
    ]
    components.append({"component": "Python interpreter", "version": platform.python_version()})
    inventory = {
        "schemaVersion": 1, "noticeSHA256": hashlib.sha256(notice).hexdigest(),
        "components": components, "unresolved": 0,
    }
    if failure == "missing-notice":
        (tmp_path / "NOTICE").unlink()
    elif failure == "stale-hash":
        inventory["noticeSHA256"] = "0" * 64
    elif failure == "package-mismatch":
        components[0]["version"] = "fixture-nonexistent-version"
    elif failure == "unresolved":
        inventory["unresolved"] = 1
    elif failure == "missing-interpreter":
        components.pop()
    (directory / "inventory.json").write_text(json.dumps(inventory), encoding="utf-8")
    result = subprocess.run(
        [sys.executable, "-c",
         "import runpy,sys; from pathlib import Path; "
         "runpy.run_path(sys.argv[1])['verify_legal_files'](Path(sys.argv[2]))",
         str(IMAGE_CHECKER), str(tmp_path)],
        capture_output=True, text=True, timeout=30,
    )
    if failure == "none":
        assert result.returncode == 0, result.stderr
    else:
        assert result.returncode != 0
        assert result.stderr
