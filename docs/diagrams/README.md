# RepoMind — visual reference

Three diagrams that explain how the system is shaped and how a webhook turns into a Confluence release note. **Open this file in any Markdown viewer that renders Mermaid** (GitHub web UI, VS Code with the *Markdown Preview Mermaid Support* extension, JetBrains, Obsidian, etc.).

For deeper detail see [docs/ARCHITECTURE.md](../ARCHITECTURE.md).

---

## 1. Architecture — Clean Architecture layers

Inward dependency rule: `interfaces → application → domain ← infrastructure`. The domain layer has zero imports outside itself.

```mermaid
graph TB
    classDef ext     fill:#fff5e6,stroke:#cc8400,color:#000
    classDef ifc     fill:#e6f3ff,stroke:#0066cc,color:#000
    classDef app     fill:#f0f0ff,stroke:#4040cc,color:#000
    classDef dom     fill:#fff0f0,stroke:#cc4040,color:#000
    classDef inf     fill:#e6ffe6,stroke:#008800,color:#000

    subgraph EXT["External systems (the world)"]
        direction LR
        GH["GitHub<br/>App webhooks + REST"]:::ext
        JR["Jira Cloud<br/>REST"]:::ext
        AI["LLM provider<br/>OpenAI / Azure OpenAI / GitHub Models"]:::ext
        CF["Confluence Cloud<br/>REST"]:::ext
        DB[(PostgreSQL 16<br/>outbox + LISTEN/NOTIFY trigger)]:::ext
    end

    subgraph IFC["Interfaces layer (cmd/repomind + internal/interfaces/http)"]
        direction LR
        HTTPSRV["HTTP server<br/>POST /webhooks/github<br/>POST /api/v1/sprints/{id}/release-notes<br/>GET /healthz · /readyz"]:::ifc
        WORKER["Sprint worker<br/>Dispatcher (LISTEN + 30s poll)<br/>+ SprintTicker (5min Jira poll)"]:::ifc
    end

    subgraph APP["Application layer (internal/application)"]
        direction TB
        ORCH["orchestration<br/>Enqueuer · Dispatcher · 3 handlers · Ticker"]:::app
        ANAL["analysis<br/>risk · qa · release-notes composers (LLM-bound)"]:::app
        PORTS["ports<br/>jira.Client · github.Client · publish.Publisher<br/>persistence.Stores · llm.Provider · github.BranchPolicy"]:::app
    end

    subgraph DOM["Domain layer (internal/domain — zero external deps)"]
        direction LR
        AGG["Aggregates<br/>Repository · PullRequest · Sprint ·<br/>Release · Analysis"]:::dom
        SK["Shared kernel<br/>typed IDs · Author · Branch · SHA · TicketKey ·<br/>Clock · EventEmitter · sentinels"]:::dom
    end

    subgraph INF["Infrastructure layer (internal/infrastructure)"]
        direction TB
        IGH["github adapter<br/>ghinstallation/v2 +<br/>HMAC verifier +<br/>RepoPolicy (YAML)"]:::inf
        IJR["jira adapter<br/>REST + Basic auth"]:::inf
        IAI["openai adapter<br/>3 deployments,<br/>JSON-schema strict mode"]:::inf
        ICF["confluence adapter<br/>section markers +<br/>409 retry"]:::inf
        IPG["postgres adapter<br/>pgx/v5 + UoW +<br/>outbox NOTIFY trigger"]:::inf
        ICFG["config loader<br/>(envconfig)"]:::inf
    end

    GH -.->|webhook delivery| HTTPSRV
    HTTPSRV --> ORCH
    ORCH --> PORTS
    ORCH --> ANAL
    ANAL --> PORTS
    ORCH --> AGG
    ANAL --> AGG
    AGG --- SK

    WORKER --> ORCH
    WORKER -.LISTEN repomind_outbox.- IPG

    PORTS -.satisfied by.-> IGH
    PORTS -.satisfied by.-> IJR
    PORTS -.satisfied by.-> IAI
    PORTS -.satisfied by.-> ICF
    PORTS -.satisfied by.-> IPG

    IGH --> GH
    IJR --> JR
    IAI --> AI
    ICF --> CF
    IPG --> DB
    ICFG -.populates.-> APP
```

