// Copyright 2018 The Go Cloud Development Kit Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package blob provides an easy and portable way to interact with blobs
// within a storage location, hereafter called a "bucket".
//
// It supports operations like reading and writing blobs (using standard
// interfaces from the io package), deleting blobs, and listing blobs in a
// bucket.
//
// Subpackages contain distinct implementations of blob for various providers,
// including Cloud and on-prem solutions. For example, "fileblob" supports
// blobs backed by a filesystem. Your application should import one of these
// provider-specific subpackages and use its exported function(s) to create a
// *Bucket; do not use the NewBucket function in this package. For example:
//
//  bucket, err := fileblob.OpenBucket("path/to/dir", nil)
//  if err != nil {
//      return fmt.Errorf("could not open bucket: %v", err)
//  }
//  buf, err := bucket.ReadAll(context.Background(), "myfile.txt")
//  ...
//
// Then, write your application code using the *Bucket type. You can easily
// reconfigure your initialization code to choose a different provider.
// You can develop your application locally using fileblob, or deploy it to
// multiple Cloud providers. You may find http://github.com/google/wire useful
// for managing your initialization code.
//
// Alternatively, you can construct a *Bucket using blob.Open by providing
// a URL that's supported by a blob subpackage that you have linked
// in to your application.
//
//
// Errors
//
// The errors returned from this package can be inspected in several ways:
//
// The Code function from gocloud.dev/gcerrors will return an error code, also
// defined in that package, when invoked on an error.
//
// The Bucket.ErrorAs method can retrieve the driver error underlying the returned
// error.
//
//
// OpenCensus Integration
//
// OpenCensus supports tracing and metric collection for multiple languages and
// backend providers. See https://opencensus.io.
//
// This API collects OpenCensus traces and metrics for the following methods:
//  - Attributes
//  - Delete
//  - NewRangeReader, from creation until the call to Close. (NewReader and ReadAll
//    are included because they call NewRangeReader.)
//  - NewWriter, from creation until the call to Close.
// All trace and metric names begin with the package import path.
// The traces add the method name.
// For example, "gocloud.dev/blob/Attributes".
// The metrics are "completed_calls", a count of completed method calls by provider,
// method and status (error code); and "latency", a distribution of method latency
// by provider and method.
// For example, "gocloud.dev/blob/latency".
//
// It also collects the following metrics:
// - gocloud.dev/blob/bytes_read: the total number of bytes read, by provider.
// - gocloud.dev/blob/bytes_written: the total number of bytes written, by provider.
//
// To enable trace collection in your application, see "Configure Exporter" at
// https://opencensus.io/quickstart/go/tracing.
// To enable metric collection in your application, see "Exporting stats" at
// https://opencensus.io/quickstart/go/metrics.
package blob // import "gocloud.dev/blob"

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/stats/view"
	"go.opencensus.io/tag"
	"gocloud.dev/blob/driver"
	"gocloud.dev/internal/gcerr"
	"gocloud.dev/internal/oc"
)

// Reader reads bytes from a blob.
// It implements io.ReadCloser, and must be closed after
// reads are finished.
type Reader struct {
	b        driver.Bucket
	r        driver.Reader
	end      func(error) // called at Close to finish trace and metric collection
	provider string      // for metric collection
}

// Read implements io.Reader (https://golang.org/pkg/io/#Reader).
func (r *Reader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	stats.RecordWithTags(context.Background(), []tag.Mutator{tag.Upsert(oc.ProviderKey, r.provider)},
		bytesReadMeasure.M(int64(n)))
	return n, wrapError(r.b, err)
}

// Close implements io.Closer (https://golang.org/pkg/io/#Closer).
func (r *Reader) Close() error {
	err := wrapError(r.b, r.r.Close())
	r.end(err)
	return err
}

// ContentType returns the MIME type of the blob.
func (r *Reader) ContentType() string {
	return r.r.Attributes().ContentType
}

// ModTime returns the time the blob was last modified.
func (r *Reader) ModTime() time.Time {
	return r.r.Attributes().ModTime
}

// Size returns the size of the blob content in bytes.
func (r *Reader) Size() int64 {
	return r.r.Attributes().Size
}

// As converts i to provider-specific types.
// See Bucket.As for more details.
func (r *Reader) As(i interface{}) bool {
	return r.r.As(i)
}

