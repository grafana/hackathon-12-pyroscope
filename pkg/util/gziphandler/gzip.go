// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/nytimes/gziphandler/blob/2f8bb1d30d9d69c8e0c3714da5a9917125a87769/gzip.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: Copyright 2016-2017 The New York Times Company.

package gziphandler

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	vary            = "Vary"
	acceptEncoding  = "Accept-Encoding"
	contentEncoding = "Content-Encoding"
	contentType     = "Content-Type"
	contentLength   = "Content-Length"
)

type codings map[string]float64

const (
	// DefaultQValue is the default qvalue to assign to an encoding if no explicit qvalue is set.
	// This is actually kind of ambiguous in RFC 2616, so hopefully it's correct.
	// The examples seem to indicate that it is.
	DefaultQValue = 1.0

	// DefaultMinSize is the default minimum size until we enable gzip compression.
	// 1500 bytes is the MTU size for the internet since that is the largest size allowed at the network layer.
	// If you take a file that is 1300 bytes and compress it to 800 bytes, it’s still transmitted in that same 1500 byte packet regardless, so you’ve gained nothing.
	// That being the case, you should restrict the gzip compression to files with a size greater than a single packet, 1400 bytes (1.4KB) is a safe value.
	DefaultMinSize = 1400
)

// gzipWriterPools stores a sync.Pool for each compression level for reuse of
// gzip.Writers. Use poolIndex to covert a compression level to an index into
// gzipWriterPools.
var gzipWriterPools [gzip.BestCompression - gzip.BestSpeed + 2]*sync.Pool

func init() {
	for i := gzip.BestSpeed; i <= gzip.BestCompression; i++ {
		addLevelPool(i)
	}
	addLevelPool(gzip.DefaultCompression)
}

// poolIndex maps a compression level to its index into gzipWriterPools. It
// assumes that level is a valid gzip compression level.
func poolIndex(level int) int {
	// gzip.DefaultCompression == -1, so we need to treat it special.
	if level == gzip.DefaultCompression {
		return gzip.BestCompression - gzip.BestSpeed + 1
	}
	return level - gzip.BestSpeed
}

func addLevelPool(level int) {
	gzipWriterPools[poolIndex(level)] = &sync.Pool{
		New: func() interface{} {
			// NewWriterLevel only returns error on a bad level, we are guaranteeing
			// that this will be a valid level so it is okay to ignore the returned
			// error.
			w, _ := gzip.NewWriterLevel(nil, level)
			return w
		},
	}
}

// GzipResponseWriter provides an http.ResponseWriter interface, which gzips
// bytes before writing them to the underlying response. This doesn't close the
// writers, so don't forget to do that.
// It can be configured to skip response smaller than minSize.
type GzipResponseWriter struct {
	http.ResponseWriter
	index int // Index for gzipWriterPools.
	gw    *gzip.Writer

	code int // Saves the WriteHeader value.

	minSize int    // Specifies the minimum response size to gzip. If the response length is bigger than this value, it is compressed.
	buf     []byte // Holds the first part of the write before reaching the minSize or the end of the write.
	ignore  bool   // If true, then we immediately passthru writes to the underlying ResponseWriter.

	contentTypes    []parsedContentType // Only compress if the response is one of these content-types. All are accepted if empty.
	rejectsIdentity bool                // If true, then request explicitly rejected non-encoded requests.
}

