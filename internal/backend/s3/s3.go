// Package s3 implements backend.Backend over any S3-compatible object store — AWS S3,
// MinIO, Ceph RGW, and so on (ADR 0013). MinIO is the self-hosted default the
// architecture assumes, so a custom endpoint is a first-class config field, not an
// afterthought.
package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Config is the s3 kind's non-secret registration config.
type Config struct {
	// Endpoint is the service URL, e.g. "https://s3.amazonaws.com" or
	// "http://minio.storage.svc:9000". Empty means AWS S3. The scheme decides TLS.
	Endpoint string `json:"endpoint,omitempty"`
	Region   string `json:"region,omitempty"`
	Bucket   string `json:"bucket"`
	// Prefix confines this backend to one key prefix inside the bucket; listed paths
	// are relative to it.
	Prefix string `json:"prefix,omitempty"`
	// PathStyle forces path-style addressing (http://host/bucket/key). MinIO and most
	// self-hosted stores need it; AWS works with either.
	PathStyle bool `json:"pathStyle,omitempty"`
}

// Credentials are the s3 kind's secret fields, delivered from a Kubernetes Secret
// (ADR 0020). Both empty means anonymous access.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Credential secret keys, shared with the registry's validation.
const (
	CredAccessKeyID     = "accessKeyId"
	CredSecretAccessKey = "secretAccessKey"
	CredSessionToken    = "sessionToken"
)

// Validate checks the config is self-consistent without touching the network.
func (c Config) Validate() error {
	if c.Bucket == "" {
		return errors.New("bucket is required")
	}
	if _, _, err := c.hostAndTLS(); err != nil {
		return err
	}
	if _, err := backend.CleanPrefix(c.Prefix); err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	return nil
}

func (c Config) hostAndTLS() (host string, secure bool, err error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		return "s3.amazonaws.com", true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false, fmt.Errorf("endpoint %q must be a full http(s):// URL", endpoint)
	}
	if u.Path != "" && u.Path != "/" {
		return "", false, fmt.Errorf("endpoint %q must not include a path", endpoint)
	}
	return u.Host, u.Scheme == "https", nil
}

// Backend is a backend.Backend over one bucket (optionally confined to a prefix).
type Backend struct {
	client *minio.Client
	core   minio.Core
	bucket string
	prefix string // "" or ends in "/"
}

// New builds a client for cfg. It doesn't contact the service; Check does.
func New(cfg Config, creds Credentials) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	host, secure, _ := cfg.hostAndTLS()
	prefix, _ := backend.CleanPrefix(cfg.Prefix)

	var provider *credentials.Credentials
	if creds.AccessKeyID != "" || creds.SecretAccessKey != "" {
		provider = credentials.NewStaticV4(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken)
	} else {
		provider = credentials.NewStaticV4("", "", "") // anonymous
	}

	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        provider,
		Secure:       secure,
		Region:       cfg.Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("creating s3 client: %w", err)
	}
	return &Backend{client: client, core: minio.Core{Client: client}, bucket: cfg.Bucket, prefix: prefix}, nil
}

func (b *Backend) Check(ctx context.Context) error {
	// A one-key list proves the endpoint is reachable, the bucket exists, and the
	// credentials can list — the same permission browsing needs. BucketExists alone
	// would pass for credentials that can't actually list anything.
	if _, err := b.core.ListObjectsV2(b.bucket, b.prefix, "", "", "/", 1); err != nil {
		return describe(err)
	}
	return nil
}

func (b *Backend) List(ctx context.Context, opts backend.ListOptions) (backend.ListResult, error) {
	prefix, err := backend.CleanPrefix(opts.Prefix)
	if err != nil {
		return backend.ListResult{}, err
	}
	delimiter := "/"
	if opts.Recursive {
		delimiter = ""
	}

	res, err := b.core.ListObjectsV2(b.bucket, b.prefix+prefix, "", opts.Cursor, delimiter, opts.EffectiveLimit())
	if err != nil {
		return backend.ListResult{}, describe(err)
	}

	out := backend.ListResult{Entries: make([]backend.ObjectInfo, 0, len(res.Contents)+len(res.CommonPrefixes))}
	for _, cp := range res.CommonPrefixes {
		out.Entries = append(out.Entries, backend.ObjectInfo{Path: strings.TrimPrefix(cp.Prefix, b.prefix), IsDir: true})
	}
	for _, obj := range res.Contents {
		// Some tools create zero-byte "folder marker" keys ending in "/"; they aren't
		// objects, and in delimited mode they'd duplicate the common prefix entry.
		if strings.HasSuffix(obj.Key, "/") && !(opts.Recursive && opts.IncludeFolderMarkers) {
			continue
		}
		out.Entries = append(out.Entries, backend.ObjectInfo{
			Path:        strings.TrimPrefix(obj.Key, b.prefix),
			Size:        obj.Size,
			ModTime:     obj.LastModified.UTC(),
			ContentType: obj.ContentType,
		})
	}
	if res.IsTruncated {
		out.NextCursor = res.NextContinuationToken
	}
	return out, nil
}