// Attributes contains attributes about a blob.
type Attributes struct {
	// CacheControl specifies caching attributes that providers may use
	// when serving the blob.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control
	CacheControl string
	// ContentDisposition specifies whether the blob content is expected to be
	// displayed inline or as an attachment.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Disposition
	ContentDisposition string
	// ContentEncoding specifies the encoding used for the blob's content, if any.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Encoding
	ContentEncoding string
	// ContentLanguage specifies the language used in the blob's content, if any.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Language
	ContentLanguage string
	// ContentType is the MIME type of the blob. It will not be empty.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Type
	ContentType string
	// Metadata holds key/value pairs associated with the blob.
	// Keys are guaranteed to be in lowercase, even if the backend provider
	// has case-sensitive keys (although note that Metadata written via
	// this package will always be lowercased). If there are duplicate
	// case-insensitive keys (e.g., "foo" and "FOO"), only one value
	// will be kept, and it is undefined which one.
	Metadata map[string]string
	// ModTime is the time the blob was last modified.
	ModTime time.Time
	// Size is the size of the blob's content in bytes.
	Size int64
	// MD5 is an MD5 hash of the blob contents or nil if not available.
	MD5 []byte

	asFunc func(interface{}) bool
}

// As converts i to provider-specific types.
// See Bucket.As for more details.
func (a *Attributes) As(i interface{}) bool {
	if a.asFunc == nil {
		return false
	}
	return a.asFunc(i)
}

// Writer writes bytes to a blob.
//
// It implements io.WriteCloser (https://golang.org/pkg/io/#Closer), and must be
// closed after all writes are done.
type Writer struct {
	b          driver.Bucket
	w          driver.Writer
	end        func(error) // called at Close to finish trace and metric collection
	cancel     func()      // cancels the ctx provided to NewTypedWriter if contentMD5 verification fails
	contentMD5 []byte
	md5hash    hash.Hash
	provider   string // for metric collection

	// These fields exist only when w is not yet created.
	//
	// A ctx is stored in the Writer since we need to pass it into NewTypedWriter
	// when we finish detecting the content type of the blob and create the
	// underlying driver.Writer. This step happens inside Write or Close and
	// neither of them take a context.Context as an argument. The ctx is set
	// to nil after we have passed it to NewTypedWriter.
	ctx  context.Context
	key  string
	opts *driver.WriterOptions
	buf  *bytes.Buffer
}

// sniffLen is the byte size of Writer.buf used to detect content-type.
const sniffLen = 512

// Write implements the io.Writer interface (https://golang.org/pkg/io/#Writer).
//
// Writes may happen asynchronously, so the returned error can be nil
// even if the actual write eventually fails. The write is only guaranteed to
// have succeeded if Close returns no error.
func (w *Writer) Write(p []byte) (n int, err error) {
	if len(w.contentMD5) > 0 {
		if _, err := w.md5hash.Write(p); err != nil {
			return 0, err
		}
	}
	if w.w != nil {
		return w.write(p)
	}

	// If w is not yet created due to no content-type being passed in, try to sniff
	// the MIME type based on at most 512 bytes of the blob content of p.

	// Detect the content-type directly if the first chunk is at least 512 bytes.
	if w.buf.Len() == 0 && len(p) >= sniffLen {
		return w.open(p)
	}

	// Store p in w.buf and detect the content-type when the size of content in
	// w.buf is at least 512 bytes.
	w.buf.Write(p)
	if w.buf.Len() >= sniffLen {
		return w.open(w.buf.Bytes())
	}
	return len(p), nil
}

// Close closes the blob writer. The write operation is not guaranteed to have succeeded until
// Close returns with no error.
// Close may return an error if the context provided to create the Writer is
// canceled or reaches its deadline.
func (w *Writer) Close() (err error) {
	defer func() { w.end(err) }()
	if len(w.contentMD5) > 0 {
		// Verify the MD5 hash of what was written matches the ContentMD5 provided
		// by the user.
		md5sum := w.md5hash.Sum(nil)
		if !bytes.Equal(md5sum, w.contentMD5) {
			// No match! Return an error, but first cancel the context and call the
			// driver's Close function to ensure the write is aborted.
			w.cancel()
			if w.w != nil {
				_ = w.w.Close()
			}
			return fmt.Errorf("blob: the ContentMD5 you specified (%X) did not match what was written (%X)", w.contentMD5, md5sum)
		}
	}

	defer w.cancel()
	if w.w != nil {
		return wrapError(w.b, w.w.Close())
	}
	if _, err := w.open(w.buf.Bytes()); err != nil {
		return err
	}
	return wrapError(w.b, w.w.Close())
}

