// Package azure implements backend.Backend over an Azure Blob Storage container
// (ADR 0013).
package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/projectbooth/booth-storage/internal/backend"
)

// Config is the azure kind's non-secret registration config.
type Config struct {
	AccountName string `json:"accountName"`
	Container   string `json:"container"`
	// Endpoint overrides the blob service URL. Empty means public Azure
	// (https://<account>.blob.core.windows.net); set it for sovereign clouds, private
	// endpoints, or an emulator such as Azurite (which includes the account in the path,
	// e.g. http://127.0.0.1:10000/devstoreaccount1).
	Endpoint string `json:"endpoint,omitempty"`
	// Prefix confines this backend to one blob-name prefix inside the container.
	Prefix string `json:"prefix,omitempty"`
}

// Credential secret keys, shared with the registry's validation. Exactly one is used.
const (
	CredAccountKey = "accountKey" // storage account shared key (base64)
	CredSASToken   = "sasToken"   // a SAS token, with or without the leading "?"
)

// Credentials are the azure kind's secret fields, delivered from a Kubernetes Secret
// (ADR 0020).
type Credentials struct {
	AccountKey string
	SASToken   string
}

var (
	accountNameRE = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
	containerRE   = regexp.MustCompile(`^[a-z0-9](-?[a-z0-9])*$`)
)

// Validate checks the config is self-consistent without touching the network.
func (c Config) Validate() error {
	if !accountNameRE.MatchString(c.AccountName) {
		return errors.New("accountName must be 3-24 lowercase letters and digits")
	}
	if len(c.Container) < 3 || len(c.Container) > 63 || !containerRE.MatchString(c.Container) {
		return errors.New("container must be 3-63 characters: lowercase letters, digits, and single hyphens")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("endpoint %q must be a full http(s):// URL", c.Endpoint)
		}
		if u.RawQuery != "" {
			return errors.New("endpoint must not contain a query string; supply SAS tokens as credentials")
		}
	}
	if _, err := backend.CleanPrefix(c.Prefix); err != nil {
		return fmt.Errorf("prefix: %w", err)
	}
	return nil
}

func (c Config) containerURL() string {
	base := strings.TrimRight(c.Endpoint, "/")
	if base == "" {
		base = fmt.Sprintf("https://%s.blob.core.windows.net", c.AccountName)
	}
	return base + "/" + c.Container
}

// Backend is a backend.Backend over one container (optionally confined to a prefix).
type Backend struct {
	container *container.Client
	prefix    string // "" or ends in "/"
}

// New builds a client for cfg. It doesn't contact the service; Check does.
func New(cfg Config, creds Credentials) (*Backend, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	prefix, _ := backend.CleanPrefix(cfg.Prefix)

	var (
		client *container.Client
		err    error
	)
	switch {
	case creds.AccountKey != "" && creds.SASToken != "":
		return nil, errors.New("provide either accountKey or sasToken, not both")
	case creds.AccountKey != "":
		var cred *container.SharedKeyCredential
		cred, err = container.NewSharedKeyCredential(cfg.AccountName, creds.AccountKey)
		if err != nil {
			return nil, fmt.Errorf("invalid accountKey (must be the base64 key from the Azure portal): %w", err)
		}
		client, err = container.NewClientWithSharedKeyCredential(cfg.containerURL(), cred, nil)
	case creds.SASToken != "":
		sas := strings.TrimPrefix(creds.SASToken, "?")
		client, err = container.NewClientWithNoCredential(cfg.containerURL()+"?"+sas, nil)
	default:
		return nil, errors.New("a credential is required: provide accountKey or sasToken")
	}
	if err != nil {
		return nil, fmt.Errorf("creating azure client: %w", err)
	}
	return &Backend{container: client, prefix: prefix}, nil
}

func (b *Backend) Check(ctx context.Context) error {
	// Listing one blob proves the container exists and the credential can list — the
	// permission browsing needs. GetProperties on the container would demand a broader
	// permission than a list-only SAS grants.
	max := int32(1)
	pager := b.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &b.prefix, MaxResults: &max})
	if _, err := pager.NextPage(ctx); err != nil {
		return describe(err)
	}
	return nil
}

