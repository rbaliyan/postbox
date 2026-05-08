# Postbox

**Postbox** is an agent communication backbone for AI agent swarms. It gives every principal — human, AI agent, or service — a durable, addressable mailbox and a capability registry so agents can discover each other by skill, exchange structured messages asynchronously, and be observed or interrupted by humans using any standard mail client.

It can also serve as a lightweight SMTP-to-storage gateway for any service that needs addressable, persistent messaging.

## Why Mail for Agents?

| Problem | Mail-native solution |
|---------|----------------------|
| Agents run at different speeds | Async delivery — senders never block on receivers |
| Agents restart or crash | Messages persist in the mailbox until consumed |
| Human oversight needed | Humans CC themselves or reply like any other participant |
| Agent-to-agent RPC is brittle | Loose coupling via address; no shared runtime required |
| "Who can do X?" at runtime | Capability registry — search by type, skills, region, model |
| Push delivery to agents | Webhook dispatcher — signed POSTs with retry and backoff |
| External mail from humans/tools | SMTP gateway routes inbound traffic into agent mailboxes |

## Architecture

```
postbox serve
├── Controller
│   ├── Node Store (SQLite / PostgreSQL)  — user profiles, domains, delivery jobs
│   ├── Mailbox Store (memory / PostgreSQL / MongoDB)  — message persistence
│   ├── Registry (local + Redis)          — 3-tier routing: email → domain → default
│   ├── Webhook Dispatcher                — async push delivery to agent endpoints
│   ├── SMTP Server (optional)            — ingress from external SMTP clients
│   ├── LMTP Server (optional)            — local delivery from an upstream MTA
│   ├── Outbound Relay (optional)         — SMTP / SendGrid delivery + DKIM signing
│   └── Plugin Chain                      — CrowdSec → DNSBL → Address/Attachment →
│                                           SMTPSecurity → EmailAuth → Spam → AV → Agent
└── gRPC API (PostboxService)
    ├── User RPCs    — RegisterUser, GetUser, SearchUsers, ListUsers
    ├── Domain RPCs  — RegisterDomain, ListDomains, RemapDomain
    ├── Mail RPCs    — SendMessage, ListMessages, GetMessage, …
    ├── Webhook RPCs — GetDeliveryStatus, ListDeliveryFailures
    ├── SMTP RPCs    — StartSMTP, StopSMTP, GetSMTPStatus
    └── Node RPCs    — GetStatus, GetNodePublicKey
```

## Quick Start

### 1. Start the node

```bash
postbox serve --port 50051 --db postbox.db
```

