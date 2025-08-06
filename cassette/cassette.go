// Copyright (c) 2015-2024 Marin Atanasov Nikolov <dnaeon@gmail.com>
// All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions
// are met:
// 1. Redistributions of source code must retain the above copyright
//    notice, this list of conditions and the following disclaimer
//    in this position and unchanged.
// 2. Redistributions in binary form must reproduce the above copyright
//    notice, this list of conditions and the following disclaimer in the
//    documentation and/or other materials provided with the distribution.
//
// THIS SOFTWARE IS PROVIDED BY THE AUTHOR(S) ``AS IS'' AND ANY EXPRESS OR
// IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES
// OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED.
// IN NO EVENT SHALL THE AUTHOR(S) BE LIABLE FOR ANY DIRECT, INDIRECT,
// INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT
// NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF
// THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

package cassette

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// CassetteFormatVersion is the supported cassette version.
	CassetteFormatVersion = 2
)

var (
	// ErrInteractionNotFound indicates that a requested interaction was not
	// found in the cassette file.
	ErrInteractionNotFound = errors.New("requested interaction not found")

	// ErrCassetteNotFound indicates that a requested casette doesn't exist.
	ErrCassetteNotFound = errors.New("requested cassette not found")

	// ErrUnsupportedCassetteFormat is returned when attempting to use an
	// older and potentially unsupported format of a cassette.
	ErrUnsupportedCassetteFormat = fmt.Errorf("unsupported cassette version format")
)

// Request represents a client request as recorded in the cassette file.
type Request struct {
	Proto            string      `yaml:"proto"`
	ProtoMajor       int         `yaml:"proto_major"`
	ProtoMinor       int         `yaml:"proto_minor"`
	ContentLength    int64       `yaml:"content_length"`
	TransferEncoding []string    `yaml:"transfer_encoding,omitempty"`
	Trailer          http.Header `yaml:"trailer,omitempty"`
	Host             string      `yaml:"host"`
	RemoteAddr       string      `yaml:"remote_addr,omitempty"`
	RequestURI       string      `yaml:"request_uri,omitempty"`

	// Body of request
	Body string `yaml:"body,omitempty"`

	// Form values
	Form url.Values `yaml:"form,omitempty"`

	// Request headers
	Headers http.Header `yaml:"headers,omitempty"`

	// Request URL
	URL string `yaml:"url"`

	// Request method
	Method string `yaml:"method"`
}

// Response represents a server response as recorded in the cassette file.
type Response struct {
	Proto            string      `yaml:"proto"`
	ProtoMajor       int         `yaml:"proto_major"`
	ProtoMinor       int         `yaml:"proto_minor"`
	TransferEncoding []string    `yaml:"transfer_encoding,omitempty"`
	Trailer          http.Header `yaml:"trailer,omitempty"`
	ContentLength    int64       `yaml:"content_length"`
	Uncompressed     bool        `yaml:"uncompressed,omitempty"`

	// Body of response
	Body string `yaml:"body"`

	// Response headers
	Headers http.Header `yaml:"headers"`

	// Response status message
	Status string `yaml:"status"`

	// Response status code
	Code int `yaml:"code"`

	// Response duration
	Duration time.Duration `yaml:"duration"`
}

// Interaction type contains a pair of request/response for a single HTTP
// interaction between a client and a server.
type Interaction struct {
	// ID is the id of the interaction
	ID int `yaml:"id"`

	// Request is the recorded request
	Request Request `yaml:"request"`

	// Response is the recorded response
	Response Response `yaml:"response"`

	// DiscardOnSave if set to true will discard the interaction as a whole
	// and it will not be part of the final interactions when saving the
	// cassette on disk.
	DiscardOnSave bool `yaml:"-"`

	// replayed is true when this interaction has been played already.
	replayed bool `yaml:"-"`
}

// WasReplayed returns a boolean indicating whether the given interaction was
// already replayed.
func (i *Interaction) WasReplayed() bool {
	return i.replayed
}

// GetHTTPRequest converts the recorded interaction request to http.Request
// instance.
func (i *Interaction) GetHTTPRequest() (*http.Request, error) {
	return toHTTPRequest(i.Request)
}

func toHTTPRequest(req Request) (*http.Request, error) {
	url, err := url.Parse(req.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse request URL %s: %w", req.URL, err)
	}

	return &http.Request{
		Proto:            req.Proto,
		ProtoMajor:       req.ProtoMajor,
		ProtoMinor:       req.ProtoMinor,
		ContentLength:    req.ContentLength,
		TransferEncoding: req.TransferEncoding,
		Trailer:          req.Trailer,
		Host:             req.Host,
		RemoteAddr:       req.RemoteAddr,
		RequestURI:       req.RequestURI,
		Body:             io.NopCloser(strings.NewReader(req.Body)),
		Form:             req.Form,
		Header:           req.Headers,
		URL:              url,
		Method:           req.Method,
	}, nil
}

