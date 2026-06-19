// Package archives provides recursive walking of archive and compressed-container
// files (zip, tar, gzip, bzip2, xz, zstd, ar/deb), yielding each contained file
// as a stream. It is built for tools that need to read inside nested containers
// — firmware images, Android APKs, Debian packages, JARs — such as scanners,
// indexers, and forensics utilities.
//
// The entry point is [Walk]. Walking is bounded against decompression bombs and
// pathological nesting via [Options] (maximum depth and maximum total
// decompressed bytes). The package depends only on the standard library plus
// pure-Go xz and zstd decoders, so it adds no CGO.
package archives

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// PathSeparator joins an archive path to a member path in virtual paths, e.g.
// "firmware.tar.gz!/bin/busybox". It mirrors the convention used by jar URLs.
const PathSeparator = "!/"

// Default limits guarding against decompression bombs and pathological nesting.
const (
	DefaultMaxDepth = 8
	// DefaultMaxBytes caps total decompressed bytes processed per top-level
	// input (2 GiB), a backstop against decompression bombs.
	DefaultMaxBytes int64 = 2 << 30
)

// errBudgetExceeded is returned internally when the decompressed-byte budget is
// exhausted; Walk treats it as a graceful stop, not a failure.
var errBudgetExceeded = errors.New("archive: decompressed-size budget exceeded")

// Options configures a Walk.
type Options struct {
	// MaxDepth bounds archive nesting; members deeper than this are emitted as
	// opaque leaves rather than descended into. 0 uses DefaultMaxDepth.
	MaxDepth int
	// MaxBytes bounds total decompressed bytes per top-level input. 0 uses
	// DefaultMaxBytes; negative means unlimited.
	MaxBytes int64
	// Logger receives non-fatal warnings encountered during a walk — an
	// unreadable member, a malformed sub-archive, the byte budget being hit.
	// These conditions are recovered (the walk skips the member, scans the raw
	// bytes, or stops gracefully) rather than returned as errors, so they are
	// reported here. A nil Logger (the default) discards them, keeping the
	// library silent unless the caller opts in.
	Logger *slog.Logger
}

// warn emits a non-fatal walk warning to the configured Logger, if any.
func (o Options) warn(msg string, args ...any) {
	if o.Logger != nil {
		o.Logger.Warn(msg, args...)
	}
}

func (o Options) maxDepth() int {
	if o.MaxDepth <= 0 {
		return DefaultMaxDepth
	}
	return o.MaxDepth
}

func (o Options) maxBytes() int64 {
	if o.MaxBytes == 0 {
		return DefaultMaxBytes
	}
	return o.MaxBytes
}

// WalkFunc is invoked once per leaf file discovered in the archive tree. ctx is
// the context passed to Walk, allowing the callback to honor cancellation in its
// own work. path is the virtual path (e.g. "pkg.deb!/data.tar!/usr/bin/htop") and
// r streams the member's decompressed contents. Returning an error aborts the
// walk.
type WalkFunc func(ctx context.Context, path string, r io.Reader) error

// budget tracks remaining decompressed bytes across a single walk.
type budget struct {
	remaining int64
	unlimited bool
}

func (b *budget) reader(r io.Reader) io.Reader {
	if b.unlimited {
		return r
	}
	return &budgetReader{b: b, r: r}
}

// budgetReader decrements the shared budget as bytes are read and stops the walk
// once it is exhausted.
type budgetReader struct {
	b *budget
	r io.Reader
}

func (br *budgetReader) Read(p []byte) (int, error) {
	if br.b.remaining <= 0 {
		return 0, errBudgetExceeded
	}
	if int64(len(p)) > br.b.remaining {
		p = p[:br.b.remaining]
	}
	n, err := br.r.Read(p)
	br.b.remaining -= int64(n)
	return n, err
}

// ctxReader aborts a read once ctx is cancelled, making long single reads (large
// members, leaves, nested-zip buffering) cancellable without waiting for a member
// boundary.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (cr *ctxReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	return cr.r.Read(p)
}

// wrap composes the context check (outermost, so cancellation short-circuits
// before budget accounting) over the budget reader for a stream handed to
// recursion or a callback.
func wrap(ctx context.Context, b *budget, r io.Reader) io.Reader {
	return &ctxReader{ctx: ctx, r: b.reader(r)}
}

// Walk opens the file at path, detects its container format, and invokes fn for
// every leaf file in the (possibly nested) archive tree. A non-archive file is
// emitted as a single leaf at its own path. Decompression-bomb and depth limits
// are enforced per Options.
//
// The walk is cancellable via ctx: cancellation is observed between members and
// mid-stream during long reads, and Walk returns ctx.Err() (context.Canceled or
// context.DeadlineExceeded) when the walk is aborted. A graceful budget-exceeded
// stop is reported as a nil error.
func Walk(ctx context.Context, path string, opts Options, fn WalkFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("error opening file: %w", err)
	}
	defer func() { _ = f.Close() }()

	b := &budget{remaining: opts.maxBytes(), unlimited: opts.maxBytes() < 0}
	br := bufio.NewReaderSize(f, peekSize)

	// Top-level zip: use random access via the *os.File to avoid buffering the
	// whole archive (APKs/jars can be hundreds of MB).
	if detect(br) == FormatZip {
		err = walkTopLevelZip(ctx, path, f, opts, b, fn)
	} else {
		err = walkStream(ctx, path, br, 0, opts, b, fn)
	}

	if errors.Is(err, errBudgetExceeded) {
		opts.warn("decompression-size budget reached, stopping recursion", "path", path)
		return nil
	}
	return err
}

// IsArchiveName reports whether a filename looks like a supported archive by its
// extension. Used to decide, before opening, whether recursion is applicable.
func IsArchiveName(name string) bool {
	lower := strings.ToLower(name)
	for _, ext := range archiveExtensions {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

var archiveExtensions = []string{
	".zip", ".jar", ".apk", ".ipa", ".aar", ".war",
	".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tar.xz", ".txz",
	".tar.zst", ".gz", ".bz2", ".xz", ".zst", ".zstd",
	".deb", ".a", ".ar",
}
