# mekami-core-go

Go language frontend for [Mekami](https://github.com/Wolf258/Mekami).

This package implements the `api.Frontend` contract from
[`github.com/Wolf258/mekami-api/api/v1`](https://github.com/Wolf258/mekami-api).
The Mekami build pipeline calls `ResolveLayout`, `ResolveModules`,
`RootModule`, `ResolveFile`, and `ParseFile` to walk a Go source tree and
extract the symbols and reference edges that make up the code graph.

`init()` registers the frontend with `api.Global`, so any binary that
blank-imports this package (typically via the generated
`mekami-core/frontend/all_gen/all_gen.go`) gets the Go indexer for free.

## Installation

The mekami binary installs this module on demand with
`mekami core-install go`. To add it manually to a Go project:

```sh
go get github.com/Wolf258/mekami-core-go
```

Then add a blank import to the generated `all_gen.go` file:

```go
import (
    _ "github.com/Wolf258/mekami-core-go"
)
```

## Development

Clone the repository as a sibling of `mekami-api`, `mekami-core`, and
`mekami-cli` so the local `go.work` covers all of them. From the
`Mekami` repo root:

```sh
cd /path/to/Mekami
go work use ./mekami-core-go
go test ./mekami-core-go/...
go test -tags=integration ./mekami-core/integration_test/...
```

Without a `go.work` setup, run the module in isolation:

```sh
cd mekami-core-go
go test ./...
```

## License

MIT. See [LICENSE](LICENSE).
