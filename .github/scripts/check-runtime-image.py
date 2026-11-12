# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

"""Verify release runtime contracts inside the exact platform image."""

import argparse
import hashlib
import importlib.metadata
import importlib.util
import json
import os
import platform
from pathlib import Path


def verify_legal_files(directory: Path) -> int:
    for name in ("LICENSE.TXT", "NOTICE", "PRIVACY"):
        if not (directory / name).is_file() or not (directory / name).read_bytes():
            raise SystemExit(f"release runtime is missing legal file: {name}")
    inventory = json.loads((directory / "python" / "inventory.json").read_text(encoding="utf-8"))
    if inventory["schemaVersion"] != 1:
        raise SystemExit("unsupported Python legal inventory schema")
    notice = (directory / "python" / "NOTICE").read_bytes()
    if inventory["noticeSHA256"] != hashlib.sha256(notice).hexdigest():
        raise SystemExit("Python legal inventory does not match the shipped notice")
    installed = {(distribution.metadata["Name"], distribution.version)
                 for distribution in importlib.metadata.distributions()}
    recorded = {(component["component"], component["version"])
                for component in inventory["components"] if component["component"] != "Python interpreter"}
    if not any(component["component"] == "Python interpreter" and
               component["version"] == platform.python_version()
               for component in inventory["components"]):
        raise SystemExit("Python legal inventory omits the running interpreter")
    if installed != recorded:
        raise SystemExit("Python legal inventory does not match installed distributions")
    if inventory["unresolved"] != 0:
        raise SystemExit("Python legal inventory has unresolved components; release review is required")
    return len(recorded)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", choices=("native", "autogen", "ingester"), required=True)
    parser.add_argument("--architecture", choices=("amd64", "arm64"), required=True)
    arguments = parser.parse_args()
    if os.getuid() == 0:
        raise SystemExit("release runtime must run as its non-root image user")
    machines = {"amd64": "x86_64", "arm64": "aarch64"}
    if platform.machine() != machines[arguments.architecture]:
        raise SystemExit("runtime architecture differs from the selected image platform")
    if importlib.util.find_spec("pip") is not None:
        raise SystemExit("release runtime must not contain the build-only pip installer")
    result = {
        "profile": arguments.profile,
        "architecture": arguments.architecture,
        "uid": os.getuid(),
        "python": platform.python_version(),
        "pip": "absent",
        "legalComponents": verify_legal_files(Path("/usr/share/kognition")),
    }
    if arguments.profile == "autogen":
        for package, expected in (
            ("autogen-agentchat", "0.7.5"),
            ("autogen-ext", "0.7.5"),
            ("pillow", "13.0.0"),
        ):
            actual = importlib.metadata.version(package)
            if actual != expected:
                raise SystemExit(f"{package} version differs: expected {expected}, got {actual}")
            result[package] = actual
    elif arguments.profile == "native" and importlib.util.find_spec("autogen_agentchat") is not None:
        raise SystemExit("native runtime unexpectedly contains the optional AutoGen SDK")
    print(json.dumps(result))


if __name__ == "__main__":
    main()