// Write appends data to the gzip writer.
func (w *GzipResponseWriter) Write(b []byte) (int, error) {
	// GZIP responseWriter is initialized. Use the GZIP responseWriter.
	if w.gw != nil {
		return w.gw.Write(b)
	}

	// If we have already decided not to use GZIP, immediately passthrough.
	if w.ignore {
		return w.ResponseWriter.Write(b)
	}

	// Save the write into a buffer for later use in GZIP responseWriter (if content is long enough) or at close with regular responseWriter.
	// On the first write, w.buf changes from nil to a valid slice
	w.buf = append(w.buf, b...)

	var (
		cl, _ = strconv.Atoi(w.Header().Get(contentLength))
		ct    = w.Header().Get(contentType)
		ce    = w.Header().Get(contentEncoding)
	)

	// Don't encode again encoded content.
	// There's no need to check whether the client rejected the identity encoding
	// because we already know that this has a different encoding.
	if ce != "" {
		return w.startPlainWrite(len(b))
	}

	// Don't encode when content length is known, it's less than min size,
	// and the caller didn't reject the identity encoding.
	if cl > 0 && cl < w.minSize && !w.rejectsIdentity {
		return w.startPlainWrite(len(b))
	}

	// Only continue if we handle the content type (or it's still unknown).
	if handleContentType(w.contentTypes, ct) {
		// If the current buffer is less than minSize and a Content-Length isn't set, then wait until we have more data.
		if len(w.buf) < w.minSize && cl == 0 && !w.rejectsIdentity {
			return len(b), nil
		}
		// If the Content-Length is larger than minSize or the current buffer is larger than minSize, then continue.
		if cl >= w.minSize || len(w.buf) >= w.minSize || w.rejectsIdentity {
			// If a Content-Type wasn't specified, infer it from the current buffer.
			if ct == "" {
				ct = http.DetectContentType(w.buf)
				w.Header().Set(contentType, ct)
			}
			// If the Content-Type is acceptable to GZIP, initialize the GZIP writer.
			// Note that we're ignoring the `rejectsIdentity` here, because we'd have to return a 406 Not Acceptable
			// in that case, but we still might be wrapped by another handler that handles a different encoding.
			if handleContentType(w.contentTypes, ct) {
				if err := w.startGzip(); err != nil {
					return 0, err
				}
				return len(b), nil
			}
		}
	}

	// If we got here, we should not GZIP this response.
	return w.startPlainWrite(len(b))
}

func (w *GzipResponseWriter) startPlainWrite(blen int) (int, error) {
	if err := w.startPlain(); err != nil {
		return 0, err
	}
	return blen, nil
}

// startGzip initializes a GZIP writer and writes the buffer.
func (w *GzipResponseWriter) startGzip() error {
	// Set the GZIP header.
	w.Header().Set(contentEncoding, "gzip")

	// if the Content-Length is already set, then calls to Write on gzip
	// will fail to set the Content-Length header since its already set
	// See: https://github.com/golang/go/issues/14975.
	w.Header().Del(contentLength)

	// Write the header to gzip response.
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}

	// Initialize and flush the buffer into the gzip response if there are any bytes.
	// If there aren't any, we shouldn't initialize it yet because on Close it will
	// write the gzip header even if nothing was ever written.
	if len(w.buf) > 0 {
		// Initialize the GZIP response.
		w.init()
		n, err := w.gw.Write(w.buf)

		// This should never happen (per io.Writer docs), but if the write didn't
		// accept the entire buffer but returned no specific error, we have no clue
		// what's going on, so abort just to be safe.
		if err == nil && n < len(w.buf) {
			err = io.ErrShortWrite
		}
		return err
	}
	return nil
}

// startPlain writes to sent bytes and buffer the underlying ResponseWriter without gzip.
func (w *GzipResponseWriter) startPlain() error {
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
		// Ensure that no other WriteHeader's happen
		w.code = 0
	}
	w.ignore = true
	// If Write was never called then don't call Write on the underlying ResponseWriter.
	if w.buf == nil {
		return nil
	}
	n, err := w.ResponseWriter.Write(w.buf)
	w.buf = nil
	// This should never happen (per io.Writer docs), but if the write didn't
	// accept the entire buffer but returned no specific error, we have no clue
	// what's going on, so abort just to be safe.
	if err == nil && n < len(w.buf) {
		err = io.ErrShortWrite
	}
	return err
}

// WriteHeader just saves the response code until close or GZIP effective writes.
func (w *GzipResponseWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}

// init graps a new gzip writer from the gzipWriterPool and writes the correct
// content encoding header.
func (w *GzipResponseWriter) init() {
	// Bytes written during ServeHTTP are redirected to this gzip writer
	// before being written to the underlying response.
	gzw := gzipWriterPools[w.index].Get().(*gzip.Writer)
	gzw.Reset(w.ResponseWriter)
	w.gw = gzw
}

// Close will close the gzip.Writer and will put it back in the gzipWriterPool.
func (w *GzipResponseWriter) Close() error {
	if w.ignore {
		return nil
	}

	if w.gw == nil {
		// GZIP not triggered yet, write out regular response.
		err := w.startPlain()
		// Returns the error if any at write.
		if err != nil {
			err = fmt.Errorf("gziphandler: write to regular responseWriter at close gets error: %q", err.Error())
		}
		return err
	}

	err := w.gw.Close()
	gzipWriterPools[w.index].Put(w.gw)
	w.gw = nil
	return err
}

