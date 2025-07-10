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
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	// Hash is the pre-computed hash of the request for fast matching.
	// If empty, the hash will be computed on load.
	Hash string `yaml:"hash,omitempty"`

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

// RequestMatcher generates a deterministic hash from an HTTP request for matching.
// Two requests that should be considered equivalent must produce the same hash.
type RequestMatcher interface {
	Hash(r *http.Request) (string, error)
}

// MatcherOption is a function which configures a matcher.
type MatcherOption func(m *defaultMatcher)

// WithIgnoreUserAgent is a [MatcherOption] that configures the matcher
// to ignore the User-Agent HTTP header when matching.
func WithIgnoreUserAgent() MatcherOption {
	return func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, "User-Agent")
	}
}

// WithIgnoreAuthorization is a [MatcherOption] that configures the matcher
// to ignore the Authorization HTTP header when matching.
func WithIgnoreAuthorization() MatcherOption {
	return func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, "Authorization")
	}
}

// WithIgnoreHeaders is a [MatcherOption] that configures the matcher
// to ignore the specified HTTP headers when matching.
func WithIgnoreHeaders(val ...string) MatcherOption {
	return func(m *defaultMatcher) {
		m.ignoreHeaders = append(m.ignoreHeaders, val...)
	}
}

// defaultMatcher is the default RequestMatcher implementation.
type defaultMatcher struct {
	ignoreHeaders []string
}

// Hash implements RequestMatcher.
func (m *defaultMatcher) Hash(r *http.Request) (string, error) {
	return defaultInteractionRequestHasher(r, m.ignoreHeaders)
}

// NewMatcher creates a new RequestMatcher with the given options.
func NewMatcher(opts ...MatcherOption) RequestMatcher {
	m := &defaultMatcher{}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// DefaultMatcher is the default RequestMatcher used to match HTTP requests
// with recorded interactions.
var DefaultMatcher = NewMatcher()

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

	// Matcher generates hashes from requests for matching.
	Matcher RequestMatcher `yaml:"-"`

	// IsNew specifies whether this is a newly created cassette.
	// Returns false, when the cassette was loaded from an
	// existing source, e.g. a file.
	IsNew bool `yaml:"-"`

	nextInteractionId int              `yaml:"-"`
	hashIndex         map[string][]int `yaml:"-"`
}

