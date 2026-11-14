# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Prepare scoped distribution notices without downloading dependencies."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import importlib.metadata
import json
from pathlib import Path
import re
import sys
import sysconfig
import tarfile
import subprocess


LEGAL_NAME = re.compile(r"^(licen[cs]e|copying|copyright|notice|authors)([._-].*)?$", re.I)
CHARTS = (
    "deploy/helm/kognition",
    "deploy/helm/kognition-knowledge-azuresearch",
    "deploy/helm/kognition-knowledge-pgvector",
    "samples/helm-governance",
    "samples/helm-multi-agent",
    "samples/helm-umbrella",
)
LEGAL_FILES = ("LICENSE.TXT", "NOTICE", "PRIVACY")
SOURCE_SUFFIXES = {".py", ".pyc", ".pyo", ".go", ".js", ".ts", ".map", ".json", ".so", ".dll"}


def is_legal_file(path: Path) -> bool:
    return bool(LEGAL_NAME.fullmatch(path.name)) and path.suffix.lower() not in SOURCE_SUFFIXES


@dataclass(frozen=True)
class LegalText:
    name: str
    content: bytes
    source: str | None = None


@dataclass(frozen=True)
class Component:
    name: str
    version: str
    license: str
    texts: list[LegalText]
    installed: bool = True


