# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`github.com/richardwooding/archives` is a single-package Go library (module Go 1.24) that recursively walks nested archive/compressed containers (zip, tar, gzip, bzip2, xz, zstd, ar/deb) and hands the caller an `io.Reader` for every leaf file. It was extracted from [`txtr`](https://github.com/richardwooding/txtr). Pure Go, no CGO — the only deps are `klauspost/compress` (zstd) and `ulikunitz/xz`.

## Commands

```bash
go build ./...
go test ./...                                   # all tests
go test -run TestWalkNestedZipInTar .           # a single test
go test -race ./...                             # what CI runs
go test -fuzz=FuzzWalkStream -fuzztime=60s .    # fuzz the walker (CI runs 60s)
golangci-lint run --timeout=5m                  # config in .golangci.yml
gofmt -l .                                      # must report nothing
```

CI (`.github/workflows/ci.yml`) runs three jobs — Test (`-race`), Lint (golangci-lint, pinned `version: v2.9.0`), and Fuzz (60s) — on every PR. Run all four commands above locally before pushing; they mirror CI.

## Architecture

The public surface is tiny: `Walk(ctx, path, opts, fn)`, the `WalkFunc` callback, `Options`, the `Format` enum, and `IsArchiveName`. Everything else is internal.

**The recursion funnel.** Every descent routes through `walkStream` (walk.go). It calls `detect` (magic-byte sniffing, detect.go — content-based, never by extension) and dispatches to a per-format walker (`walkTar`, `walkZipStream`/`walkZipReader`, `walkAr`, `walkCompressed`), each of which iterates members and recurses back into `walkStream`. When nothing is recognized — or the depth limit is hit — the stream is emitted as a leaf via `fn`. Because all recursion and format dispatch passes through this one function, it is the place to add cross-cutting concerns.

**Virtual paths.** Each nesting level appends `PathSeparator` (`"!/"`), e.g. `pkg.deb!/data.tar!/usr/bin/htop`. Member names are run through `cleanMemberName`.

**Two safety budgets (Options).**
- `MaxDepth` — `depth` counts containers already opened; at the limit a stream is emitted as an opaque leaf instead of being descended into.
- `MaxBytes` — enforced by `budget`/`budgetReader`, applied **only at decompression-expansion points** (compressor outputs and zip members), since uncompressed tar/ar traversal is already bounded by the on-disk file size. Exhaustion surfaces the `errBudgetExceeded` sentinel, which `Walk` treats as a **graceful stop (returns nil)**, not a failure.

**Context cancellation.** Observed at two levels: a `ctx.Err()` check at the top of `walkStream` and at the top of each member loop (between-member granularity), plus a `ctxReader` wrapping every stream handed to recursion or a callback (mid-stream granularity, e.g. the 512 MiB nested-zip buffering and the `walkAr` drain). `Walk` returns `ctx.Err()` distinctly from the budget stop. The context is also passed to `WalkFunc`.

**Zip is special.** A top-level zip uses random access on the `*os.File` (`walkTopLevelZip`) to avoid buffering hundreds of MB. A *nested* zip arrives as a stream, so it is buffered into memory capped at `zipMemoryCap` (512 MiB); larger ones are processed partially.

**Diagnostics.** Non-fatal anomalies (unreadable member, malformed sub-archive, budget hit) are reported via an optional `Options.Logger` (`*slog.Logger`) as `slog.Warn` records. The library is **silent by default** (nil Logger) — never write to `os.Stderr`.

### Invariants to preserve

- New format support or cross-cutting checks belong in/around `walkStream`, not scattered across walkers.
- Generic read-error branches in the walkers `return nil` to keep walking on corrupt data — but they must **not** swallow `ctx.Err()` or `errBudgetExceeded`; both are checked before the generic fallback. Preserve this when editing those branches.
- `Walk` is the single place that converts `errBudgetExceeded` to a nil return; walkers propagate it upward.
