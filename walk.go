package archives

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
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
func walkStream(name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	// At the depth limit, stop descending and treat the stream as a leaf so its
	// raw bytes are still scanned. depth counts containers already opened to
	// reach this stream, so MaxDepth=1 opens only the top-level container.
	if depth >= opts.maxDepth() {
		return fn(name, br)
	}

	switch detect(br) {
	case FormatZip:
		return walkZipStream(name, br, depth, opts, b, fn)
	case FormatTar:
		return walkTar(name, br, depth, opts, b, fn)
	case FormatAr:
		return walkAr(name, br, depth, opts, b, fn)
	case FormatGzip:
		return walkCompressed(name, ".gz", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return gzip.NewReader(r)
		})
	case FormatBzip2:
		return walkCompressed(name, ".bz2", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return bzip2.NewReader(r), nil
		})
	case FormatXz:
		return walkCompressed(name, ".xz", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			return xz.NewReader(r)
		})
	case FormatZstd:
		return walkCompressed(name, ".zst", br, depth, opts, b, fn, func(r io.Reader) (io.Reader, error) {
			zr, err := zstd.NewReader(r)
			if err != nil {
				return nil, err
			}
			return zr.IOReadCloser(), nil
		})
	default:
		return fn(name, br) // leaf
	}
}

// walkCompressed decompresses a single-stream compressor and recurses on the
// decompressed content (which may itself be a tar or another archive). trimExt
// is stripped to derive the inner name (e.g. "data.tar.xz" -> "data.tar").
func walkCompressed(name, trimExt string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc, open func(io.Reader) (io.Reader, error)) error {
	dr, err := open(br)
	if err != nil {
		fmt.Fprintf(os.Stderr, "txtr: %s: cannot decompress (%v), scanning raw\n", name, err)
		return fn(name, br)
	}
	inner := strings.TrimSuffix(name, trimExt)
	if inner == name {
		inner = name + ".out"
	}
	// Budget the decompressed output: this is where bomb expansion occurs.
	return walkStream(inner, bufio.NewReaderSize(b.reader(dr), peekSize), depth+1, opts, b, fn)
}

func walkTar(name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	tr := tar.NewReader(br)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if errors.Is(err, errBudgetExceeded) {
				return err
			}
			fmt.Fprintf(os.Stderr, "txtr: %s: tar read error: %v\n", name, err)
			return nil
		}
		// Process regular files only. 0 is the legacy regular-file flag used by
		// old tar writers (modern equivalent of the deprecated TypeRegA).
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != 0 {
			continue
		}
		child := name + PathSeparator + cleanMemberName(hdr.Name)
		if err := walkStream(child, bufio.NewReaderSize(tr, peekSize), depth+1, opts, b, fn); err != nil {
			return err
		}
	}
}

// walkTopLevelZip handles a zip file on disk using random access via the
// *os.File, avoiding buffering the whole archive into memory.
func walkTopLevelZip(name string, f *os.File, opts Options, b *budget, fn WalkFunc) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(f, fi.Size())
	if err != nil {
		fmt.Fprintf(os.Stderr, "txtr: %s: cannot open zip: %v\n", name, err)
		return nil
	}
	return walkZipReader(name, zr, 0, opts, b, fn)
}

// walkZipStream handles a zip arriving as a stream (nested inside another
// archive). zip requires random access, so the stream is buffered (capped).
func walkZipStream(name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	data, err := io.ReadAll(io.LimitReader(br, zipMemoryCap))
	if err != nil {
		if errors.Is(err, errBudgetExceeded) {
			return err
		}
		fmt.Fprintf(os.Stderr, "txtr: %s: zip read error: %v\n", name, err)
		return nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "txtr: %s: cannot open zip: %v\n", name, err)
		return nil
	}
	return walkZipReader(name, zr, depth, opts, b, fn)
}

// walkZipReader iterates zip members, budgeting each member's decompressed
// stream.
func walkZipReader(name string, zr *zip.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			fmt.Fprintf(os.Stderr, "txtr: %s: cannot open zip member %q: %v\n", name, zf.Name, err)
			continue
		}
		child := name + PathSeparator + cleanMemberName(zf.Name)
		err = walkStream(child, bufio.NewReaderSize(b.reader(rc), peekSize), depth+1, opts, b, fn)
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