// New creates a new empty cassette
func New(name string) *Cassette {
	return &Cassette{
		Name:                   name,
		Version:                CassetteFormatVersion,
		Interactions:           make([]*Interaction, 0),
		Matcher:                DefaultMatcher,
		ReplayableInteractions: false,
		CompressionEnabled:     false,
		IsNew:                  true,
		nextInteractionId:      0,
		hashIndex:              make(map[string][]int),
	}
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

	c.IsNew = false
	if err := yaml.NewDecoder(reader).Decode(c); err != nil {
		return fmt.Errorf("failed to decode cassette file %s: %w", file, err)
	}

	if c.Version != CassetteFormatVersion {
		return fmt.Errorf("%w: found version %d, but reader supports version %d", ErrUnsupportedCassetteFormat, c.Version, CassetteFormatVersion)
	}

	c.nextInteractionId = len(c.Interactions)

	upgraded, err := c.buildHashIndex()
	if err != nil {
		return fmt.Errorf("failed to build hash index for cassette %s: %w", c.Name, err)
	}

	// Auto-upgrade: save cassette with computed hashes for faster future loads
	if upgraded {
		if err := c.Save(); err != nil {
			slog.Warn("failed to save upgraded cassette", "cassette", c.Name, "error", err)
		}
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

// buildHashIndex builds the hash index for fast lookups.
// Returns true if any hashes were computed (i.e., cassette needs upgrade).
func (c *Cassette) buildHashIndex() (upgraded bool, err error) {
	if c.Matcher == nil {
		return false, nil
	}

	for i, interaction := range c.Interactions {
		hash := interaction.Hash

		// Fall back to computing hash for old cassettes without pre-computed hashes
		if hash == "" {
			req, err := interaction.GetHTTPRequest()
			if err != nil {
				return false, fmt.Errorf("failed to get HTTP request for interaction %d: %w", interaction.ID, err)
			}

			hash, err = c.Matcher.Hash(req)
			if err != nil {
				return false, fmt.Errorf("failed to hash request for interaction %d: %w", interaction.ID, err)
			}

			// Store computed hash for persistence
			interaction.Hash = hash
			upgraded = true
		}

		c.hashIndex[hash] = append(c.hashIndex[hash], i)
	}

	return upgraded, nil
}

// AddInteraction appends a new interaction to the cassette
func (c *Cassette) AddInteraction(i *Interaction) error {
	c.Lock()
	defer c.Unlock()
	i.ID = c.nextInteractionId
	c.nextInteractionId++

	if c.Matcher != nil {
		req, err := i.GetHTTPRequest()
		if err != nil {
			return fmt.Errorf("failed to get HTTP request for interaction %d: %w", i.ID, err)
		}

		hash, err := c.Matcher.Hash(req)
		if err != nil {
			return fmt.Errorf("failed to hash request for interaction %d: %w", i.ID, err)
		}

		// Store hash in interaction for persistence
		i.Hash = hash
		c.hashIndex[hash] = append(c.hashIndex[hash], len(c.Interactions))
	}

	c.Interactions = append(c.Interactions, i)
	return nil
}

// GetInteraction retrieves a recorded request/response interaction
func (c *Cassette) GetInteraction(r *http.Request) (*Interaction, error) {
	c.Lock()
	defer c.Unlock()

	if c.Matcher == nil {
		return nil, fmt.Errorf("cassette has no matcher defined")
	}

	if r.Body == nil {
		r.Body = http.NoBody
	}

	// Read and cache the request body.
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read request body for matching: %w", err)
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	reqHash, err := c.Matcher.Hash(r)
	if err != nil {
		return nil, fmt.Errorf("failed to hash request: %w", err)
	}

	interactionIndices, ok := c.hashIndex[reqHash]
	if !ok {
		slog.Warn("no interactions found for request hash", "hash", reqHash)
		return nil, ErrInteractionNotFound
	}

	var lastReplayedIdx int = -1
	for _, idx := range interactionIndices {
		interaction := c.Interactions[idx]
		if !c.ReplayableInteractions && interaction.replayed {
			lastReplayedIdx = idx
			continue
		}

		interaction.replayed = true
		return c.overrideRecordedRequestBody(r, interaction, bodyBytes)
	}

	if lastReplayedIdx != -1 {
		slog.Warn("all interactions for request hash have been replayed, returning last one", "hash", reqHash, "interaction_id", c.Interactions[lastReplayedIdx].ID)
		return c.overrideRecordedRequestBody(r, c.Interactions[lastReplayedIdx], bodyBytes)
	}

	slog.Warn("no matching interaction found for request hash", "hash", reqHash)
	return nil, ErrInteractionNotFound
}

// overrideRecordedRequestBody reads the request body from the HTTP request and
// overrides the recorded request body in the interaction with the actual
// request.  This is useful when the request body contains dynamic data that
// needs to be re-used in the response, such as JSON-RPC id fields.
func (c *Cassette) overrideRecordedRequestBody(r *http.Request, originalInteraction *Interaction, bodyBytes []byte) (*Interaction, error) {
	// Restore body for form parsing.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	_ = r.ParseForm()

	// Restore body again for further use.
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Create a copy of the interaction to modify.
	interaction := *originalInteraction
	interaction.Request = originalInteraction.Request

	interaction.Request.Body = string(bodyBytes)
	interaction.Request.Form = r.PostForm

	return &interaction, nil
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
			nextId++
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
