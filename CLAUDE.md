# CLAUDE.md

Instructions for coding agents in **spotify-mcp**: the MCP server that controls Spotify playback, with an invite-link connect page (Go, Helm chart in `charts/`).

- Target layout (from PR #2, `refactor/layering`): domain packages `internal/account` and `internal/playback`; adapters `internal/spotify`, `internal/filestore`, `internal/store`; transports `internal/mcpserver`, `internal/tools`, `internal/web`. Code on `main` from before PR #2 doesn't follow it yet; new code does. Keep the README in step.
- Lint config is `.golangci.yml`: run `golangci-lint run` before handing over.

## Go conventions

Jan reads this code as production Go, even for a home project. Follow these from the first commit; don't argue against them with Go proverbs ("accept interfaces, return structs", "changing interfaces breaks implementers"). Explain a trade-off only when it is concrete.

### Layers

- **Domain first.** One domain package holds the types, the rules and the use cases (a `Service` with methods). It declares the interfaces (ports) it needs and knows nothing about HTTP, files, SQL or external APIs.
- **Adapters** (storage, file formats, external APIs) implement the domain's ports.
- **Transports** (HTTP, MCP, CLI) depend only on domain interfaces.
- **Thin `main`:** an `app` struct holds the parts; commands are its methods.

### Types and interfaces

- **Each layer owns its types and maps them explicitly** at every boundary: wire types, DB rows/columns, file formats. Never pass storage or generated types across a layer, and never `json.Marshal` domain types into a column.
- **Interfaces between layers**, defined by the consumer and kept small. A new capability gets a new or composed interface instead of a bigger one.
- **Exported interface, unexported implementation:** `type DB interface{…}`, `type db struct{…}`, `func Open(…) (DB, error)`.
- **Methods on types**, not free functions that all take the same dependency.
- **Typed values** (dates, enums, durations) instead of strings and ints; inject a clock instead of calling `time.Now()` in the domain.
- **Generated code** lives in its own package and is never edited by hand. HTTP APIs are spec-first (OpenAPI + oapi-codegen) where the repo has a spec.

### Values, entities and constructors

- No exported named basic types (`type ProfileID string`) for values that carry a rule or a secret: they can be created by plain conversion and skip validation.
- Values and entities with a rule or a secret are an exported interface with an unexported implementation, created only through functions that validate (`ParseX`, `NewX`, `RestoreX` for adapters reading storage). Seal value interfaces with an unexported marker method; keep a defensive check where a value ends up in a file name or key.
- Entities passed between layers are interfaces too, not exported structs with open fields. Services and adapters: `NewX` returns an exported interface (or the port it implements).
- No exported mutable globals for rules (scope lists, kind lists): use functions that return a copy.
- Secrets print as redacted (`String()`), and logs pass explicit strings (slog's JSON handler doesn't call `String()`).
- Fine to keep: sentinel errors and error types (`errors.Is/As`), functional options, plain config/input structs with no rule. Say so explicitly when keeping one.
- Don't copy an existing `type X string` pattern from a codebase; flag it.

### Tests and verification

- Tests per layer: table tests for rules, handlers against fakes, storage against the real thing (SQLite file, temp dir), one end-to-end test.
- Before handing over: `go build ./...`, `go vet ./...`, `go test -race ./...`, and `golangci-lint run` if the repo has a config. Run the service locally once.
- Report what was verified and what was not. Keep the README's code layout section current when packages change.

### Working rules

- Technical text (code, comments, commits, PRs, docs) in English, even for a German UI.
- Don't commit, push or merge without Jan's OK.
