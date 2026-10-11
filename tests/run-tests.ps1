# Tests the s3client binary against a local RustFS server (bucket: test-bucket) - native Windows PowerShell.
# Needs only Go, Docker Desktop (Linux containers) and curl.exe (ships with Windows 10/11). No WSL or bash.
# Works in Windows PowerShell 5.1 and PowerShell 7+.
#
# NO ARGUMENTS -> test suite (14 checks)
#   .\tests\run-tests.ps1                          # 200 MB test file
#   $env:BIG_MB = 1100; .\tests\run-tests.ps1      # >1 GB test file
#   $env:CLEANUP = 1; .\tests\run-tests.ps1        # stop RustFS and DELETE its data afterwards
#
# WITH ARGUMENTS -> your own files, real s3client output (live progress bar), then an independent check
#   .\tests\run-tests.ps1 big.iso notes.txt                 # upload, then verify the objects are on the server
#   .\tests\run-tests.ps1 C:\data\*.zip
#   .\tests\run-tests.ps1 -prefix demo/ -parallel 2 a.bin b.bin
#   .\tests\run-tests.ps1 download demo/a.bin               # download into tests\downloads, then verify it
#   .\tests\run-tests.ps1 download -out .\elsewhere demo/a.bin
#   .\tests\run-tests.ps1 help
#
# RustFS is started if needed and LEFT RUNNING, its data kept in the docker volume "tests_rustfs-data",
# so you can look at /data in the container. Stop it with
#   docker compose -f tests\docker-compose.yml --profile tools down       (keep the data)
#   docker compose -f tests\docker-compose.yml --profile tools down -v    (delete the data)
#
# If scripts are blocked:  powershell -ExecutionPolicy Bypass -File .\tests\run-tests.ps1 <args>

$ErrorActionPreference = 'Continue'   # native tools write progress to stderr; we check exit codes ourselves

$here      = $PSScriptRoot
$root      = Split-Path $here -Parent
$work      = if ($env:WORK) { $env:WORK } else { Join-Path $here '.work' }
$downloads = Join-Path $here 'downloads'
$compose   = Join-Path $here 'docker-compose.yml'
$results   = Join-Path $here 'results'
$bin       = Join-Path $work 's3client.exe'
$goodEnv   = Join-Path $work 'good.env'
$manifest  = Join-Path $work 'uploaded.tsv'     # key <TAB> sha256 <TAB> size  (what we uploaded)
$bigMB     = if ($env:BIG_MB) { [int]$env:BIG_MB } else { 200 }
$s3Host    = if ($env:S3_HOST) { $env:S3_HOST } else { 'localhost' }
$container = 's3client-test-rustfs'

function Stop-Script($msg, $code = 2) { Write-Host $msg -ForegroundColor Red; exit $code }
function Say($msg) { Write-Host $msg -ForegroundColor Cyan }

# docker/go output is shown, exit code is returned
function Invoke-Native {
    $exe = $args[0]; $rest = @($args | Select-Object -Skip 1)
    & $exe @rest 2>&1 | ForEach-Object { "$_" } | Write-Host
    return $LASTEXITCODE
}

# ---- prerequisites ---------------------------------------------------------
if (-not (Get-Command go -ErrorAction SilentlyContinue))       { Stop-Script "'go' not found in PATH (see tests\README.md)" }
if (-not (Get-Command docker -ErrorAction SilentlyContinue))   { Stop-Script "'docker' not found in PATH (see tests\README.md)" }
if (-not (Get-Command curl.exe -ErrorAction SilentlyContinue)) { Stop-Script "'curl.exe' (7.75+) not found in PATH (see tests\README.md)" }
docker info *> $null
if ($LASTEXITCODE -ne 0) { Stop-Script 'Docker daemon is not running. Start Docker Desktop (Linux containers) and retry.' }

New-Item -ItemType Directory -Force $work | Out-Null

# ---- build + config --------------------------------------------------------
Say "== Building s3client (windows/$(go env GOARCH))"
Push-Location $root
$rc = Invoke-Native go build -o $bin .
Pop-Location
if ($rc -ne 0) { Stop-Script 'go build failed' }

(Get-Content (Join-Path $here 'test.env')) -replace 'localhost:9000', "${s3Host}:9000" | Set-Content $goodEnv
function Get-EnvVal($name) { ((Get-Content $goodEnv) | Where-Object { $_ -like "$name=*" } | Select-Object -First 1) -replace "^$name=", '' }
$bucket   = Get-EnvVal 'S3_BUCKET'
$endpoint = Get-EnvVal 'S3_ENDPOINT_URL'
# old curl (< 8.x) does not add x-amz-content-sha256 itself, so send it (SHA-256 of an empty body: GET/HEAD)
$curlAuth = @('-H', 'x-amz-content-sha256: e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855', '--user', "$(Get-EnvVal 'S3_ACCESS_KEY'):$(Get-EnvVal 'S3_SECRET_KEY')", '--aws-sigv4', "aws:amz:$(Get-EnvVal 'S3_REGION'):s3")