func (b *Backend) Read(ctx context.Context, p string) (io.ReadCloser, backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return nil, backend.ObjectInfo{}, err
	}
	obj, err := b.client.GetObject(ctx, b.bucket, b.prefix+clean, minio.GetObjectOptions{})
	if err != nil {
		return nil, backend.ObjectInfo{}, describe(err)
	}
	// GetObject is lazy — nothing has hit the wire yet. Stat forces the request so a
	// missing object surfaces here as ErrNotFound rather than as a read error later.
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, backend.ObjectInfo{}, describe(err)
	}
	return obj, backend.ObjectInfo{Path: clean, Size: st.Size, ModTime: st.LastModified.UTC(), ContentType: st.ContentType}, nil
}

func (b *Backend) Write(ctx context.Context, p string, r io.Reader, opts backend.WriteOptions) (backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return backend.ObjectInfo{}, err
	}
	// Size -1 streams with a multipart upload, so an arbitrarily large body never
	// has to be buffered in memory or on disk. An upload that fails part-way is
	// aborted by the client and never becomes visible.
	info, err := b.client.PutObject(ctx, b.bucket, b.prefix+clean, r, -1, minio.PutObjectOptions{ContentType: opts.ContentType})
	if err != nil {
		return backend.ObjectInfo{}, describe(err)
	}
	return backend.ObjectInfo{Path: clean, Size: info.Size, ModTime: info.LastModified.UTC(), ContentType: opts.ContentType}, nil
}

// ---- folder and mutation operations (ADR 0038) ----

var _ backend.FlatStore = (*Backend)(nil)

func (b *Backend) Mkdir(ctx context.Context, path string) error {
	folder, err := backend.CleanFolder(path)
	if err != nil {
		return err
	}
	// The conventional zero-byte "folder/" placeholder every object-store browser uses.
	_, err = b.client.PutObject(ctx, b.bucket, b.prefix+folder, strings.NewReader(""), 0, minio.PutObjectOptions{})
	return describe(err)
}

func (b *Backend) DeleteObject(ctx context.Context, path string) error {
	clean, err := backend.CleanPath(path)
	if err != nil {
		return err
	}
	// S3 reports success when deleting a missing key, so check first to honour ErrNotFound.
	if ok, err := b.KeyExists(ctx, clean); err != nil {
		return err
	} else if !ok {
		return backend.ErrNotFound
	}
	return b.DeleteKey(ctx, clean)
}

func (b *Backend) DeleteFolder(ctx context.Context, path string) (int, error) {
	folder, err := backend.CleanFolder(path)
	if err != nil {
		return 0, err
	}
	return backend.DeleteFolderFlat(ctx, b, folder)
}

func (b *Backend) MoveObject(ctx context.Context, from, to string) error {
	f, err := backend.CleanPath(from)
	if err != nil {
		return err
	}
	t, err := backend.CleanPath(to)
	if err != nil {
		return err
	}
	return backend.MoveObjectFlat(ctx, b, f, t)
}

func (b *Backend) MoveFolder(ctx context.Context, from, to string) error {
	f, err := backend.CleanFolder(from)
	if err != nil {
		return err
	}
	t, err := backend.CleanFolder(to)
	if err != nil {
		return err
	}
	return backend.MoveFolderFlat(ctx, b, f, t)
}

func (b *Backend) CopyKey(ctx context.Context, from, to string) error {
	// Server-side copy: the bytes never pass through booth-storage. The client switches to
	// a multipart compose by itself for objects above the single-copy size limit.
	_, err := b.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: b.bucket, Object: b.prefix + to},
		minio.CopySrcOptions{Bucket: b.bucket, Object: b.prefix + from})
	return describe(err)
}

func (b *Backend) DeleteKey(ctx context.Context, key string) error {
	return describe(b.client.RemoveObject(ctx, b.bucket, b.prefix+key, minio.RemoveObjectOptions{}))
}

func (b *Backend) KeyExists(ctx context.Context, key string) (bool, error) {
	_, err := b.client.StatObject(ctx, b.bucket, b.prefix+key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if errors.Is(describe(err), backend.ErrNotFound) {
		return false, nil
	}
	return false, describe(err)
}

// describe maps service errors onto the package-independent errors callers branch on,
// and turns the SDK's terse messages into something an admin can act on.
func describe(err error) error {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		switch resp.Code {
		case "NoSuchKey":
			return backend.ErrNotFound
		case "NoSuchBucket":
			return fmt.Errorf("bucket does not exist: %s", resp.BucketName)
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch":
			return fmt.Errorf("access denied by the storage service (%s) — check the credentials and bucket permissions", resp.Code)
		}
	}
	return err
}
