---
name: test-s3client-rustfs
description: Test the s3client Go CLI end to end against a local RustFS (S3-compatible) server with bucket test-bucket using Docker, on Linux or Windows PowerShell. Use when asked to test, verify, or smoke-test uploads/downloads, to check that uploaded files really are in the bucket, to download them into tests/downloads, or after changing main.go, client.go, config.go or progress.go.
---

# Test s3client against RustFS

Everything lives in `tests/` (see `tests/README.md`). RustFS replaces MinIO (its official images/binaries are
gone). Credentials are local throw-away (`minioadmin` / `minioadmin123`), bucket `test-bucket`, port 9000.

## Scenario: upload -> verify -> download into tests/downloads

```bash
./tests/run-tests.sh -prefix demo/ ./file.bin      # upload, then independent check (curl+SigV4) + look inside /data
./tests/run-tests.sh download demo/file.bin        # saves to tests/downloads/, compares sha256 with the upload
```
Windows: `.\tests\run-tests.ps1 -prefix demo/ .\file.bin` and `.\tests\run-tests.ps1 download demo/file.bin`.
Output is the real s3client output (progress bar); verification lines say VERIFIED / MISSING / MISMATCH, exit code 1 on problems.

## Full suite

- Docker only (builds the Linux binary in a Go container):
  `docker compose -f tests/docker-compose.yml --profile linux-test run --rm tester` then `... down -v`
- Linux host with Go+Docker+curl: `./tests/run-tests.sh` (`BIG_MB=1100` for >1 GB, `CLEANUP=1` to delete data after)
- Windows: `.\tests\run-tests.ps1`

Procedure: `go test ./...` first, then the suite; read `tests/results/last-run.txt` (all PASS); update
`tests/RESULTS.md` (bump version/date/versions) if results should be recorded.

## Facts to remember

- RustFS stores an object as a FOLDER: `/data/test-bucket/<key>/xl.meta` + `<uuid>/part.N`. Not a plain file.
- The scripts leave RustFS running; data is in docker volume `tests_rustfs-data`. `down` keeps it, `down -v` deletes it
  (an "empty bucket" usually means `down -v` ran, or the user looked for a file instead of a folder).
- Independent check uses `curl --aws-sigv4` (>= 7.75); old curl needs `x-amz-content-sha256` sent explicitly
  and `/` in query strings written as `%2F` (the scripts do both).
- Do not mount tmpfs on `/data` (Permission denied). Port 9000 must be free.
- Never put real AWS keys in `tests/test.env`.
- Speeds are loopback; the live progress bar needs an interactive terminal.