# ---- server helpers --------------------------------------------------------
function Start-RustFS {
    if (docker ps -q --filter "name=$container" --filter status=running) { Say '== Using the RustFS server that is already running'; return }
    Say '== Starting RustFS and creating test-bucket'
    if ((Invoke-Native docker compose -f $compose up -d rustfs) -ne 0) { Stop-Script 'could not start RustFS' }
    if ((Invoke-Native docker compose -f $compose --profile tools run --rm createbucket) -ne 0) { Stop-Script 'could not create test-bucket' }
}
function Get-UrlKey($k) { $k -replace ' ', '%20' }
# size of an object as reported by the SERVER (independent of s3client: plain curl + SigV4); $null if missing
function Get-ObjSize($key) {
    $h = & curl.exe -s -I @curlAuth "$endpoint/$bucket/$(Get-UrlKey $key)" 2>$null
    if (-not $h -or $h[0] -notmatch ' 200') { return $null }
    $line = $h | Where-Object { $_ -match '^content-length:' } | Select-Object -First 1
    if ($line) { return [int64](($line -split ':')[1].Trim()) }
    return $null
}
# list of @{Size=..;Key=..} for the bucket (optionally under a prefix)
function Get-BucketList($prefix = '') {
    # '/' must be sent as %2F inside a query string (old curl does not encode it before signing)
    $p = ($prefix -replace ' ', '%20') -replace '/', '%2F'
    $xml = (& curl.exe -s @curlAuth "$endpoint/${bucket}?list-type=2&prefix=$p" 2>$null) -join ''
    [regex]::Matches($xml, '<Key>(.*?)</Key>.*?<Size>(\d+)</Size>') | ForEach-Object { @{ Key = $_.Groups[1].Value; Size = [int64]$_.Groups[2].Value } }
}
# does the object physically exist inside the container's /data ? (RustFS stores key/xl.meta + part files)
function Test-OnDisk($key) { docker exec $container test -f "/data/$bucket/$key/xl.meta" 2>$null; return ($LASTEXITCODE -eq 0) }
function Show-Disk($key)   { docker exec $container sh -c "du -ah '/data/$bucket/$key' 2>/dev/null" | ForEach-Object { "        $_" } }

