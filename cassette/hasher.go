package cassette

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const defaultDelimiter = "::"

// RequestHasher builds a deterministic hash from various request components.
type RequestHasher struct {
	hash  hash.Hash
	first bool
}

func NewRequestHasher() *RequestHasher {
	return &RequestHasher{
		hash:  sha256.New(),
		first: true,
	}
}

func (h *RequestHasher) Add(part string) {
	if !h.first {
		h.hash.Write([]byte(defaultDelimiter))
	}
	h.first = false
	h.hash.Write([]byte(part))
}

func (h *RequestHasher) AddInt(n int) {
	h.Add(strconv.Itoa(n))
}

func (h *RequestHasher) Hash() string {
	return fmt.Sprintf("%x", h.hash.Sum(nil))
}

// serializeHeaders creates a deterministic string representation of http.Header.
func serializeHeaders(h http.Header, ignore []string) string {
	if len(h) == 0 {
		return ""
	}

	headersToIgnore := make(map[string]struct{}, len(ignore))
	for _, header := range ignore {
		headersToIgnore[http.CanonicalHeaderKey(header)] = struct{}{}
	}

	keys := make([]string, 0, len(h))
	for k := range h {
		if _, ok := headersToIgnore[http.CanonicalHeaderKey(k)]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var b strings.Builder
	for i, k := range keys {
		// Copy values to avoid modifying the original header.
		values := make([]string, len(h[k]))
		copy(values, h[k])
		sort.Strings(values)
		b.WriteString(fmt.Sprintf("%s:%s", k, strings.Join(values, ",")))
		if i < len(keys)-1 {
			b.WriteString(";")
		}
	}
	return b.String()
}

func serializeTransferEncoding(te []string) string {
	if len(te) == 0 {
		return ""
	}

	copied := make([]string, len(te))
	copy(copied, te)
	sort.Strings(copied)
	return strings.Join(copied, ",")
}

// defaultInteractionRequestHasher generates a hash from a live http.Request.
func defaultInteractionRequestHasher(r *http.Request, ignoreHeaders []string) (string, error) {
	// Read and restore the body so it can be used by subsequent handlers.
	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return "", err
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}

	// Parse form for relevant methods. This is safe because the body was restored.
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		if err := r.ParseForm(); err != nil {
			return "", err
		}
	}

	hasher := NewRequestHasher()

	hasher.Add(r.Method)
	hasher.Add(r.Host)
	hasher.Add(r.URL.String())
	hasher.Add(r.Proto)
	hasher.AddInt(r.ProtoMajor)
	hasher.AddInt(r.ProtoMinor)
	hasher.Add(serializeHeaders(r.Header, ignoreHeaders))
	hasher.Add(string(bodyBytes))
	hasher.AddInt(int(r.ContentLength))
	hasher.Add(serializeHeaders(r.Trailer, nil))
	hasher.Add(serializeTransferEncoding(r.TransferEncoding))
	hasher.Add(r.RemoteAddr)
	hasher.Add(r.RequestURI)

	return hasher.Hash(), nil
}
