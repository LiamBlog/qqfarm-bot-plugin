package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// Static code is shared by all admin instances. No account data or credentials
// are included in the page, and the normal authorization gate runs first.
type adminAsset struct {
	plain, zipped               []byte
	contentType, etag, gzipETag string
}

var adminAssetsOnce sync.Once
var sharedAdminAssets map[string]*adminAsset

func prepareAdminAssets() map[string]*adminAsset {
	adminAssetsOnce.Do(func() {
		read := func(name string) []byte {
			b, err := webFS.ReadFile("web/" + name)
			if err != nil {
				panic("embedded admin resource missing: " + name)
			}
			return b
		}
		page, css, js := read("index.html"), read("enhancements.css"), read("enhancements.js")
		page = bytes.Replace(page, []byte(`<link rel="stylesheet" href="enhancements.css">`), append(append([]byte("<style>\n"), css...), []byte("\n</style>")...), 1)
		page = bytes.Replace(page, []byte(`<script src="enhancements.js"></script>`), append(append([]byte("<script>\n"), js...), []byte("\n</script>")...), 1)
		makeAsset := func(body []byte, mime string) *adminAsset {
			var compressed bytes.Buffer
			w, err := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
			if err != nil {
				panic(err)
			}
			if _, err = w.Write(body); err != nil {
				panic(err)
			}
			if err = w.Close(); err != nil {
				panic(err)
			}
			zipped := compressed.Bytes()
			return &adminAsset{plain: body, zipped: zipped, contentType: mime, etag: fmt.Sprintf(`"%x"`, sha256.Sum256(body)), gzipETag: fmt.Sprintf(`"%x"`, sha256.Sum256(zipped))}
		}
		html := makeAsset(page, "text/html; charset=utf-8")
		sharedAdminAssets = map[string]*adminAsset{"/": html, "/index.html": html, "/enhancements.css": makeAsset(css, "text/css; charset=utf-8"), "/enhancements.js": makeAsset(js, "text/javascript; charset=utf-8")}
	})
	return sharedAdminAssets
}

func acceptsGzip(header string) bool {
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		name := strings.ToLower(strings.TrimSpace(fields[0]))
		quality := 1.0
		for _, parameter := range fields[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if ok && strings.EqualFold(key, "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if err != nil {
					q = 0
				}
				quality = q
			}
		}
		if name == "gzip" {
			return quality > 0
		}
		if name == "*" {
			wildcard = quality > 0
		}
	}
	return wildcard
}

func etagMatches(header, tag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
			return true
		}
	}
	return false
}

func (a *Admin) serveAdminAsset(w http.ResponseWriter, r *http.Request) bool {
	asset, ok := prepareAdminAssets()[r.URL.Path]
	if !ok {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return true
	}
	body, tag := asset.plain, asset.etag
	if acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body, tag = asset.zipped, asset.gzipETag
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", tag)
	w.Header().Add("Vary", "Accept-Encoding")
	if etagMatches(r.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	return true
}
