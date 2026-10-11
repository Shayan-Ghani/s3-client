package main

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

type Client struct {
	Configs
	s3         *s3.S3
	session    *session.Session
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
}

// Dial builds a client tuned for large transfers over slow links:
//   - files are streamed from disk (never fully loaded into memory)
//   - uploads are multipart, so a failed chunk is retried instead of the whole file
//   - there is no overall HTTP timeout (troublesome for large uploads);
//     only connect / TLS / "waiting for the server's reply" are time-limited
func Dial(cfg Configs) (*Client, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.DialContext = dialer.DialContext
	base.TLSHandshakeTimeout = 20 * time.Second
	base.ResponseHeaderTimeout = cfg.S3Timeout
	base.MaxIdleConnsPerHost = 64

	awsCfg := &aws.Config{
		Region:      aws.String(cfg.S3Region),
		Credentials: credentials.NewStaticCredentials(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		HTTPClient:  &http.Client{Transport: &countingTransport{base: base}}, // no http.Client.Timeout on purpose
		MaxRetries:  aws.Int(cfg.S3MaxRetries),
	}
	if cfg.S3EndpointURL.Host != "" {
		awsCfg.Endpoint = aws.String(cfg.S3EndpointURL.String())
		awsCfg.S3ForcePathStyle = aws.Bool(true)
	}

	sess, err := session.NewSession(awsCfg)
	if err != nil {
		return nil, err
	}

	partSize := cfg.S3PartSizeMB * 1024 * 1024
	return &Client{
		Configs: cfg,
		session: sess,
		s3:      s3.New(sess),
		uploader: s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
			u.PartSize = partSize
			u.Concurrency = cfg.S3Concurrency
			u.LeavePartsOnError = false
			u.RequestOptions = []request.Option{abortEvenIfCancelled}
		}),
		downloader: s3manager.NewDownloader(sess, func(d *s3manager.Downloader) {
			d.PartSize = partSize
			d.Concurrency = cfg.S3Concurrency
		}),
	}, nil
}

// abortEvenIfCancelled makes the AbortMultipartUpload request ignore the
// cancelled context, so pressing Ctrl+C still cleans up the partial upload.
func abortEvenIfCancelled(r *request.Request) {
	if r.Operation != nil && r.Operation.Name == "AbortMultipartUpload" {
		r.SetContext(context.Background())
	}
}

func (c *Client) Upload(ctx context.Context, bucket, key, path string, public bool, tr *tracker) (*url.URL, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if tr != nil {
		ctx = context.WithValue(ctx, trackerKey{}, tr)
	}

	in := &s3manager.UploadInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
		Body:   f, // *os.File is an io.ReaderAt: parts are read straight from disk, not buffered in RAM
	}
	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); ct != "" {
		in.ContentType = aws.String(ct)
	}
	if public {
		in.ACL = aws.String("public-read")
	}

	res, err := c.uploader.UploadWithContext(ctx, in)
	if err != nil {
		return nil, err
	}
	return url.Parse(res.Location)
}

// Download streams bucket/key into destPath (via destPath+".part", renamed on success).
func (c *Client) Download(ctx context.Context, bucket, key, destPath string, tr *tracker) error {
	if tr != nil {
		ctx = context.WithValue(ctx, trackerKey{}, tr)
	}
	tmp := destPath + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = c.downloader.DownloadWithContext(ctx, f, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, destPath)
}

func (c *Client) Size(ctx context.Context, bucket, key string) (int64, error) {
	out, err := c.s3.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return 0, err
	}
	return aws.Int64Value(out.ContentLength), nil
}

func (c *Client) Delete(ctx context.Context, bucket, key string) error {
	_, err := c.s3.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	return err
}

// Progress is counted at the HTTP layer: reading the file is not a reliable signal,
// because the SDK may read it extra times (e.g. to hash it for plain-HTTP endpoints).

type trackerKey struct{}

type countingTransport struct{ base http.RoundTripper }

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr, _ := req.Context().Value(trackerKey{}).(*tracker)
	if tr == nil {
		return t.base.RoundTrip(req)
	}

	switch req.Method {
	case http.MethodPut:
		if req.Body != nil && req.Body != http.NoBody {
			r2 := req.Clone(req.Context())
			r2.Body = &countingBody{ReadCloser: req.Body, tr: tr, key: uploadChunkKey(req.URL)}
			req = r2
		}
		return t.base.RoundTrip(req)

	case http.MethodGet:
		key := downloadChunkKey(req.Header.Get("Range"))
		resp, err := t.base.RoundTrip(req)
		if err == nil && resp.StatusCode < 300 {
			resp.Body = &countingBody{ReadCloser: resp.Body, tr: tr, key: key}
		}
		return resp, err
	}
	return t.base.RoundTrip(req)
}

type countingBody struct {
	io.ReadCloser
	tr  *tracker
	key int64
	n   int64 // bytes in this attempt
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.n += int64(n)
		b.tr.record(b.key, b.n)
	}
	return n, err
}

func uploadChunkKey(u *url.URL) int64 {
	n, _ := strconv.ParseInt(u.Query().Get("partNumber"), 10, 64)
	return n // 0 for a single-request PutObject
}

// downloadChunkKey turns "bytes=START-END" into START.
func downloadChunkKey(rng string) int64 {
	rng = strings.TrimPrefix(rng, "bytes=")
	if i := strings.IndexByte(rng, '-'); i > 0 {
		rng = rng[:i]
	}
	n, _ := strconv.ParseInt(rng, 10, 64)
	return n
}

func (c *Client) String() string {
	if c.S3EndpointURL.Host != "" {
		return fmt.Sprintf("%s (bucket %s)", c.S3EndpointURL.Host, c.S3Bucket)
	}
	return fmt.Sprintf("AWS S3 %s (bucket %s)", c.S3Region, c.S3Bucket)
}