# ============================ manual mode: arguments given ====================
if ($args.Count -gt 0) {
    Start-RustFS
    $a = @($args)
    if ($a[0] -match '^(help|-h|-help|--help)$') { & $bin help; exit 0 }
    $cmd = 'upload'
    if ($a[0] -match '^(upload|download)$') { $cmd = $a[0]; $a = @($a | Select-Object -Skip 1) }

    # find prefix / out dir / positional arguments (files for upload, keys for download)
    $prefix = ''; $outDir = ''; $pos = @()
    $valueFlags = '^-{1,2}(env|parallel|part-size|concurrency)$'
    for ($i = 0; $i -lt $a.Count; $i++) {
        $x = [string]$a[$i]
        if     ($x -match '^-{1,2}prefix$') { $prefix = [string]$a[++$i] }
        elseif ($x -match '^-{1,2}prefix=(.*)$') { $prefix = $Matches[1] }
        elseif ($x -match '^-{1,2}out$')    { $outDir = [string]$a[++$i] }
        elseif ($x -match '^-{1,2}out=(.*)$') { $outDir = $Matches[1] }
        elseif ($x -match $valueFlags)      { $i++ }
        elseif ($x.StartsWith('-'))         { }
        else                                { $pos += $x }
    }
    $prefix = ($prefix -replace '\\', '/').TrimStart('/')
    if ($prefix -and -not $prefix.EndsWith('/')) { $prefix += '/' }

    $extra = @()
    if ($cmd -eq 'download' -and -not $outDir) { $outDir = $downloads; $extra = @('-out', $outDir) }

    Say "== > s3client.exe $cmd -env tests\.work\good.env $($extra -join ' ') $($a -join ' ')"
    Write-Host ''
    & $bin $cmd -env $goodEnv @extra @a        # not captured: exactly what s3client prints (progress bar included)
    $code = $LASTEXITCODE
    Write-Host ''
    Say "== s3client exit code: $code"

    $bad = $false
    Say '== Independent check against the server (curl + SigV4, not s3client)'
    if ($cmd -eq 'upload') {
        if (-not (Test-Path $manifest)) { '' | Set-Content $manifest }
        $files = @(); foreach ($p in $pos) { $files += @(Get-ChildItem -Path $p -File -ErrorAction SilentlyContinue) }
        foreach ($f in $files) {
            $key = $prefix + $f.Name; $remote = Get-ObjSize $key
            if ($remote -eq $f.Length) {
                Write-Host "  VERIFIED   $key  ($($f.Length) bytes is on the server)" -ForegroundColor Green
                if (Test-OnDisk $key) { Write-Host "        stored in the container at /data/$bucket/$key/ :"; Show-Disk $key }
                $keep = @(Get-Content $manifest | Where-Object { $_ -and -not $_.StartsWith("$key`t") })
                $keep + "$key`t$((Get-FileHash $f.FullName).Hash.ToLower())`t$($f.Length)" | Set-Content $manifest
            } else {
                $r = if ($null -eq $remote) { 'not found' } else { "$remote bytes" }
                Write-Host "  MISSING    $key  (local $($f.Length) bytes, server: $r)" -ForegroundColor Red; $bad = $true
            }
        }
        Write-Host '  Bucket now contains:'
        Get-BucketList | Sort-Object { $_.Key } | ForEach-Object { '        {0,12}  {1}' -f $_.Size, $_.Key }
    } else {
        foreach ($k in $pos) {
            $k = $k.TrimStart('/'); $f = Join-Path $outDir ($k -split '/')[-1]; $remote = Get-ObjSize $k
            if (-not (Test-Path $f)) { Write-Host "  MISSING    $f was not downloaded" -ForegroundColor Red; $bad = $true; continue }
            $len = (Get-Item $f).Length
            if ($len -ne $remote) { Write-Host "  MISMATCH   $f is $len bytes, server has $remote" -ForegroundColor Red; $bad = $true; continue }
            $want = $null
            if (Test-Path $manifest) { $row = Get-Content $manifest | Where-Object { $_.StartsWith("$k`t") } | Select-Object -Last 1; if ($row) { $want = ($row -split "`t")[1] } }
            if (-not $want) { Write-Host "  VERIFIED   $f  (size matches the server; no upload record to compare content with)" -ForegroundColor Green }
            elseif ($want -eq (Get-FileHash $f).Hash.ToLower()) { Write-Host "  VERIFIED   $f  (identical to the file you uploaded, sha256 $($want.Substring(0,16))...)" -ForegroundColor Green }
            else { Write-Host "  MISMATCH   $f differs from the file you uploaded" -ForegroundColor Red; $bad = $true }
        }
        Write-Host "  Downloaded files are in: $outDir"
    }
    if ($bad) { $code = 1 }
    Say "== RustFS left running; objects live in /data of container $container (volume tests_rustfs-data)"
    exit $code
}

# ============================ test-suite mode: no arguments ===================
$data = Join-Path $work 'data'; $out = Join-Path $work 'out'
Remove-Item -Recurse -Force $data, $out -ErrorAction SilentlyContinue
New-Item -ItemType Directory -Force $data, $out, $results | Out-Null

$script:pass = 0; $script:fail = 0; $script:log = @()
function Check($name, [bool]$ok, $detail = '') {
    if ($ok) { $script:pass++; $line = "PASS  $name" } else { $script:fail++; $line = "FAIL  $name  $detail" }
    $script:log += $line
    Write-Host $line -ForegroundColor $(if ($ok) { 'Green' } else { 'Red' })
}
# run s3client, capture merged output and exit code
function Run-S3 {
    $o = & $bin @args 2>&1 | ForEach-Object { "$_" } | Out-String
    return @{ Out = $o; Code = $LASTEXITCODE }
}
function New-RandomFile($path, [int]$mb) {
    $rnd = New-Object System.Random
    $f = [IO.File]::Create($path); $buf = New-Object byte[] (1MB)
    for ($i = 0; $i -lt $mb; $i++) { $rnd.NextBytes($buf); $f.Write($buf, 0, $buf.Length) }
    $f.Close()
}

