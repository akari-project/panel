// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func writeApp(t *testing.T, web, app, index string) {
	t.Helper()
	dist := filepath.Join(web, app, "dist")
	if err := os.MkdirAll(filepath.Join(dist, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	must(t, os.WriteFile(filepath.Join(dist, "index.html"), []byte(index), 0o644))
	must(t, os.WriteFile(filepath.Join(dist, "assets", "index-AbC12_-9.js"), bytes.Repeat([]byte("console.log(1);"), 200), 0o644))
	must(t, os.WriteFile(filepath.Join(dist, "assets", "logo-QQQQQQQQ.png"), bytes.Repeat([]byte{1}, 4096), 0o644))
	must(t, os.WriteFile(filepath.Join(dist, "tiny.txt"), []byte("x"), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPrepare(t *testing.T) {
	web, out := t.TempDir(), t.TempDir()
	for _, app := range apps {
		writeApp(t, web, app, `<html><head><script type="module" src="./assets/index-AbC12_-9.js"></script></head></html>`)
	}
	must(t, os.WriteFile(filepath.Join(out, ".gitkeep"), nil, 0o644))
	must(t, os.MkdirAll(filepath.Join(out, "portal"), 0o755))
	must(t, os.WriteFile(filepath.Join(out, "portal", "stale.js"), []byte("old"), 0o644))

	if err := prepare(web, out, commit); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "build.json"))
	if err != nil || !strings.Contains(string(b), commit) {
		t.Fatalf("build.json = %s, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(out, "portal", "stale.js")); !os.IsNotExist(err) {
		t.Error("stale output from a previous build kept")
	}
	if _, err := os.Stat(filepath.Join(out, ".gitkeep")); err != nil {
		t.Error(".gitkeep removed")
	}

	js := filepath.Join(out, "admin", "assets", "index-AbC12_-9.js")
	raw, _ := os.ReadFile(js)
	br, err := os.ReadFile(js + ".br")
	if err != nil {
		t.Fatal(err)
	}
	dec, _ := io.ReadAll(brotli.NewReader(bytes.NewReader(br)))
	if !bytes.Equal(dec, raw) {
		t.Error(".br does not decompress to the original")
	}
	gzf, err := os.Open(js + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	defer gzf.Close()
	zr, err := gzip.NewReader(gzf)
	if err != nil {
		t.Fatal(err)
	}
	dec, _ = io.ReadAll(zr)
	if !bytes.Equal(dec, raw) {
		t.Error(".gz does not decompress to the original")
	}
	for _, p := range []string{"admin/assets/logo-QQQQQQQQ.png.br", "admin/tiny.txt.br", "admin/index.html.br"} {
		if _, err := os.Stat(filepath.Join(out, p)); !os.IsNotExist(err) {
			t.Errorf("%s should not be generated", p)
		}
	}
}

func TestPrepareRejects(t *testing.T) {
	web := t.TempDir()
	writeApp(t, web, "portal", `<html><head><script src="/assets/index.js"></script></head></html>`)
	writeApp(t, web, "admin", `<html></html>`)
	if err := prepare(web, t.TempDir(), commit); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Errorf("absolute asset path: %v", err)
	}
	if err := prepare(web, t.TempDir(), "abc"); err == nil {
		t.Error("short commit accepted")
	}
	web2 := t.TempDir()
	for _, app := range apps {
		writeApp(t, web2, app, `<html></html>`)
	}
	must(t, os.WriteFile(filepath.Join(web2, "admin", "dist", "build.json"), []byte(`{"commit":"ffffffffffffffffffffffffffffffffffffffff"}`), 0o644))
	if err := prepare(web2, t.TempDir(), commit); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("stale frontend build.json: %v", err)
	}
	if err := prepare(t.TempDir(), t.TempDir(), commit); err == nil {
		t.Error("missing dist accepted")
	}
}