// open tries to detect the MIME type of p and write it to the blob.
// The error it returns is wrapped.
func (w *Writer) open(p []byte) (int, error) {
	ct := http.DetectContentType(p)
	var err error
	if w.w, err = w.b.NewTypedWriter(w.ctx, w.key, ct, w.opts); err != nil {
		return 0, wrapError(w.b, err)
	}
	w.buf = nil
	w.ctx = nil
	w.key = ""
	w.opts = nil
	return w.write(p)
}

func (w *Writer) write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	stats.RecordWithTags(context.Background(), []tag.Mutator{tag.Upsert(oc.ProviderKey, w.provider)},
		bytesWrittenMeasure.M(int64(n)))
	return n, wrapError(w.b, err)
}

// ListOptions sets options for listing blobs via Bucket.List.
type ListOptions struct {
	// Prefix indicates that only blobs with a key starting with this prefix
	// should be returned.
	Prefix string
	// Delimiter sets the delimiter used to define a hierarchical namespace,
	// like a filesystem with "directories". It is highly recommended that you
	// use "" or "/" as the Delimiter. Other values should work through this API,
	// but provider UIs generally assume "/".
	//
	// An empty delimiter means that the bucket is treated as a single flat
	// namespace.
	//
	// A non-empty delimiter means that any result with the delimiter in its key
	// after Prefix is stripped will be returned with ListObject.IsDir = true,
	// ListObject.Key truncated after the delimiter, and zero values for other
	// ListObject fields. These results represent "directories". Multiple results
	// in a "directory" are returned as a single result.
	Delimiter string

	// BeforeList is a callback that will be called before each call to the
	// the underlying provider's list functionality.
	// asFunc converts its argument to provider-specific types.
	// See Bucket.As for more details.
	BeforeList func(asFunc func(interface{}) bool) error
}

// ListIterator iterates over List results.
type ListIterator struct {
	b       driver.Bucket
	opts    *driver.ListOptions
	page    *driver.ListPage
	nextIdx int
}

// Next returns a *ListObject for the next blob. It returns (nil, io.EOF) if
// there are no more.
func (i *ListIterator) Next(ctx context.Context) (*ListObject, error) {
	if i.page != nil {
		// We've already got a page of results.
		if i.nextIdx < len(i.page.Objects) {
			// Next object is in the page; return it.
			dobj := i.page.Objects[i.nextIdx]
			i.nextIdx++
			return &ListObject{
				Key:     dobj.Key,
				ModTime: dobj.ModTime,
				Size:    dobj.Size,
				MD5:     dobj.MD5,
				IsDir:   dobj.IsDir,
				asFunc:  dobj.AsFunc,
			}, nil
		}
		if len(i.page.NextPageToken) == 0 {
			// Done with current page, and there are no more; return io.EOF.
			return nil, io.EOF
		}
		// We need to load the next page.
		i.opts.PageToken = i.page.NextPageToken
	}
	// Loading a new page.
	p, err := i.b.ListPaged(ctx, i.opts)
	if err != nil {
		return nil, wrapError(i.b, err)
	}
	i.page = p
	i.nextIdx = 0
	return i.Next(ctx)
}

// ListObject represents a single blob returned from List.
type ListObject struct {
	// Key is the key for this blob.
	Key string
	// ModTime is the time the blob was last modified.
	ModTime time.Time
	// Size is the size of the blob's content in bytes.
	Size int64
	// MD5 is an MD5 hash of the blob contents or nil if not available.
	MD5 []byte
	// IsDir indicates that this result represents a "directory" in the
	// hierarchical namespace, ending in ListOptions.Delimiter. Key can be
	// passed as ListOptions.Prefix to list items in the "directory".
	// Fields other than Key and IsDir will not be set if IsDir is true.
	IsDir bool

	asFunc func(interface{}) bool
}

// As converts i to provider-specific types.
// See Bucket.As for more details.
func (o *ListObject) As(i interface{}) bool {
	if o.asFunc == nil {
		return false
	}
	return o.asFunc(i)
}

// Bucket provides an easy and portable way to interact with blobs
// within a "bucket", including read, write, and list operations.
// To create a Bucket, use constructors found in provider-specific
// subpackages.
type Bucket struct {
	b      driver.Bucket
	tracer *oc.Tracer
}