def digest(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def write_notices(
    output: Path, scope: str, components: list[Component], manifests: list[Path]
) -> int:
    components.sort(key=lambda component: (component.name.lower(), component.version))
    lines = [
        "Kognition Third-Party Notices",
        "",
        f"Scope: {scope}",
        "Original legal texts are retained below. This inventory does not grant",
        "permissions, establish license compatibility or certify a complete release.",
        "OS packages, native libraries and base-image contents require separate review.",
        "",
    ]
    records = []
    unresolved = 0
    for component in components:
        if not component.installed:
            status = "Not installed; optional dependency, legal texts not collected"
        elif not component.texts or component.license == "UNRESOLVED":
            status = "Unresolved; legal review required"
            unresolved += 1
        else:
            status = "Legal texts retained; distribution review pending"
        lines.extend((
            "=" * 80,
            f"Component: {component.name}",
            f"Version: {component.version}",
            f"Declared license: {component.license}",
            f"Status: {status}",
        ))
        texts = []
        for text in component.texts:
            lines.extend(("", f"--- Original {text.name} ---", text.content.decode("utf-8-sig"), ""))
            texts.append({"name": text.name, "sha256": digest(text.content),
                          **({"source": text.source} if text.source else {})})
        records.append({
            "component": component.name,
            "version": component.version,
            "license": component.license,
            "installed": component.installed,
            "status": status,
            "texts": texts,
        })
    notice = ("\n".join(lines) + "\n").encode("utf-8")
    inventory = {
        "schemaVersion": 1,
        "scope": scope,
        "completeDistributionClearance": False,
        "sourceManifests": {path.name: digest(path.read_bytes()) for path in manifests},
        "noticeSHA256": digest(notice),
        "unresolved": unresolved,
        "components": records,
    }
    output.mkdir(parents=True, exist_ok=True)
    (output / "NOTICE").write_bytes(notice)
    (output / "inventory.json").write_text(
        json.dumps(inventory, indent=2, ensure_ascii=True) + "\n", encoding="utf-8", newline="\n"
    )
    print(json.dumps({"components": len(records), "unresolved": unresolved, "scope": scope}))
    if unresolved:
        print(f"{unresolved} component(s) need legal-text or license verification; see inventory.json",
              file=sys.stderr)
        for component in components:
            if component.installed and (not component.texts or component.license == "UNRESOLVED"):
                reason = "missing legal text" if not component.texts else "unresolved declared license"
                print(f"Unresolved: {component.name}@{component.version} "
                      f"(declared license: {component.license}; {reason})", file=sys.stderr)
    return unresolved


def supplemental_npm_texts(
    manifest: Path, packages: dict
) -> dict[tuple[str, str], tuple[str, LegalText]]:
    data = json.loads(manifest.read_text(encoding="utf-8"))
    if data.get("schemaVersion") != 1 or not isinstance(data.get("components"), list):
        raise ValueError("Invalid supplemental npm notices manifest")
    known = {(relative.rsplit("node_modules/", 1)[-1], metadata["version"])
             for relative, metadata in packages.items() if relative}
    result = {}
    directory = manifest.parent.resolve()
    for entry in data["components"]:
        if not isinstance(entry, dict) or set(entry) != {
            "name", "version", "license", "file", "sha256", "source",
        } or not all(isinstance(value, str) and value for value in entry.values()):
            raise ValueError("Supplemental notice entries require exact component and source fields")
        key = (entry["name"], entry["version"])
        if key not in known or key in result:
            raise ValueError(f"Unknown or duplicate supplemental npm component: {key}")
        if not re.fullmatch(r"[a-f0-9]{64}", entry["sha256"]) or not re.fullmatch(
            r"https://raw\.githubusercontent\.com/[^/?#]+/[^/?#]+/[a-f0-9]{40}/[^?#]+",
            entry["source"],
        ):
            raise ValueError(f"Supplemental notice requires a checksum and immutable source: {key}")
        filename = directory / entry["file"]
        if not filename.resolve().is_relative_to(directory) or filename.is_symlink() or not filename.is_file():
            raise ValueError(f"Supplemental legal file is missing or escapes its manifest: {entry['file']}")
        content = filename.read_bytes()
        if not content.strip() or digest(content) != entry["sha256"]:
            raise ValueError(f"Supplemental legal file checksum mismatch: {entry['file']}")
        content.decode("utf-8-sig")
        result[key] = (entry["license"], LegalText(
            f"supplemental/{entry['file']}", content, source=entry["source"],
        ))
    return result


def npm_components(root: Path, supplemental_notices: Path | None = None) -> list[Component]:
    lock = json.loads((root / "package-lock.json").read_text(encoding="utf-8"))
    if lock.get("lockfileVersion") != 3 or not isinstance(lock.get("packages"), dict):
        raise ValueError("A version 3 npm lockfile with package records is required")
    supplements = supplemental_npm_texts(supplemental_notices, lock["packages"]) if supplemental_notices else {}
    components = []
    for relative, metadata in sorted(lock["packages"].items()):
        if not relative:
            continue
        directory = (root / relative).resolve()
        if not directory.is_relative_to(root.resolve()) or "node_modules" not in Path(relative).parts:
            raise ValueError(f"Package path is outside the dependency tree: {relative}")
        version = metadata["version"]
        license_name = metadata.get("license") or "UNRESOLVED"
        manifest = directory / "package.json"
        if not manifest.is_file():
            if not metadata.get("optional"):
                raise ValueError(f"Required npm package is not installed: {relative}; run npm ci")
            components.append(Component(relative, version, license_name, [], installed=False))
            continue
        installed = json.loads(manifest.read_text(encoding="utf-8"))
        if installed["version"] != version:
            raise ValueError(f"Installed npm package version differs from lockfile: {relative}")
        texts = []
        for path in sorted(directory.rglob("*")):
            if not path.is_file() or not is_legal_file(path):
                continue
            if "node_modules" in path.relative_to(directory).parts:
                continue
            if not path.resolve().is_relative_to(directory):
                raise ValueError(f"Legal file escapes its npm package: {relative}")
            texts.append(LegalText(path.relative_to(directory).as_posix(), path.read_bytes()))
        license_name = metadata.get("license") or installed.get("license") or "UNRESOLVED"
        supplement = supplements.get((installed["name"], version))
        if supplement:
            expected_license, legal_text = supplement
            if expected_license != license_name:
                raise ValueError(f"Supplemental license differs from the declared npm license: {relative}")
            if not texts:
                texts.append(legal_text)
        components.append(Component(installed["name"], version, license_name, texts))
    return components


def python_components(site_packages: Path | None) -> list[Component]:
    distributions = (
        importlib.metadata.distributions(path=[str(site_packages)])
        if site_packages else importlib.metadata.distributions()
    )
    components = []
    for distribution in distributions:
        name = distribution.metadata["Name"]
        if not name or not distribution.version:
            raise ValueError("Installed Python distribution has no name or version")
        license_name = (
            distribution.metadata.get("License-Expression")
            or distribution.metadata.get("License")
            or "; ".join(value for value in distribution.metadata.get_all("Classifier") or []
                         if value.startswith("License ::"))
            or "UNRESOLVED"
        )
        texts = []
        for record in sorted(distribution.files or [], key=str):
            if not is_legal_file(Path(str(record))) or "__pycache__" in record.parts:
                continue
            path = Path(distribution.locate_file(record))
            if not path.is_file():
                raise ValueError(f"Recorded legal file is missing for {name}: {record}")
            texts.append(LegalText(str(record).replace("\\", "/"), path.read_bytes()))
        components.append(Component(name, distribution.version, license_name, texts))
    if not components:
        raise ValueError("No installed Python distributions found")
    return components


def prepare_charts(root: Path, check: bool) -> None:
    for chart in CHARTS:
        directory = root / chart
        if not (directory / "Chart.yaml").is_file():
            raise ValueError(f"Chart is missing: {chart}")
        for name in LEGAL_FILES:
            content = (root / name).read_bytes()
            destination = directory / name
            if check:
                if not destination.is_file() or destination.read_bytes() != content:
                    raise ValueError(f"Missing or stale chart legal file: {chart}/{name}")
            else:
                destination.write_bytes(content)
    print(json.dumps({"charts": len(CHARTS), "mode": "check" if check else "prepare"}))


def check_archives(root: Path, archives: list[Path]) -> None:
    for path in archives:
        with tarfile.open(path, "r:gz") as archive:
            members = archive.getmembers()
            roots = {member.name.split("/")[0] for member in members}
            if len(roots) != 1:
                raise ValueError(f"Chart archive must have exactly one root: {path.name}")
            chart = roots.pop()
            for name in LEGAL_FILES:
                matches = [member for member in members if member.name == f"{chart}/{name}"]
                if len(matches) != 1 or not matches[0].isfile():
                    raise ValueError(f"Chart archive has missing or invalid legal file: {path.name}/{name}")
                content = archive.extractfile(matches[0])
                if content is None or content.read() != (root / name).read_bytes():
                    raise ValueError(f"Chart archive has stale legal file: {path.name}/{name}")
    print(json.dumps({"archives": len(archives), "legalFiles": "verified"}))


def check_source_binaries(root: Path) -> None:
    result = subprocess.run(
        ["git", "-c", "core.quotepath=false", "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        cwd=root, check=True, capture_output=True,
    )
    paths = sorted(set(path for path in result.stdout.decode("utf-8").split("\0") if path))
    failures = []
    for relative in paths:
        path = root / relative
        if not path.is_file():
            continue
        with path.open("rb") as source:
            signature = source.read(8)
        if (
            signature.startswith((b"MZ", b"\x7fELF", b"\0asm", b"!<arch>\n"))
            or signature[:4] in (b"\xfe\xed\xfa\xce", b"\xfe\xed\xfa\xcf",
                                 b"\xce\xfa\xed\xfe", b"\xcf\xfa\xed\xfe", b"\xca\xfe\xba\xbe")
            or path.suffix.lower() in {".exe", ".dll", ".so", ".dylib", ".o", ".a", ".lib", ".class", ".jar", ".wasm", ".pyc"}
        ):
            failures.append(relative)
    if failures:
        raise ValueError("Prebuilt code cannot be published in the source tree: " + ", ".join(failures))
    print(json.dumps({"sourceFiles": len(paths), "prebuiltBinaries": 0}))


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    npm = commands.add_parser("npm")
    npm.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    npm.add_argument("--supplemental-notices", type=Path)
    python = commands.add_parser("python")
    python.add_argument("--site-packages", type=Path)
    python.add_argument("--include-interpreter", action="store_true")
    for command in (npm, python):
        command.add_argument("--output", type=Path, required=True)
        command.add_argument("--strict", nargs="?", choices=("true", "false"), const="true", default="false")
    charts = commands.add_parser("charts")
    charts.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    charts.add_argument("--check", action="store_true")
    archives = commands.add_parser("archives")
    archives.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    archives.add_argument("paths", type=Path, nargs="+")
    source = commands.add_parser("source")
    source.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[1])
    arguments = parser.parse_args()
    if arguments.command == "charts":
        prepare_charts(arguments.root, arguments.check)
        return
    if arguments.command == "archives":
        check_archives(arguments.root, arguments.paths)
        return
    if arguments.command == "source":
        check_source_binaries(arguments.root)
        return
    if arguments.command == "npm":
        components = npm_components(arguments.root, arguments.supplemental_notices)
        manifests = [arguments.root / "package.json", arguments.root / "package-lock.json"]
        scope = "npm lockfile dependency graph; absent optional packages explicitly identified"
        if arguments.supplemental_notices:
            manifests.append(arguments.supplemental_notices)
            scope += "; missing packaged legal texts use checksum-pinned immutable upstream sources"
    else:
        components = python_components(arguments.site_packages)
        manifests = []
        scope = "Installed Python distributions in the invoking interpreter; exact image resolution only when run inside that image"
        if arguments.include_interpreter:
            candidates = (
                Path(sys.base_prefix) / "LICENSE.txt",
                Path(sysconfig.get_path("stdlib")) / "LICENSE.txt",
            )
            legal = next((path for path in candidates if path.is_file()), None)
            if legal is None:
                raise ValueError("Cannot locate the Python interpreter license")
            components.append(Component(
                "Python interpreter", sys.version.split()[0], "PSF-2.0 and bundled notices",
                [LegalText("LICENSE.txt", legal.read_bytes())],
            ))
    unresolved = write_notices(arguments.output, scope, components, manifests)
    if arguments.strict == "true" and unresolved:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
