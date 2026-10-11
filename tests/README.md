# Testing s3client against a local S3 server (RustFS)

These tests run the real `s3client` binary against [RustFS](https://github.com/rustfs/rustfs), a local
S3-compatible server that checks credentials and request signatures (a stand-in for AWS, no AWS account
needed). Everything uses a bucket called `test-bucket` and throw-away credentials.

The tests do not just trust s3client's "uploaded" message: after an upload they ask the **server** (with plain
`curl` + SigV4, a different client) whether the object exists and has the right size, and they look inside
the RustFS container's `/data`.

## The scenario most people want

1. **Upload** a file to the bucket
2. **Make sure it is uploaded** (independent check, and see where it lives in the container)
3. **Download** it into `tests/downloads/` and check it is identical to what you uploaded

```bash
./tests/run-tests.sh -prefix demo/ ./big.iso          # 1 + 2  (Windows: .\tests\run-tests.ps1 -prefix demo/ .\big.iso)
./tests/run-tests.sh download demo/big.iso            # 3      -> tests/downloads/big.iso, content compared
```

Example output of step 1+2 (shortened):

```
1 uploaded, 0 failed | 25.0 MiB in 0s
== Independent check against the server (curl + SigV4, not s3client)
  VERIFIED   demo/scenario.bin  (26214400 bytes is on the server)
        stored in the container at /data/test-bucket/demo/scenario.bin/ :
        4.0K   .../scenario.bin/xl.meta
        8.1M   .../scenario.bin/<id>/part.1   ... part.2, part.3, part.4
  Bucket now contains:
            26214400  demo/scenario.bin
```

### "The bucket is empty when I open /data in the container" — read this

- RustFS does **not** store `big.iso` as a plain file. Each object is a **folder**:
  `/data/test-bucket/demo/big.iso/xl.meta` plus the data in `/data/test-bucket/demo/big.iso/<uuid>/part.N`.
  Look for the folder, not for a file called `big.iso`.
- The data is only there while the volume exists. The scripts now **leave RustFS running** and keep the data in
  the docker volume `tests_rustfs-data`. Earlier versions ran `docker compose down -v` at the end, which deleted
  every object, so the bucket looked empty afterwards.
- `docker compose ... down` stops the server but keeps the data; `down -v` deletes it.
- Look inside: `docker exec s3client-test-rustfs find /data/test-bucket -name xl.meta`
- The container-only way (Way 1) deletes everything when you run its `down -v` cleanup.

## What is in this folder

| File | Purpose |
|------|---------|
| `docker-compose.yml` | RustFS server (data in a docker volume), a one-shot job that creates `test-bucket`, and an optional Linux test-runner container |
| `test.env` | Config for the tests (endpoint `localhost:9000`, bucket `test-bucket`, local-only keys) |
| `run-tests.sh` | Linux/macOS. No arguments: runs the checks. With arguments: upload/download your files and verify them |
| `run-tests.ps1` | Same for native Windows PowerShell (no Linux needed) |
| `downloads/` | Where manual-mode downloads are saved (created on demand, not committed) |
| `RESULTS.md` | Results of the last verified runs (v2) |
| `results/last-run.txt` | Written by every run of the suite |
| `.work/` | Scratch space: built binary, generated env files, test files, `uploaded.tsv` (hash of what you uploaded) |

## Requirements

| Tool | Version tested | Minimum |
|------|----------------|---------|
| Linux (or macOS / WSL2) with bash | Debian 12 and Alpine 3 containers (x86_64) | bash 4+, coreutils (`sha256sum`, `head`, `sed`, `awk`) |
| Windows (only for the `.ps1`) | Windows 11 10.0.26200, Windows PowerShell 5.1 | Windows 10, PowerShell 5.1 or 7+ |
| Docker Engine / Docker Desktop | 29.8.1 | 20.10+ |
| Docker Compose plugin (`docker compose`) | v5.5.1 | v2.20+ |
| Go (not needed for Way 1) | 1.22.12 (Linux), 1.27.1 (Windows) | 1.20 (see `go.mod`) |
| curl (independent verification) | 7.88.1 (Linux), 8.21.0 (Windows `curl.exe`) | 7.75 (needs `--aws-sigv4`) |
| Free TCP port | 9000 | |
| Internet | needed once, to pull images and Go modules | |

