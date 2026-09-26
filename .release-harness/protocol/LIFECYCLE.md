# Release-Harness continuous lifecycle

This is the canonical provider-neutral capability for carrying accepted release
intent across sessions, branches, worktrees, merges, and later releases.

The proposition model is unchanged:

```text
subject + assertions + requires + execution bindings
```

Lifecycle reviews sit around accepted contracts. They never enter contract
identity, rewrite acceptance, or decide a verdict.

The optional lifecycle capability is Git-backed. When one accepted subject really
spans source in multiple repositories, configure each additional exact source:

```text
release-harness lifecycle source set <source-id> --path <directory>
```

`source-id` is bookkeeping identity only. It is not a component, role, service,
or topology concept. Exact normative contract dependencies under `requires` do
not automatically become lifecycle source roots.

## Start every session by resuming state

Run:

```text
release-harness lifecycle status
```

Read existing accepted contracts, drafts, authoring records, confirmed reviews,
unresolved questions, and source-coverage facts before broad repository
inspection. If accepted state exists, ask what changed since the covered source
and whether that changes what should be certified. Do not rediscover the project
as if earlier decisions never existed.

## Choose one workflow

### adopt

Use when no accepted proposition exists.

1. Follow the installed `ADOPTION.md` protocol.
2. Author a draft and factual authoring record.
3. Leave operator decisions unresolved.
4. The operator accepts the proposition explicitly.
5. Start a `baseline` lifecycle review against current source.
6. Map every accepted assertion and `requires` identity.
7. The operator confirms the exact review if it concludes `reuse_contract`.
8. Report untracked durable state; never commit automatically.

Acceptance means only responsibility for the proposition. Baseline review means
that proposition was separately reviewed as appropriate for exact source.

### review-change

Use after source changes.

```text
release-harness review start <name> --base <ref> --contract <digest>
```

The CLI resolves exact source identities and changed paths. It does not infer
semantic impact. Inspect the changed evidence and author:

- factual observations under `facts`;
- one impact entry for every accepted assertion;
- one impact entry for every exact `requires` identity;
- advisory possible binding changes;
- unresolved questions;
- `reuse_contract`, `rebind`, `reauthor`, or `block` conclusion.

An implementation/blob change is factual. “The proposition remains appropriate”
is a semantic judgment and belongs under impact/conclusion, never an `observed`
fact.

`reuse_contract` and source-affecting `rebind` require attributable operator
confirmation. `block`, `reauthor`, and uncertainty already block certification
without confirmation.

### prepare-release

1. Run `release-harness lifecycle check`.
2. Stop if review is missing, stale, unconfirmed, blocking, or source is not
   established.
3. Inspect unresolved drafts and normative references.
4. Select or update the execution binding independently.
5. Run certification only when deterministic lifecycle and ordinary
   contract/binding/reference gates permit it.
6. Run `release-harness verify`.

A new source revision does not require a new contract when a confirmed review
says the accepted proposition remains appropriate. A changed proposition does.
A binding-only environment change does not inherently require source review.

### investigate-result

1. Verify the historical run before interpreting it.
2. Read deterministic cause classification and sealed observations.
3. Delegate only bounded evidence questions when useful.
4. Propose corrective action without editing sealed evidence, historical
   manifests, accepted contracts, or verdicts.
5. After source changes, return to `review-change`.

FAIL and UNPROVEN remain exactly what the deterministic core recorded. No agent,
worker, operator, or adapter can turn either into PASS.

## Temporary workers

A coordinator may ask temporary workers to inspect changed tests, a changed
build/deployment file, or a repository concretely named by evidence. Give each
worker an exact question and bounded scope. Workers return factual observations;
they do not accept contracts, confirm reviews, decide semantic impact, or claim
release readiness.

## Durable state

Usually source-control:

```text
.release-harness/config.json
.release-harness/drafts/
.release-harness/accepted/
.release-harness/reviews/
nonsecret deterministic fixtures
project-local capability adapters
package manifest and lockfile
```

Usually keep local or externally retain:

```text
.release-harness/runs/
.release-harness/bindings/
credentials
large or sensitive evidence
caches
```

Portable nonsecret CI bindings may be deliberately committed. Release-Harness
reports tracking facts and never runs `git add` or commits.

## Migration

Legacy topology-era artifacts (`topology.json`, `origins.json`,
`harness.config.json`, `scenarios/`) are investigation leads only. Preserve them
outside the active installation and do not convert generated values into claims.

For current beta/vNext state, preserve drafts, records, accepted contracts,
bindings, and runs. Refresh only harness-owned protocol/adapters. Existing
contracts remain valid historical identities; missing old authoring provenance
may reduce explainability but does not invalidate them.