// withMetadata asks the listing to include blob metadata, needed to recognise the directory
// marker blobs that hierarchical-namespace (ADR 0037) accounts expose through the Blob API.
var withMetadata = container.ListBlobsInclude{Metadata: true}

// isFolderMarker reports whether a listed blob is really a directory placeholder. Accounts
// with a hierarchical namespace have true directories; through the Blob API they can
// appear as zero-length blobs tagged hdi_isfolder=true, which must not be shown as files.
// Real folders still surface as prefixes in a delimited listing, so nothing is lost.
func isFolderMarker(md map[string]*string) bool {
	for k, v := range md {
		if strings.EqualFold(k, "hdi_isfolder") && v != nil && strings.EqualFold(*v, "true") {
			return true
		}
	}
	return false
}

func (b *Backend) List(ctx context.Context, opts backend.ListOptions) (backend.ListResult, error) {
	prefix, err := backend.CleanPrefix(opts.Prefix)
	if err != nil {
		return backend.ListResult{}, err
	}
	full := b.prefix + prefix
	max := int32(opts.EffectiveLimit())
	var marker *string
	if opts.Cursor != "" {
		marker = &opts.Cursor
	}

	out := backend.ListResult{Entries: []backend.ObjectInfo{}}

	var (
		items    []*container.BlobItem
		prefixes []*container.BlobPrefix
		next     *string
	)
	if opts.Recursive {
		pager := b.container.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &full, MaxResults: &max, Marker: marker, Include: withMetadata})
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return backend.ListResult{}, describe(err)
		}
		items, next = resp.Segment.BlobItems, resp.NextMarker
	} else {
		pager := b.container.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{Prefix: &full, MaxResults: &max, Marker: marker, Include: withMetadata})
		resp, err := pager.NextPage(ctx)
		if err != nil {
			return backend.ListResult{}, describe(err)
		}
		items, prefixes, next = resp.Segment.BlobItems, resp.Segment.BlobPrefixes, resp.NextMarker
	}

	for _, p := range prefixes {
		if p.Name != nil {
			out.Entries = append(out.Entries, backend.ObjectInfo{Path: strings.TrimPrefix(*p.Name, b.prefix), IsDir: true})
		}
	}
	for _, it := range items {
		if it.Name == nil {
			continue
		}
		if (strings.HasSuffix(*it.Name, "/") || isFolderMarker(it.Metadata)) && !(opts.Recursive && opts.IncludeFolderMarkers) {
			continue // folder-marker blobs aren't objects
		}
		info := backend.ObjectInfo{Path: strings.TrimPrefix(*it.Name, b.prefix)}
		if it.Properties != nil {
			if it.Properties.ContentLength != nil {
				info.Size = *it.Properties.ContentLength
			}
			if it.Properties.LastModified != nil {
				info.ModTime = it.Properties.LastModified.UTC()
			}
			if it.Properties.ContentType != nil {
				info.ContentType = *it.Properties.ContentType
			}
		}
		out.Entries = append(out.Entries, info)
	}
	if next != nil {
		out.NextCursor = *next
	}
	return out, nil
}

func (b *Backend) Read(ctx context.Context, p string) (io.ReadCloser, backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return nil, backend.ObjectInfo{}, err
	}
	resp, err := b.container.NewBlobClient(b.prefix+clean).DownloadStream(ctx, nil)
	if err != nil {
		return nil, backend.ObjectInfo{}, describe(err)
	}
	info := backend.ObjectInfo{Path: clean}
	if resp.ContentLength != nil {
		info.Size = *resp.ContentLength
	}
	if resp.LastModified != nil {
		info.ModTime = resp.LastModified.UTC()
	}
	if resp.ContentType != nil {
		info.ContentType = *resp.ContentType
	}
	return resp.Body, info, nil
}

