# Security Policy

Safeslice takes the security and privacy of sensitive database workloads seriously. This policy outlines our supported versions, reporting procedures, scope, and rules of engagement.

---

## Supported Versions

We provide security updates exclusively for the latest minor release of Safeslice. Older versions are unsupported; please verify your issue on the latest release prior to reporting.

| Version | Supported          |
| :------ | :----------------- |
| `v0.x` (Latest Release) | :white_check_mark: |
| `< Latest Release`      | :x:                |

---

## Reporting a Vulnerability

**Do not file public GitHub issues, discussions, or pull requests for security vulnerabilities.**

All vulnerability disclosures must be submitted privately via:
:shield: **[GitHub Security Advisories](https://github.com/Autometiq/safeslice/security/advisories/new)**

### Report Requirements
To allow efficient triage, reports must include:
1. **Clear Description**: Exact failure mechanism and security impact.
2. **Minimal Reproducer**: A self-contained, minimal schema (`CREATE TABLE`) and configuration file.
3. **Environment Details**: Safeslice version, database engine (PostgreSQL/MySQL/SQLite), and OS.
4. **Proof of Concept**: Step-by-step reproduction command (e.g. `safeslice slice ...`).

We aim to acknowledge receipt of valid reports within **72 business hours** on a best-effort basis.

---

## Scope & Vulnerability Criteria

Safeslice exists to keep personal and sensitive data off developer machines. Vulnerabilities are evaluated strictly against that threat model.

### In Scope (Qualifying Security Bugs)
- **Masking Bypass**: Any schema or condition where unclassified or sensitive columns reach output unmasked.
- **Strict-Mode Escape**: Any schema where unclassified text columns load without `strict: true` halting execution.
- **Source Database Mutation**: Source connections are strictly read-only; any execution path that mutates the source database is considered critical.
- **Credential Disclosure**: Passwords, raw connection strings, or unmasked row values leaking into logs, stderr, or `verify` reports.
- **SQL Injection**: Injection via config parameters, `--where` predicates, or `virtual_keys` expression compilation.

### Out of Scope (Non-Qualifying Issues)
- **Deterministic / Pseudonymous Masking Design**: Masked values are deterministic by design to preserve relational join integrity. Re-identification via seed knowledge is expected behavior, not a vulnerability.
- **Free-Text & Unstructured Data**: Heuristics do not parse unlabelled free-text blobs. Use explicit `redact` transforms and `safeslice verify`.
- **User Configuration Choices**: Explicitly configured `keep` rules or missing user classifications.
- **Attacks Requiring Prior Compromise**: Attacks requiring root/admin access to the local machine, runtime binary tampering, or compromised environment variables.
- **Automated Scanner Output**: Generic automated vulnerability scanner reports without a verified, exploitable proof of concept.
- **Denial of Service / Resource Exhaustion**: Memory exhaustion or prolonged runtimes resulting from intentionally massive local slices or cyclic schemas.

---

## Coordinated Disclosure & Embargo

We follow coordinated vulnerability disclosure. By submitting a report, you agree to:
- **Maintain Strict Confidentiality**: Do not disclose, discuss, or publish any details of the vulnerability to third parties or the public until an official security advisory and patched release are published by maintainers.
- **Allow Remediation Time**: Allow maintainers a standard **90-day remediation window** before any public coordination.

---

## Safe Harbor & Rules of Engagement

We consider security research conducted in accordance with this policy to be authorized and in good faith. Safe harbor applies **only** when all of the following conditions are met:
- **Local / Sandboxed Testing Only**: Testing must be conducted solely against local, isolated, non-production test databases owned by the researcher.
- **No Disruption or Exfiltration**: You do not attempt data destruction, service degradation, social engineering, phishing, or access to any external systems.
- **No Extortion / Bug Bounty Demands**: **Safeslice is an open-source project and does not offer financial compensation or bug bounties.** Threatening disclosure or withholding vulnerability details in exchange for payment immediately voids safe harbor.
- **Compliance with Laws**: Research must comply with all applicable laws and regulations.

---

## Disclaimer

Safeslice is provided under the terms of the project's [LICENSE](LICENSE) on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
