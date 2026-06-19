package archives

import (
	"bufio"
	"bytes"
)

// Format identifies a container/compression format.
type Format int

// Supported formats.
const (
	FormatNone  Format = iota // not a recognized archive (leaf file)
	FormatZip                 // zip and zip-based (apk/jar/ipa/...)
	FormatTar                 // uncompressed tar
	FormatGzip                // gzip stream
	FormatBzip2               // bzip2 stream
	FormatXz                  // xz stream
	FormatZstd                // zstandard stream
	FormatAr                  // Unix ar archive (Debian .deb)
)

// peekSize is the number of leading bytes inspected for magic detection. Tar's
// "ustar" magic lives at offset 257, so we need at least 263 bytes.
const peekSize = 512

// detect inspects the leading bytes of br (without consuming them) and returns
// the container format, or FormatNone for a leaf file.
func detect(br *bufio.Reader) Format {
	hdr, _ := br.Peek(peekSize)

	switch {
	case hasPrefix(hdr, []byte{0x50, 0x4b, 0x03, 0x04}),
		hasPrefix(hdr, []byte{0x50, 0x4b, 0x05, 0x06}),
		hasPrefix(hdr, []byte{0x50, 0x4b, 0x07, 0x08}):
		return FormatZip
	case hasPrefix(hdr, []byte{0x1f, 0x8b}):
		return FormatGzip
	case hasPrefix(hdr, []byte{0x42, 0x5a, 0x68}): // "BZh"
		return FormatBzip2
	case hasPrefix(hdr, []byte{0xfd, 0x37, 0x7a, 0x58, 0x5a, 0x00}): // xz
		return FormatXz
	case hasPrefix(hdr, []byte{0x28, 0xb5, 0x2f, 0xfd}): // zstd
		return FormatZstd
	case hasPrefix(hdr, []byte("!<arch>\n")): // ar / .deb
		return FormatAr
	case isTar(hdr):
		return FormatTar
	default:
		return FormatNone
	}
}

func hasPrefix(b, prefix []byte) bool {
	return len(b) >= len(prefix) && bytes.Equal(b[:len(prefix)], prefix)
}

// isTar checks for the POSIX "ustar" magic at offset 257.
func isTar(hdr []byte) bool {
	if len(hdr) < 263 {
		return false
	}
	magic := hdr[257:262]
	return bytes.Equal(magic, []byte("ustar"))
}
