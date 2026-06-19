package archives

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// zipMemoryCap bounds how many bytes a *nested* zip stream is buffered into
// memory (zip needs random access). Larger nested zips are processed partially.
const zipMemoryCap = 512 << 20

// walkStream detects the format of the stream named name and either descends
// into it (archives/compressors) or emits it as a leaf via fn.
//
// Budget accounting is applied only at decompression-expansion points (the
// outputs of compressors and zip members); traversal of uncompressed containers
// (tar/ar) is bounded by the on-disk file size and needs no budgeting.
func walkStream(ctx context.Context, name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	// Every recursion routes through here, so a single ctx check at the funnel
	// aborts the walk promptly between containers and members.
	if err := ctx.Err(); err != nil {
		return err
	}

	// At the depth limit, stop descending and treat the stream as a leaf so its
	// raw bytes are still scanned. depth counts containers already opened to
	// reach this stream, so MaxDepth=1 opens only the top-level container.
	if depth >= opts.maxDepth() {
		return fn(ctx, name, br)
	}

	switch detect(br) {
	case FormatZip:
		return walkZipStream(ctx, name, br, depth, opts, b, fn)
	case FormatTar:
		return walkTar(ctx, name, br, depth, opts, b, fn)
	case FormatAr:
		return walkAr(ctx, name, br, depth, opts, b, fn)
	case FormatGzip:
		return walkCompressed(ctx, name, ".gz", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return gzip.NewReader(r)
		})
	case FormatBzip2:
		return walkCompressed(ctx, name, ".bz2", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return bzip2.NewReader(r), nil
		})
	case FormatXz:
		return walkCompressed(ctx, name, ".xz", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return xz.NewReader(r)
		})
	case FormatZstd:
		return walkCompressed(ctx, name, ".zst", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zr.IOReadCloser(), nil
		})
	default:
		return fn(ctx, name, br) // leaf
	}
}

// walkCompressed decompresses a single-stream compressor and recurses on the
// decompressed content (which may itself be a tar or another archive). trimExt
// is stripped to derive the inner name (e.g. "data.tar.xz" -> "data.tar").
func walkCompressed(ctx context.Context, name, trimExt string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc, open func(io.Reader) (io.Reader, error)) error {
	dr, err := open(br)
	if err != nil {
		opts.warn("cannot decompress, scanning raw", "path", name, "err", err)
		return fn(ctx, name, br)
	}
	inner := strings.TrimSuffix(name, trimExt)
	if inner == name {
		inner = name + ".out"
	}
	// Budget the decompressed output (where bomb expansion occurs) and make the
	// read cancellable mid-stream.
	return walkStream(ctx, inner, bufio.NewReaderSize(wrap(ctx, b, dr), peekSize), depth+1, opts, b, fn)
}

func walkTar(ctx context.Context, name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	tr := tar.NewReader(br)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errBudgetExceeded) || ctx.Err() != nil {
				return err
			}
			opts.warn("tar read error", "path", name, "err", err)
			return nil
		}
		// Process regular files only. 0 is the legacy regular-file flag used by
		// old tar writers (modern equivalent of the deprecated TypeRegA).
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != 0 {
			continue
		}
		child := name + PathSeparator + cleanMemberName(hdr.Name)
		if err := walkStream(ctx, child, bufio.NewReaderSize(&ctxReader{ctx: ctx, r: tr}, peekSize), depth+1, opts, b, fn); err != nil {
			return err
		}
	}
}

// walkTopLevelZip handles a zip file on disk using random access via the
// *os.File, avoiding buffering the whole archive into memory.
func walkTopLevelZip(ctx context.Context, name string, f *os.File, opts Options, b *budget, fn WalkFunc) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		opts.warn("cannot open zip", "path", name, "err", err)
		return nil
	}
	return walkZipReader(ctx, name, zr, 0, opts, b, fn)
}

// walkZipStream handles a zip arriving as a stream (nested inside another
// archive). zip requires random access, so the stream is buffered (capped).
func walkZipStream(ctx context.Context, name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	// Wrap so buffering up to zipMemoryCap (512 MiB) is cancellable mid-read.
	data, err := io.ReadAll(io.LimitReader(&ctxReader{ctx: ctx, r: br}, zipMemoryCap))
	if err != nil {
		if errors.Is(err, errBudgetExceeded) || ctx.Err() != nil {
			return err
		}
		opts.warn("zip read error", "path", name, "err", err)
		return nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		opts.warn("cannot open zip", "path", name, "err", err)
		return nil
	}
	return walkZipReader(ctx, name, zr, depth, opts, b, fn)
}

// walkZipReader iterates zip members, budgeting each member's decompressed
// stream.
func walkZipReader(ctx context.Context, name string, zr *zip.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	for _, zf := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			opts.warn("cannot open zip member", "path", name, "member", zf.Name, "err", err)
			continue
		}
		child := name + PathSeparator + cleanMemberName(zf.Name)
		err = walkStream(ctx, child, bufio.NewReaderSize(wrap(ctx, b, rc), peekSize), depth+1, opts, b, fn)
		_ = rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// cleanMemberName trims a leading "./" and any leading slash so virtual paths
// read cleanly.
func cleanMemberName(name string) string {
	name = strings.TrimPrefix(name, "./")
	return strings.TrimPrefix(name, "/")
}
