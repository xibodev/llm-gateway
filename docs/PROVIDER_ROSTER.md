# Provider discovery roster

The discovery roster is data, separate from configured providers. Refreshing it
must never change an instance, credential, owner, model selection, or route.
Adding a roster candidate copies its reviewed setup into local configuration.

## Feed contract

The transport is validated JSON served over public HTTPS. The payload structure:

```json
{
  "schema_version": 1,
  "revision": 1,
  "published_at": "2026-01-01T00:00:00Z",
  "entries": [
    {
      "id": "endpoint-0123456789abcdef",
      "name": "Example provider",
      "protocol": "openai",
      "base_url": "https://api.example.com/v1",
      "signup_url": "https://example.com/api-keys",
      "auth": "api_key",
      "offer": "unknown",
      "description": "Source-reported offer; confirm provider terms.",
      "setup": "candidate",
      "state": "active",
      "sources": [{"repo": "owner/directory", "commit": "commit-sha", "url": "https://github.com/owner/directory"}],
      "probe": {"status": "not_checked", "checked_at": "", "http_status": 0},
      "logo": {"mime": "image/png", "data": "", "sha256": "", "license": "", "source_url": ""},
      "report": {"issue_url": "", "confirmations": 0, "reason": ""}
    }
  ],
  "sources": [{"repo": "owner/directory", "commit": "commit-sha", "status": "ok"}]
}
```

HTTPS plus strict structural validation is the trust model for discovery-only
data. Optional Ed25519 signing is available for operators who want an additional
publisher-identity layer; when no signing key is configured, the consumer
validates the JSON payload directly.

Fields use closed vocabularies:

- `protocol`: `openai`, `anthropic`, `unknown`.
- `auth`: `api_key`, `none`, `unknown`. Remote data cannot introduce OAuth adapters.
- `offer`: `free_tier`, `recurring_credit`, `trial`, `paid`, `unknown`.
- `setup`: `compatible` or `candidate`. Only compatible entries with known auth
  can prefill a shipped generic adapter; this is not certification of inference.
- `state`: `active`, `quarantined`, `withdrawn`.
- `probe.status`: `not_checked`, `reachable`, `auth_required`, `rate_limited`,
  `failed`, `blocked`.

An entry can additionally carry `offer_expires_at` as an RFC3339 timestamp,
`requirements` as a string array, and `conflicts` as a string array. Known
conflicts must remain visible rather than being resolved by optimistic ranking.
Optional objects and string fields can be omitted when unknown. Unknown is not
equivalent to free, anonymous, compatible, or healthy.

Stable entry IDs derive from protocol plus canonical endpoint URL, preserving
meaningful path distinctions. Alias mappings can merge known provider identities,
but local/cloud, regional endpoints, and independent gateways remain distinct.
Sources and required asset notices are metadata, not obligatory UI decoration.
Raster logos are bounded, content-validated and embedded in the signed feed;
the browser never inlines an arbitrary remote SVG or fetches a tracking image.

## Collection and community state

Source-specific code parses pinned repository snapshots without executing source
code. Collection deduplicates before bounded public-endpoint probes. Failed
fetches/parsers retain previous entries and mark the source stale. A complete
failure does not publish an empty replacement. Probes have no provider credentials
and establish HTTP reachability, not inference entitlement or zero cost.

Reports identify known roster IDs. One canonical GitHub issue collects distinct
human-account confirmations for an incident. Ten confirmations quarantine the
discovery entry; they do not delete configured providers or prove independent
failures. Persisted tombstones prevent source collection from resurrecting an
entry. A maintainer restore override starts a new incident epoch.

The UI opens GitHub's authenticated issue/report flow. No GitHub token is shipped
to installations, and the gateway does not post reports on behalf of users.

## Staging boundary

Workflows build and validate downloadable artifacts only. Publishing a trusted
home feed, provisioning its signing secret, and deploying services are separate
operator actions. Scheduled workflows execute from GitHub's default branch, so
a workflow present only on staging can be exercised manually but has no active
daily schedule until integrated into the default branch.

## Staging operations

`provider-roster.yml` has three isolated jobs: collection, read-only GitHub
report reconciliation, and payload validation/packaging. Checkouts do not persist
credentials. The collector receives no provider credentials or GitHub token;
GitHub reads use the short-lived workflow token only in the steps that need it.
The packaging job validates payload size and schema, then uploads the plain JSON.
There are no issue/comment/reaction event triggers, issue writes, Pages deployment,
release publication, or signing secrets in this workflow.

The cron expression is `23 5 * * *` (daily, UTC). GitHub schedules run only from
the default branch and can be delayed. A staging branch alone has no active cron.
To dispatch a staging revision, GitHub must first know the workflow on the default
branch; select the staging ref in the manual workflow UI. Local validation remains
available before that integration. Scheduled collection enables bounded public
reachability probes; manual dispatch defaults `no_probe` to true for staging and
can explicitly enable probes. Collector fixture tests run before collection.
Issue-supplied URLs are never probe targets.

The first manual run requires `bootstrap: true`. Subsequent runs retrieve
`provider-roster-staging` from the latest successful run of this workflow
on the same branch. They pass its `payload.json` to the collector and its
`report-state.json` to reconciliation. Jobs are serialized per branch. Missing,
expired, inaccessible, malformed, or pagination-truncated baseline state stops
the build rather than silently resetting it or falling back to older state.
Artifacts are retained for 90 days; this is staging continuity, not a durable
production state store. Preserve both files before retention expires. If the
latest artifact is lost, recover the saved pair and validate locally; changing
branches or deleting run history must not be used to bypass existing tombstones.

