"""Candidate archives contain only explicitly reviewed runtime files."""

import stat
import zipfile

import pytest

from scripts.package import BINARY_NAME, RUNTIME_FILES, collect_archive, write_archive


@pytest.fixture
def candidate(tmp_path):
    integration = tmp_path / "custom_components" / "intelbras_allo"
    for name in RUNTIME_FILES:
        path = integration / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("synthetic runtime")
    for name in ("LICENSE", "THIRD_PARTY_NOTICES.md"):
        (tmp_path / name).write_text("synthetic notice")
    binary = tmp_path / "helper"
    header = bytearray(64)
    header[:7] = b"\x7fELF\x02\x01\x01"
    header[16:20] = b"\x02\x00\x3e\x00"
    binary.write_bytes(header)
    return tmp_path, integration, binary


def test_archive_is_allowlisted_and_reproducible(candidate):
    root, integration, binary = candidate
    for name in ("private.key", "captured.pem", "test_runtime.py", "frontend/token.json"):
        (integration / name).write_text("must never ship")
    entries = collect_archive(root, binary)
    entries["licenses/synthetic/LICENSE"] = b"dependency notice"
    first = root / "first.zip"
    second = root / "second.zip"
    write_archive(first, entries)
    binary.chmod(0o600)
    write_archive(
        second, collect_archive(root, binary) | {"licenses/synthetic/LICENSE": b"dependency notice"}
    )
    assert first.read_bytes() == second.read_bytes()
    with zipfile.ZipFile(first) as archive:
        assert archive.namelist() == sorted(
            (
                *RUNTIME_FILES,
                BINARY_NAME,
                "LICENSE",
                "THIRD_PARTY_NOTICES.md",
                "licenses/synthetic/LICENSE",
            )
        )
        assert archive.read(BINARY_NAME) == binary.read_bytes()
        assert stat.S_IMODE(archive.getinfo(BINARY_NAME).external_attr >> 16) == 0o755
        assert all(info.date_time == (1980, 1, 1, 0, 0, 0) for info in archive.infolist())
        assert all(not name.startswith("custom_components/") for name in archive.namelist())


def test_missing_frontend_stops_packaging(candidate):
    root, integration, binary = candidate
    (integration / "frontend/allo-intercom-card.js").unlink()
    with pytest.raises(ValueError, match="Missing runtime file: frontend/allo-intercom-card.js"):
        collect_archive(root, binary)


def test_symlink_runtime_is_rejected(candidate):
    root, integration, binary = candidate
    target = integration / "strings.json"
    target.unlink()
    target.symlink_to(binary)
    with pytest.raises(ValueError, match="regular file"):
        collect_archive(root, binary)


def test_wrong_binary_architecture_is_rejected(candidate):
    root, _, binary = candidate
    binary.write_bytes(b"not an amd64 ELF")
    with pytest.raises(ValueError, match="Linux amd64 ELF"):
        collect_archive(root, binary)


def test_dependency_notices_carry_transitive_and_go_licenses(tmp_path, monkeypatch):
    from scripts.package import dependency_notices

    module = tmp_path / "module"
    (module / "nested").mkdir(parents=True)
    (module / "LICENSE").write_text("module copyright")
    (module / "nested" / "NOTICE.txt").write_text("nested copyright")
    (module / "credentials.json").write_text("excluded")
    goroot = tmp_path / "go"
    goroot.mkdir()
    (goroot / "LICENSE").write_text("Go copyright")
    (goroot / "PATENTS").write_text("Go patents")
    outputs = iter([f"\nexample.org/module@v1.0.0|{module}\n", str(goroot)])
    monkeypatch.setattr("subprocess.check_output", lambda *args, **kwargs: next(outputs))
    assert dependency_notices(tmp_path, {}) == {
        "licenses/example.org/module@v1.0.0/LICENSE": b"module copyright",
        "licenses/example.org/module@v1.0.0/nested/NOTICE.txt": b"nested copyright",
        "licenses/go-1.26.3/LICENSE": b"Go copyright",
        "licenses/go-1.26.3/PATENTS": b"Go patents",
    }
