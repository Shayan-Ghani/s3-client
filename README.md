# s3client

A small command-line tool to upload (and download) large files to AWS S3 or any
S3-compatible storage (MinIO, Cloudflare R2, etc.).

- Handles files on slow connections (multipart, failed chunks are retried)
- Uploads several files in one command
- Shows live progress: percent, size, speed and ETA
- Configuration is read from a `.env` file

## 1. Configure

Create a `.env` file i nthe folder you run the tool from (or next to the binary):

```env
S3_ENDPOINT_URL=https://s3.example.com   # optional, leave out for real AWS S3
S3_REGION=us-east-1                      # optional, default us-east-1
S3_BUCKET=my-bucket
S3_ACCESS_KEY=your-access-key
S3_SECRET_KEY=your-secret-key

# optional tuning
# S3_PART_SIZE_MB=8      # chunk size, minimum 5
# S3_CONCURRENCY=3       # chunks uploaded in parallel per file
# S3_MAX_RETRIES=8       # retries per failed request
# S3_TIMEOUT=60s         # connect / wait-for-reply timeout
```

Real environment variables (`export S3_BUCKET=...`) also work and take priority over `.env`.

## 2. Run it

### Option A: pre-built binary

If you have a binary (e.g. `s3client` for Linux, `s3client.exe` for Windows), no Go is needed:

```bash
chmod +x s3client            # Linux/macOS only, once
./s3client big-file.iso
```

### Option B: build it (needs Go 1.20+)

```bash
go build -o s3client .
./s3client big-file.iso
```

On Windows the output name must end in `.exe`, otherwise Windows asks "how do you want to open this file?":

```powershell
go build -o s3client.exe .
.\s3client.exe big-file.iso
```

Build for another system (for example a Linux server, from Windows or macOS):

```bash
GOOS=linux GOARCH=amd64 go build -o s3client .
```

PowerShell version:

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; go build -o s3client .
```

### Option C: run directly from source (needs Go 1.20+)

```bash
go run . big-file.iso
```

The first run downloads dependencies, so internet access is needed once.

## 3. Examples

The files to upload are passed as arguments after the program name.

```bash
# one file
./s3client backup.tar.gz

# several files
./s3client video1.mp4 video2.mp4 notes.txt

# files in other folders (Windows paths work too)
./s3client /data/dump.sql ./logs/app.log
./s3client "C:\Users\me\Videos\movie.mkv"

# wildcards (also work on Windows, which does not expand them itself)
./s3client *.iso
./s3client ./exports/*.csv

# put files under a folder (prefix) in the bucket -> backups/2026/dump.sql
./s3client -prefix backups/2026/ dump.sql

# upload 2 files at the same time
./s3client -parallel 2 a.zip b.zip c.zip

# make the uploaded file publicly readable
./s3client -public logo.png
```

With `go run`, replace `./s3client` with `go run .`, for example `go run . -prefix backups/ dump.sql`.

The object name in the bucket is the file name (plus the prefix, if given).
**Directories are not uploaded; pass the files inside them with a wildcard.**
### What you will see

```
Target: s3.example.com (bucket my-bucket) | chunk 8 MB x 3 parallel | retries 8
Uploading 2 file(s), 1.3 GiB total
[0/2] video1.mp4  42.0% | total 560.0 MiB / 1.3 GiB 42.0% | 2.1 MiB/s | ETA 6m10s
done    video1.mp4 (1.0 GiB)
```

### Download

```bash
./s3client download backups/2026/dump.sql
./s3client download -out ./restored a.zip videos/b.mp4
```

## Flags

| Flag | Applies to | Meaning |
|------|-----------|---------|
| `-prefix` | upload | folder/prefix for the object keys |
| `-public` | upload | upload with ACL `public-read` |
| `-out` | download | destination folder (default `.`) |
| `-parallel N` | both | files transferred at once (default 1) |
| `-part-size MB` | both | chunk size, overrides `S3_PART_SIZE_MB` |
| `-concurrency N` | both | parallel chunks per file, overrides `S3_CONCURRENCY` |
| `-env path` | both | use a different `.env` file |

Flags go **before** the file names. Run `./s3client help` for the full list.

## Tips

- **Very slow or unstable connection:** try `-concurrency 1 -part-size 5`. Smaller chunks mean less data is re-sent after a failure.
- **Ctrl+C** stops the transfer and cleans up the partial upload on S3.
- An interrupted upload starts over from the beginning (resume is not supported). Consider a bucket lifecycle rule that aborts incomplete multipart uploads.
- Exit code is `0` when everything succeeded and `1` if any file failed, so it works in scripts.

## Tests

```bash
go test ./...
```
also see [test-against-rustfs](https://github.com/Shayan-Ghani/s3-client/tree/main/tests) for an integration test with a minIo like s3 compatible object storage that is setup using docker and a prebuilt `.env` and `test-bucket`.