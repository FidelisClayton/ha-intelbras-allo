#!/usr/bin/env python3
"""Build a Linux amd64 HACS candidate; never publish a release."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import zipfile

GO_VERSION = "go1.26.3"
BINARY_NAME = "bin/allo-ha-linux-amd64"
RUNTIME_FILES = (
    "__init__.py",
    "camera.py",
    "config_flow.py",
    "const.py",
    "diagnostics.py",
    "entity.py",
    "event.py",
    "helper.py",
    "frontend_assets.py",
    "websocket.py",
    "manifest.json",
    "strings.json",
    "translations/en.json",
    "frontend/allo-intercom-card.js",
)


def read_regular(path):
    """Reject links instead of following them into unreviewed private files."""
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError(f"Expected regular file: {path.name}")
    return path.read_bytes()


def collect_archive(root, binary):
    """Collect the explicit integration-root allowlist, never a directory glob."""
    entries = {}
    integration = root / "custom_components" / "intelbras_allo"
    for name in RUNTIME_FILES:
        path = integration / name
        # Check parents too: an allowed file must not traverse a directory symlink.
        if any(parent.is_symlink() for parent in (path, *path.parents)):
            raise ValueError(f"Expected regular file: {name}")
        try:
            entries[name] = read_regular(path)
        except FileNotFoundError:
            raise ValueError(f"Missing runtime file: {name}") from None
    helper = read_regular(binary)
    if (
        len(helper) < 64
        or helper[:7] != b"\x7fELF\x02\x01\x01"
        or helper[16:18] not in (b"\x02\x00", b"\x03\x00")
        or helper[18:20] != b"\x3e\x00"
    ):
        raise ValueError("Helper must be a Linux amd64 ELF binary")
    entries[BINARY_NAME] = helper
    for name in ("LICENSE", "THIRD_PARTY_NOTICES.md"):
        entries[name] = read_regular(root / name)
    return entries


def dependency_notices(root, env):
    """Carry upstream license files for modules actually linked by this command."""
    modules = subprocess.check_output(
        [
            "go",
            "list",
            "-mod=readonly",
            "-deps",
            "-f",
            "{{if .Module}}{{if not .Module.Main}}"
            "{{.Module.Path}}@{{.Module.Version}}|{{.Module.Dir}}{{end}}{{end}}",
            "./cmd/allo-ha",
        ],
        cwd=root,
        env=env,
        text=True,
    )
    entries = {}
    for module in sorted(set(modules.splitlines()) - {""}):
        name, directory = module.split("|", 1)
        found = False
        for path in sorted(Path(directory).rglob("*")):
            if path.name.upper().split(".")[0] not in (
                "LICENSE",
                "LICENCE",
                "COPYING",
                "COPYRIGHT",
                "NOTICE",
                "PATENTS",
            ):
                continue
            if path.is_dir():
                continue
            entries[f"licenses/{name}/{path.relative_to(directory).as_posix()}"] = read_regular(
                path
            )
            found = True
        if not found:
            raise ValueError(f"Missing dependency license: {name}")
    goroot = Path(
        subprocess.check_output(
            ["go", "env", "GOROOT"],
            cwd=root,
            env=env,
            text=True,
        ).strip()
    )
    for name in ("LICENSE", "PATENTS"):
        entries[f"licenses/go-{GO_VERSION[2:]}/{name}"] = read_regular(goroot / name)
    return entries


def write_archive(destination, entries):
    """Use fixed order, timestamps, modes and stored bytes for reproducible ZIPs."""
    with zipfile.ZipFile(destination, "w", compression=zipfile.ZIP_STORED) as archive:
        for name, contents in sorted(entries.items()):
            info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
            info.create_system = 3
            mode = 0o755 if name == BINARY_NAME else 0o644
            info.external_attr = (stat.S_IFREG | mode) << 16
            archive.writestr(info, contents)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True, help="candidate output directory")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    env = os.environ | {
        "CGO_ENABLED": "0",
        "GOOS": "linux",
        "GOARCH": "amd64",
        "GOAMD64": "v1",
        "GOENV": "off",
        "GOFLAGS": "",
        "GOTOOLCHAIN": "local",
        "GOWORK": "off",
        "GOEXPERIMENT": "",
        "GOFIPS140": "off",
    }
    version = subprocess.check_output(["go", "env", "GOVERSION"], env=env, text=True).strip()
    if version != GO_VERSION:
        parser.error(f"Use pinned Go {GO_VERSION[2:]}; found {version}")
    hacs = json.loads((root / "hacs.json").read_text())
    if hacs.get("zip_release") is not True or hacs.get("filename") != "intelbras_allo.zip":
        parser.error("hacs.json must declare zip_release and intelbras_allo.zip")
    with tempfile.TemporaryDirectory(prefix="allo-candidate-") as temporary:
        binary = Path(temporary) / "allo-ha"
        subprocess.run(
            [
                "go",
                "build",
                "-mod=readonly",
                "-trimpath",
                "-buildvcs=false",
                "-ldflags=-s -w",
                "-o",
                str(binary),
                "./cmd/allo-ha",
            ],
            cwd=root,
            env=env,
            check=True,
        )
        entries = collect_archive(root, binary) | dependency_notices(root, env)
        args.output.mkdir(parents=True, exist_ok=True)
        destination = args.output / hacs["filename"]
        write_archive(destination, entries)
        checksum = hashlib.sha256(destination.read_bytes()).hexdigest()
        destination.with_suffix(".zip.sha256").write_text(f"{checksum}  {destination.name}\n")
        print(f"Candidate only: {destination} (sha256 {checksum})")


if __name__ == "__main__":
    main()
