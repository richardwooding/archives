# archives

[![CI](https://github.com/richardwooding/archives/actions/workflows/ci.yml/badge.svg)](https://github.com/richardwooding/archives/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/richardwooding/archives.svg)](https://pkg.go.dev/github.com/richardwooding/archives)
[![Go Report Card](https://goreportcard.com/badge/github.com/richardwooding/archives)](https://goreportcard.com/report/github.com/richardwooding/archives)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

Recursively walk archive and compressed-container files in Go and get a stream for **every file
inside**, no matter how deeply nested. Point it at an APK, a `.deb`, or a `firmware.tar.gz` and it
hands you each contained file in turn — descending through `ar → tar.xz → ELF` or `zip → dex`
automatically — with built-in decompression-bomb guards.

Supported containers: **zip** (and zip-based `.apk`/`.jar`/`.ipa`/`.aar`), **tar**, **gzip**,
**bzip2**, **xz**, **zstd**, and **ar** (`.deb`). Nested combinations are recursed automatically.

Extracted from [`txtr`](https://github.com/richardwooding/txtr), where it powers `--recurse`.

## Install

```bash
go get github.com/richardwooding/archives
```

## Usage

```go
package main

import (
	"fmt"
	"io"

	"github.com/richardwooding/archives"
)

func main() {
	err := archives.Walk("pkg.deb", archives.Options{}, func(path string, r io.Reader) error {
		n, _ := io.Copy(io.Discard, r)
		fmt.Printf("%-60s %d bytes\n", path, n)
		// pkg.deb!/data.tar!/usr/bin/htop   123456 bytes
		return nil
	})
	if err != nil {
		panic(err)
	}
}
```

`Walk` opens the file, detects its container format by magic bytes, and invokes the callback once
per **leaf** file with a virtual path (using `!/` to separate nesting levels) and a reader over its
decompressed contents. A non-archive file is emitted as a single leaf at its own path, so callers
can use `Walk` uniformly without checking the type first.

## Safety

Untrusted archives can be hostile (decompression bombs, deeply nested containers). `Walk` is bounded
by `Options`:

```go
archives.Walk(path, archives.Options{
	MaxDepth: 8,                 // max nesting; deeper members are emitted as opaque leaves
	MaxBytes: 2 << 30,           // max total decompressed bytes per input (<0 = unlimited)
}, fn)
```

Zero values use the defaults (`DefaultMaxDepth` = 8, `DefaultMaxBytes` = 2 GiB). When the byte
budget is exhausted, the walk stops gracefully rather than erroring.

## Design

- **Streaming** — files are handed to the callback as `io.Reader`s; the whole archive is never
  loaded into memory (a top-level zip is read via random access on the file).
- **Magic-byte detection** — format is detected from content, not file extension (`IsArchiveName`
  is provided separately for extension-based pre-filtering).
- **Pure Go** — standard library plus the pure-Go `klauspost/compress` (zstd) and `ulikunitz/xz`
  decoders. No CGO.

## License

MIT — see [LICENSE](LICENSE).
