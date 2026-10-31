# sqlite-go

A Go wrapper for SQLite's C API, with bundled SQLite and no Go module dependencies. Includes prepared statements, hooks, sessions and changesets, online backup, and incremental blob I/O. It does not use `database/sql`.

## Build

Requires Go 1.27+, cgo, and a C compiler. Builds use the checked-in SQLite sources without downloading an engine.

```sh
go test ./...
go run ./internal/cmd/smoke
```

## Example

```go
package main

import (
    "fmt"
    "log"
    sqlite "github.com/oskoi/sqlite-go"
)

func main() {
    if err := run(); err != nil { log.Fatal(err) }
}

func run() (err error) {
    conn, err := sqlite.Open(":memory:", 0)
    if err != nil { return err }
    defer func() { if closeErr := conn.Close(); err == nil { err = closeErr } }()
    stmt, _, err := conn.Prepare("SELECT ?1, ?2", sqlite.PreparePersistent)
    if err != nil { return err }
    defer func() { if closeErr := stmt.Close(); err == nil { err = closeErr } }()
    if err := stmt.BindInt64(1, 42); err != nil { return err }
    if err := stmt.BindText(2, "hello"); err != nil { return err }
    row, err := stmt.Step()
    if err != nil { return err }
    if row {
        n, _, err := stmt.ColumnInt64(0)
        if err != nil { return err }
        text, _, err := stmt.ColumnText(1)
        if err != nil { return err }
        fmt.Println(n, text)
    }
    return stmt.Reset() // keeps bindings; bind again or execute the same values
}
```

Usage rules:

- Close statements and other child handles before their connection. There are no finalizers.
- Give a connection and its children one owner at a time. Do not copy handles. Independent connections can run concurrently.
- Use `ColumnText` and `ColumnBlob` for owned copies. Borrowed views expire after Step, Reset, Close, or conversion of the same column.

See the [package documentation](doc.go) for API contracts.

## Optional Multiwriter engine

Build with `-tags=sqlite_multiwriter` on POSIX to enable [sqlite-multiwriter](https://github.com/sqliteai/sqlite-multiwriter). Use `sqlite.OpenMultiwriter(path, flags)` for writable on-disk databases. Zero flags use normal defaults. Without the tag, or on Windows, the opener returns `ErrMultiwriterUnavailable`.

Use one process and a local filesystem. Multiprocess operation, ATTACH atomicity, backup/conversion, and crash durability are not qualified.

Keep the `-mw*` sidecars and reopen through `OpenMultiwriter`. Do not mix standard SQLite access with an active Multiwriter database or copy only its main file. Even a successful Close does not make standard-SQLite reopening safe.

## Engine and maintenance

Edit [versions.json](versions.json): SQLite version, archive year and ZIP SHA3-256; Multiwriter version label and commit. From the repository root, without concurrent builds or updates, run:

```sh
go run ./internal/cmd/update-deps
```

The updater replaces vendored sources, including the whole Multiwriter `src/` tree, and runs tests, vet, and smoke checks. Review the diff, then commit.

Returned installation or verification errors trigger rollback. This does not cover crashes. If rollback fails, keep the reported `.deps-update-*` directory: its `.old` paths hold recovery copies.

## License

Project code uses [Apache License 2.0](LICENSE). SQLite is public domain. sqlite-multiwriter retains its [Apache-2.0 license](libs/sqlite-multiwriter/LICENSE), [NOTICE](libs/sqlite-multiwriter/NOTICE), and source notices.