// Flush flushes the underlying *gzip.Writer and then the underlying
// http.ResponseWriter if it is an http.Flusher. This makes GzipResponseWriter
// an http.Flusher.
func (w *GzipResponseWriter) Flush() {
	if w.gw == nil && !w.ignore {
		// Only flush once startGzip or startPlain has been called.
		//
		// Flush is thus a no-op until we're certain whether a plain
		// or gzipped response will be served.
		return
	}

	if w.gw != nil {
		w.gw.Flush()
	}

	if fw, ok := w.ResponseWriter.(http.Flusher); ok {
		fw.Flush()
	}
}

// Hijack implements http.Hijacker. If the underlying ResponseWriter is a
// Hijacker, its Hijack method is returned. Otherwise an error is returned.
func (w *GzipResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("http.Hijacker interface is not supported")
}

// verify Hijacker interface implementation
var _ http.Hijacker = &GzipResponseWriter{}

// MustNewGzipLevelHandler behaves just like NewGzipLevelHandler except that in
// an error case it panics rather than returning an error.
func MustNewGzipLevelHandler(level int) func(http.Handler) http.Handler {
	wrap, err := NewGzipLevelHandler(level)
	if err != nil {
		panic(err)
	}
	return wrap
}

// NewGzipLevelHandler returns a wrapper function (often known as middleware)
// which can be used to wrap an HTTP handler to transparently gzip the response
// body if the client supports it (via the Accept-Encoding header). Responses will
// be encoded at the given gzip compression level. An error will be returned only
// if an invalid gzip compression level is given, so if one can ensure the level
// is valid, the returned error can be safely ignored.
func NewGzipLevelHandler(level int) (func(http.Handler) http.Handler, error) {
	return NewGzipLevelAndMinSize(level, DefaultMinSize)
}

// NewGzipLevelAndMinSize behave as NewGzipLevelHandler except it let the caller
// specify the minimum size before compression.
func NewGzipLevelAndMinSize(level, minSize int) (func(http.Handler) http.Handler, error) {
	return GzipHandlerWithOpts(CompressionLevel(level), MinSize(minSize))
}

// GzipHandlerWithOpts creates a middleware that wraps http.Handler with GzipHandler, configured with provided options.
//
//nolint:revive
func GzipHandlerWithOpts(opts ...Option) (func(http.Handler) http.Handler, error) {
	c := &config{
		level:   gzip.DefaultCompression,
		minSize: DefaultMinSize,
	}

	for _, o := range opts {
		o(c)
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return func(h http.Handler) http.Handler {
		index := poolIndex(c.level)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add(vary, acceptEncoding)
			if acceptsGzip, rejectsIdentity := requestAcceptance(r); acceptsGzip {
				gw := &GzipResponseWriter{
					ResponseWriter:  w,
					index:           index,
					minSize:         c.minSize,
					contentTypes:    c.contentTypes,
					rejectsIdentity: rejectsIdentity,
				}
				defer gw.Close()

				h.ServeHTTP(gw, r)

			} else {
				h.ServeHTTP(w, r)
			}
		})
	}, nil
}

// Parsed representation of one of the inputs to ContentTypes.
// See https://golang.org/pkg/mime/#ParseMediaType
type parsedContentType struct {
	mediaType string
	params    map[string]string
}

// equals returns whether this content type matches another content type.
func (pct parsedContentType) equals(mediaType string, params map[string]string) bool {
	if pct.mediaType != mediaType {
		return false
	}
	// if pct has no params, don't care about other's params
	if len(pct.params) == 0 {
		return true
	}

	// if pct has any params, they must be identical to other's.
	if len(pct.params) != len(params) {
		return false
	}
	for k, v := range pct.params {
		if w, ok := params[k]; !ok || v != w {
			return false
		}
	}
	return true
}

// Used for functional configuration.
type config struct {
	minSize      int
	level        int
	contentTypes []parsedContentType
}

func (c *config) validate() error {
	if c.level != gzip.DefaultCompression && (c.level < gzip.BestSpeed || c.level > gzip.BestCompression) {
		return fmt.Errorf("invalid compression level requested: %d", c.level)
	}

	if c.minSize < 0 {
		return fmt.Errorf("minimum size must be more than zero")
	}

	return nil
}

type Option func(c *config)

func MinSize(size int) Option {
	return func(c *config) {
		c.minSize = size
	}
}

func CompressionLevel(level int) Option {
	return func(c *config) {
		c.level = level
	}
}

