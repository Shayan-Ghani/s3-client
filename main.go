package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const usage = `s3client - upload / download files to S3 with progress

Usage:
  s3client [flags] <file>...                 upload files (default command)
  s3client upload   [flags] <file>...        same as above
  s3client download [flags] <key>...         download objects from the bucket

Upload flags:
  -prefix string   key prefix inside the bucket, e.g. backups/2026/
  -public          upload with ACL public-read (default: bucket default / private)
  -parallel int    files transferred at the same time (default 1)

Download flags:
  -out string      destination directory (default ".")
  -parallel int    files transferred at the same time (default 1)

Common flags:
  -env string      path to the .env file (default: ./.env, then next to the binary)
  -part-size int   multipart chunk size in MB (overrides S3_PART_SIZE_MB)
  -concurrency int parallel chunks per file   (overrides S3_CONCURRENCY)

Wildcards (e.g. *.iso) are expanded by the program, so they work on Windows too.
Configuration comes from the .env file / environment:
  S3_ENDPOINT_URL (optional), S3_REGION, S3_BUCKET, S3_ACCESS_KEY, S3_SECRET_KEY,
  S3_TIMEOUT, S3_MAX_RETRIES, S3_PART_SIZE_MB, S3_CONCURRENCY
`

func main() { os.Exit(run()) }

func run() int {
	args := os.Args[1:]
	cmd := "upload"
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	{
		switch args[0] {
		case "upload", "download":
			cmd, args = args[0], args[1:]
		case "help", "-h", "-help", "--help":
			fmt.Print(usage)
			return 0
		}
	}

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	envPath := fs.String("env", "", "")
	partSize := fs.Int("part-size", 0, "")
	concurrency := fs.Int("concurrency", 0, "")
	parallel := fs.Int("parallel", 1, "")
	prefix := fs.String("prefix", "", "")
	public := fs.Bool("public", false, "")
	outDir := fs.String("out", ".", "")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	targets := fs.Args()
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "error: no files given")
		fs.Usage()
		return 2
	}
	if *parallel < 1 {
		return fail("-parallel must be at least 1")
	}

	if err := LoadEnvFile(*envPath); err != nil {
		return fail("%v", err)
	}
	cfg, err := LoadConfigs()
	if err != nil {
		return fail("%v", err)
	}
	if *partSize > 0 {
		cfg.S3PartSizeMB = int64(*partSize)
	}
	if *concurrency > 0 {
		cfg.S3Concurrency = *concurrency
	}
	if err := cfg.validate(); err != nil {
		return fail("%v", err)
	}

	client, err := Dial(cfg)
	if err != nil {
		return fail("connecting: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Target: %s | chunk %d MB x %d parallel | retries %d\n",
		client, cfg.S3PartSizeMB, cfg.S3Concurrency, cfg.S3MaxRetries)

	var jobs []job
	if cmd == "upload" {
		jobs, err = planUploads(ctx, targets, *prefix)
	} else {
		jobs, err = planDownloads(ctx, client, targets, *outDir)
	}
	if err != nil {
		return fail("%v", err)
	}

	return execute(ctx, client, cmd, jobs, *parallel, *public)
}

// job is one file transfer. For uploads src is the local path and key the
// object key; for downloads src is the object key and dst the local path.
type job struct {
	src, key, dst string
	size          int64
	tr            *tracker
}

type result struct {
	j   job
	loc string
	err error
}

func planUploads(_ context.Context, args []string, prefix string) ([]job, error) {
	paths, err := expandArgs(args)
	if err != nil {
		return nil, err
	}
	prefix = strings.TrimLeft(strings.ReplaceAll(prefix, "\\", "/"), "/")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	var jobs []job
	seen := map[string]string{}
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file (directories are not uploaded recursively)", p)
		}
		key := prefix + filepath.Base(p)
		if other, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s and %s would both be uploaded as %q; use different file names", other, p, key)
		}
		seen[key] = p
		jobs = append(jobs, job{src: p, key: key, size: st.Size(), tr: newTracker(filepath.Base(p), st.Size())})
	}
	return jobs, nil
}

func planDownloads(ctx context.Context, c *Client, keys []string, outDir string) ([]job, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	var jobs []job
	seen := map[string]string{}
	for _, key := range keys {
		key = strings.TrimLeft(key, "/")
		name := path.Base(key)
		if other, dup := seen[name]; dup {
			return nil, fmt.Errorf("%s and %s would both be saved as %q", other, key, name)
		}
		seen[name] = key
		size, err := c.Size(ctx, c.S3Bucket, key)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		jobs = append(jobs, job{src: key, key: key, dst: filepath.Join(outDir, name), size: size, tr: newTracker(name, size)})
	}
	return jobs, nil
}

func expandArgs(args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		if !strings.ContainsAny(a, "*?[") {
			out = append(out, a)
			continue
		}
		matches, err := filepath.Glob(a)
		if err != nil {
			return nil, fmt.Errorf("bad pattern %q: %w", a, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no files match %q", a)
		}
		out = append(out, matches...)
	}
	return out, nil
}

func execute(ctx context.Context, c *Client, cmd string, jobs []job, parallel int, public bool) int {
	trackers := make([]*tracker, len(jobs))
	var totalBytes int64
	for i, j := range jobs {
		trackers[i] = j.tr
		totalBytes += j.size
	}
	verb, past := "Uploading", "uploaded"
	if cmd == "download" {
		verb, past = "Downloading", "downloaded"
	}
	fmt.Printf("%s %d file(s), %s total\n", verb, len(jobs), fmtBytes(totalBytes))

	rep := newReporter(os.Stdout, trackers)
	rep.Start()
	started := time.Now()

	queue := make(chan job)
	results := make(chan result, len(jobs))
	var wg sync.WaitGroup
	for i := 0; i < parallel && i < len(jobs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				j.tr.setState(stateActive)
				var res = result{j: j}
				if cmd == "upload" {
					var loc string
					u, err := c.Upload(ctx, c.S3Bucket, j.key, j.src, public, j.tr)
					if u != nil {
						loc = u.String()
					}
					res.loc, res.err = loc, err
				} else {
					res.err = c.Download(ctx, c.S3Bucket, j.key, j.dst, j.tr)
					res.loc = j.dst
				}
				if res.err != nil {
					j.tr.setState(stateFailed)
					rep.Logf("FAILED  %s: %v", j.tr.name, res.err)
				} else {
					j.tr.complete()
					rep.Logf("done    %s (%s)", j.tr.name, fmtBytes(j.size))
				}
				results <- res
			}
		}()
	}
loop:
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			break loop
		}
	}
	close(queue)
	wg.Wait()
	close(results)
	rep.Stop()

	var ok, failed int
	var okBytes int64
	for r := range results {
		if r.err != nil {
			failed++
			continue
		}
		ok++
		okBytes += r.j.size
		fmt.Printf("  %s -> %s\n", r.j.tr.name, r.loc)
	}
	skipped := len(jobs) - ok - failed
	elapsed := time.Since(started)
	avg := float64(okBytes) / elapsed.Seconds()

	fmt.Printf("\n%d %s, %d failed", ok, past, failed)
	if skipped > 0 {
		fmt.Printf(", %d not started (interrupted)", skipped)
	}
	fmt.Printf(" | %s in %s (avg %s/s)\n", fmtBytes(okBytes), fmtDuration(elapsed), fmtBytes(int64(avg)))

	if failed > 0 || skipped > 0 {
		return 1
	}
	return 0
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	return 1
}
