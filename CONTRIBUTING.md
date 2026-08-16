# Contributing to SafeSlice

Thank you for your interest in contributing to SafeSlice! We welcome contributions of all kinds: bug fixes, performance improvements, new masking algorithms, test schema edge cases, and documentation enhancements.

This document provides a comprehensive guide to setting up your local development environment, understanding the architecture, testing against real PostgreSQL instances, and submitting pull requests.

---

## Table of Contents

- [Project Scope & Philosophy](#project-scope--philosophy)
- [The Two Inviolable Gates](#the-two-inviolable-gates)
- [Getting Started](#getting-started)
  - [Prerequisites](#prerequisites)
  - [Fork and Clone](#fork-and-clone)
  - [Building Locally](#building-locally)
- [Repository Architecture](#repository-architecture)
- [Running Tests](#running-tests)
  - [Unit Tests](#unit-tests)
  - [End-to-End & Integration Tests](#end-to-end--integration-tests)
  - [Adding Schema Fixtures](#adding-schema-fixtures)
- [Code Style & Standards](#code-style--standards)
  - [Formatting & Linting](#formatting--linting)
  - [License Headers](#license-headers)
  - [Code Comments](#code-comments)
  - [Error Messages](#error-messages)
- [Contribution Workflow](#contribution-workflow)
  - [1. Create a Branch](#1-create-a-branch)
  - [2. Implement & Test](#2-implement--test)
  - [3. Pre-Commit Verification Checklist](#3-pre-commit-verification-checklist)
  - [4. Commit Guidelines](#4-commit-guidelines)
  - [5. Submit a Pull Request](#5-submit-a-pull-request)
- [Community & Security](#community--security)

---

## Project Scope & Philosophy

**SafeSlice is a single static CLI tool.** It is deliberately not a platform, SaaS service, or orchestrator.

A single static binary has zero running costs, requires no external control planes or background servers, and cannot be shut down. Proposals to add a server, a scheduler, or a multi-tenant control plane will be declined — a wrapper around the CLI is the proper place for those.

Key tenets:
1. **Zero Runtime Dependencies**: Pure Go with `CGO_ENABLED=0`, using pure-Go SQLite for the key store so there is no libc dependency.
2. **Safety First**: Source databases are accessed in read-only mode. Production PII is transformed in-flight before ever landing in the destination.
3. **Database Engine Focus**: PostgreSQL is our primary focus (supporting PostgreSQL 13 through 17). Support for additional database engines will only be considered once PostgreSQL support is rock solid.

---

## The Two Inviolable Gates

Every bug SafeSlice has historically encountered was a PostgreSQL schema shape nobody anticipated: a timestamp inside a primary key, a foreign key inherited by a partitioned table, or a circular dependency whose constraints were not `DEFERRABLE`. Unit tests caught none of them.

Therefore, every pull request touching how rows are selected, masked, or loaded must satisfy the **Two Inviolable Gates**:

1. **Zero foreign-key orphans** after loading into a target database.
2. **Zero canaries** — planted sensitive values in the source must never survive into the destination or logs.

> [!IMPORTANT]
> If your change modifies query building, row traversal, masking rules, or data loading, you **must** add a reproduction fixture to `testdata/schemas/` and an assertion in `e2e/`. A test that only exercises Go logic without PostgreSQL is not sufficient proof.

---

## Getting Started

### Prerequisites

- **Go**: Version 1.25 or newer (or latest stable Go toolchain).
- **Docker**: For spinning up ephemeral PostgreSQL test containers.
- **Git**: For version control.

### Fork and Clone

1. Fork the repository on GitHub: [https://github.com/Autometiq/safeslice](https://github.com/Autometiq/safeslice)
2. Clone your fork locally:
   ```bash
   git clone https://github.com/<your-username>/safeslice.git
   cd safeslice
   ```
3. Set up the upstream remote:
   ```bash
   git remote add upstream https://github.com/Autometiq/safeslice.git
   ```

### Building Locally

Compile the CLI binary:

```bash
# Linux / macOS
go build -o safeslice ./cmd/safeslice

# Windows (PowerShell)
go build -o safeslice.exe ./cmd/safeslice
```

Verify your local build:

```bash
./safeslice --version
# or on Windows
.\safeslice.exe --version
```

You can test the interactive terminal wizard with the built-in demo environment:

```bash
./safeslice demo
```

---

## Repository Architecture

```
safeslice/
├── cmd/safeslice/          # CLI entry point (Cobra commands, flags, global error handling)
├── internal/
│   ├── catalog/            # PostgreSQL catalog introspection (tables, columns, PKs, FKs, types)
│   ├── config/             # YAML configuration parsing, resolution, and validation
│   ├── demo/               # Built-in isolated demo sandbox environment
│   ├── extract/            # Read-only streaming data extraction from source Postgres
│   ├── graph/              # Dependency graph, cycle detection, topological table sorting
│   ├── keyset/             # Keyset resolution, sampling queries, virtual foreign keys
│   ├── load/               # High-throughput batched loading (COPY protocol) into destination
│   ├── mask/               # Deterministic pseudonymization & masking generators
│   ├── profile/            # Column profiling and automatic PII detection heuristics
│   ├── report/             # Terminal summaries, visual DAGs, and interactive HTML reports
│   ├── sink/               # Target database session management and constraint deferrals
│   ├── ui/                 # Interactive Terminal UI (TUI) built with Lip Gloss
│   └── verify/             # Post-slice verification (FK orphan scanner & canary leak tests)
├── e2e/                    # End-to-end integration tests using real PostgreSQL instances
├── testdata/schemas/       # Real-world SQL schema fixtures for edge-case testing
└── scripts/                # Utility scripts (e.g. addlicense.sh)
```

---

## Running Tests

### Unit Tests

Run the standalone unit tests:

```bash
go test ./...
```

### End-to-End & Integration Tests

Catalog and end-to-end tests require a live PostgreSQL instance.

> [!WARNING]
> Without `SAFESLICE_TEST_DSN`, end-to-end and catalog tests **skip** rather than fail. A green `go test ./...` without `SAFESLICE_TEST_DSN` set does not validate database interactions. Always set the environment variable before trusting the results.

#### 1. Start a local PostgreSQL test container:

```bash
docker run -d --name safeslice-test-pg -e POSTGRES_PASSWORD=pw -p 55432:5432 postgres:17
```

*(Note: SafeSlice supports PostgreSQL 13 through 17. You can switch the image tag to `postgres:13` to test backward compatibility.)*

#### 2. Run the test suite with the test DSN:

**Linux / macOS (Bash):**
```bash
SAFESLICE_TEST_DSN="postgres://postgres:pw@localhost:55432/postgres" go test -race -count=1 ./...
```

**Windows (PowerShell):**
```powershell
$env:SAFESLICE_TEST_DSN="postgres://postgres:pw@localhost:55432/postgres"
go test -race -count=1 ./...
```

To clean up the test container when finished:
```bash
docker rm -f safeslice-test-pg
```

### Adding Schema Fixtures

When fixing a bug or handling a new PostgreSQL feature:

1. Add a minimal SQL schema reproducing the shape into [`testdata/schemas/`](testdata/schemas/).
2. Add corresponding table definitions, seed rows, and canaries.
3. Write assertions in [`e2e/e2e_test.go`](e2e/e2e_test.go) verifying that slicing preserves referential integrity and masks all sensitive columns.

---

## Code Style & Standards

### Formatting & Linting

CI enforces `gofmt` and `go vet` on all commits.

Format your code before committing:
```bash
gofmt -s -w .
go vet ./...
```

### License Headers

All `.go` files must contain the Apache 2.0 copyright header. We provide a script to check and apply headers:

```bash
# Check if any files are missing license headers
bash ./scripts/addlicense.sh --check

# Automatically apply headers to missing files
bash ./scripts/addlicense.sh
```

### Code Comments

- Comments should explain **why** a decision was made, particularly where a simpler approach fails due to PostgreSQL engine quirks.
- Example: Explaining why `SET CONSTRAINTS ALL DEFERRED` is ineffective on non-`DEFERRABLE` foreign keys and how SafeSlice works around it.

### Error Messages

- Error messages must be **actionable and specific**.
- Name the exact column, constraint, table, or configuration option that caused the issue.
- *Poor:* `"Cycle cannot be deferred"`
- *Good:* `"circular dependency between 'orders' and 'payments' cannot be deferred because foreign key 'fk_payment_order' is not DEFERRABLE; alter constraint with DEFERRABLE INITIALLY DEFERRED"`

---

## Contribution Workflow

### 1. Create a Branch

Always create a feature or bugfix branch based on the latest `main` branch:

```bash
git checkout main
git pull upstream main
git checkout -b fix/cycle-deferral-warning
# or
git checkout -b feat/iban-masking-strategy
```

### 2. Implement & Test

Make your changes, write tests, and verify against a real PostgreSQL instance as outlined above.

### 3. Pre-Commit Verification Checklist

Before submitting your changes, run this local checklist:

- [ ] Code is formatted: `gofmt -s -w .`
- [ ] Linter passes: `go vet ./...`
- [ ] License headers are applied: `bash ./scripts/addlicense.sh --check`
- [ ] Unit tests pass: `go test ./...`
- [ ] E2E tests pass against live Postgres: `SAFESLICE_TEST_DSN=... go test -race -count=1 ./...`
- [ ] Schema fixtures added (if modifying catalog, graph, keyset, mask, or load).

### 4. Commit Guidelines

We use [Conventional Commits](https://www.conventionalcommits.org/) for clear commit history and automated release notes:

```
<type>(<scope>): <short description in present tense>
```

Common types:
- `feat`: A new feature or capability (e.g., `feat(mask): add synthetic credit card generator`)
- `fix`: A bug fix (e.g., `fix(catalog): handle composite foreign keys in partitioned tables`)
- `docs`: Documentation updates (e.g., `docs: clarify postgres 13 compatibility requirements`)
- `test`: Adding or updating tests / schema fixtures (e.g., `test(e2e): add deferrable constraint cycle fixture`)
- `chore`: Maintenance or build script updates (e.g., `chore: update dependencies`)
- `ci`: CI/CD workflow modifications (e.g., `ci: test across postgres 13 and 17 matrices`)

### 5. Submit a Pull Request

1. Push your branch to your fork:
   ```bash
   git push origin <your-branch-name>
   ```
2. Open a Pull Request on GitHub against `main`.
3. Provide a clear description of the problem, the solution, and reference any relevant issue numbers (e.g., `Fixes #42`).
4. Ensure all GitHub Actions CI checks pass.

---

## Community & Security

- **Code of Conduct**: All participants are expected to adhere to our [Code of Conduct](CODE_OF_CONDUCT.md).
- **Security Policy**: If you discover a security vulnerability (such as a masking bypass, canary leak, or unauthorized source mutation), please report it privately following our [Security Policy](SECURITY.md) via [GitHub Security Advisories](https://github.com/Autometiq/safeslice/security/advisories/new) rather than opening a public issue.