**How to read it**
- Inward arrows = "depends on"; the domain has none pointing out.
- Dotted lines = "implements an interface defined by".
- All infrastructure adapters are swappable behind their port (the analysers don't know whether the LLM is GitHub Models or Azure OpenAI; the publisher could be Notion tomorrow).

---

## 2. End-to-end flow — from PR opened to Confluence updated

The two columns on the right are the M10 worker handlers; everything between `outbox: ...` arrows runs asynchronously via the dispatcher.

```mermaid
sequenceDiagram
    autonumber
    participant Dev as Developer
    participant GH as GitHub
    participant API as HTTP server (M9)
    participant DB as Postgres outbox
    participant Disp as Dispatcher (M10)
    participant WH as WebhookHandler
    participant PRH as PRAnalysisHandler
    participant LLM as LLM (risk + qa)
    participant Op as Operator / Ticker
    participant RNH as ReleaseNotesHandler
    participant Jira as Jira
    participant Conf as Confluence

    rect rgb(245,245,255)
    note over Dev,API: PR ingestion — M9 + M10 Phase 3
    Dev->>GH: open PR against `qa` branch
    GH->>API: POST /webhooks/github + HMAC
    API->>API: HMAC + repo allow-list + branch policy
    API->>DB: INSERT outbox (event_type=github.webhook.received, dedup_key=delivery_id)
    DB-->>API: pg_notify trigger fires
    API-->>GH: 202 Accepted
    end

    DB->>Disp: NOTIFY repomind_outbox
    Disp->>DB: SELECT pending rows
    Disp->>WH: Handle(github.webhook.received)
    WH->>WH: decode envelope · action analysable? · base.ref allowed?
    WH->>DB: INSERT outbox (event_type=pr.analysis.requested, dedup_key=pr-analysis:owner/name#n@head_sha)
    WH-->>Disp: nil
    Disp->>DB: MarkDispatched

    rect rgb(245,255,245)
    note over Disp,LLM: PR analysis — M10 Phase 4
    DB->>Disp: NOTIFY repomind_outbox
    Disp->>PRH: Handle(pr.analysis.requested)
    PRH->>GH: GetPullRequest + files + commits + reviews
    PRH->>DB: UoW.Within: upsert Repository + upsert PullRequest
    PRH->>LLM: risk.Analyze(pr)
    PRH->>DB: Analyses.Save(risk)
    PRH->>LLM: qa.Analyze(pr)
    PRH->>DB: Analyses.Save(qa)
    PRH-->>Disp: nil
    Disp->>DB: MarkDispatched
    end

    rect rgb(255,250,240)
    note over Op,Conf: Release notes — M10 Phases 5 + 6
    alt Operator-triggered
        Op->>API: POST /api/v1/sprints/{id}/release-notes (Bearer)
    else Ticker-triggered (active sprint closed)
        Op->>Jira: ListSprints(board)
        Op->>DB: worker_state bookmark active→closed?
        Op->>DB: INSERT outbox (release_notes.requested, dedup_key=...)
    end

    DB->>Disp: NOTIFY
    Disp->>RNH: Handle(release_notes.requested)
    RNH->>DB: Sprints.FindByID
    RNH->>Jira: ListSprintIssues → ticket keys
    RNH->>DB: PullRequests.ListByTickets → filter Merged
    RNH->>RNH: compute content_hash<br/>(L3) latest release same hash + published < 1h? SKIP
    RNH->>LLM: composer.Analyze(sprint + PRs)
    RNH->>DB: UoW.Within: Draft Release (content_hash, v0.0.N) + Save Analysis
    RNH->>Conf: Publish (full-page PUT, version+1)
    Conf-->>RNH: 200 (or 409 → internal retry, L6)
    RNH->>DB: release.Publish(now) + Save
    RNH-->>Disp: nil
    Disp->>DB: MarkDispatched
    end
```

**Where the 6 duplicate-prevention layers fire** (see [ARCHITECTURE.md §5 Module 10](../ARCHITECTURE.md#module-10--sprint-worker-dispatcher--ticker)):

| Layer | Step in the diagram |
|---|---|
| L1 outbox dedup on insert | "INSERT outbox" steps — `UNIQUE(dedup_key)` collapses retries |
| L2 dispatcher idempotent mark | `MarkDispatched` is a no-op if already dispatched |
| L3 content-hash short-circuit | RNH `compute content_hash` step — skips LLM + Confluence |
| L4 transactional release insert | RNH `UoW.Within` — `UNIQUE(sprint_id, version)` enforced |
| L5 full-page PUT | RNH `Conf.Publish` — tickets can't appear twice in the page |
| L6 Confluence 409 retry | `Conf 200 or 409 retry` arrow |

---

## 3. Outbox event lifecycle (state machine)

Every `outbox` row is one of these states. M10's dispatcher drives the transitions.

```mermaid
stateDiagram-v2
    [*] --> pending: Enqueue / EnqueueWithDedup<br/>(L1 dedup_key UNIQUE)

    pending --> dispatched: handler returned nil<br/>(MarkDispatched)

    pending --> pending: handler returned error<br/>+ attempts < MaxAttempts<br/>(MarkFailed → backoff 30s · 1m · 2m · 4m · 8m, cap 1h)

    pending --> failed: handler returned error<br/>+ attempts == MaxAttempts<br/>(MarkPermanentlyFailed)

    pending --> failed: no handler registered<br/>for event_type<br/>(operator bug, fail-loud)

    dispatched --> [*]
    failed --> [*]: terminal -- operator inspects last_error
```

**Visibility for operators:**

```sql
-- live dispatcher health
SELECT status, count(*) FROM outbox GROUP BY status;

-- in-flight events ready to attempt
SELECT id, event_type, attempts, dedup_key, next_attempt_at
FROM outbox
WHERE status = 'pending' AND next_attempt_at <= now()
ORDER BY next_attempt_at;

-- permanent failures with cause
SELECT id, event_type, attempts, last_error, created_at
FROM outbox
WHERE status = 'failed'
ORDER BY created_at DESC
LIMIT 20;
```

---

## How to keep these diagrams accurate

When a code change affects:

- **layering / new adapter** → update diagram 1.
- **handler registry / new event type / new outbound system** → update diagram 2.
- **outbox lifecycle / new outbox status** → update diagram 3.
- **6-layer dedup design** → update the table under diagram 2 + the matching §5 Module 10 entry in [ARCHITECTURE.md](../ARCHITECTURE.md).

Keep the diagrams in this file; do not fork a new diagrams file per module — the cognitive cost of "which diagram is canonical" outweighs the small benefit of separation.
