package archives

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
)

// arMagic is the global header of a Unix ar archive.
const arMagic = "!<arch>\n"

// arHeaderSize is the size of each per-member ar header.
const arHeaderSize = 60

// walkAr parses a Unix ar archive (the container format of Debian .deb files)
// and recurses into each member. Member layout: a 60-byte header (16-byte name,
// ..., 10-byte decimal size at offset 48, "`\n" terminator) followed by size
// bytes of data, padded to an even boundary with a trailing newline.
//
// The ar container is uncompressed, so it reads directly from br without
// budgeting; compressed members (e.g. data.tar.xz) are budgeted downstream.
func walkAr(ctx context.Context, name string, br *bufio.Reader, depth int, opts Options, b *budget, fn WalkFunc) error {
	magic := make([]byte, len(arMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != arMagic {
		opts.warn("not a valid ar archive", "path", name)
		return nil
	}

	hdr := make([]byte, arHeaderSize)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := io.ReadFull(br, hdr)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			opts.warn("ar header read error", "path", name, "err", err)
			return nil
		}

		member := strings.TrimSuffix(strings.TrimRight(string(hdr[0:16]), " "), "/")
		size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
		if err != nil {
			opts.warn("invalid ar member size", "path", name, "err", err)
			return nil
		}

		// Bound the member to its declared size, recurse, then drain whatever
		// the walker left unread so the next header is correctly aligned. The
		// ctxReader makes both the recursion and the drain cancellable.
		limited := bufio.NewReaderSize(&ctxReader{ctx: ctx, r: io.LimitReader(br, size)}, peekSize)
		child := name + PathSeparator + cleanMemberName(member)
		walkErr := walkStream(ctx, child, limited, depth+1, opts, b, fn)
		if _, derr := io.Copy(io.Discard, limited); derr != nil && !errors.Is(derr, errBudgetExceeded) {
			return derr
		}
		if walkErr != nil {
			return walkErr
		}

		// Skip the 1-byte padding for odd-sized members.
		if size%2 == 1 {
			if _, err := io.CopyN(io.Discard, br, 1); err != nil && !errors.Is(err, io.EOF) {
				return nil
			}
		}
	}
}