// GetHTTPResponse converts the recorded interaction response to http.Response
// instance.
func (i *Interaction) GetHTTPResponse() (*http.Response, error) {
	req, err := i.GetHTTPRequest()
	if err != nil {
		return nil, err
	}

	resp := &http.Response{
		Status:           i.Response.Status,
		StatusCode:       i.Response.Code,
		Proto:            i.Response.Proto,
		ProtoMajor:       i.Response.ProtoMajor,
		ProtoMinor:       i.Response.ProtoMinor,
		TransferEncoding: i.Response.TransferEncoding,
		Trailer:          i.Response.Trailer,
		ContentLength:    i.Response.ContentLength,
		Uncompressed:     i.Response.Uncompressed,
		Body:             io.NopCloser(strings.NewReader(i.Response.Body)),
		Header:           i.Response.Headers,
		Close:            true,
		Request:          req,
	}

	return resp, nil
}

// MatcherFunc is a predicate, which returns true when the actual request
// matches an interaction from the cassette. It is used for matching if a
// HasherFunc is not provided.
type MatcherFunc func(*http.Request, Request) bool

// HasherFunc generates a hash from an HTTP request for fast matching.
// The hash should be deterministic and identical for equivalent requests.
type HasherFunc func(r *http.Request) (string, error)

// defaultMatcher is the default matcher used to match HTTP requests with
// recorded interactions.
type defaultMatcher struct {
	// If set, the default matcher will ignore matching on any of the
	// defined headers.
	ignoreHeaders []string
}

// DefaultMatcherOption is a function which configures the default matcher.
type DefaultMatcherOption func(m *defaultMatcher)

// WithIgnoreUserAgent is a [DefaultMatcherOption], which configures the default
// matcher to ignore matching on the User-Agent HTTP header.
func WithIgnoreUserAgent() DefaultMatcherOption {
	opt := func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, "User-Agent")
	}
	return opt
}

// WithIgnoreAuthorization is a [DefaultMatcherOption], which configures the default
// matcher to ignore matching on the Authorization HTTP header.
func WithIgnoreAuthorization() DefaultMatcherOption {
	opt := func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, "Authorization")
	}
	return opt
}

// WithIgnoreHeaders is a [DefaultMatcherOption], which configures the default
// matcher to ignore matching on the defined HTTP headers.
func WithIgnoreHeaders(val ...string) DefaultMatcherOption {
	opt := func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, val...)
	}
	return opt
}

// NewDefaultMatcher returns the default matcher.
func NewDefaultMatcher(opts ...DefaultMatcherOption) MatcherFunc {
	m := &defaultMatcher{}
	for _, opt := range opts {
		opt(m)
	}
	return m.matcher
}

// NewDefaultHasher returns the default hasher.
func NewDefaultHasher(opts ...DefaultMatcherOption) HasherFunc {
	m := &defaultMatcher{}
	for _, opt := range opts {
		opt(m)
	}
	return m.hasher
}

// matcher is a predicate which matches the provided HTTP request against a
// recorded interaction request by comparing their hashes.
func (m *defaultMatcher) matcher(r *http.Request, i Request) bool {
	hasher := defaultInteractionRequestHasher

	reqHash, err := hasher(r, m.ignoreHeaders)
	if err != nil {
		return false
	}

	interactionReq, err := toHTTPRequest(i)
	if err != nil {
		return false
	}

	recHash, err := hasher(interactionReq, m.ignoreHeaders)
	if err != nil {
		return false
	}

	return reqHash == recHash
}

// hasher generates a hash from an HTTP request using the default hashing logic.
func (m *defaultMatcher) hasher(r *http.Request) (string, error) {
	return defaultInteractionRequestHasher(r, m.ignoreHeaders)
}

// DefaultMatcher is the default matcher used to match HTTP requests with
// recorded interactions.
var DefaultMatcher = NewDefaultMatcher()

// DefaultHasher is the default hasher used to generate hashes for matching.
var DefaultHasher = NewDefaultHasher()