const pkgName = "gocloud.dev/blob"

var (
	latencyMeasure      = oc.LatencyMeasure(pkgName)
	bytesReadMeasure    = stats.Int64(pkgName+"/bytes_read", "Total bytes read", stats.UnitBytes)
	bytesWrittenMeasure = stats.Int64(pkgName+"/bytes_written", "Total bytes written", stats.UnitBytes)

	// OpenCensusViews are predefined views for OpenCensus metrics.
	// The views include counts and latency distributions for API method calls,
	// and total bytes read and written.
	// See the example at https://godoc.org/go.opencensus.io/stats/view for usage.
	OpenCensusViews = append(
		oc.Views(pkgName, latencyMeasure),
		&view.View{
			Name:        pkgName + "/bytes_read",
			Measure:     bytesReadMeasure,
			Description: "Sum of bytes read from the provider service.",
			TagKeys:     []tag.Key{oc.ProviderKey},
			Aggregation: view.Sum(),
		},
		&view.View{
			Name:        pkgName + "/bytes_written",
			Measure:     bytesWrittenMeasure,
			Description: "Sum of bytes written to the provider service.",
			TagKeys:     []tag.Key{oc.ProviderKey},
			Aggregation: view.Sum(),
		})
)

// NewBucket is intended for use by provider implementations.
var NewBucket = newBucket

// newBucket creates a new *Bucket based on a specific driver implementation.
// End users should use subpackages to construct a *Bucket instead of this
// function; see the package documentation for details.
func newBucket(b driver.Bucket) *Bucket {
	return &Bucket{
		b: b,
		tracer: &oc.Tracer{
			Package:        pkgName,
			Provider:       oc.ProviderName(b),
			LatencyMeasure: latencyMeasure,
		},
	}
}

// As converts i to provider-specific types.
//
// This function (and the other As functions in this package) are inherently
// provider-specific, and using them will make that part of your application
// non-portable, so use with care.
//
// See the documentation for the subpackage used to instantiate Bucket to see
// which type(s) are supported.
//
// Usage:
//
// 1. Declare a variable of the provider-specific type you want to access.
//
// 2. Pass a pointer to it to As.
//
// 3. If the type is supported, As will return true and copy the
// provider-specific type into your variable. Otherwise, it will return false.
//
// Provider-specific types that are intended to be mutable will be exposed
// as a pointer to the underlying type.
//
// See
// https://github.com/google/go-cloud/blob/master/internal/docs/design.md#as
// for more background.
func (b *Bucket) As(i interface{}) bool {
	if i == nil {
		return false
	}
	return b.b.As(i)
}

// ErrorAs converts i to provider-specific types.
// ErrorAs panics if i is nil or not a pointer.
// ErrorAs returns false if err == nil.
// See Bucket.As for more details.
func (b *Bucket) ErrorAs(err error, i interface{}) bool {
	return gcerr.ErrorAs(err, i, b.b.ErrorAs)
}