func (b *Backend) Write(ctx context.Context, p string, r io.Reader, opts backend.WriteOptions) (backend.ObjectInfo, error) {
	clean, err := backend.CleanPath(p)
	if err != nil {
		return backend.ObjectInfo{}, err
	}
	blockClient := b.container.NewBlockBlobClient(b.prefix + clean)

	// UploadStream stages blocks and commits the block list only once the whole stream
	// has been read, so a failed upload leaves uncommitted blocks that expire on their
	// own and never a visible partial blob.
	uploadOpts := &blockblob.UploadStreamOptions{}
	if opts.ContentType != "" {
		uploadOpts.HTTPHeaders = &blob.HTTPHeaders{BlobContentType: &opts.ContentType}
	}
	if _, err := blockClient.UploadStream(ctx, r, uploadOpts); err != nil {
		return backend.ObjectInfo{}, describe(err)
	}

	props, err := blockClient.GetProperties(ctx, nil)
	if err != nil {
		return backend.ObjectInfo{}, describe(err)
	}
	info := backend.ObjectInfo{Path: clean}
	if props.ContentLength != nil {
		info.Size = *props.ContentLength
	}
	if props.LastModified != nil {
		info.ModTime = props.LastModified.UTC()
	}
	if props.ContentType != nil {
		info.ContentType = *props.ContentType
	}
	return info, nil
}

// ---- folder and mutation operations (ADR 0038) ----

var _ backend.FlatStore = (*Backend)(nil)

func (b *Backend) Mkdir(ctx context.Context, path string) error {
	folder, err := backend.CleanFolder(path)
	if err != nil {
		return err
	}
	// The zero-byte "folder/" placeholder blob. On a hierarchical-namespace account this
	// would ideally be a real directory (ADR 0038); that mode can't be exercised here (no
	// emulator), so the placeholder convention is used for both — see README "Not done".
	_, err = b.container.NewBlockBlobClient(b.prefix+folder).Upload(ctx, streaming.NopCloser(strings.NewReader("")), nil)
	return describe(err)
}

func (b *Backend) DeleteObject(ctx context.Context, path string) error {
	clean, err := backend.CleanPath(path)
	if err != nil {
		return err
	}
	_, err = b.container.NewBlobClient(b.prefix+clean).Delete(ctx, nil) // BlobNotFound -> ErrNotFound
	return describe(err)
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
	src := b.container.NewBlobClient(b.prefix + from)
	dst := b.container.NewBlobClient(b.prefix + to)
	// Server-side copy from the source's own URL (authorized by the same credential, as
	// both blobs are in one account). Copy Blob may complete asynchronously for large
	// blobs, so wait for it to finish before reporting success.
	if _, err := dst.StartCopyFromURL(ctx, src.URL(), nil); err != nil {
		return describe(err)
	}
	for {
		props, err := dst.GetProperties(ctx, nil)
		if err != nil {
			return describe(err)
		}
		if props.CopyStatus == nil || *props.CopyStatus == blob.CopyStatusTypeSuccess {
			return nil
		}
		if *props.CopyStatus != blob.CopyStatusTypePending {
			return fmt.Errorf("copy of %q did not complete (status %s)", from, *props.CopyStatus)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (b *Backend) DeleteKey(ctx context.Context, key string) error {
	_, err := b.container.NewBlobClient(b.prefix+key).Delete(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return nil
	}
	return describe(err)
}

func (b *Backend) KeyExists(ctx context.Context, key string) (bool, error) {
	_, err := b.container.NewBlobClient(b.prefix+key).GetProperties(ctx, nil)
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return false, nil
	}
	return err == nil, describe(err)
}

// describe maps service errors onto the package-independent errors callers branch on,
// with messages an admin can act on.
func describe(err error) error {
	switch {
	case bloberror.HasCode(err, bloberror.BlobNotFound):
		return backend.ErrNotFound
	case bloberror.HasCode(err, bloberror.ContainerNotFound):
		return errors.New("container does not exist")
	case bloberror.HasCode(err, bloberror.AuthenticationFailed, bloberror.AuthorizationFailure,
		bloberror.AuthorizationPermissionMismatch, bloberror.InvalidAuthenticationInfo):
		return errors.New("access denied by Azure Blob Storage — check the account key / SAS token and its permissions")
	}
	return err
}