// Cassette represents a cassette containing recorded interactions.
type Cassette struct {
	sync.Mutex `yaml:"-"`

	// Name of the cassette
	Name string `yaml:"-"`

	// Cassette format version
	Version int `yaml:"version"`

	// Interactions between client and server
	Interactions []*Interaction `yaml:"interactions"`

	// ReplayableInteractions defines whether to allow
	// interactions to be replayed or not
	ReplayableInteractions bool `yaml:"-"`

	// CompressionEnabled defines whether to compress the cassette
	CompressionEnabled bool `yaml:"compression_enabled,omitempty"`

	// Matcher matches an actual request with an interaction.
	// It is only used if Hasher is nil.
	Matcher MatcherFunc `yaml:"-"`

	// Hasher generates a hash from a request for fast matching.
	// If set, this takes precedence over Matcher.
	Hasher HasherFunc `yaml:"-"`

	// IsNew specifies whether this is a newly created cassette.
	// Returns false, when the cassette was loaded from an
	// existing source, e.g. a file.
	IsNew bool `yaml:"-"`

	nextInteractionId int              `yaml:"-"`
	hashIndex         map[string][]int `yaml:"-"`
}

// New creates a new empty cassette
func New(name string) *Cassette {
	c := &Cassette{
		Name:                   name,
		Version:                CassetteFormatVersion,
		Interactions:           make([]*Interaction, 0),
		Matcher:                DefaultMatcher,
		Hasher:                 DefaultHasher,
		ReplayableInteractions: false,
		CompressionEnabled:     false,
		IsNew:                  true,
		nextInteractionId:      0,
		hashIndex:              make(map[string][]int),
	}

	return c
}

// File returns the cassette file name as written on disk.
func (c *Cassette) File() string {
	if c == nil {
		return ""
	}

	file := fmt.Sprintf("%s.yaml", c.Name)

	if c.CompressionEnabled {
		return file + ".gz"
	}

	return file
}

func (c *Cassette) Load() error {
	if c == nil {
		return fmt.Errorf("cassette is nil")
	}

	file := c.File()

	f, err := os.Open(file)
	if err != nil {
		// Return the original os.ReadFile error format for consistency.
		if os.IsNotExist(err) {
			return err
		}
		return fmt.Errorf("failed to read cassette file %s: %w", file, err)
	}
	defer f.Close()

	var reader io.Reader = f
	if c.CompressionEnabled {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader for %s: %w", file, err)
		}
		defer gz.Close()
		reader = gz
	}

	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read cassette data from %s: %w", file, err)
	}

	c.IsNew = false
	if err := yaml.Unmarshal(data, c); err != nil {
		return fmt.Errorf("failed to unmarshal cassette file %s: %w", file, err)
	}

	if c.Version != CassetteFormatVersion {
		return fmt.Errorf("%w: found version %d, but reader supports version %d", ErrUnsupportedCassetteFormat, c.Version, CassetteFormatVersion)
	}

	c.nextInteractionId = len(c.Interactions)

	if err := c.buildHashIndex(); err != nil {
		return fmt.Errorf("failed to build hash index for cassette %s: %w", c.Name, err)
	}

	return nil
}

// Load is a convenience function which loads a cassette from disk and returns
// it.
func Load(name string) (*Cassette, error) {
	c := New(name)

	if err := c.Load(); err != nil {
		return nil, fmt.Errorf("failed to load cassette %s: %w", name, err)
	}

	return c, nil
}

func (c *Cassette) buildHashIndex() error {
	if c.Hasher == nil {
		return nil
	}

	for i, interaction := range c.Interactions {
		req, err := interaction.GetHTTPRequest()
		if err != nil {
			return fmt.Errorf("failed to get HTTP request for interaction %d: %w", interaction.ID, err)
		}

		hash, err := c.Hasher(req)
		if err != nil {
			return fmt.Errorf("failed to hash request for interaction %d: %w", interaction.ID, err)
		}

		if _, exists := c.hashIndex[hash]; !exists {
			c.hashIndex[hash] = make([]int, 0)
		}

		c.hashIndex[hash] = append(c.hashIndex[hash], i)
	}

	return nil
}

// AddInteraction appends a new interaction to the cassette
func (c *Cassette) AddInteraction(i *Interaction) error {
	c.Lock()
	defer c.Unlock()
	i.ID = c.nextInteractionId
	c.nextInteractionId += 1

	if c.Hasher != nil {
		req, err := i.GetHTTPRequest()
		if err != nil {
			return fmt.Errorf("failed to get HTTP request for interaction %d: %w", i.ID, err)
		}

		hash, err := c.Hasher(req)
		if err != nil {
			return fmt.Errorf("failed to hash request for interaction %d: %w", i.ID, err)
		}

		c.hashIndex[hash] = append(c.hashIndex[hash], i.ID)
	}

	c.Interactions = append(c.Interactions, i)
	return nil
}

