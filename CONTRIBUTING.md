# Contributing

Bug reports and patches are welcome, including "this does not work with my
setup" reports with no patch attached.

## Before sending a patch

```sh
make test    # gofmt, vet, and the test suite under the race detector
```

If you touched the opam file scanner, also run it against the real repository
and fuzz it, because that is where a mistake will be quiet rather than loud:

```sh
git clone --depth 1 https://github.com/ocaml/opam-repository /tmp/opam-repository
make corpus CORPUS=/tmp/opam-repository
make fuzz
```

## What this project tries to be

Small, and boring to operate. Some consequences of that:

- **No dependencies outside the Go standard library.** git is a runtime
  requirement and that is the whole list. A mirror exists to remove
  dependencies from a build, so it is a poor place to add any.
- **No state that has to be kept in step.** The upstream checkout still holds
  every original URL and checksum, so the mapping from a mirror URL back to
  its source is always derivable. Storing it separately would mean two sources
  of truth, and nobody keeps two sources of truth aligned for long.
- **Nothing partial or unverified ever gets a real name.** Archives are
  streamed to a temporary file, hashed on the way through, and renamed into
  place only once they match what the package declares.
- **Failures leave the previous state serving.** A refresh that cannot reach
  upstream logs the failure and keeps the mirror it already had.

## Reporting a parsing failure

The most useful bug report for this project is an `opam` file that the scanner
gets wrong. Include the file, or the package name and version, and what you
expected `url.src` to be. A failing case added to `internal/opamfile` is even
better.
