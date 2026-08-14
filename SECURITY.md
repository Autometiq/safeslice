# Security Policy

## Reporting a vulnerability

Please report security issues privately through
[GitHub Security Advisories](https://github.com/Autometiq/safeslice/security/advisories/new),
not as a public issue.

You should get a first response within 72 hours.

## What counts as a security issue

safeslice exists to keep personal data off developer machines, so anything that
defeats that is a security bug, not a feature request:

- **A masking bypass.** Any input where a column that should be masked reaches
  the output unmasked.
- **A strict-mode escape.** Any schema where an unclassified text column loads
  without `strict: true` refusing the run.
- **A write to the source.** The source session is opened read-only; any path
  that modifies a source database is critical.
- **Credential disclosure.** Connection strings, passwords or row values
  appearing in logs, error messages or `verify` output.
- **SQL injection** through a config value, a `--where` predicate or a
  `virtual_keys` `when` clause reaching a context that is not parameterised.

Please include the schema shape that triggers it. A minimal `CREATE TABLE`
reproducing the case is far more useful than a description of it.

## What does not count

- **Weak fake values.** Masked output is deterministic by design, so identical
  inputs produce identical fakes. That is what keeps joins working. It also
  means masked data is pseudonymous, not anonymous — someone with the seed and
  the source can correlate rows. Treat a slice as sensitive, just far less so
  than the original.
- **Free-text columns.** No name-based heuristic can find an address buried in a
  support ticket. Use `redact`, and run `safeslice verify`.
- **Columns you marked `keep`.** That is a decision the config records.

## Supported versions

The latest minor release. safeslice is pre-1.0; please upgrade before reporting.