// ReadAll is a shortcut for creating a Reader via NewReader with nil
// ReaderOptions, and reading the entire blob.
func (b *Bucket) ReadAll(ctx context.Context, key string) (_ []byte, err error) {
	r, err := b.NewReader(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return ioutil.ReadAll(r)
}

// List returns a ListIterator that can be used to iterate over blobs in a
// bucket, in lexicographical order of UTF-8 encoded keys. The underlying
// implementation fetches results in pages.
//
// A nil ListOptions is treated the same as the zero value.
//
// List is not guaranteed to include all recently-written blobs;
// some providers are only eventually consistent.
func (b *Bucket) List(opts *ListOptions) *ListIterator {
	if opts == nil {
		opts = &ListOptions{}
	}
	dopts := &driver.ListOptions{
		Prefix:     opts.Prefix,
		Delimiter:  opts.Delimiter,
		BeforeList: opts.BeforeList,
	}
	return &ListIterator{b: b.b, opts: dopts}
}

// Attributes returns attributes for the blob stored at key.
//
// If the blob does not exist, Attributes returns an error for which
// gcerrors.Code will return gcerrors.NotFound.
func (b *Bucket) Attributes(ctx context.Context, key string) (_ Attributes, err error) {
	ctx = b.tracer.Start(ctx, "Attributes")
	defer func() { b.tracer.End(ctx, err) }()

	a, err := b.b.Attributes(ctx, key)
	if err != nil {
		return Attributes{}, wrapError(b.b, err)
	}
	var md map[string]string
	if len(a.Metadata) > 0 {
		// Providers are inconsistent, but at least some treat keys
		// as case-insensitive. To make the behavior consistent, we
		// force-lowercase them when writing and reading.
		md = make(map[string]string, len(a.Metadata))
		for k, v := range a.Metadata {
			md[strings.ToLower(k)] = v
		}
	}
	return Attributes{
		CacheControl:       a.CacheControl,
		ContentDisposition: a.ContentDisposition,
		ContentEncoding:    a.ContentEncoding,
		ContentLanguage:    a.ContentLanguage,
		ContentType:        a.ContentType,
		Metadata:           md,
		ModTime:            a.ModTime,
		Size:               a.Size,
		MD5:                a.MD5,
		asFunc:             a.AsFunc,
	}, nil
}

// NewReader is a shortcut for NewRangedReader with offset=0 and length=-1.
func (b *Bucket) NewReader(ctx context.Context, key string, opts *ReaderOptions) (*Reader, error) {
	return b.NewRangeReader(ctx, key, 0, -1, opts)
}

// NewRangeReader returns a Reader to read content from the blob stored at key.
// It reads at most length bytes starting at offset (>= 0).
// If length is negative, it will read till the end of the blob.
//
// If the blob does not exist, NewRangeReader returns an error for which
// gcerrors.Code will return gcerrors.NotFound. Attributes is a lighter-weight way to
// check for existence.
//
// A nil ReaderOptions is treated the same as the zero value.
//
// The caller must call Close on the returned Reader when done reading.
func (b *Bucket) NewRangeReader(ctx context.Context, key string, offset, length int64, opts *ReaderOptions) (_ *Reader, err error) {
	if offset < 0 {
		return nil, errors.New("blob.NewRangeReader: offset must be non-negative")
	}
	if opts == nil {
		opts = &ReaderOptions{}
	}
	dopts := &driver.ReaderOptions{}
	tctx := b.tracer.Start(ctx, "NewRangeReader")
	defer func() {
		// If err == nil, we handed the end closure off to the returned *Writer; it
		// will be called when the Writer is Closed.
		if err != nil {
			b.tracer.End(tctx, err)
		}
	}()
	r, err := b.b.NewRangeReader(ctx, key, offset, length, dopts)
	if err != nil {
		return nil, wrapError(b.b, err)
	}
	end := func(err error) { b.tracer.End(tctx, err) }
	return &Reader{b: b.b, r: r, end: end, provider: b.tracer.Provider}, nil
}

// WriteAll is a shortcut for creating a Writer via NewWriter and writing p.
func (b *Bucket) WriteAll(ctx context.Context, key string, p []byte, opts *WriterOptions) (err error) {
	w, err := b.NewWriter(ctx, key, opts)
	if err != nil {
		return err
	}
	if _, err := w.Write(p); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// NewWriter returns a Writer that writes to the blob stored at key.
// A nil WriterOptions is treated the same as the zero value.
//
// If a blob with this key already exists, it will be replaced.
// The blob being written is not guaranteed to be readable until Close
// has been called; until then, any previous blob will still be readable.
// Even after Close is called, newly written blobs are not guaranteed to be
// returned from List; some providers are only eventually consistent.
//
// The returned Writer will store ctx for later use in Write and/or Close.
// To abort a write, cancel ctx; otherwise, it must remain open until
// Close is called.
//
// The caller must call Close on the returned Writer, even if the write is
// aborted.
func (b *Bucket) NewWriter(ctx context.Context, key string, opts *WriterOptions) (_ *Writer, err error) {
	var dopts *driver.WriterOptions
	var w driver.Writer
	if opts == nil {
		opts = &WriterOptions{}
	}
	dopts = &driver.WriterOptions{
		CacheControl:       opts.CacheControl,
		ContentDisposition: opts.ContentDisposition,
		ContentEncoding:    opts.ContentEncoding,
		ContentLanguage:    opts.ContentLanguage,
		ContentMD5:         opts.ContentMD5,
		BufferSize:         opts.BufferSize,
		BeforeWrite:        opts.BeforeWrite,
	}
	if len(opts.Metadata) > 0 {
		// Providers are inconsistent, but at least some treat keys
		// as case-insensitive. To make the behavior consistent, we
		// force-lowercase them when writing and reading.
		md := make(map[string]string, len(opts.Metadata))
		for k, v := range opts.Metadata {
			if k == "" {
				return nil, errors.New("blob.NewWriter: WriterOptions.Metadata keys may not be empty strings")
			}
			lowerK := strings.ToLower(k)
			if _, found := md[lowerK]; found {
				return nil, fmt.Errorf("blob.NewWriter: duplicate case-insensitive metadata key %q", lowerK)
			}
			md[lowerK] = v
		}
		dopts.Metadata = md
	}
	ctx, cancel := context.WithCancel(ctx)
	tctx := b.tracer.Start(ctx, "NewWriter")
	end := func(err error) { b.tracer.End(tctx, err) }

	defer func() {
		if err != nil {
			end(err)
		}
	}()

	if opts.ContentType != "" {
		t, p, err := mime.ParseMediaType(opts.ContentType)
		if err != nil {
			cancel()
			return nil, err
		}
		ct := mime.FormatMediaType(t, p)
		w, err = b.b.NewTypedWriter(ctx, key, ct, dopts)
		if err != nil {
			cancel()
			return nil, wrapError(b.b, err)
		}
		return &Writer{
			b:          b.b,
			w:          w,
			end:        end,
			cancel:     cancel,
			contentMD5: opts.ContentMD5,
			md5hash:    md5.New(),
			provider:   b.tracer.Provider,
		}, nil
	}
	return &Writer{
		ctx:        ctx,
		cancel:     cancel,
		b:          b.b,
		end:        end,
		key:        key,
		opts:       dopts,
		buf:        bytes.NewBuffer([]byte{}),
		contentMD5: opts.ContentMD5,
		md5hash:    md5.New(),
		provider:   b.tracer.Provider,
	}, nil
}

// Delete deletes the blob stored at key.
//
// If the blob does not exist, Delete returns an error for which
// gcerrors.Code will return gcerrors.NotFound.
func (b *Bucket) Delete(ctx context.Context, key string) (err error) {
	ctx = b.tracer.Start(ctx, "Delete")
	defer func() { b.tracer.End(ctx, err) }()
	return wrapError(b.b, b.b.Delete(ctx, key))
}

// SignedURL returns a URL that can be used to GET the blob for the duration
// specified in opts.Expiry.
//
// A nil SignedURLOptions is treated the same as the zero value.
//
// It is valid to call SignedURL for a key that does not exist.
//
// If the provider implementation does not support this functionality, SignedURL
// will return an error for which gcerrors.Code will return gcerrors.Unimplemented.
func (b *Bucket) SignedURL(ctx context.Context, key string, opts *SignedURLOptions) (string, error) {
	if opts == nil {
		opts = &SignedURLOptions{}
	}
	if opts.Expiry < 0 {
		return "", errors.New("blob.SignedURL: SignedURLOptions.Expiry must be >= 0")
	}
	if opts.Expiry == 0 {
		opts.Expiry = DefaultSignedURLExpiry
	}
	dopts := driver.SignedURLOptions{
		Expiry: opts.Expiry,
	}
	url, err := b.b.SignedURL(ctx, key, &dopts)
	return url, wrapError(b.b, err)
}

// DefaultSignedURLExpiry is the default duration for SignedURLOptions.Expiry.
const DefaultSignedURLExpiry = 1 * time.Hour

// SignedURLOptions sets options for SignedURL.
type SignedURLOptions struct {
	// Expiry sets how long the returned URL is valid for.
	// Defaults to DefaultSignedURLExpiry.
	Expiry time.Duration
}

// ReaderOptions sets options for NewReader and NewRangedReader.
// It is provided for future extensibility.
type ReaderOptions struct{}

// WriterOptions sets options for NewWriter.
type WriterOptions struct {
	// BufferSize changes the default size in bytes of the chunks that
	// Writer will upload in a single request; larger blobs will be split into
	// multiple requests.
	//
	// This option may be ignored by some provider implementations.
	//
	// If 0, the provider implementation will choose a reasonable default.
	//
	// If the Writer is used to do many small writes concurrently, using a
	// smaller BufferSize may reduce memory usage.
	BufferSize int

	// CacheControl specifies caching attributes that providers may use
	// when serving the blob.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Cache-Control
	CacheControl string

	// ContentDisposition specifies whether the blob content is expected to be
	// displayed inline or as an attachment.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Disposition
	ContentDisposition string

	// ContentEncoding specifies the encoding used for the blob's content, if any.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Encoding
	ContentEncoding string

	// ContentLanguage specifies the language used in the blob's content, if any.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Language
	ContentLanguage string

	// ContentType specifies the MIME type of the blob being written. If not set,
	// it will be inferred from the content using the algorithm described at
	// http://mimesniff.spec.whatwg.org/.
	// https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers/Content-Type
	ContentType string

	// ContentMD5 is used as a message integrity check.
	// If len(ContentMD5) > 0, the MD5 hash of the bytes written must match
	// ContentMD5, or Close will return an error without completing the write.
	// https://tools.ietf.org/html/rfc1864
	ContentMD5 []byte

	// Metadata holds key/value strings to be associated with the blob, or nil.
	// Keys may not be empty, and are lowercased before being written.
	// Duplicate case-insensitive keys (e.g., "foo" and "FOO") will result in
	// an error.
	Metadata map[string]string

	// BeforeWrite is a callback that will be called exactly once, before
	// any data is written (unless NewWriter returns an error, in which case
	// it will not be called at all). Note that this is not necessarily during
	// or after the first Write call, as providers may buffer bytes before
	// sending an upload request.
	//
	// asFunc converts its argument to provider-specific types.
	// See Bucket.As for more details.
	BeforeWrite func(asFunc func(interface{}) bool) error
}

// A type that implements BucketURLOpener can open buckets based on a URL.
// The opener must not modify the URL argument. OpenBucketURL must be safe to
// call from multiple goroutines.
//
// This interface is generally implemented by types in driver packages.
type BucketURLOpener interface {
	OpenBucketURL(ctx context.Context, u *url.URL) (*Bucket, error)
}

// URLMux is a URL opener multiplexer. It matches the scheme of the URLs
// against a set of registered schemes and calls the opener that matches the
// URL's scheme.
//
// The zero value is a multiplexer with no registered schemes.
type URLMux struct {
	schemes map[string]BucketURLOpener
}

// RegisterBucket registers the opener with the given scheme. If an opener
// already exists for the scheme, RegisterBucket panics.
func (mux *URLMux) RegisterBucket(scheme string, opener BucketURLOpener) {
	if mux.schemes == nil {
		mux.schemes = make(map[string]BucketURLOpener)
	} else if _, exists := mux.schemes[scheme]; exists {
		panic(fmt.Errorf("scheme %q already registered on mux", scheme))
	}
	mux.schemes[scheme] = opener
}

// OpenBucket calls OpenBucketURL with the URL parsed from urlstr.
// OpenBucket is safe to call from multiple goroutines.
func (mux *URLMux) OpenBucket(ctx context.Context, urlstr string) (*Bucket, error) {
	u, err := url.Parse(urlstr)
	if err != nil {
		return nil, fmt.Errorf("open bucket: %v", err)
	}
	return mux.OpenBucketURL(ctx, u)
}

// OpenBucketURL dispatches the URL to the opener that is registered with the
// URL's scheme. OpenBucketURL is safe to call from multiple goroutines.
func (mux *URLMux) OpenBucketURL(ctx context.Context, u *url.URL) (*Bucket, error) {
	if u.Scheme == "" {
		return nil, fmt.Errorf("open bucket %q: no scheme in URL", u)
	}
	var opener BucketURLOpener
	if mux != nil {
		opener = mux.schemes[u.Scheme]
	}
	if opener == nil {
		return nil, fmt.Errorf("open bucket %q: no provider registered for %s", u, u.Scheme)
	}
	return opener.OpenBucketURL(ctx, u)
}

var defaultURLMux = new(URLMux)

// DefaultURLMux returns the URLMux used by OpenBucket.
//
// Driver packages can use this to register their BucketURLOpener on the mux.
func DefaultURLMux() *URLMux {
	return defaultURLMux
}

// OpenBucket opens the bucket identified by the URL given. URL openers must be
// registered in the DefaultURLMux, which is typically done in driver
// packages' initialization.
//
// See the URLOpener documentation in provider-specific subpackages for more
// details on supported scheme(s) and URL parameter(s).
func OpenBucket(ctx context.Context, urlstr string) (*Bucket, error) {
	return defaultURLMux.OpenBucket(ctx, urlstr)
}

func wrapError(b driver.Bucket, err error) error {
	if err == nil {
		return nil
	}
	if gcerr.DoNotWrap(err) {
		return err
	}
	return gcerr.New(b.ErrorCode(err), err, 2, "blob")
}