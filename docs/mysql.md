# Supporting MySQL and MariaDB

Notes from an audit of what MySQL support would actually take, written down so
the same investigation does not have to happen twice.

No engine abstraction exists in the tree. One was started and removed: it was
unreferenced, it duplicated `internal/catalog`, and it abstracted the parts that
are easy rather than the parts that differ. The findings below are why.

## The blocker: MySQL cannot make the guarantee this tool sells

Safeslice prints, after every run and in every report:

> Zero FK orphans — foreign keys stayed enforced through the load

That is true because PostgreSQL can defer constraint checks to `COMMIT`. A
successful commit **is the database confirming** there are no orphans. Nothing
asserts it; Postgres proves it.

InnoDB has no deferred foreign keys. Every MySQL bulk loader sets
`FOREIGN_KEY_CHECKS = 0` for the duration, which turns the checks off entirely.
On MySQL the options are:

1. Drop the claim, or
2. Re-verify orphans afterwards with a query per foreign key, and say clearly
   that it was checked after the fact rather than enforced during.

Option 2 is defensible, but it is a different and weaker statement, and it must
not be printed in the same words as the PostgreSQL one.

## What actually differs

| Concern | PostgreSQL | MySQL / MariaDB |
|---|---|---|
| Deferred FK checks | `SET CONSTRAINTS ALL DEFERRED` | none; `FOREIGN_KEY_CHECKS = 0` |
| Bulk load | `COPY` binary protocol | `LOAD DATA` or multi-row `INSERT` |
| Cross-connection snapshot | `pg_export_snapshot()` | none |
| Single-connection snapshot | `REPEATABLE READ` | `START TRANSACTION WITH CONSISTENT SNAPSHOT` |
| Identifier quoting | `"name"` | `` `name` `` |
| Auto keys | identity columns, sequences | `AUTO_INCREMENT` |
| Reset after load | `setval()` | `ALTER TABLE … AUTO_INCREMENT = n` |
| Generated columns | `attgenerated` | `information_schema` extra |
| Introspection | `pg_catalog` | `information_schema` |
| Schema model | database → schema → table | database == schema → table |
| Triggers during load | `ALTER TABLE … DISABLE TRIGGER` | no equivalent; drop and recreate |

## Where the seams are

Nine files import `pgx` today. The packages that would need an engine seam:

- `catalog` — introspection is entirely `pg_catalog`
- `extract` — transaction setup and the snapshot
- `load` — cycle plans, identity handling, sequence resets, triggers
- `sink` — `pgx.CopyFrom`
- `verify` — regex operators and quoting
- `mask` — `kindOf` reads PostgreSQL type names

`graph`, `keyset`, `config`, `profile`, `report` and `ui` are already engine-agnostic.

## If this gets built

Write the second engine first, against a real MySQL, and let the interface fall
out of what the two implementations actually need in common. An interface
designed before the second implementation exists will abstract the wrong things
— which is exactly what happened to the version that was removed.

Add `github.com/go-sql-driver/mysql` for MySQL only. Keep `pgx` for PostgreSQL:
it is faster, its `CopyFrom` has no `database/sql` equivalent, and swapping it
out would be a rewrite of working, tested code for no user-visible gain.

CI would need MySQL 8 and MariaDB 11 alongside PostgreSQL 13 and 17.

## Prior art worth reading first

[Greenmask](https://greenmask.io) already ships MySQL support via a `mysqldump`
drop-in, and its subsetting handles virtual, polymorphic and cyclic references.
If a team needs MySQL today, that is the honest recommendation — and the reason
to be sure this is worth building before starting.
