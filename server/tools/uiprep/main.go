// SPDX-License-Identifier: AGPL-3.0-or-later

// Command uiprep 把前端构建产物放进 internal/webui/dist，供 go build 嵌入（spec/40 DEP-01、DEP-03）：
//
//  1. 复制 web/portal/dist 与 web/admin/dist 到 <out>/portal 与 <out>/admin；
//  2. 为可压缩的静态文件预生成 .br 与 .gz（只保留比原文件小的）；
//  3. 写入 <out>/build.json，记录构建所用的 git 提交，与二进制 -ldflags 中的提交一致。
//
// 用法：go run ./tools/uiprep -web ../web -out internal/webui/dist -commit $(git rev-parse HEAD)
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/andybalholm/brotli"
)

var apps = []string{"portal", "admin"}

// 预压缩的扩展名；图片等已压缩格式不处理。index.html 每次响应都会注入配置，不预压缩。
var compressible = map[string]bool{
	".js": true, ".mjs": true, ".css": true, ".html": true, ".svg": true, ".json": true,
	".txt": true, ".xml": true, ".map": true, ".wasm": true, ".webmanifest": true, ".ico": true,
}

const minCompressSize = 1024

// absoluteAsset 匹配 index.html 中以 / 开头的资源引用：挂载在路径前缀下时会失效（spec/32 UI-06）。
var absoluteAsset = regexp.MustCompile(`(?i)\b(?:src|href)\s*=\s*["']/[^/"']`)

func main() {
	web := flag.String("web", "../web", "directory containing portal/dist and admin/dist")
	out := flag.String("out", "internal/webui/dist", "output directory embedded by internal/webui")
	commit := flag.String("commit", "", "git commit the frontends were built from")
	flag.Parse()
	if err := prepare(*web, *out, *commit); err != nil {
		fmt.Fprintln(os.Stderr, "uiprep:", err)
		os.Exit(1)
	}
}

func prepare(web, out, commit string) error {
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(commit) {
		return fmt.Errorf("-commit must be a full 40-character git commit, got %q", commit)
	}
	for _, app := range apps {
		src := filepath.Join(web, app, "dist")
		idx, err := os.ReadFile(filepath.Join(src, "index.html"))
		if err != nil {
			return fmt.Errorf("%s: %w (run pnpm -r build first)", app, err)
		}
		// 前端构建写入的 build.json 必须与本次构建的提交一致（DEP-01）。
		if b, err := os.ReadFile(filepath.Join(src, "build.json")); err == nil {
			var bi struct {
				Commit string `json:"commit"`
			}
			if err := json.Unmarshal(b, &bi); err != nil || bi.Commit != commit {
				return fmt.Errorf("%s/dist/build.json commit %q does not match %s; rebuild the frontends", app, bi.Commit, commit)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if m := absoluteAsset.Find(idx); m != nil {
			return fmt.Errorf("%s/index.html references an absolute path (%s); assets must use relative paths (spec/32 UI-06)", app, m)
		}
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	for _, app := range apps {
		dst := filepath.Join(out, app)
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		if err := copyTree(filepath.Join(web, app, "dist"), dst); err != nil {
			return fmt.Errorf("%s: %w", app, err)
		}
		if err := precompress(dst); err != nil {
			return fmt.Errorf("%s: %w", app, err)
		}
	}
	b, _ := json.Marshal(map[string]string{"commit": commit})
	return os.WriteFile(filepath.Join(out, "build.json"), append(b, '\n'), 0o644)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s: symlinks are not allowed in build output", rel)
		case strings.HasSuffix(p, ".br") || strings.HasSuffix(p, ".gz"):
			return nil // 由本工具重新生成
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

func precompress(dir string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Base(p) == "index.html" && filepath.Dir(p) == dir {
			return nil
		}
		if !compressible[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil || len(raw) < minCompressSize {
			return err
		}
		var br, gz bytes.Buffer
		bw := brotli.NewWriterLevel(&br, brotli.BestCompression)
		gw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		if _, err := bw.Write(raw); err != nil {
			return err
		}
		if _, err := gw.Write(raw); err != nil {
			return err
		}
		if err := errors.Join(bw.Close(), gw.Close()); err != nil {
			return err
		}
		for ext, buf := range map[string]*bytes.Buffer{".br": &br, ".gz": &gz} {
			if buf.Len() < len(raw) {
				if err := os.WriteFile(p+ext, buf.Bytes(), 0o644); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
