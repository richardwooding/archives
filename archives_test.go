package archives

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// collect walks path and returns a map of virtual-path -> contents for every
// leaf the walker emits.
func collect(t *testing.T, path string, opts Options) map[string]string {
	t.Helper()
	got := map[string]string{}
	err := Walk(context.Background(), path, opts, func(_ context.Context, p string, r io.Reader) error {
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		got[p] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk(%s) error: %v", path, err)
	}
	return got
}

func writeTemp(t testing.TB, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return p
}

func makeZip(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// deterministic order
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeTar(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		body := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestWalkZip(t *testing.T) {
	z := makeZip(t, map[string]string{"a.txt": "alpha", "dir/b.txt": "bravo"})
	path := writeTemp(t, "test.zip", z)
	got := collect(t, path, Options{})

	if got[path+"!/a.txt"] != "alpha" {
		t.Errorf("a.txt = %q, want alpha", got[path+"!/a.txt"])
	}
	if got[path+"!/dir/b.txt"] != "bravo" {
		t.Errorf("dir/b.txt = %q, want bravo", got[path+"!/dir/b.txt"])
	}
}

func TestWalkTarGz(t *testing.T) {
	tarball := makeTar(t, map[string]string{"./etc/passwd": "root:x:0:0", "bin/sh": "ELFsh"})
	path := writeTemp(t, "test.tar.gz", gz(t, tarball))
	got := collect(t, path, Options{})

	// The .gz layer is stripped, so members hang off the decompressed .tar name.
	base := strings.TrimSuffix(path, ".gz")
	if got[base+"!/etc/passwd"] != "root:x:0:0" {
		t.Errorf("etc/passwd = %q (keys: %v)", got[base+"!/etc/passwd"], keys(got))
	}
	if got[base+"!/bin/sh"] != "ELFsh" {
		t.Errorf("bin/sh = %q", got[base+"!/bin/sh"])
	}
}

func TestWalkNestedZipInTar(t *testing.T) {
	inner := makeZip(t, map[string]string{"secret.txt": "deep"})
	tarball := makeTar(t, map[string]string{"payload.zip": string(inner)})
	path := writeTemp(t, "outer.tar", tarball)
	got := collect(t, path, Options{})

	want := path + "!/payload.zip!/secret.txt"
	if got[want] != "deep" {
		t.Errorf("nested member %q = %q, want deep (got keys: %v)", want, got[want], keys(got))
	}
}

func TestWalkPlainFile(t *testing.T) {
	// A non-archive file is emitted as a single leaf at its own path.
	path := writeTemp(t, "plain.bin", []byte("just some bytes"))
	got := collect(t, path, Options{})
	if len(got) != 1 || got[path] != "just some bytes" {
		t.Errorf("plain file walk = %v, want single leaf with raw bytes", got)
	}
}

func TestMaxDepthStopsDescending(t *testing.T) {
	// zip inside tar; with MaxDepth=1 the inner zip is emitted as an opaque leaf
	// rather than descended into.
	inner := makeZip(t, map[string]string{"secret.txt": "deep"})
	tarball := makeTar(t, map[string]string{"payload.zip": string(inner)})
	path := writeTemp(t, "outer.tar", tarball)

	got := collect(t, path, Options{MaxDepth: 1})
	for k := range got {
		if strings.Contains(k, "secret.txt") {
			t.Errorf("MaxDepth=1 should not have descended into inner zip; got %q", k)
		}
	}
	// The inner zip itself should appear as a leaf.
	if _, ok := got[path+"!/payload.zip"]; !ok {
		t.Errorf("expected inner zip as leaf; got keys %v", keys(got))
	}
}

func TestMaxBytesBudget(t *testing.T) {
	// A gzip member that decompresses to more than the budget is truncated
	// gracefully (no error, partial data).
	big := bytes.Repeat([]byte("A"), 1<<20) // 1 MiB
	path := writeTemp(t, "big.gz", gz(t, big))

	var total int
	err := Walk(context.Background(), path, Options{MaxBytes: 4096}, func(_ context.Context, _ string, r io.Reader) error {
		n, _ := io.Copy(io.Discard, r)
		total += int(n)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if total > 4096 {
		t.Errorf("read %d bytes, want <= budget 4096", total)
	}
}

func TestIsArchiveName(t *testing.T) {
	yes := []string{"a.zip", "b.APK", "c.tar.gz", "d.tgz", "e.deb", "f.tar.xz", "g.zst"}
	no := []string{"a.txt", "b.elf", "c.so", "d"}
	for _, n := range yes {
		if !IsArchiveName(n) {
			t.Errorf("IsArchiveName(%q) = false, want true", n)
		}
	}
	for _, n := range no {
		if IsArchiveName(n) {
			t.Errorf("IsArchiveName(%q) = true, want false", n)
		}
	}
}

func TestWalkCancelBeforeStart(t *testing.T) {
	z := makeZip(t, map[string]string{"a.txt": "alpha"})
	path := writeTemp(t, "test.zip", z)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	err := Walk(ctx, path, Options{}, func(_ context.Context, _ string, _ io.Reader) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if called {
		t.Error("fn should not be invoked when ctx is already cancelled")
	}
}

func TestWalkCancelFromCallback(t *testing.T) {
	// Multi-member tar; the callback cancels after the first leaf, so the walk
	// must stop before visiting the rest.
	tarball := makeTar(t, map[string]string{"a.txt": "a", "b.txt": "b", "c.txt": "c"})
	path := writeTemp(t, "multi.tar", tarball)

	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	err := Walk(ctx, path, Options{}, func(_ context.Context, _ string, _ io.Reader) error {
		count++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if count != 1 {
		t.Errorf("visited %d leaves, want 1 before cancellation stopped the walk", count)
	}
}

func TestWalkCancelMidStream(t *testing.T) {
	// A gzip leaf that decompresses to 1 MiB. The callback reads a little, then
	// cancels and keeps reading: the ctxReader must abort the long single read.
	big := bytes.Repeat([]byte("A"), 1<<20)
	path := writeTemp(t, "big.gz", gz(t, big))

	ctx, cancel := context.WithCancel(context.Background())
	err := Walk(ctx, path, Options{MaxBytes: -1}, func(_ context.Context, _ string, r io.Reader) error {
		if _, err := io.ReadFull(r, make([]byte, 4096)); err != nil {
			return err
		}
		cancel()
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestLoggerReceivesWarnings(t *testing.T) {
	// A gzip member decompressing past the budget triggers a graceful stop that
	// is reported via the configured Logger rather than written to stderr.
	big := bytes.Repeat([]byte("A"), 1<<20)
	path := writeTemp(t, "big.gz", gz(t, big))

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Propagating the read error surfaces the exhausted budget to Walk, which
	// recovers it as a graceful (nil) stop and reports it via the logger.
	err := Walk(context.Background(), path, Options{MaxBytes: 4096, Logger: logger}, func(_ context.Context, _ string, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if !strings.Contains(buf.String(), "budget") {
		t.Errorf("expected a budget warning in the log, got %q", buf.String())
	}
}

func TestNilLoggerIsSilent(t *testing.T) {
	// With no Logger configured (the default), hitting the warn path must not
	// panic and the walk still recovers gracefully.
	big := bytes.Repeat([]byte("A"), 1<<20)
	path := writeTemp(t, "big.gz", gz(t, big))

	err := Walk(context.Background(), path, Options{MaxBytes: 4096}, func(_ context.Context, _ string, r io.Reader) error {
		_, err := io.Copy(io.Discard, r)
		return err
	})
	if err != nil {
		t.Fatalf("Walk error: %v", err)
	}
}

func keys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func FuzzWalkStream(f *testing.F) {
	f.Add(makeZip(f, map[string]string{"x": "y"}))
	f.Add(gz(f, []byte("hello")))
	f.Add([]byte("not an archive"))
	f.Add([]byte("!<arch>\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		path := writeTemp(t, "fuzz.bin", data)
		// Must never panic; errors are acceptable. Bound the budget so fuzzed
		// bombs cannot run away.
		_ = Walk(context.Background(), path, Options{MaxBytes: 1 << 20, MaxDepth: 4}, func(_ context.Context, _ string, r io.Reader) error {
			_, _ = io.Copy(io.Discard, r)
			return nil
		})
	})
}
