package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeS3 implements just enough of the S3 multipart protocol for tests.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	parts    map[string]map[int][]byte // uploadId -> part -> data
	failOnce int32                     // fail first attempt of this part number (after reading the body)
	failed   int32
	aborted  int32
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	key := strings.TrimPrefix(r.URL.Path, "/")

	switch {
	case r.Method == "POST" && q.Has("uploads"):
		id := "upload-1"
		f.parts[id] = map[int][]byte{}
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>%s</Key><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, key, id)

	case r.Method == "PUT" && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		data, _ := io.ReadAll(r.Body)
		if n == int(f.failOnce) && atomic.CompareAndSwapInt32(&f.failed, 0, 1) {
			http.Error(w, "<Error><Code>InternalError</Code></Error>", 500)
			return
		}
		f.parts[q.Get("uploadId")][n] = data
		w.Header().Set("ETag", `"etag`+strconv.Itoa(n)+`"`)

	case r.Method == "POST" && q.Has("uploadId"):
		p := f.parts[q.Get("uploadId")]
		var nums []int
		for n := range p {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var all []byte
		for _, n := range nums {
			all = append(all, p[n]...)
		}
		f.objects[key] = all
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Location>http://%s/%s</Location><Bucket>b</Bucket><Key>%s</Key></CompleteMultipartUploadResult>`, r.Host, key, key)

	case r.Method == "DELETE" && q.Has("uploadId"):
		atomic.AddInt32(&f.aborted, 1)
		w.WriteHeader(204)

	case r.Method == "PUT":
		data, _ := io.ReadAll(r.Body)
		f.objects[key] = data
		w.Header().Set("ETag", `"x"`)

	case r.Method == "HEAD" || r.Method == "GET":
		data, ok := f.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", 404)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var s, e int
			fmt.Sscanf(rng, "bytes=%d-%d", &s, &e)
			if e >= len(data) {
				e = len(data) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(data)))
			w.Header().Set("Content-Length", strconv.Itoa(e-s+1))
			w.WriteHeader(206)
			if r.Method == "GET" {
				w.Write(data[s : e+1])
			}
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == "GET" {
			w.Write(data)
		}
	}
}

func testClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	c, err := Dial(Configs{
		S3EndpointURL: *u, S3Region: "us-east-1", S3Bucket: "b",
		S3AccessKey: "k", S3SecretKey: "s",
		S3Timeout: 10 * time.Second, S3MaxRetries: 3, S3PartSizeMB: 5, S3Concurrency: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestUploadDownloadRoundTripWithRetry(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}, failOnce: 2}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := testClient(t, srv)

	data := make([]byte, 17*1024*1024+123) // 4 parts of 5 MB
	rand.Read(data)
	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}

	up := newTracker("big.bin", int64(len(data)))
	if _, err := c.Upload(context.Background(), "b", "big.bin", src, false, up); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fake.objects["b/big.bin"], data) {
		t.Fatal("uploaded content differs")
	}
	if fake.failed != 1 {
		t.Fatal("expected the injected failure to have happened (retry path untested)")
	}
	// Raw counter (not clamped): the retried part must not be double counted.
	if got := atomic_load(&up.sent); got != up.total {
		t.Fatalf("upload progress = %d, want exactly %d", got, up.total)
	}

	size, err := c.Size(context.Background(), "b", "big.bin")
	if err != nil || size != int64(len(data)) {
		t.Fatalf("size = %d, %v", size, err)
	}
	down := newTracker("big.bin", size)
	dst := filepath.Join(dir, "out.bin")
	if err := c.Download(context.Background(), "b", "big.bin", dst, down); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatal("downloaded content differs")
	}
	if got := atomic_load(&down.sent); got != down.total {
		t.Fatalf("download progress = %d, want exactly %d", got, down.total)
	}
	if _, err := os.Stat(dst + ".part"); err == nil {
		t.Fatal(".part file left behind")
	}
}

func TestSmallFileSinglePut(t *testing.T) {
	fake := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[int][]byte{}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	c := testClient(t, srv)

	src := filepath.Join(t.TempDir(), "small.txt")
	os.WriteFile(src, []byte("hello"), 0o644)
	tr := newTracker("small.txt", 5)
	if _, err := c.Upload(context.Background(), "b", "dir/small.txt", src, true, tr); err != nil {
		t.Fatal(err)
	}
	if string(fake.objects["b/dir/small.txt"]) != "hello" || atomic_load(&tr.sent) != 5 {
		t.Fatalf("bad result: %q sent=%d", fake.objects["b/dir/small.txt"], tr.sent)
	}
}

func atomic_load(p *int64) int64 { return atomic.LoadInt64(p) }
