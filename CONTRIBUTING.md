# Contributing

## Running the tests

```bash
go test ./...
```

Catalog and end-to-end tests need a real PostgreSQL:

```bash
docker run -d -e POSTGRES_PASSWORD=pw -p 55432:5432 postgres:17
SAFESLICE_TEST_DSN="postgres://postgres:pw@localhost:55432/postgres" go test ./...
```

Without `SAFESLICE_TEST_DSN` those tests **skip** rather than fail, so a green
`go test ./...` on its own does not mean much. Set the variable before trusting
a result.

## The bar for a change

Every bug this project has had was a schema shape nobody thought about: a
timestamp in a primary key, a foreign key inherited by a partition, a cycle whose
constraints were not `DEFERRABLE`. Unit tests caught none of them.

So: **if a change touches how rows are selected, masked or loaded, add a fixture
to `testdata/schemas/` that reproduces the shape**, and an assertion in `e2e/`.
A test that only exercises Go logic is not evidence about PostgreSQL.

The two end-to-end gates matter more than any unit test:

1. **Zero foreign-key orphans** after loading into a target.
2. **Zero canaries** — planted values in the source that must not survive.

A change that breaks either is wrong, whatever else it improves.

## Style

- `gofmt` and `go vet` are enforced in CI.
- Comments explain *why*, especially where a simpler-looking approach is wrong.
  `SET CONSTRAINTS ALL DEFERRED` silently doing nothing on non-`DEFERRABLE`
  constraints is the kind of thing the next reader needs told.
- Errors name the fix. "Cycle cannot be deferred" is not as useful as naming the
  constraint and what to alter.

## Scope

safeslice is a CLI. It is deliberately not a platform.

Snaplet and Neosync both died as companies, and Neosync in particular was an
orchestration service built on Temporal — which is why it needed revenue at all.
A single static binary has no running costs and cannot be shut down. Proposals
that add a server, a scheduler or a control plane will be declined; a wrapper
around the CLI is the right place for those.

New database engines are welcome once PostgreSQL support is solid.
