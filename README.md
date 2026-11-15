# AI Relay — modified New API corresponding source

This is a modified version of [New API](https://github.com/QuantumNous/new-api).
Frontend design and development by New API contributors.

This repository contains **complete corresponding source** for our modified
New API gateway, including the upstream frontend and backend, our changes,
dependency lockfiles, build recipe and original legal notices. It is a fresh
source snapshot, without the private site's Git history, credentials or data.

- Upstream commit: `7d8b91268dab69d7150f345c448b5e250163146a` (existing v1.0.0-rc.46 line).
- Modification date: 2026-11-12; source snapshot date: 2026-11-15.
- Local development image: `sha256:6deeff3adfa8aa4f07047f451ad504e5695cb8350febc44d20845ec9b4a40e17`. Production is not deployed.
- License: **AGPL-3.0**, with New API's additional Section 7 terms in [NOTICE](NOTICE).
- [Modification details](CHANGES.md) · [Original upstream README](new-api/README.md).

## Verify and build

Install Python 3.11+ and Docker with BuildKit support. Python uses only its
standard library. From this repository root:

```sh
python build.py --verify-only
python build.py --tag ai-relay/new-api:public-source
```

The verifier reconstructs the exact build-context archive and checks its SHA-256
against the successful development build, restoring Git LF/CRLF conversions only
when the original file hash matches. The builder sends that verified tar
to Docker; unrelated checkout files are never sent. Build downloads require
network access. Bun 1.4.0, Go 1.26.10 and the runtime image are fixed by digest
in `new-api/Dockerfile`; Go modules and web dependencies use committed lockfiles.
The recipe retains the tested Go proxy fallback, checksum validation and retry
settings. It rebuilds both the Go executable and embedded web UI from source.
Build output is not promised to be byte-for-byte identical across environments.

The base OS/runtime image supplies system dependencies; it does not supply the
gateway executable used by the final image. The source tree retains the full
upstream recipe and licenses for review. See `new-api/README.md` and
`new-api/.env.example` for standalone installation guidance. Supply your own
database connection and secrets; no production credentials are needed to build.

## Scope and notices

This release covers the modified New API program. Independent LibreChat, site
pages, identity HTTP service, operations records and private infrastructure are
not part of this source snapshot. Do not infer their license from this repository.
The gateway's HTTP handlers and data definitions are included in full.

New API copyright and third-party notices remain in `new-api/LICENSE`,
`new-api/NOTICE` and `new-api/THIRD-PARTY-LICENSES.md`; GoPay v1.5.123 is pinned
in `new-api/go.mod` and remains Apache-2.0 licensed. The modifications and this
snapshot's build helper are provided under the same AGPL-3.0 terms. This release
does not remove or replace any upstream attribution or third-party license.

Future deployed gateway changes require a matching new source snapshot. A source
link must remain available to network users through the site's legal/about UI.