// GetInteraction retrieves a recorded request/response interaction
func (c *Cassette) GetInteraction(r *http.Request) (*Interaction, error) {
	return c.getInteraction(r)
}

// getInteraction searches for the interaction corresponding to the given HTTP
// request, using either hash-based or matcher-based lookup.
func (c *Cassette) getInteraction(r *http.Request) (*Interaction, error) {
	c.Lock()
	defer c.Unlock()
	if r.Body == nil {
		r.Body = http.NoBody
	}

	var interaction *Interaction
	var err error

	if c.Hasher != nil {
		interaction, err = c.getInteractionByHash(r)
		if err != nil {
			return nil, fmt.Errorf("failed to get interaction by hash: %w", err)
		}
	} else {
		interaction, err = c.getInteractionByMatcher(r)
		if err != nil {
			return nil, fmt.Errorf("failed to get interaction by matcher: %w", err)
		}
	}

	return c.overrideRecordedRequestBody(r, interaction)
}

func (c *Cassette) getInteractionByHash(r *http.Request) (*Interaction, error) {
	if c.Hasher == nil {
		return nil, fmt.Errorf("cassette has no hasher defined")
	}

	reqHash, err := c.Hasher(r)
	if err != nil {
		return nil, fmt.Errorf("failed to hash request: %w", err)
	}

	interactions, ok := c.hashIndex[reqHash]
	if !ok {
		return nil, ErrInteractionNotFound
	}

	var interaction *Interaction
	for _, id := range interactions {
		interaction = c.Interactions[id]
		if !c.ReplayableInteractions && interaction.replayed {
			continue
		}

		interaction.replayed = true
		return interaction, nil
	}

	return nil, ErrInteractionNotFound
}

// overrideRecordedRequestBody reads the request body from the HTTP request and
// overrides the recorded request body in the interaction with the actual
// request.  This is useful when the request body contains dynamic data that
// needs to be re-used in the response, such as JSON-RPC id fields.
func (c *Cassette) overrideRecordedRequestBody(r *http.Request, originalInteraction *Interaction) (*Interaction, error) {
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body: %w", err)
	}

	r.Body.Close()
	r.Body = io.NopCloser(strings.NewReader(string(buf)))

	reqBytes, err := httputil.DumpRequestOut(r, true)
	if err != nil {
		return nil, err
	}

	reqBuffer := bytes.NewBuffer(reqBytes)
	copiedReq, err := http.ReadRequest(bufio.NewReader(reqBuffer))
	if err != nil {
		return nil, err
	}

	err = copiedReq.ParseForm()
	if err != nil {
		return nil, err
	}

	interaction := *originalInteraction
	interaction.Request = originalInteraction.Request

	interaction.Request.Body = string(buf)
	interaction.Request.Form = copiedReq.PostForm

	return &interaction, nil
}

func (c *Cassette) getInteractionByMatcher(r *http.Request) (*Interaction, error) {
	if c.Matcher == nil {
		return nil, fmt.Errorf("cassette has no matcher defined")
	}

	for _, interaction := range c.Interactions {
		if !c.ReplayableInteractions && interaction.replayed {
			continue
		}

		if c.Matcher(r, interaction.Request) {
			interaction.replayed = true
			return interaction, nil
		}
	}

	return nil, ErrInteractionNotFound
}

// Save writes the cassette data on disk for future re-use
func (c *Cassette) Save() error {
	c.Lock()
	defer c.Unlock()

	file := c.File()

	// Create directory for cassette if missing
	cassetteDir := filepath.Dir(file)
	if _, err := os.Stat(cassetteDir); os.IsNotExist(err) {
		if err = os.MkdirAll(cassetteDir, 0o755); err != nil {
			return err
		}
	}

	// Filter out interactions which should be discarded. While discarding
	// interactions we should also fix the interaction IDs, so that we don't
	// introduce gaps in the final results.
	nextId := 0
	interactions := make([]*Interaction, 0)
	for _, i := range c.Interactions {
		if !i.DiscardOnSave {
			i.ID = nextId
			interactions = append(interactions, i)
			nextId += 1
		}
	}
	c.Interactions = interactions

	// Marshal to YAML and save interactions
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}

	// underlying file
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()

	var w io.WriteCloser = f
	if c.CompressionEnabled {
		w = gzip.NewWriter(f)
		defer w.Close()
	}

	// Honor the YAML structure specification
	// http://www.yaml.org/spec/1.2/spec.html#id2760395
	_, err = w.Write([]byte("---\n"))
	if err != nil {
		return err
	}

	_, err = w.Write(data)
	if err != nil {
		return err
	}

	return nil
}