// ContentTypes specifies a list of content types to compare
// the Content-Type header to before compressing. If none
// match, the response will be returned as-is.
//
// Content types are compared in a case-insensitive, whitespace-ignored
// manner.
//
// A MIME type without any other directive will match a content type
// that has the same MIME type, regardless of that content type's other
// directives. I.e., "text/html" will match both "text/html" and
// "text/html; charset=utf-8".
//
// A MIME type with any other directive will only match a content type
// that has the same MIME type and other directives. I.e.,
// "text/html; charset=utf-8" will only match "text/html; charset=utf-8".
//
// By default, responses are gzipped regardless of
// Content-Type.
func ContentTypes(types []string) Option {
	return func(c *config) {
		c.contentTypes = []parsedContentType{}
		for _, v := range types {
			mediaType, params, err := mime.ParseMediaType(v)
			if err == nil {
				c.contentTypes = append(c.contentTypes, parsedContentType{mediaType, params})
			}
		}
	}
}

// GzipHandler wraps an HTTP handler, to transparently gzip the response body if
// the client supports it (via the Accept-Encoding header). This will compress at
// the default compression level.
func GzipHandler(h http.Handler) http.Handler {
	wrapper, _ := NewGzipLevelHandler(gzip.DefaultCompression)
	return wrapper(h)
}

// requestAcceptance checks whether a given HTTP request indicates that it will
// accept a gzipped response and whether it's going to reject an non-encoded response.
//
// acceptsGzip is true if the given HTTP request indicates that it will
// accept a gzipped response and/or an identity request.
// rejectsIdentity is false if the given HTTP request didn't explicitly exclude identity encoding.
// I.e., either "identity;q=0" or "*;q=0" without a more specific entry for "identity".
// See https://datatracker.ietf.org/doc/html/rfc7231#section-5.3.4
func requestAcceptance(r *http.Request) (acceptsGzip bool, rejectsIdentity bool) {
	acceptedEncodings, _ := parseEncodings(r.Header.Get(acceptEncoding))

	identity, iset := acceptedEncodings["identity"]
	wildcard, wset := acceptedEncodings["*"]
	rejectsIdentity = (iset && identity == 0) || (!iset && wset && wildcard == 0)

	gzip, gzset := acceptedEncodings["gzip"]
	acceptsGzip = gzip > 0 || (!gzset && wildcard > 0)

	return acceptsGzip, rejectsIdentity
}

// returns true if we've been configured to compress the specific content type.
func handleContentType(contentTypes []parsedContentType, ct string) bool {
	// If unknown, then handle by default.
	if ct == "" {
		return true
	}

	// If contentTypes is empty we handle all content types.
	if len(contentTypes) == 0 {
		return true
	}

	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}

	for _, c := range contentTypes {
		if c.equals(mediaType, params) {
			return true
		}
	}

	return false
}

// parseEncodings attempts to parse a list of codings, per RFC 2616, as might
// appear in an Accept-Encoding header. It returns a map of content-codings to
// quality values, and an error containing the errors encountered. It's probably
// safe to ignore those, because silently ignoring errors is how the internet
// works.
//
// See: http://tools.ietf.org/html/rfc2616#section-14.3.
func parseEncodings(s string) (codings, error) {
	c := make(codings)
	var e []string
	var ss string
	var found bool

	for {
		ss, s, found = strings.Cut(s, ",")
		coding, qvalue, err := parseCoding(ss)

		if err != nil {
			e = append(e, err.Error())
		} else {
			c[coding] = qvalue
		}

		if !found {
			break
		}
	}

	// TODO (adammck): Use a proper multi-error struct, so the individual errors
	//                 can be extracted if anyone cares.
	if len(e) > 0 {
		return c, fmt.Errorf("errors while parsing encodings: %s", strings.Join(e, ", "))
	}

	return c, nil
}

// parseCoding parses a single conding (content-coding with an optional qvalue),
// as might appear in an Accept-Encoding header. It attempts to forgive minor
// formatting errors.
func parseCoding(s string) (coding string, qvalue float64, err error) {
	for n, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		qvalue = DefaultQValue

		if n == 0 {
			coding = strings.ToLower(part)
		} else if strings.HasPrefix(part, "q=") {
			qvalue, err = strconv.ParseFloat(strings.TrimPrefix(part, "q="), 64)

			if qvalue < 0.0 {
				qvalue = 0.0
			} else if qvalue > 1.0 {
				qvalue = 1.0
			}
		}
	}

	if coding == "" {
		err = fmt.Errorf("empty content-coding")
	}

	return
}