Messages are stored **in memory by default** — they are lost on restart. For persistence see [Mailbox Store Backends](#mailbox-store-backends).

### 2. Register a domain

```bash
postbox register --domain example.ai --default
```

### 3. Register users with capabilities

```bash
# Register an AI agent with skills and a webhook endpoint
postbox user register \
  --email researcher@example.ai \
  --type agent \
  --meta skills=web-search,summarization \
  --meta model=claude-sonnet-4-6 \
  --meta endpoint=https://researcher.example.ai/webhook \
  --meta team=analytics

# Register a human observer
postbox user register \
  --email alice@example.ai \
  --type human

# Register a service account
postbox user register \
  --email ingest@example.ai \
  --type service \
  --meta skills=data-ingestion \
  --meta region=us-east-1
```

### 4. Discover users by capability

```bash
# All agents that can summarize (substring match on skills)
postbox user search --meta skills=summarization

# All agents on the analytics team
postbox user search --meta team=analytics

# Combined: type + skill
postbox user search --meta type=agent --meta skills=web-search

# Look up a specific user
postbox user get researcher@example.ai
```

### 5. Send a task

```bash
postbox mail send \
  --from manager@example.ai \
  --to researcher@example.ai \
  --subject "Task: market trends" \
  --body "Summarize the top 5 AI agent frameworks this week."
```

### 6. Receive and act on messages

```bash
postbox mail list --user researcher@example.ai
postbox mail read --user researcher@example.ai --id <message-id>
```

### 7. Enable webhook push delivery

```bash
# Start server with webhook dispatcher enabled
postbox serve --db postbox.db --webhook-enabled --webhook-workers 8
```

Agents registered with `--meta endpoint=<url>` will receive a signed HTTP POST
whenever a message arrives in their mailbox. Postbox signs each payload with
its Ed25519 node key; agents verify the `X-Postbox-Signature-Ed25519` header
using the public key returned by `GetNodePublicKey`.

## User Model

Every principal is a **User** with three layers:

| Field | Purpose |
|-------|---------|
| `email` | Unique address and mailbox ID |
| `type` | `human` \| `agent` \| `service` \| `` (unset) |
| `public_key` | Ed25519 public key (base64); for agents that sign their own messages |
| `metadata` | Open key-value map: `skills`, `endpoint`, `region`, `model`, … |

Metadata values are substring-matched during search, so `skills=web-search`
matches `"web-search,summarization"`.

## CLI Reference

### User registry

```
postbox user register  --email <addr> [--type human|agent|service]
                       [--public-key KEY] [--meta k=v]...
postbox user list
postbox user search    [--meta k=v]...
postbox user get       <email>
```

### Domain management

```
postbox domain list
postbox domain remap    --domain <name> --node <node-id>
```

### Quick registration (domain or user)

```
postbox register --domain <name> [--default]
postbox register --user  <email> [--meta k=v]...
```

### Discovery

```
postbox discover <email-or-domain>
```

### Mailbox

```
postbox mail send   --from <addr> --to <addr> --subject <s> --body <b>
                    [--reply-to <id>] [--thread <id>]
postbox mail list   --user <addr> [--folder __inbox] [--limit 20] [--offset 0]
postbox mail read   --user <addr> --id <message-id>
postbox mail tag    --user <addr> --id <message-id> [--add <label>] [--remove <label>]
postbox mail move   --user <addr> --id <message-id> --folder <name>
```

### SMTP gateway

```
postbox smtp start  [--port 2525] [--domain example.ai] [--insecure-auth]
                    [--max-msg-bytes 0] [--max-recipients 0]
                    [--rate-conns 0] [--rate-burst 0]
postbox smtp stop
postbox smtp status
```

### Node

```
postbox serve   [--port 50051] [--host localhost] [--db postbox.db]
                [--mode standalone|temp] [--node-id ""]
                [--grpc-auth-token ""]
                [--redis-url redis://localhost:6379]
                [--mailbox-backend memory|postgres|mongo]
                [--mailbox-dsn <connection-string>]
                [--metrics-port 9090] [--health-enabled]
                [--smtp-enabled] [--smtp-port 2525] [--smtp-domain <d>]
                [--smtp-insecure-auth] [--smtp-tls-cert <f>] [--smtp-tls-key <f>]
                [--lmtp-enabled] [--lmtp-socket-path <path>] [--lmtp-port 2424]
                [--webhook-enabled] [--webhook-workers 4]
                [--outbound-backend smtp|sendgrid]
                [--outbound-smtp-host <host:port>]
                [--outbound-dkim-private-key-file <f>]
                [--outbound-queue]
postbox status
```

## Configuration File

All flags can be set via a YAML config file (`--config postbox.yaml`). See `postbox.example.yaml` for the full annotated reference. A minimal example:

```yaml
server:
  port: "50051"
  host: localhost
  db: postbox.db
  mode: standalone

grpc:
  auth_token: ""        # set a token to require auth on every gRPC call

mailbox_store:
  backend: memory       # memory | postgres | mongo — default is in-memory (data lost on restart)
  dsn: ""

observability:
  metrics_port: 9090    # 0 = disabled
  health_enabled: true

cluster:
  redis_url: ""         # enables multi-node routing, e.g. redis://localhost:6379

smtp:
  enabled: false
  port: 2525
  domain: localhost
  insecure_auth: false  # allow AUTH over plain-text; enable only on private networks

webhook:
  enabled: false
  workers: 4
  max_attempts: 5
  timeout_secs: 10
```

## Mailbox Store Backends

The mailbox store holds messages. The default is **in-memory** — messages are lost on restart. Use PostgreSQL or MongoDB for production deployments.

### PostgreSQL

```bash
postbox serve --mailbox-backend postgres \
  --mailbox-dsn "postgres://user:pass@host:5432/postbox?sslmode=disable"
```

### MongoDB

```bash
postbox serve --mailbox-backend mongo \
  --mailbox-dsn "mongodb://localhost:27017" \
  --mailbox-db postbox
```

## Observability

### Prometheus metrics

When `--metrics-port` is non-zero (default: 9090), a `/metrics` endpoint is served using OpenTelemetry with a Prometheus bridge:

```bash
postbox serve --metrics-port 9090
curl http://localhost:9090/metrics
```

### gRPC health check

Enabled by default via `--health-enabled`. Compatible with standard gRPC health probes:

```bash
grpc_health_probe -addr=localhost:50051
```

## Outbound Relay

Messages addressed to recipients not registered locally are routed to the outbound relay. Requires `--outbound-backend`.

### SMTP relay (covers AWS SES, Mailgun, Postfix, any SMTP relay)

```bash
postbox serve \
  --outbound-backend smtp \
  --outbound-from noreply@example.ai \
  --outbound-smtp-host email-smtp.us-east-1.amazonaws.com:587 \
  --outbound-smtp-username AKID... \
  --outbound-smtp-password secret
```

### SendGrid

```bash
postbox serve \
  --outbound-backend sendgrid \
  --outbound-from noreply@example.ai \
  --outbound-sendgrid-api-key SG.xxxx
```

### DKIM signing (SMTP backend only)

```bash
postbox serve \
  --outbound-backend smtp \
  --outbound-dkim-private-key-file /etc/postbox/dkim.pem \
  --outbound-dkim-domain example.ai \
  --outbound-dkim-selector postbox
```

The public key must be published at `postbox._domainkey.example.ai` in DNS.

### Async delivery queue

Add `--outbound-queue` to buffer messages and retry with exponential backoff instead of failing inline:

```bash
postbox serve --outbound-backend smtp --outbound-queue \
  --outbound-queue-workers 4 \
  --outbound-queue-max-retries 7 \
  --outbound-queue-base-delay-secs 5
```

## LMTP Gateway

Receive local delivery from an upstream MTA (e.g. Postfix) via LMTP (RFC 2033). Using a Unix socket is recommended for co-located deployments:

```bash
postbox serve --lmtp-enabled \
  --lmtp-socket-path /run/postbox/lmtp.sock
```

For TCP:

```bash
postbox serve --lmtp-enabled --lmtp-port 2424 --lmtp-bind-addr 127.0.0.1
```

## Plugins

Plugins run in a chain — cheapest checks first. Each plugin can reject or pass a message. All plugins are **disabled by default**.

| Plugin | Flag prefix | Description |
|--------|-------------|-------------|
| CrowdSec | `--crowdsec-*` | IP reputation via LAPI bouncer API |
| DNSBL | `--dnsbl-*` | DNS block-list lookup (Spamhaus ZEN, etc.) |
| Address filter | `--address-filter-*` | Allow/block by sender or recipient pattern (rules: YAML only) |
| Attachment filter | `--attachment-*` | Attachment count, size, and MIME type limits |
| SMTP security | `--smtp-security-*` | IP lockout, spoof detection, sender domain controls |
| Email auth | `--email-auth-*` | SPF, DKIM, and DMARC policy enforcement |
| Spam checker | `--spam-*` | Delegate scoring to an HTTP endpoint (rspamd, etc.) |
| Antivirus | `--av-*` | Delegate AV scanning to an HTTP endpoint (ClamAV, etc.) |
| Security agent | `--security-*` | Per-sender rate limits and message size caps |

Example — enable DNSBL and spam check:

```bash
postbox serve \
  --dnsbl-enabled --dnsbl-zones zen.spamhaus.org \
  --spam-enabled --spam-endpoint http://rspamd:11333/checkv2 --spam-threshold 5.0
```

## Development

```bash
mise install          # install Go, buf, just, golangci-lint
just build            # compile
just test             # unit tests
just test-race        # race detector
just lint             # golangci-lint
just proto            # regenerate gRPC stubs from proto/postbox/v1/postbox.proto
```

### Node store backends

SQLite (default, no external dependencies):

```bash
postbox serve --db /var/lib/postbox/postbox.db
```

PostgreSQL:

```bash
postbox serve --db "" --mode standalone \
  # note: --db is the node store (users/domains/jobs), not messages
  # for message persistence use --mailbox-backend postgres
```

### Running tests

```bash
go test ./...                                   # all unit tests
go test ./internal/store/sqlite/...             # SQLite store only
POSTGRES_DSN=postgres://... go test ./internal/store/sqlstore/...
```

## Roadmap

- [x] **Phase 1** — Production foundation: durable mailbox backends (PostgreSQL, MongoDB), OpenTelemetry metrics, gRPC health, DNSBL, Redis IP lockout, outbound relay (SMTP + SendGrid + DKIM signing), DMARC enforcement, async delivery queue, 9-plugin security chain
- [x] **Phase 2** — Distributed routing: Redis-backed routing table (`internal/registry/redis`), TTL-based node heartbeat, `Discover` returns the real owning-node address, forwarding interceptor transparently delivers messages to remote nodes via `deliver_to`
- [ ] **Phase 3** — Complete inbound compliance: implicit TLS (port 465), IMAP server, OAuth2/OIDC authentication
- [ ] **Phase 4** — Direct outbound delivery: MX lookup + direct SMTP dialer, persistent delivery queue, bounce/DSN generation
- [ ] **Phase 5** — Enterprise and scale: per-mailbox quotas and retention, full-text search, Milter protocol, multi-tenancy, admin REST API + web UI
