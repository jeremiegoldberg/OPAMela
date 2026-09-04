# opamela — OCaml implementation

This is the OCaml port of opamela, the opam-repository mirror. It behaves the
same as the Go implementation in the parent directory: it clones
opam-repository, rewrites every `url.src` (and every `extra-source`) to point at
itself, serves the result as an ordinary opam repository, and downloads each
archive once, verifying it against the checksum the package declares.

Why an OCaml version at all: opamela mirrors OCaml packages, so it may as well
be available in the ecosystem's own language. The two implementations are kept
behaviourally identical, and both are checked against a real opam-repository
checkout, where they produce the same counts (22,155 of 22,751 packages
rewritten, the rest sourceless or git-only).

## Dependencies

The same "almost nothing" stance as the Go version, adapted to OCaml's thinner
standard library:

- **Compile time:** the standard library, `unix`, `str` and `threads`. Nothing
  from opam.
- **Run time:** `git` (to keep the checkout in sync), `curl` (to fetch
  archives over TLS), `tar`/`gzip` (to build the index tarball), and the
  `sha256sum` / `sha512sum` / `md5sum` tools (to verify archives). OCaml's
  stdlib has none of TLS, gzip, tar or the digests, so these are delegated to
  the tools that are already present on any machine that runs CI, exactly as
  the Go version delegates to `git`.

## Build, run, test

```sh
dune build
dune exec bin/opamela.exe -- -base-url https://opam.internal -state /var/lib/opamela

dune test            # unit tests for the parser and the mirror builder
```

Two test suites are gated behind a real checkout, as in the Go version:

```sh
git clone --depth 1 https://github.com/ocaml/opam-repository /tmp/opam-repository
OPAMELA_CORPUS=/tmp/opam-repository dune exec test/test_opamela.exe
```

That parses and round-trips all 22,751 package files and builds the whole
mirror, checking the counts against what the Go implementation reports.

## Layout

| Module | Role |
| --- | --- |
| `lib/opamfile.ml` | parse and rewrite the `url` and `extra-source` sections |
| `lib/opamrepo.ml` | locate packages across a base repository and overlays |
| `lib/mirror.ml` | rewrite the tree and generate `index.tar.gz` |
| `lib/gitrepo.ml` | keep the upstream checkout in sync (shells out to `git`) |
| `lib/fetch.ml` | download once, verify, cache (shells out to `curl` and `sha*sum`) |
| `lib/server.ml` | a small hand-written HTTP/1.1 server (stdlib has none) |
| `bin/opamela.ml` | the command line |

## Differences from the Go implementation

- The mirror is generated sequentially. OCaml 4.x runs one thread of OCaml code
  at a time, so a worker pool would not speed up the CPU-bound rewrite; the
  full build still takes a few seconds. `-workers` is accepted and ignored.
- Downloads are serialised per archive with a per-key lock rather than the Go
  version's detached single-flight: concurrent requests for the same cold
  archive still cause exactly one download.

For everything else — the flags, the routes, the checksum handling, the
overlay precedence, the passthrough of git and ftp sources — see the top-level
[README](../README.md).
