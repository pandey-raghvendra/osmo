# Contributing to osmo

Thanks for taking the time. osmo is a small, focused tool — contributions that stay in that spirit are most welcome.

## What fits osmo

- Bug fixes with a clear reproduction
- Absorb support for patterns that are currently reported as unresolved (with test fixtures)
- New block identity keys for the built-in registry
- Provider test fixtures (GCP, Azure, etc.)
- OpenTofu compatibility improvements
- CI / packaging improvements

## What probably doesn't

- New flags that duplicate existing Terraform/tofu functionality
- Provider-specific business logic that belongs in the provider
- Behaviour changes that widen osmo's scope beyond drift absorption

If in doubt, open an issue first to discuss.

## Development setup

```sh
git clone https://github.com/pandey-raghvendra/osmo
cd osmo
go build ./...
go test ./...
```

You need Go ≥ 1.21 and Terraform ≥ 1.0 (or OpenTofu ≥ 1.6) on your `PATH`.

## Running tests

```sh
go test ./...                       # all unit tests
go test ./internal/e2e/...          # e2e (needs terraform or tofu on PATH)
OSMO_DEBUG=1 go test ./...          # with debug output
```

The localstack e2e test (`TestAbsorbAgainstRealResourceDrift`) uses a pre-captured fixture and runs without Docker.

## Adding a drift absorption test

Tests for new drift patterns go in [`internal/absorb/absorb_test.go`](internal/absorb/absorb_test.go). The pattern is:

1. Write a `.tf` fixture as a string in `t.TempDir()`
2. Construct a minimal `terraform show -json` blob (see existing tests for shape)
3. Call `absorb.Plan(dir, drifts, []byte(cfgJSON))`
4. Assert the expected file content or `ResourceEdit`

See `TestScalarAttrLiteral` as the simplest example.

## Code style

- Standard Go formatting (`gofmt`); run `go vet ./...` before opening a PR
- No new global state
- New exported functions need a doc comment
- Tests for any new absorb behaviour are required; tests for bug fixes are strongly encouraged

## Pull request checklist

- [ ] `go build ./...` passes
- [ ] `go vet ./...` passes
- [ ] `go test ./...` passes
- [ ] New drift patterns have a corresponding `absorb_test.go` case
- [ ] PR description explains the drift pattern and links any relevant issue

## Commit messages

Follow [Conventional Commits](https://www.conventionalcommits.org/):
`feat`, `fix`, `docs`, `refactor`, `test`, `chore`. Subject ≤ 72 chars.

## License

By contributing you agree your changes will be licensed under the [Apache 2.0 License](LICENSE).
