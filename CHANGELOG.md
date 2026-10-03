# CHANGE LOG

## v3.0.0

Not compatible with databases set up by v2.

- Moved the metadata table to `pgfs.metadata`;
- Renamed `MigrateUp`/`MigrateDown` to `Install`/`Uninstall`;
- Replaced `github.com/google/uuid` with the standard library `uuid` package;
- `FS.Create` takes a `uuid.UUID`. Removed `GenerateUUID`;
- Named the root directory "." instead of "";
- Sorted directory entries by name;
- Returned errors as `*fs.PathError`;
- Added `io.ReaderAt` to files;
- Made `FS` safe for concurrent use;
- Fixed `ModTime` on servers not set to UTC;
- Fixed `ReadDir` silently truncating results on error;
- Removed logging;
- Upgraded minimal Go version (1.27).

## v2.0.0

- Fixed issue with `ReadDir`/`Readdir` not handling case where n <= 0;
- Fixed issue with `pgfs.ServeFile` not setting Content-Type when a generic `fs.File` is passed;
- Fixed issue with `Sys.Scan`;
- Fixed issue with dangling lo when `pgfs.Create` (INSERT) fails;
- Fixed issue with `pgfs.MigrateDown` not using the correct query;
- Modified file ordering to `created_at DESC`.
- Upgraded logging to `slog`;
- Upgraded dependencies;
- Upgraded minimal Go version (1.23);

## v1.0.1

- Upgraded dependencies;
- Improved test coverage;
- Improved documentation.

## v1.0.0

- Fixed issue with type detection;
- Improved documentation.

## v0.6.1

- Fixed issue with `ServeFile` not using `http.ServeContent` when possible;

## v0.6.0

- Improved `FS.Create` with the ability to guess content types.
- Fixed typos in the documentation.

## v0.5.5

- Fixed typos in the documentation.

## v0.5.4

- Fixed issue with opening empty root directory;
- Fixed issue with root `Readdir` returning `io.EOF`;
- Upgraded dependencies;
- Improved testing and code coverage.
