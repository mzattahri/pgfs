# pgfs

[![GoDoc reference](https://img.shields.io/badge/godoc-reference-blue.svg)](https://pkg.go.dev/mz.attahri.com/code/pgfs/v3)
![CI](https://github.com/mzattahri/pgfs/actions/workflows/ci.yml/badge.svg)
[![Coverage Status](https://coveralls.io/repos/github/mzattahri/pgfs/badge.svg)](https://coveralls.io/github/mzattahri/pgfs)
[![Go Report Card](https://goreportcard.com/badge/mz.attahri.com/pgfs)](https://goreportcard.com/report/mz.attahri.com/code/pgfs/v3)

`pgfs` is a Go library that implements [fs.FS](https://pkg.go.dev/io/fs) using
[Large Objects](https://www.postgresql.org/docs/current/largeobjects.html) on
Postgres.

## Documentation

See [documentation](https://pkg.go.dev/mz.attahri.com/code/pgfs/v3) for more details.

## Installation

```shell
go get mz.attahri.com/code/pgfs/v3
```

## Testing

Tests launch an ephemeral Postgres instance with Podman or Docker,
whichever is installed (Podman is preferred), on a free port picked
by the container runtime. Set `CONTAINER_RUNTIME` to choose one
explicitly.

```sh
make test
make test CONTAINER_RUNTIME=docker
```
