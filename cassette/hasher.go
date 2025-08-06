package cassette

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

const defaultDelimiter = "::"

// RequestHasher builds a deterministic hash from various request components.
type RequestHasher struct {
	builder   strings.Builder
	delimiter string
}

func NewRequestHasher() *RequestHasher {
	return &RequestHasher{
		delimiter: defaultDelimiter,
	}
}

func (h *RequestHasher) Add(part string) {
	if h.builder.Len() > 0 {
		h.builder.WriteString(h.delimiter)
	}
	h.builder.WriteString(part)
}

func (h *RequestHasher) String() string {
	return h.builder.String()
}

func (h *RequestHasher) Hash() string {
	hashBytes := sha256.Sum256([]byte(h.builder.String()))
	return fmt.Sprintf("%x", hashBytes)
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
		values := h[k]
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
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
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
	hasher.Add(fmt.Sprintf("%d", r.ProtoMajor))
	hasher.Add(fmt.Sprintf("%d", r.ProtoMinor))
	hasher.Add(serializeHeaders(r.Header, ignoreHeaders))
	hasher.Add(string(bodyBytes))
	hasher.Add(fmt.Sprintf("%d", r.ContentLength))
	hasher.Add(serializeHeaders(r.Trailer, nil))
	hasher.Add(serializeTransferEncoding(r.TransferEncoding))
	hasher.Add(r.RemoteAddr)
	hasher.Add(r.RequestURI)

	return hasher.Hash(), nil
}
