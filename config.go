package main

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Configs holds everything read from the environment / .env file.
//
//	S3_ENDPOINT_URL   optional  custom endpoint (MinIO, R2, ...). Empty = real AWS S3
//	S3_REGION         optional  default us-east-1
//	S3_BUCKET         required
//	S3_ACCESS_KEY     required
//	S3_SECRET_KEY     required
//	S3_TIMEOUT        optional  dial / TLS / wait-for-response timeout (default 60s)
//	S3_MAX_RETRIES    optional  retries per request (default 8)
//	S3_PART_SIZE_MB   optional  multipart chunk size, min 5 (default 8)
//	S3_CONCURRENCY    optional  parallel chunks per file (default 3)
type Configs struct {
	S3EndpointURL url.URL
	S3Region      string
	S3Bucket      string
	S3AccessKey   string
	S3SecretKey   string
	S3Timeout     time.Duration
	S3MaxRetries  int
	S3PartSizeMB  int64
	S3Concurrency int
}

// LoadEnvFile loads variables from a .env file without overriding variables
// that are already set in the real environment.
//
// If path is empty, ".env" in the working directory is tried first and then
// ".env" next to the executable; a missing file is not an error in that case.
// If path is given explicitly, it must exist.
func LoadEnvFile(path string) error {
	if path != "" {
		return godotenv.Load(path)
	}
	candidates := []string{".env"}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, c := range candidates {
		err := godotenv.Load(c)
		if err == nil {
			return nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("reading %s: %w", c, err)
		}
	}
	return nil
}

// LoadConfigs reads the configuration from the process environment
// (call LoadEnvFile first to populate it from .env).
func LoadConfigs() (Configs, error) {
	var c Configs
	var missing []string
	required := func(name string) string {
		v := strings.TrimSpace(os.Getenv(name))
		if v == "" {
			missing = append(missing, name)
		}
		return v
	}

	c.S3Bucket = required("S3_BUCKET")
	c.S3AccessKey = required("S3_ACCESS_KEY")
	c.S3SecretKey = required("S3_SECRET_KEY")
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if raw := strings.TrimSpace(os.Getenv("S3_ENDPOINT_URL")); raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return c, fmt.Errorf("S3_ENDPOINT_URL %q must be a full URL like https://host:port", raw)
		}
		c.S3EndpointURL = *u
	}

	c.S3Region = envString("S3_REGION", "us-east-1")

	var err error
	if c.S3Timeout, err = envDuration("S3_TIMEOUT", 60*time.Second); err != nil {
		return c, err
	}
	if c.S3MaxRetries, err = envInt("S3_MAX_RETRIES", 8); err != nil {
		return c, err
	}
	partMB, err := envInt("S3_PART_SIZE_MB", 8)
	if err != nil {
		return c, err
	}
	c.S3PartSizeMB = int64(partMB)
	if c.S3Concurrency, err = envInt("S3_CONCURRENCY", 3); err != nil {
		return c, err
	}
	return c, c.validate()
}

func (c Configs) validate() error {
	if c.S3PartSizeMB < 5 {
		return fmt.Errorf("part size must be at least 5 MB (S3 limit), got %d", c.S3PartSizeMB)
	}
	if c.S3Concurrency < 1 {
		return fmt.Errorf("concurrency must be at least 1, got %d", c.S3Concurrency)
	}
	if c.S3MaxRetries < 0 {
		return fmt.Errorf("S3_MAX_RETRIES must not be negative")
	}
	if c.S3Timeout <= 0 {
		return fmt.Errorf("S3_TIMEOUT must be positive")
	}
	return nil
}

func envString(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", name, v)
	}
	return n, nil
}

func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (examples: 30s, 2m)", name, v)
	}
	return d, nil
}