try {
    Start-RustFS

    Write-Host "== Generating test files (big = $bigMB MB)" -ForegroundColor Cyan
    New-RandomFile (Join-Path $data 'big.bin') $bigMB
    New-RandomFile (Join-Path $data 'mid.bin') 13     # not a multiple of the 8 MB chunk size
    'hello s3client' | Set-Content (Join-Path $data 'small.txt')
    $names = 'big.bin', 'mid.bin', 'small.txt'

    Write-Host '== Running tests' -ForegroundColor Cyan

    $r = Run-S3
    Check 'T1 no arguments prints usage, exit 0' ($r.Code -eq 0 -and $r.Out -match 'Usage') $r.Out

    $emptyEnv = Join-Path $work 'empty.env'; '' | Set-Content $emptyEnv
    $saved = @{}
    foreach ($k in 'S3_BUCKET', 'S3_ACCESS_KEY', 'S3_SECRET_KEY', 'S3_ENDPOINT_URL') {
        $saved[$k] = [Environment]::GetEnvironmentVariable($k); [Environment]::SetEnvironmentVariable($k, $null)
    }
    $r = Run-S3 -env $emptyEnv (Join-Path $data 'small.txt')
    foreach ($k in $saved.Keys) { [Environment]::SetEnvironmentVariable($k, $saved[$k]) }
    Check 'T2 missing config -> clear error, exit 1' ($r.Code -eq 1 -and $r.Out -match 'missing required environment variables') $r.Out

    $badEnv = Join-Path $work 'bad.env'
    (Get-Content $goodEnv) -replace '^S3_SECRET_KEY=.*', 'S3_SECRET_KEY=WRONG' | Set-Content $badEnv
    $r = Run-S3 -env $badEnv (Join-Path $data 'small.txt')
    Check 'T3 wrong secret key rejected (SignatureDoesNotMatch)' ($r.Code -eq 1 -and $r.Out -match 'SignatureDoesNotMatch') $r.Out

    $r = Run-S3 -env $goodEnv (Join-Path $data 'does-not-exist.bin')
    Check 'T4 missing local file -> exit 1' ($r.Code -eq 1) $r.Out

    $r = Run-S3 -env $goodEnv -prefix tests/run1/ -parallel 2 (Join-Path $data '*')
    Check 'T5 wildcard + prefix + parallel upload of 3 files' ($r.Code -eq 0 -and $r.Out -match '3 uploaded, 0 failed') $r.Out
    Write-Host $r.Out

    # independent proof that the files are really on the server
    $ok = $true; foreach ($n in $names) { if ((Get-ObjSize "tests/run1/$n") -ne (Get-Item (Join-Path $data $n)).Length) { $ok = $false } }
    Check 'T6 server reports all 3 objects with the exact local size (independent curl check)' $ok
    $listed = @(Get-BucketList 'tests/run1/').Count
    Check 'T7 bucket listing shows 3 objects under tests/run1/' ($listed -eq 3) "listed=$listed"
    $ok = $true; foreach ($n in $names) { if (-not (Test-OnDisk "tests/run1/$n")) { $ok = $false } }
    Check "T8 objects physically stored in the container under /data/$bucket/tests/run1/" $ok

    $r = Run-S3 -env $goodEnv -prefix tests/run2/ -part-size 5 -concurrency 1 (Join-Path $data 'mid.bin') (Join-Path $data 'small.txt')
    Check 'T9 explicit files with -part-size 5 -concurrency 1' ($r.Code -eq 0 -and $r.Out -match '2 uploaded, 0 failed') $r.Out

    $r = Run-S3 download -env $goodEnv -out $out tests/run1/big.bin tests/run1/mid.bin tests/run1/small.txt
    Check 'T10 download 3 objects' ($r.Code -eq 0 -and $r.Out -match '3 downloaded, 0 failed') $r.Out
    foreach ($n in $names) {
        $same = (Get-FileHash (Join-Path $data $n)).Hash -eq (Get-FileHash (Join-Path $out $n)).Hash
        Check "T11 SHA-256 identical after round trip: $n" $same
    }
    Check 'T12 no leftover .part files' (-not (Get-ChildItem $out -Filter '*.part'))

    $r = Run-S3 download -env $goodEnv -out $out tests/nope/missing.bin
    Check 'T13 download of missing key -> exit 1' ($r.Code -eq 1) $r.Out
}
finally {
    if ($env:CLEANUP) {
        Say '== Stopping RustFS and deleting its data'
        docker compose -f $compose --profile tools down -v *> $null
    } else {
        Say "== RustFS left running with the test objects in it (inspect: docker exec $container ls /data/$bucket/tests)."
        Say '   Stop: docker compose -f tests\docker-compose.yml --profile tools down      (add -v to delete the data)'
    }
}

$summary = "RESULT: $($script:pass) passed, $($script:fail) failed"
Write-Host ''
Write-Host $summary -ForegroundColor $(if ($script:fail) { 'Red' } else { 'Green' })
@("s3client test run $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')",
  "platform: Windows $([Environment]::OSVersion.Version) | big file: $bigMB MB | mode: host (PowerShell $($PSVersionTable.PSVersion))",
  "go: $(go version)", "curl: $((curl.exe --version | Select-Object -First 1))", "docker: $(docker --version)", '') + $script:log + @('', $summary) |
    Set-Content (Join-Path $results 'last-run.txt')
exit $(if ($script:fail) { 1 } else { 0 })
