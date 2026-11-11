package storage_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"

	"github.com/s-aga-r/cairn/internal/config"
	"github.com/s-aga-r/cairn/internal/storage"
	"github.com/s-aga-r/cairn/internal/testenv"
)

func TestMain(m *testing.M) {
	// Failure tests point at dead endpoints; retries only slow them down.
	minio.MaxRetry = 1
	os.Exit(m.Run())
}

// open returns a store on the test bucket after edit changed its config.
func open(t *testing.T, edit func(*config.Storage)) *storage.Store {
	t.Helper()
	cfg := testenv.Env(t, testenv.Variant(t, nil), testenv.Postgres).Config
	if edit != nil {
		edit(&cfg.Storage)
	}
	store, err := storage.Open(cfg.Storage, cfg.Compression)
	testenv.Must(t, err)
	return store
}

func dead(t *testing.T) *storage.Store {
	return open(t, func(cfg *config.Storage) { cfg.Endpoint = "http://127.0.0.1:1" })
}

func failed(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s must fail", what)
	}
}

func TestOpenRejectsBadEndpoints(t *testing.T) {
	for _, endpoint := range []string{"http://[::1", "http:///no-host"} {
		_, err := storage.Open(config.Storage{Endpoint: endpoint}, config.Compression{})
		failed(t, endpoint, err)
	}
}

func TestPing(t *testing.T) {
	ctx := testenv.Context(t)
	testenv.Must(t, open(t, nil).Ping(ctx))
	failed(t, "a missing bucket", open(t, func(cfg *config.Storage) { cfg.Bucket = "cairn-no-such-bucket" }).Ping(ctx))
	failed(t, "a dead endpoint", dead(t).Ping(ctx))
}

func TestCredentialsFromEnvironment(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "cairn")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "cairn-secret")
	store := open(t, func(cfg *config.Storage) { cfg.AccessKey, cfg.SecretKey = "", "" })
	testenv.Must(t, store.Ping(testenv.Context(t)))
}

func TestStreamRoundTrip(t *testing.T) {
	ctx := testenv.Context(t)
	store := open(t, nil)
	// Larger than one part, so the upload is a real multipart upload.
	payload := bytes.Repeat([]byte("cairn stacks stones. "), 1<<20)
	object, err := store.Upload(ctx, "big.zst", func(w io.Writer) error {
		_, err := w.Write(payload)
		return err
	})
	testenv.Must(t, err)
	if object.RawSize != int64(len(payload)) || object.Size <= 0 || object.Size >= object.RawSize {
		t.Fatalf("unexpected sizes: %+v", object)
	}
	var got []byte
	testenv.Must(t, store.Download(ctx, "big.zst", func(r io.Reader) (err error) {
		got, err = io.ReadAll(r)
		return err
	}))
	if !bytes.Equal(got, payload) {
		t.Fatal("downloaded stream differs from the upload")
	}
}

func TestFileRoundTrip(t *testing.T) {
	ctx := testenv.Context(t)
	store := open(t, nil)
	source := filepath.Join(t.TempDir(), "source")
	testenv.Must(t, os.WriteFile(source, []byte("file content"), 0o600))
	_, err := store.UploadFile(ctx, "file.zst", source)
	testenv.Must(t, err)

	copied := filepath.Join(t.TempDir(), "copy")
	testenv.Must(t, store.DownloadFile(ctx, "file.zst", copied))
	if data, _ := os.ReadFile(copied); string(data) != "file content" {
		t.Fatalf("got %q", data)
	}

	_, err = store.UploadFile(ctx, "none.zst", filepath.Join(t.TempDir(), "missing"))
	failed(t, "uploading a missing file", err)
	failed(t, "an unwritable destination", store.DownloadFile(ctx, "file.zst", filepath.Join(copied, "below-a-file")))
	absent := filepath.Join(t.TempDir(), "absent")
	failed(t, "a missing object", store.DownloadFile(ctx, "missing.zst", absent))
	if _, err := os.Stat(absent); !os.IsNotExist(err) {
		t.Fatal("a failed download must leave no file")
	}
}

func TestSmallObjects(t *testing.T) {
	ctx := testenv.Context(t)
	store := open(t, nil)
	testenv.Must(t, store.PutBytes(ctx, "dir/raw", []byte("raw")))
	testenv.Must(t, store.PutCompressed(ctx, "dir/packed.zst", []byte("packed")))
	raw, err := store.GetBytes(ctx, "dir/raw")
	if err != nil || string(raw) != "raw" {
		t.Fatalf("got %q, %v", raw, err)
	}
	testenv.Must(t, store.Download(ctx, "dir/packed.zst", func(r io.Reader) error {
		data, err := io.ReadAll(r)
		if string(data) != "packed" {
			return errors.New("content differs")
		}
		return err
	}))

	entries, err := store.List(ctx, "dir/")
	testenv.Must(t, err)
	if len(entries) != 2 || entries[0].Key != "dir/packed.zst" || entries[1].Key != "dir/raw" || entries[1].Size != 3 {
		t.Fatalf("unexpected listing: %+v", entries)
	}
	testenv.Must(t, store.Delete(ctx, []string{"dir/raw", "dir/packed.zst"}))
	if entries, _ := store.List(ctx, "dir/"); len(entries) != 0 {
		t.Fatalf("objects survived the delete: %+v", entries)
	}
	_, err = store.GetBytes(ctx, "dir/raw")
	failed(t, "reading a deleted object", err)
}

func TestWithoutPrefix(t *testing.T) {
	ctx := testenv.Context(t)
	store := open(t, func(cfg *config.Storage) { cfg.Prefix = "" })
	key := testenv.Unique(t)
	testenv.Must(t, store.PutBytes(ctx, key, []byte("x")))
	entries, err := store.List(ctx, key)
	if err != nil || len(entries) != 1 || entries[0].Key != key {
		t.Fatalf("unexpected listing: %+v, %v", entries, err)
	}
	testenv.Must(t, store.Delete(ctx, []string{key}))
	// An empty key is invalid before any request is made.
	_, err = store.GetBytes(ctx, "")
	failed(t, "an empty key", err)
	failed(t, "an empty key", store.Download(ctx, "", func(io.Reader) error { return nil }))
}

func TestDeadEndpoint(t *testing.T) {
	ctx := testenv.Context(t)
	store := dead(t)
	_, err := store.Upload(ctx, "x.zst", func(w io.Writer) error {
		_, err := io.Copy(w, strings.NewReader("data"))
		return err
	})
	failed(t, "upload", err)
	failed(t, "put", store.PutBytes(ctx, "x", []byte("data")))
	_, err = store.List(ctx, "")
	failed(t, "list", err)
	failed(t, "delete", store.Delete(ctx, []string{"x"}))
}