Docker images (pinned by digest in `docker-compose.yml`):

- `rustfs/rustfs@sha256:1803faef…a6c` (S3 server)
- `amazon/aws-cli@sha256:ce44b800…bce` (only creates the bucket)
- `golang:1.22-bookworm` (only for Way 1)

## Way 1 — everything in Docker (no Go needed on the host)

Builds the Linux binary inside a Go container, starts RustFS, runs all checks:

```bash
docker compose -f tests/docker-compose.yml --profile linux-test run --rm tester
docker compose -f tests/docker-compose.yml --profile linux-test down -v     # cleanup (deletes the data)
```

File larger than 1 GB: `BIG_MB=1100 docker compose -f tests/docker-compose.yml --profile linux-test run --rm tester`
(the on-disk check T8 is skipped here because the runner container cannot call `docker exec`).

## Way 2 — Linux host with Go, Docker and curl: the test suite

```bash
chmod +x tests/run-tests.sh
./tests/run-tests.sh                  # 200 MB test file
BIG_MB=1100 ./tests/run-tests.sh      # >1 GB test file
CLEANUP=1 ./tests/run-tests.sh        # also stop RustFS and DELETE its data at the end
```

By default RustFS is left running with the test objects inside, so you can inspect them. Exit code: 0 = all
checks passed, 1 = a check failed, 2 = a prerequisite is missing.

## Way 3 — Linux host: your own files (manual mode)

Give `run-tests.sh` arguments and it skips the suite. It builds the binary, starts RustFS if it is not already
running, runs `s3client` with `tests/test.env` on **your** files (nothing is captured, so you see exactly what
s3client prints, including the live progress bar) and then verifies the result with the independent check.

```bash
./tests/run-tests.sh big.iso notes.txt                       # upload + verify
./tests/run-tests.sh ./data/*.zip                            # wildcards
./tests/run-tests.sh -prefix demo/ -parallel 2 a.bin b.bin   # any s3client flag
./tests/run-tests.sh download demo/a.bin demo/b.bin          # -> tests/downloads/, content compared with the upload
./tests/run-tests.sh download -out ./restored demo/a.bin     # -out overrides tests/downloads
./tests/run-tests.sh help                                    # all s3client flags
```

Download check: file exists, size equals the server's size, and SHA-256 equals the file you uploaded earlier
(recorded in `tests/.work/uploaded.tsv`; if there is no record only the size is compared).
Stop the server when done: `docker compose -f tests/docker-compose.yml --profile tools down` (add `-v` to delete data).

## Way 4 — Windows PowerShell (no Linux, WSL or bash needed)

`tests/run-tests.ps1` does exactly the same as `run-tests.sh`. Needs Go, Docker Desktop (Linux containers) and
`curl.exe` (included with Windows 10/11).

```powershell
.\tests\run-tests.ps1                              # test suite (15 checks)
$env:BIG_MB = 1100; .\tests\run-tests.ps1          # >1 GB test file
$env:CLEANUP = 1; .\tests\run-tests.ps1            # also stop RustFS and delete its data

.\tests\run-tests.ps1 big.iso notes.txt            # manual mode: upload + verify
.\tests\run-tests.ps1 C:\data\*.zip
.\tests\run-tests.ps1 -prefix demo/ -parallel 2 a.bin b.bin
.\tests\run-tests.ps1 download demo/a.bin          # -> tests\downloads\
.\tests\run-tests.ps1 download -out .\restored demo/a.bin
```

