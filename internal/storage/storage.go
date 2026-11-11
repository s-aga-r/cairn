// Package storage streams compressed objects to and from an S3-compatible bucket.
package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/s-aga-r/cairn/internal/config"
)

// Suffix marks objects that hold a zstd stream.
const Suffix = ".zst"

type Store struct {
	client      *minio.Client
	bucket      string
	prefix      string
	partSize    uint64
	concurrency uint
	compression config.Compression
}

// Object describes one uploaded stream.
type Object struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	RawSize int64  `json:"raw_size"`
}

// Entry is one listed key, relative to the configured prefix.
type Entry struct {
	Key      string
	Size     int64
	Modified time.Time
}

func Open(cfg config.Storage, compression config.Compression) (*Store, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("storage.endpoint: %w", err)
	}
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds:  credentialsFor(cfg),
		Secure: endpoint.Scheme == "https",
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("storage.endpoint %q (expected a URL like https://s3.example.com): %w", cfg.Endpoint, err)
	}
	return &Store{
		client:      client,
		bucket:      cfg.Bucket,
		prefix:      cfg.Prefix,
		partSize:    uint64(cfg.PartSizeMB) << 20,
		concurrency: uint(cfg.Concurrency),
		compression: compression,
	}, nil
}

// Ping checks that the bucket is reachable.
func (s *Store) Ping(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	if !exists {
		return fmt.Errorf("storage: bucket %q does not exist", s.bucket)
	}
	return nil
}

// Upload compresses whatever produce writes and stores it under key as a
// parallel multipart upload. Nothing touches the local disk.
func (s *Store) Upload(ctx context.Context, key string, produce func(io.Writer) error) (Object, error) {
	reader, writer := io.Pipe()
	raw := &countingWriter{}
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		encoder := s.encoder(writer)
		raw.target = encoder
		err := produce(raw)
		writer.CloseWithError(errors.Join(err, encoder.Close()))
	}()
	info, err := s.client.PutObject(ctx, s.bucket, s.key(key), reader, -1, minio.PutObjectOptions{
		PartSize:              s.partSize,
		NumThreads:            s.concurrency,
		ConcurrentStreamParts: true,
	})
	// Unblocks the producer when the upload stopped reading early.
	reader.CloseWithError(err)
	<-produced
	if err != nil {
		return Object{}, fmt.Errorf("upload %s: %w", key, err)
	}
	return Object{Name: key, Size: info.Size, RawSize: raw.count}, nil
}

func (s *Store) UploadFile(ctx context.Context, key, path string) (Object, error) {
	return s.Upload(ctx, key, func(w io.Writer) error {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(w, file)
		return err
	})
}

// Download streams the decompressed object into consume.
func (s *Store) Download(ctx context.Context, key string, consume func(io.Reader) error) error {
	object, err := s.open(ctx, key)
	if err != nil {
		return err
	}
	defer object.Close()
	// NewReader fails only on invalid options.
	decoder, _ := zstd.NewReader(object, zstd.WithDecoderConcurrency(s.compression.Threads))
	defer decoder.Close()
	if err := consume(decoder); err != nil {
		return fmt.Errorf("download %s: %w", key, err)
	}
	return nil
}

// DownloadFile writes the decompressed object to path. A failed download
// leaves no file behind.
func (s *Store) DownloadFile(ctx context.Context, key, path string) error {
	err := s.Download(ctx, key, func(r io.Reader) error {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(file, r)
		return errors.Join(err, file.Close())
	})
	if err != nil {
		os.Remove(path)
	}
	return err
}

// PutBytes stores a small object as is, in a single request.
func (s *Store) PutBytes(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.key(key), bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("upload %s: %w", key, err)
	}
	return nil
}

// PutCompressed compresses a small payload in memory and stores it in a single request.
func (s *Store) PutCompressed(ctx context.Context, key string, data []byte) error {
	return s.PutBytes(ctx, key, s.encoder(nil).EncodeAll(data, nil))
}

func (s *Store) GetBytes(ctx context.Context, key string) ([]byte, error) {
	object, err := s.open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer object.Close()
	data, err := io.ReadAll(object)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", key, err)
	}
	return data, nil
}

// List returns every key below prefix, sorted by key.
func (s *Store) List(ctx context.Context, prefix string) ([]Entry, error) {
	var entries []Entry
	options := minio.ListObjectsOptions{Prefix: s.key(prefix), Recursive: true}
	for object := range s.client.ListObjects(ctx, s.bucket, options) {
		if object.Err != nil {
			return nil, fmt.Errorf("list %s: %w", prefix, object.Err)
		}
		entries = append(entries, Entry{
			Key:      strings.TrimPrefix(strings.TrimPrefix(object.Key, s.prefix), "/"),
			Size:     object.Size,
			Modified: object.LastModified,
		})
	}
	return entries, nil
}

func (s *Store) Delete(ctx context.Context, keys []string) error {
	objects := make(chan minio.ObjectInfo, len(keys))
	for _, key := range keys {
		objects <- minio.ObjectInfo{Key: s.key(key)}
	}
	close(objects)
	var failures []error
	for failure := range s.client.RemoveObjects(ctx, s.bucket, objects, minio.RemoveObjectsOptions{}) {
		failures = append(failures, fmt.Errorf("delete %s: %w", failure.ObjectName, failure.Err))
	}
	return errors.Join(failures...)
}

func (s *Store) open(ctx context.Context, key string) (*minio.Object, error) {
	object, err := s.client.GetObject(ctx, s.bucket, s.key(key), minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", key, err)
	}
	return object, nil
}

func (s *Store) key(key string) string {
	if s.prefix == "" {
		return key
	}
	return s.prefix + "/" + key
}

func (s *Store) encoder(w io.Writer) *zstd.Encoder {
	// NewWriter fails only on invalid options; config validation bounds the level.
	encoder, _ := zstd.NewWriter(w,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(s.compression.Level)),
		zstd.WithEncoderConcurrency(s.compression.Threads),
	)
	return encoder
}

// credentialsFor uses the configured keys, or falls back to the standard
// AWS environment variables and instance role when none are set.
func credentialsFor(cfg config.Storage) *credentials.Credentials {
	if cfg.AccessKey != "" {
		return credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, "")
	}
	return credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.IAM{},
	})
}

type countingWriter struct {
	target io.Writer
	count  int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.target.Write(p)
	c.count += int64(n)
	return n, err
}
