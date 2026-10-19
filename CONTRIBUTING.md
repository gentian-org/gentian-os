# Contributing

## License of a contribution

A contribution is licensed under the license of the files it touches:
MPL-2.0, or Apache-2.0 under `api/` and the other paths
[LICENSING.md](LICENSING.md) lists. There is no contributor agreement to sign
and no copyright to assign; you keep the copyright in what you write.

## New files

A new Go file starts with the header its directory uses — `hack/header.txt`,
or `api/hack/header.txt` under `api/` — inside a block comment. `make lint-go`
checks it. Other files need no header.

## Before a pull request

`make verify` runs everything CI runs.