If scripts are disabled: `powershell -ExecutionPolicy Bypass -File .\tests\run-tests.ps1 <args>`.
You may see a harmless `System.Management.Automation.RemoteException` line in Windows PowerShell 5.1 while Docker starts.

## Step by step by hand (the scenario without the scripts)

1. **Build**: `go build -o /tmp/s3client .`
2. **Start RustFS and the bucket**
   ```bash
   docker compose -f tests/docker-compose.yml up -d rustfs
   docker compose -f tests/docker-compose.yml --profile tools run --rm createbucket   # "bucket test-bucket ready"
   ```
3. **Make a file**: `head -c 200M /dev/urandom > /tmp/big.bin`
4. **Upload**: `/tmp/s3client -env tests/test.env -prefix manual/ /tmp/big.bin` (expect `1 uploaded, 0 failed`)
5. **Is it really there?** Ask the server, not s3client:
   ```bash
   curl -sI --user minioadmin:minioadmin123 --aws-sigv4 aws:amz:us-east-1:s3 \
     -H 'x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' \
     http://localhost:9000/test-bucket/manual/big.bin | grep -i content-length     # 209715200
   docker exec s3client-test-rustfs find /data/test-bucket/manual -type f           # xl.meta + part.N files
   ```
6. **Download into tests/downloads and compare**
   ```bash
   /tmp/s3client download -env tests/test.env -out tests/downloads manual/big.bin
   sha256sum /tmp/big.bin tests/downloads/big.bin                                   # must match
   ```
7. **Bad credentials must fail**
   ```bash
   sed 's/^S3_SECRET_KEY=.*/S3_SECRET_KEY=WRONG/' tests/test.env > /tmp/bad.env
   /tmp/s3client -env /tmp/bad.env /tmp/big.bin        # SignatureDoesNotMatch, exit code 1
   ```
8. **Stop**: `docker compose -f tests/docker-compose.yml --profile tools down -v`

## The checks of the suite

| ID | What is checked |
|----|-----------------|
| T1 | No arguments prints usage, exit 0 |
| T2 | Missing config gives a clear error, exit 1 |
| T3 | Wrong secret key is rejected (`SignatureDoesNotMatch`) |
| T4 | Non-existent local file, exit 1 |
| T5 | Wildcard + `-prefix` + `-parallel 2` upload of 3 files (incl. the big one) |
| T6 | **Server reports all 3 objects with the exact local size** (independent curl check) |
| T7 | **Bucket listing shows the 3 objects** under `tests/run1/` |
| T8 | **Objects physically stored in the container** under `/data/test-bucket/tests/run1/` (not in container mode) |
| T9 | Explicit files with slow-link settings `-part-size 5 -concurrency 1` |
| T10 | Download of 3 objects |
| T11 | SHA-256 of every file identical after upload + download (3 checks) |
| T12 | No leftover `.part` files |
| T13 | Download of a missing key, exit 1 |

## Troubleshooting

- **Bucket looks empty** — see "The bucket is empty when I open /data" above (objects are folders; `down -v` deletes data).
- **`port is already allocated` on 9000** — something else uses port 9000. Stop it, or change `"9000:9000"` in `docker-compose.yml` and the port in `test.env`.
- **`Docker daemon is not running`** — start Docker (`sudo systemctl start docker`; add yourself to the `docker` group).
- **`MISSING ... server: not found`** after an upload — the upload really did not reach the bucket; check the s3client output above it.
- **curl check fails with `missing header: x-amz-content-sha256`** — old curl; the scripts already send the header, copy it from step 5 when running by hand.
- **Image pull problems** — set `RUSTFS_IMAGE`, `AWSCLI_IMAGE` or `GO_IMAGE` to another tag.
- **RustFS exits with "Permission denied"** — do not mount a tmpfs/root-owned directory on `/data`; the compose file uses a named volume.
- `tests/.work` and `tests/downloads` are scratch folders and safe to delete.
