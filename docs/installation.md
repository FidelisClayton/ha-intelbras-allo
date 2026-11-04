# Development candidate packaging

This project has no validated installable release yet. This document describes an offline development build, not installation instructions. No release, tag or GitHub release asset is created by the packager or validation workflow.

The first target is Linux amd64 with Home Assistant 2026.10.4 and Python 3.14.9. Other architectures have no packaged helper and are rejected by the adapter.

## Build a candidate

Use the pinned Go 1.26.3 toolchain on `PATH`, with the modules in `go.sum` available. From the repository root:

```sh
.venv/bin/python scripts/package.py --output /tmp/allo-candidate
cd /tmp/allo-candidate
sha256sum --check intelbras_allo.zip.sha256
```

The packager builds `./cmd/allo-ha` with `CGO_ENABLED=0`, `GOOS=linux`, `GOARCH=amd64`, `GOAMD64=v1`, `-mod=readonly`, `-trimpath`, `-buildvcs=false` and `-ldflags="-s -w"`. It requires the reviewed frontend asset and every runtime file in its explicit allowlist. Missing files, symlinks and a helper without the expected ELF64 amd64 header stop packaging.

`intelbras_allo.zip` contains the integration files directly at archive root: the allowlisted Python modules, `manifest.json`, `strings.json`, `translations/en.json`, `brand/icon.png`, `frontend/allo-intercom-card.js`, `bin/allo-ha-linux-amd64`, `LICENSE`, `THIRD_PARTY_NOTICES.md` and upstream license/notice files under `licenses/`. It does not contain a `custom_components/intelbras_allo/` prefix. `hacs.json` selects this filename with `zip_release: true`. The 256×256 transparent PNG icon is an original geometric intercom illustration; it contains no Intelbras logo or extracted application assets.

This layout follows the [official HACS ZIP-release contract](https://contributors.home-assistant.io/marketplace/guides/releases/) and [HACS extraction implementation](https://github.com/hacs/integration/blob/main/custom_components/hacs/repositories/base.py): HACS calls `ZipFile.extractall` into the integration directory. Because that extraction does not restore Unix executable permissions, the adapter restores the helper's owner execute bit off the Home Assistant event loop. A missing file, non-regular file or failed permission change becomes `helper_unavailable`.

Only runtime files on the allowlist and dependency license/notice files are copied. Tests, Go source, development dependencies, APKs, captures, credentials and private keys are excluded. The helper contains public application metadata and the explicitly approved public Qualvision CA; the CA is not a client credential or private key. Review the final binary's embedded assets and the archive contents before approving a release. An allowlist excludes extra files, but cannot detect a secret accidentally written into an allowed source file.

The ZIP uses a fixed sorted order, timestamp and Unix modes, with uncompressed stored entries. The helper has mode `0755`; other entries have mode `0644`. The SHA-256 sidecar describes the whole archive. Building unchanged inputs twice with the same Go toolchain must produce identical archives; CI compares both files. Dependency licenses come from the modules actually used by `go list -deps ./cmd/allo-ha`, including transitive modules, and from the pinned Go toolchain.

## Offline checks

Install development dependencies from the pinned `requirements-dev.txt` in the supported Python environment, then run:

```sh
.venv/bin/ruff check custom_components tests scripts
.venv/bin/ruff format --check custom_components tests scripts
.venv/bin/python -m pytest -q
go test -race ./...
go vet ./...
npm ci
npx playwright install --with-deps chromium
npm run check
npm test
```

The validation workflow runs these checks, HACS validation, hassfest, and two candidate builds. It has read-only repository permissions and no release publication step. Synthetic archive and browser tests do not prove a clean Home Assistant installation or physical-device behavior.

## Gates before the first release

A candidate was configured in an isolated Home Assistant 2026.10.4 installation with its own receiver identity and displayed real 480×240 video in the browser. On 2026-11-04, the owner confirmed the pending physical bell test and audible audio in both directions using Listen and Hold to talk on the iPhone. This is owner-reported confirmation, not a new measurement of latency, exactly-once bell delivery, full duplex or codec negotiation.

A complete release still requires validation of a clean supported Home Assistant installation through HACS using only the owner's account and fresh per-installation push receiver registration. The live-microphone confirmation is scoped to the iPhone; other browsers have not received audible-speech acceptance. Physically verify that talk stops when released, when leaving the view, when a session ends and when the integration unloads; these lifecycle tests were not explicitly confirmed by the owner's response. Existing protocol evidence and prerecorded or silent audio do not satisfy physical acceptance gates. Installation instructions remain withheld until the complete release passes all remaining gates.
