package vcr_test

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/goware/go-vcr/cassette"
	"github.com/goware/go-vcr/recorder"
)

// basicRequestHasher generates a simple, predictable hash for testing.
// It only includes Method, URL path, query, and body - making hashes stable
// regardless of host, port, or header variations.
func basicRequestHasher(r *http.Request) (string, error) {
	var bodyBytes []byte
	if r.Body != nil && r.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			return "", err
		}
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
	}

	h := sha256.New()
	h.Write([]byte(r.Method))
	h.Write([]byte("::"))
	if r.URL != nil {
		h.Write([]byte(r.URL.Path))
		h.Write([]byte("::"))
		h.Write([]byte(r.URL.RawQuery))
	}
	h.Write([]byte("::"))
	h.Write(bodyBytes)

	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func TestMiddleware(t *testing.T) {
	cassetteName := "testdata/middleware"

	// In a real-world scenario, the recorder will run outside of unit tests
	// since you want to be able to record real application behavior
	t.Run("RecordRealInteractionsWithMiddleware", func(t *testing.T) {
		rec, err := recorder.New(
			cassetteName,
			recorder.WithMode(recorder.ModeRecordOnly),
			// Use basicRequestHasher for stable hashes across test runs with random ports
			recorder.WithHasher(basicRequestHasher),
			// Use a BeforeSaveHook to remove host, remote_addr, and duration
			// since they change whenever the test runs
			recorder.WithHook(func(i *cassette.Interaction) error {
				i.Request.Host = ""
				i.Request.RemoteAddr = ""
				i.Response.Duration = 0
				return nil
			}, recorder.BeforeSaveHook),
		)
		if err != nil {
			t.Errorf("error creating recorder: %v", err)
		}

		// Create the server handler with recorder middleware
		handler := createHandler(rec.HTTPMiddleware)
		t.Cleanup(func() {
			// Make sure recorder is stopped once done with it.
			if err := rec.Stop(); err != nil {
				t.Error(err)
			}
		})

		server := httptest.NewServer(handler)
		defer server.Close()

		_, err = http.Get(server.URL + "/request1")
		if err != nil {
			t.Errorf("error making request: %v", err)
		}

		_, err = http.Get(server.URL + "/request2?query=example")
		if err != nil {
			t.Errorf("error making request: %v", err)
		}

		_, err = http.PostForm(server.URL+"/postform", url.Values{"key": []string{"value"}})
		if err != nil {
			t.Errorf("error making request: %v", err)
		}

		_, err = http.Post(server.URL+"/postdata", "application/json", bytes.NewBufferString(`{"key":"value"}`))
		if err != nil {
			t.Errorf("error making request: %v", err)
		}
	})

	t.Run("ReplayCassetteAndCompare", func(t *testing.T) {
		cassette.TestServerReplay(t, cassetteName, createHandler(nil))
	})
}

// createHandler will return an HTTP handler with optional middleware. It will respond to
// simple requests for testing
func createHandler(middleware func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("KEY", "VALUE")

		query := r.URL.Query().Encode()
		if query != "" {
			w.Write([]byte(query + "\n"))
		}

		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			w.Write(body)
		} else {
			w.Write([]byte("OK"))
		}
	})

	if middleware != nil {
		handler = middleware(handler).ServeHTTP
	}

	mux.Handle("/", handler)
	return mux
}