The final artifact contains:

- `payload.json`, the reconciled roster with any logos still inline;
- `report-state.json`, persistent community tombstones and incident watermarks;
- `actions-plan.json`, an explicitly dry-run quarantine/restore/duplicate plan.

### Community reports and restores

Use the **Provider roster incident** issue form and copy a known canonical ID
into **Roster entry ID**. The reconciler reads open and closed issues, rejects
unknown IDs, and keeps the earliest known canonical issue number. Duplicate issues
share a union of confirmations; renaming an account does not add a vote.
Each accepted issue number is permanently bound to its first roster ID in
`report-state.json`, scoped to the source GitHub repository. This includes duplicate
issues. A later body edit naming a different or invalid ID excludes that report
and produces a dry-run `reject_identity_edit` action; it never transfers existing
reactions to another entry. Deletion, closing, reopening, and incident restores
do not remove bindings. State from another repository is rejected. There is no
automatic identity reset or issue-writing action.

Older state remains readable: canonical `issue_number` values and any recorded
`issue_numbers` arrays seed the immutable bindings on migration. Conflicting
historical attribution fails closed. Duplicate numbers never recorded by an older
artifact cannot be reconstructed from current edited bodies; recover that history
from retained artifacts for a legacy migration where those duplicates matter.

The report author, thumbs-up reactions on the root report, a comment containing
only `Roster confirmation: endpoint-0123456789abcdef` (substitute the actual ID),
and thumbs-up reactions on such human-authored confirmation comments are eligible.
Other comments and reactions are not confirmations. Only positive numeric GitHub
account IDs of type `User` count; bot accounts are excluded. Ten accounts is a
community signal, not proof of ten independent failures or Sybil resistance.

At ten confirmations the state records a quarantine tombstone. Deleted votes,
closed issues, removed source entries, and later collection cannot clear it.
An incomplete GitHub snapshot retains all previous community entries and marks
`fetch_status` stale. A failed first fetch cannot bootstrap. Pagination is bounded
to 20 pages per list and 1,000 total requests with bounded retry backoff; exceeding
either budget is treated as incomplete, never as an empty successful snapshot.

A maintainer can manually dispatch `restore_entry_id` plus `restore_since` (a
UTC RFC3339 incident start, no later than the current time). Both are handled as
data, not interpolated into shell commands. The complete current snapshot becomes
a watermark of already-observed vote events, and confirmations must also be newer
than the incident start. Reusing the same epoch is idempotent; moving it backwards
is rejected. Old comments edited into confirmations still have their old creation
time. A later incident needs ten eligible accounts again. A restore on an
incomplete fetch is deferred, so inspect the output and retry the manual dispatch.
Withdrawn entries remain withdrawn: removing a quarantined entry from sources
retains its community incident while marking it withdrawn, and restoring the
community incident does not reactivate the source-withdrawn entry.

For local maintainer overrides, copy `overrides.example.json` to a local file
outside the repository and add `restores` objects with `entry_id` and
`incident_since`. Pass its path using `--overrides`. Real state and override
configuration must not be committed. Staging has no `--apply` mode and requests
no `issues: write` permission: its plan never posts, closes, or edits an issue.
Provider/source requests use the separate form and require human review plus
source-specific extractor code; the issue itself never enrolls a source.

### Local validation commands

Use an output directory outside the repository. Replace paths with local paths;
the commands below use placeholder paths only. The report step can read public
GitHub data unauthenticated or use `GITHUB_TOKEN` supplied through the environment
with read-only issue access. Never paste a token into a command or fixture.

```bash
node --test scripts/provider-roster-community/*.test.mjs
node scripts/provider-roster/build.mjs --output /path/to/collected --previous /path/to/previous/payload.json --no-probe
node scripts/provider-roster-community/cli.mjs --repository example/roster --payload /path/to/collected/payload.json --previous /path/to/previous/report-state.json --output /path/to/reconciled
node scripts/check-docs.mjs
```

For an explicitly new staging baseline, omit the collector's `--previous` and
replace the reconciler's `--previous` pair with `--bootstrap`. The reconciler
requires a valid nonempty payload, persistent report state, and a dry-run plan.
Oversized artifacts fail before output files are written. Fixtures cover duplicate
accounts, bots, restore epochs, tombstones, API failure limits, and tampering.

### Gateway environment and refresh

| Variable | Meaning |
| --- | --- |
| `LLMGW_PROVIDER_ROSTER_URL` | Operator-configured feed URL; no default URL until publishing is configured. |
| `LLMGW_PROVIDER_ROSTER_PUBLIC_KEY` | Optional base64 of a 32-byte Ed25519 public key. When set, the feed must be a signed envelope. When absent, the feed is consumed as plain HTTPS JSON. |
| `LLMGW_PROVIDER_ROSTER_KEY_ID` | Expected envelope key ID; defaults to `staging`. Only used when `PUBLIC_KEY` is set. |
| `LLMGW_PROVIDER_ROSTER_AUTO_REFRESH` | Boolean, defaults to `true`; only refreshes when URL is configured. |

Gateway auto-refresh is independent of the GitHub collection cron. Disabling
auto-refresh does not disable authenticated admin manual refresh:
`POST /admin/api/provider-roster/refresh` (also available in the roster UI).
Refresh failures retain the last verified cache; they do not modify providers,
credentials, ownership, model selections, or routes.

The deployment Compose example forwards the variables to the container.
For direct Docker runs, pass `-e LLMGW_PROVIDER_ROSTER_AUTO_REFRESH=false` to
disable the background updater. Signing keys are optional; when absent the feed
is validated as plain HTTPS JSON with structural checks.
