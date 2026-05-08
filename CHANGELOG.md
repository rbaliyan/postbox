# Changelog

All notable changes to this project will be documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

---

## [Unreleased]

### Added

- **Mailbox store backends** — PostgreSQL and MongoDB message stores wired via `--mailbox-backend` and `--mailbox-dsn`; default remains in-memory
- **OpenTelemetry metrics** — `/metrics` endpoint served via OTel Prometheus bridge on a configurable port (`--metrics-port`); isolated registry avoids conflicts with default prometheus registry
- **gRPC health check** — `grpc.health.v1` registered on the main listener; toggle via `--health-enabled`
- **DNSBL plugin** — DNS block-list lookups (Spamhaus ZEN, Barracuda, etc.) with per-IP TTL cache; wired as `DNSBLPlugin` in the plugin chain
- **Redis IP lockout** — `SMTPSecurityPlugin` now supports a Redis-backed lockout store shared across cluster nodes (`--smtp-security-redis-lockout`); falls back to in-memory when Redis is absent
- **Outbound relay** — SMTP and SendGrid backends; `--outbound-backend`, `--outbound-smtp-host`, `--outbound-sendgrid-api-key` and related flags
- **DKIM signing** — `DKIMSigningBackend` wraps the SMTP backend and signs outbound messages using pre-built wire bytes to preserve header order; `--outbound-dkim-*` flags
- **Async delivery queue** — in-memory queue with exponential-backoff retry and graceful drain on shutdown (`--outbound-queue`); workers drain buffered jobs before exit, ctx cancelled only on drain timeout
- **DMARC enforcement** — `EmailAuthPlugin` extended with DMARC alignment check via `--email-auth-dmarc-policy`
- **LMTP gateway** — local delivery listener for upstream MTA integration (`--lmtp-enabled`, `--lmtp-socket-path`)
- **Plugin chain expanded to 9 plugins** — CrowdSec, DNSBL, AddressFilter, AttachmentFilter, SMTPSecurity, EmailAuth, SpamChecker, AntiVirus, SecurityAgent
- **gRPC auth token** — bearer token requirement on all gRPC calls (`--grpc-auth-token`); leave empty only for development
- **`postbox.example.yaml`** — comprehensive reference configuration covering all sections with inline comments

### Changed

- `postbox mail send --user` renamed to `--from` to match gRPC field semantics
- `postbox mail tag --tag` replaced by `--add` / `--remove` for explicit intent
- `smtp.insecure_auth` default changed to `false` in example config and CLI flags
- `cmd/agent.go` renamed to `cmd/user.go` to match the `user` subcommand it implements

### Fixed

- Relay queue shutdown: jobs buffered in the channel were silently discarded when context was cancelled on shutdown; queue now drains completely before exiting, cancelling ctx only if the drain timeout expires
- DKIM signing: inner backends rebuilt the message from a `map[string]string`, causing non-deterministic header order that invalidated signatures; `DKIMSigningBackend` now requires `RawBackend` and calls `SendRaw` with the pre-signed bytes

---

## [0.1.0] — Initial release

### Added

- User/principal registry with type, public key, and capability metadata
- `RegisterUser`, `GetUser`, `SearchUsers`, `ListUsers` gRPC RPCs
- Domain registration and routing
- In-memory mailbox with threads, folders, and tags
- Async webhook dispatcher with Ed25519-signed payloads and exponential backoff
- SMTP ingress with reply-chain threading
- Redis-backed distributed routing table with TTL heartbeat
- `Discover` RPC returning the owning-node address for transparent forwarding
- Plugin chain: CrowdSec, AddressFilter, AttachmentFilter, SMTPSecurity, EmailAuth, SpamChecker, AntiVirus, SecurityAgent
- Full CLI: `postbox serve`, `user`, `domain`, `register`, `mail`, `smtp`, `discover`, `status`
