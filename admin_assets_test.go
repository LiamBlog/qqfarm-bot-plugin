package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func assetRequest(t *testing.T, a *Admin, method, path, encoding, tag, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if token != "" {
		r.Header.Set("x-mengka-managed-token", token)
	}
	if encoding != "" {
		r.Header.Set("Accept-Encoding", encoding)
	}
	if tag != "" {
		r.Header.Set("If-None-Match", tag)
	}
	w := httptest.NewRecorder()
	a.handler(w, r)
	return w
}

func TestAdminSelfContainedPage(t *testing.T) {
	a := &Admin{token: "secret"}
	w := assetRequest(t, a, http.MethodGet, "/", "", "", "secret")
	body := w.Body.String()
	if w.Code != 200 || strings.Contains(body, `<script src="enhancements.js">`) || strings.Contains(body, `<link rel="stylesheet" href="enhancements.css">`) {
		t.Fatal("critical assets were not bundled")
	}
	for _, marker := range []string{"function syncAccountInputs", ".account-filters", "window.__adminInitialState=fetch", "const initial=window.__adminInitialState"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing native page content: %s", marker)
		}
	}
	if strings.Index(body, "window.__adminInitialState=fetch") > strings.Index(body, "<style>") {
		t.Fatal("initial state request starts too late")
	}
}

func TestAdminCompressedAssetsAndHead(t *testing.T) {
	a := &Admin{token: "secret"}
	for _, path := range []string{"/", "/index.html", "/enhancements.js", "/enhancements.css"} {
		t.Run(path, func(t *testing.T) {
			plain := assetRequest(t, a, http.MethodGet, path, "", "", "secret")
			compressed := assetRequest(t, a, http.MethodGet, path, "br, gzip;q=0.8", "", "secret")
			if plain.Code != 200 || compressed.Code != 200 || compressed.Header().Get("Content-Encoding") != "gzip" {
				t.Fatal("gzip negotiation failed")
			}
			reader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := io.ReadAll(reader)
			reader.Close()
			if err != nil || !bytes.Equal(decoded, plain.Body.Bytes()) {
				t.Fatal("compressed representation changed asset content")
			}
			if compressed.Body.Len() >= plain.Body.Len()/2 {
				t.Fatal("precompressed asset did not reduce transfer size")
			}
			if compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
				t.Fatal("strong validators must distinguish encodings")
			}
			head := assetRequest(t, a, http.MethodHead, path, "gzip", "", "secret")
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != strconv.Itoa(compressed.Body.Len()) {
				t.Fatal("HEAD representation is incorrect")
			}
			if compressed.Header().Get("Cache-Control") != "private, no-cache" || compressed.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatal("static caching must be private and representation-aware")
			}
		})
	}
}

func TestAdminAssetRevalidationKeepsAuthorization(t *testing.T) {
	a := &Admin{token: "secret"}
	first := assetRequest(t, a, http.MethodGet, "/", "", "", "secret")
	tag := first.Header().Get("ETag")
	if tag == "" {
		t.Fatal("asset has no validator")
	}
	cached := assetRequest(t, a, http.MethodGet, "/", "", `"other", W/`+tag, "secret")
	if cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatal("unchanged static page was retransmitted")
	}
	denied := assetRequest(t, a, http.MethodGet, "/", "", tag, "wrong")
	if denied.Code != http.StatusForbidden || denied.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("conditional requests bypassed authorization")
	}
	changed := assetRequest(t, a, http.MethodGet, "/", "", `"old"`, "secret")
	if changed.Code != 200 || changed.Body.Len() == 0 {
		t.Fatal("stale validator did not receive current content")
	}
}

func TestAdminGzipQuality(t *testing.T) {
	for _, test := range []struct {
		header string
		want   bool
	}{{"", false}, {"br", false}, {"gzip", true}, {"GZIP;q=0.5", true}, {"gzip;q=0", false}, {"gzip;q=invalid", false}, {"*;q=1", true}, {"*;q=1,gzip;q=0", false}, {"br,gzip;q=0,*;q=1", false}} {
		if got := acceptsGzip(test.header); got != test.want {
			t.Errorf("%q: gzip=%t, want %t", test.header, got, test.want)
		}
	}
}

func TestAdminStateRemainsUncached(t *testing.T) {
	t.Setenv("MENGKA_PLUGIN_ADMIN_TOKEN_FILE", "")
	t.Setenv("MENGKA_PLUGIN_ADMIN_ORIGIN", "")
	dir := t.TempDir()
	cfg := LoadConfig(dir)
	cfg.Set(FarmConfig{Enabled: false})
	a := newAdmin(cfg, newBridge(cfg, newFakeIPC(t, "X"), dir), dir)
	w := assetRequest(t, a, http.MethodGet, "/api/state?h=1", "gzip", "*", "")
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
		t.Fatalf("business state was cached: %d", w.Code)
	}
}
