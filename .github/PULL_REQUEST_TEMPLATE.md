## What this changes

<!-- One or two sentences. -->

## Why

<!-- The failure or gap this addresses. Link an issue if there is one. -->

## Checklist

- [ ] `go test ./...` passes **with `SAFESLICE_TEST_DSN` set** — without it the
      catalog and end-to-end tests skip rather than fail, so a green run without
      it proves very little
- [ ] `gofmt` and `go vet` are clean
- [ ] If this touches row selection, masking or loading: a fixture in
      `testdata/schemas/` reproduces the schema shape, and `e2e/` asserts on it
- [ ] The two end-to-end gates still hold: zero foreign-key orphans, zero
      surviving canaries
