# Test results — v2

Verified runs of `tests/run-tests.sh` / `tests/run-tests.ps1` (details of the latest run: `tests/results/last-run.txt`).
v2 adds the independent server-side verification (T6–T8) and keeps RustFS data after the run.

## Environment

| | |
|---|---|
| Date | 2026-10-11 |
| Docker / Compose | 29.8.1 / v5.5.1 (Docker Desktop, Linux engine) |
| S3 server | `rustfs/rustfs@sha256:1803faef57627e2d9c2e7d89d655d712ddded5389040054987163043fecb6a3c` |
| AWS CLI (bucket setup only) | `amazon/aws-cli@sha256:ce44b800602264c12298bbf3399055a4cefdb9ed4a098abe564afe26e7071bce` |
| Bucket / credentials | `test-bucket` / `minioadmin` + `minioadmin123` (local throw-away) |

## Runs

| Run | Platform | Go | curl | File | Checks | Result |
|-----|----------|----|------|------|--------|--------|
| A. Container mode (`tester`) | Debian 12, x86_64 | 1.22.12 | 7.88.1 | 1100 MB (>1 GB), 3 files | 14 (T8 skipped) | **14 passed, 0 failed** (before a final cosmetic listing fix; re-run below) |
| B. Container mode (`tester`), final script | Debian 12, x86_64 | 1.22.12 | 7.88.1 | 200 MB | 14 (T8 skipped) | **14 passed, 0 failed** |
| C. Host mode, bash (Alpine/BusyBox tools, docker socket) | Alpine 3, x86_64 | Alpine `go` pkg | Alpine curl | 60 MB | 15 | **15 passed, 0 failed** |
| D. PowerShell | Windows 11 (10.0.26200), PowerShell 5.1 | 1.27.1 | 8.21.0 | 200 MB | 15 | **15 passed, 0 failed** |

Run A upload: `3 uploaded, 0 failed | 1.1 GiB in 4s (avg 277.7 MiB/s)`.

## Manual scenario (upload, verify, download into tests/downloads)

Run on Linux (bash script) and Windows (PowerShell script):

```
upload   demo/a.bin (60 MiB), demo/b.txt  -> VERIFIED on server (size), stored in /data/test-bucket/demo/<key>/xl.meta + part.N
download demo/a.bin, demo/b.txt           -> tests/downloads/…  VERIFIED (sha256 identical to the uploaded file)
```

## Bugs found and fixed while writing v2

- Old curl (7.88) does not send `x-amz-content-sha256`; RustFS rejects it. The scripts now send the header themselves.
- Old curl does not encode `/` in a signed query string; the listing prefix is now sent as `%2F`.
- GNU `sed` drops the final newline when its input has none, BusyBox `sed` does not; the listing now uses `awk 'NF'`.
- Earlier versions ran `docker compose down -v` at the end, deleting all uploaded objects: RustFS now stays up and keeps its data in a named volume.

## Notes and limits

- Speeds are local loopback; they say nothing about real internet speed.
- RustFS is not AWS: ACLs, bucket policies, regions and real signature edge cases still need a smoke test against real S3.
- The live progress bar needs an interactive terminal; the scripted runs were too fast / not attached to a terminal to show it.
  Progress accounting itself is covered by the Go unit tests (`go test ./...`).
