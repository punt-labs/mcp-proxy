# Engineering

Systems design across Go and Python. Correctness over speed.

- Errors are values; wrap with context, never swallow.
- No panics or bare re-raises in library code.
- Race detection and type checking on every test run.
- Table-driven tests; happy path, boundary, invalid, missing-dependency.
- Prefer stdlib; add a dependency only when it earns its place.
- `internal/` (Go) or private modules (Python) for everything not deliberately exported.
- CI/CD as code; every quality check is a make target the human and the agent both run.
