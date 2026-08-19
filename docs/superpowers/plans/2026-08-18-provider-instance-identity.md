# Provider Instance Identity (Workstream A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the console treat a provider *instance* as the real entity and a registry tile as a mere aggregator, so N providers of the same type can be created and managed from the UI.

**Architecture:** The server already computes full per-instance data in `configuredProviderSnapshot` and then discards it while flattening to one row per registry entry. This plan stops discarding it — serializing each match through the existing `providerSnapshotRow` helper into a new additive `instances[]` field — then rebinds the console to that array and removes the four places where it resolves an instance as `[0]`. Purely additive on the wire; no route changes, no breaking changes.

**Tech Stack:** Go (stdlib only — three direct dependencies total, keep it that way), Preact + TypeScript console built with Vite.

**Spec:** `docs/superpowers/specs/2026-08-18-provider-instances-credentials-catalog-design.md`

## Global Constraints

- **This repository is public.** Before every commit check file contents, commit message, AND author identity (`git log -1 --format='%an <%ae> / %cn <%ce>'`). See `AGENTS.md`.
- Never commit real hostnames, IPs, cloud account/project/subscription ids, customer names, or credentials — not even expired ones.
- **No AI attribution trailers in commit messages.**
- Do not commit dates or version stamps into documentation.
- Keep the Go dependency tree at its current three direct dependencies. Prefer stdlib.
- Match surrounding code: comment density, naming, error style. Comments explain **why** — prefer recording a decision or a trap over restating code.
- **Go gate:** `cd go && go build ./... && go vet ./... && go test ./...`
- **Console gate:** `cd go/internal/web/console && npm ci && npm run lint && npm test && npm run check:dist`
- `check:dist` is mandatory: `dist/` is committed because the Go binary embeds it. Any console source change **must** be rebuilt and the `dist/` diff committed with it, or CI fails.
- Console has **no component-test framework**. `npm test` runs source-text assertions via `node --test`; `npm run lint` is `tsc --noEmit`. Do not add a test framework in this workstream. Console correctness = types + source assertions + live verification.

## Known Limitation (state it, do not paper over it)

Tasks 2–6 change console behaviour that the existing test tooling cannot execute. Their tests are source-text assertions, which prove the code *says* the right thing, not that it *does* the right thing. Behavioural verification for those tasks happens in the live-run step at the end. Do not claim behavioural coverage you do not have.

## File Structure

| File | Responsibility | Action |
|---|---|---|
| `go/internal/api/provider_status.go` | Builds the per-registry status snapshot; currently flattens away per-instance data | Modify (~lines 144–200) |
| `go/internal/api/provider_instances_test.go` | Proves the payload carries per-instance rows and no rollup status | Create |
| `go/internal/web/console/src/components/providers/shared.ts` | Instance-resolution helpers; currently returns `[0]` | Modify |
| `go/internal/web/console/src/components/providers/ProviderHub.tsx` | Tile grid, connect dialog, credential dialog | Modify |
| `go/internal/web/console/src/pages/ProviderDetail.tsx` | Per-instance table (shell exists, data is aggregate) | Modify |
| `go/internal/web/console/tests/smoke.test.mjs` | Source assertions for the console changes | Modify |

---

### Task 1: Server emits per-instance rows and stops rolling up status

**Files:**
- Modify: `go/internal/api/provider_status.go:144-200`
- Test: `go/internal/api/provider_instances_test.go`

**Interfaces:**
- Consumes: `configuredProviderSnapshot` (fields `id, registryID, status, catalogState, catalogRefresh, modelCount, connectionCount, disabled, lastCheck, lastVerify, configurationIssue`), `providerSnapshotRow(row map[string]any, lastCheck, lastVerify *iam.ProviderCheck) map[string]any`
- Produces: each row in `provider_statuses` gains `instances []map[string]any` and `instance_status_counts map[string]int`. Each instance row carries `id, status, model_count, connection_count, catalog_state, catalog_refreshed, disabled, configuration_issue` plus whatever `providerSnapshotRow` adds. Tile-level `status` becomes `"configured"` whenever `len(matches) > 0`.

- [ ] **Step 1: Write the failing test**

Create `go/internal/api/provider_instances_test.go`:

```go
package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// Two providers of one registry type must surface as two addressable instances,
// and the tile must not adopt either one's status as its own.
func TestStatusPayloadCarriesEachInstance(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Categories = map[string]*config.CategoryConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-prod": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global"},
			"vertex-dev":  {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-b", Location: "us-central1"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state status=%d", status)
	}

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing from provider_statuses")
	}

	instances, ok := tile["instances"].([]any)
	if !ok {
		t.Fatalf("instances missing or wrong type: %T", tile["instances"])
	}
	if len(instances) != 2 {
		t.Fatalf("instances=%d, want 2", len(instances))
	}

	seen := map[string]bool{}
	for _, raw := range instances {
		inst := raw.(map[string]any)
		id, _ := inst["id"].(string)
		seen[id] = true
		for _, field := range []string{"status", "model_count", "catalog_state", "disabled"} {
			if _, present := inst[field]; !present {
				t.Fatalf("instance %q missing %q", id, field)
			}
		}
	}
	if !seen["vertex-prod"] || !seen["vertex-dev"] {
		t.Fatalf("instance ids=%v, want vertex-prod and vertex-dev", seen)
	}

	// The tile aggregates; it is not itself an instance, so it must not wear an
	// instance's status. A healthy first instance must never mask a broken second.
	if got := tile["status"]; got != "configured" {
		t.Fatalf("tile status=%v, want \"configured\"", got)
	}
	counts, ok := tile["instance_status_counts"].(map[string]any)
	if !ok {
		t.Fatalf("instance_status_counts missing or wrong type: %T", tile["instance_status_counts"])
	}
	total := 0
	for _, v := range counts {
		total += int(v.(float64))
	}
	if total != 2 {
		t.Fatalf("instance_status_counts sums to %d, want 2", total)
	}
}

// The masking bug itself: with one sound instance and one broken instance under
// the same tile, the broken one must remain visible. Before this change the tile
// took matches[0].status, so whichever instance happened to sort first decided
// what the operator saw.
func TestBrokenInstanceIsNotMaskedByASoundOne(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	// vertex_ai without a project is a configuration error; with one it is not.
	// That gives two instances under one tile with genuinely different statuses.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Categories = map[string]*config.CategoryConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-sound":  {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global"},
			"vertex-broken": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing")
	}

	statusByInstance := map[string]string{}
	for _, raw := range tile["instances"].([]any) {
		inst := raw.(map[string]any)
		statusByInstance[stringOf(inst["id"])] = stringOf(inst["status"])
	}
	if len(statusByInstance) != 2 {
		t.Fatalf("instances=%v, want 2", statusByInstance)
	}
	broken := statusByInstance["vertex-broken"]
	sound := statusByInstance["vertex-sound"]
	if broken == "" {
		t.Fatal("vertex-broken carries no status of its own")
	}
	if broken == sound {
		t.Fatalf("both instances report %q; the broken one is indistinguishable", broken)
	}

	// The broken instance must be represented in the composition the tile
	// advertises, whatever order the instances happened to be built in.
	counts := tile["instance_status_counts"].(map[string]any)
	if _, present := counts[broken]; !present {
		t.Fatalf("instance_status_counts=%v omits the broken status %q", counts, broken)
	}

	// And the tile must not have adopted either instance's status as its own.
	if got := stringOf(tile["status"]); got == broken || got == sound {
		t.Fatalf("tile status=%q copies an instance status; want \"configured\"", got)
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Two misconfigured instances must keep their problems separate rather than
// being concatenated into one unreadable sentence.
func TestInstanceConfigurationIssuesStaySeparate(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	// vertex_ai without a project is a configuration error, so both instances
	// report an issue and we can prove they are not joined.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Categories = map[string]*config.CategoryConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-a": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
			"vertex-b": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] != "vertex_ai" {
			continue
		}
		for _, instRaw := range row["instances"].([]any) {
			inst := instRaw.(map[string]any)
			if _, present := inst["configuration_issue"]; !present {
				t.Fatalf("instance %v missing configuration_issue", inst["id"])
			}
		}
		return
	}
	t.Fatal("vertex_ai tile missing")
}
```

- [ ] **Step 2: Run the tests to verify they fail**

```bash
cd go && go test ./internal/api -run 'TestStatusPayloadCarriesEachInstance|TestBrokenInstanceIsNotMaskedByASoundOne|TestInstanceConfigurationIssuesStaySeparate' -v
```

Expected: all three FAIL — `instances missing or wrong type: <nil>`.

- [ ] **Step 3: Emit the instance rows**

In `go/internal/api/provider_status.go`, inside the `for _, entry := range providers.ProviderRegistry()` loop, add two accumulators alongside the existing ones (near `ids := make([]string, 0, len(matches))`):

```go
	instances := make([]map[string]any, 0, len(matches))
	instanceStatusCounts := map[string]int{}
```

Inside the existing `for _, match := range matches` body, after the current accumulation, append the per-instance row:

```go
		instanceStatusCounts[match.status]++
		instances = append(instances, providerSnapshotRow(map[string]any{
			"id": match.id, "status": match.status,
			"model_count": match.modelCount, "connection_count": match.connectionCount,
			"catalog_state": match.catalogState, "catalog_refreshed": match.catalogRefresh,
			"disabled": match.disabled, "configuration_issue": match.configurationIssue,
		}, match.lastCheck, match.lastVerify))
```

- [ ] **Step 4: Stop rolling one instance's status up to the tile**

Replace the tile status ladder so a configured tile reports only that it is configured:

```go
		status := "not_configured"
		if entry.ClientOnly {
			status = "client_setup"
		} else if entry.Availability != providers.ProviderAvailable {
			status = "unavailable"
		} else if len(matches) > 0 {
			// A tile aggregates instances; it is not one. Adopting matches[0]'s
			// status let a healthy first instance mask a broken second, so the
			// tile reports only that it is configured and the console renders
			// the composition from instance_status_counts.
			status = "configured"
		}
```

- [ ] **Step 5: Add both fields to the emitted row**

In the `providerSnapshotRow(map[string]any{...})` call for the tile, add:

```go
			"instances": instances, "instance_status_counts": instanceStatusCounts,
```

Leave `configuration_issue`'s existing `strings.Join(configurationIssues, " ")` in place — it stays as the tile-level summary; the per-instance value is now available separately.

- [ ] **Step 6: Run the tests to verify they pass**

```bash
cd go && go test ./internal/api -run 'TestStatusPayloadCarriesEachInstance|TestBrokenInstanceIsNotMaskedByASoundOne|TestInstanceConfigurationIssuesStaySeparate' -v
```

Expected: PASS (all three).

If `TestBrokenInstanceIsNotMaskedByASoundOne` fails on `both instances report …`, the
two fixture providers are not producing distinct statuses in this build — inspect what
`providerStatus` returns for each and adjust the fixture so one is genuinely broken and
one is not. Do **not** weaken the assertion to make it pass; the point of this test is
that a broken instance stays visible.

- [ ] **Step 7: Run the full Go gate**

```bash
cd go && go build ./... && go vet ./... && go test ./...
```

Expected: all pass. If another test asserted `status` equalled an instance status, update that assertion — the rollup was the bug.

- [ ] **Step 8: Commit**

```bash
git add go/internal/api/provider_status.go go/internal/api/provider_instances_test.go
git commit -m "feat(api): emit per-instance provider rows and drop the tile status rollup"
```

---

### Task 2: Provider detail table shows each instance's own data

**Files:**
- Modify: `go/internal/web/console/src/pages/ProviderDetail.tsx:146-149`
- Modify: `go/internal/web/console/tests/smoke.test.mjs`

**Interfaces:**
- Consumes: `entry.instances` from Task 1 — array of `{id, status, model_count, connection_count, catalog_state, catalog_refreshed, disabled, configuration_issue}`.
- Produces: nothing for later tasks.

The table already maps `providerIDs` and passes each id as the lifecycle override. Its cells render `entry.model_count` / `entry.catalog_state` / `entry.catalog_refreshed` — the **aggregate**, repeated identically on every row.

- [ ] **Step 1: Write the failing source assertion**

Append to `go/internal/web/console/tests/smoke.test.mjs`:

```javascript
test("provider detail rows render each instance's own catalog data", () => {
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  // Rows must iterate instance records, not bare ids paired with aggregate fields.
  assert.match(detail, /asList\(entry\.instances\)/);
  assert.doesNotMatch(detail, /<td>\{numberValue\(entry\.model_count\)\}/);
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd go/internal/web/console && npm test
```

Expected: FAIL — `assert.match` finds no `asList(entry.instances)`.

- [ ] **Step 3: Bind the table to instances**

Near the other derived values (around `const providerIDs = configuredProviderIDs(entry);`) add:

```tsx
  const instances = asList(entry.instances).map(asRecord);
```

Replace the row map so each row reads its own record. The per-row cells become:

```tsx
          {instances.map((instance) => {
            const providerID = stringValue(instance.id);
            return <tr key={providerID}>
              <td><strong class="technical">{providerID}</strong>{boolValue(instance.disabled) ? <small class="table-subtitle">Disabled — requests 404 until re-enabled</small> : null}</td>
              <td>{numberValue(instance.model_count)} model{numberValue(instance.model_count) === 1 ? "" : "s"} · {stringValue(instance.catalog_state, "unknown")}</td>
              <td class="technical">{stringValue(instance.catalog_refreshed, "Never synced")}</td>
```

Keep the existing actions `<td>` unchanged — it already passes `providerID` as the lifecycle override, and `providerID` is still in scope.

Remove the now-unused `disabledProviderIDs` set if nothing else references it (`tsc --noEmit` will tell you).

- [ ] **Step 4: Run the console gate**

```bash
cd go/internal/web/console && npm run lint && npm test
```

Expected: both pass.

- [ ] **Step 5: Rebuild dist and commit**

```bash
cd go/internal/web/console && npm run check:dist
cd ../../../.. && git add go/internal/web/console/src/pages/ProviderDetail.tsx go/internal/web/console/tests/smoke.test.mjs go/internal/web/console/dist
git commit -m "fix(console): show each provider instance's own catalog data"
```

If `check:dist` fails with a non-empty diff, that is the expected signal that `dist/` needs rebuilding — run `npm run build`, then re-add `dist/` and commit.

---

### Task 3: Tile renders instance composition instead of a rolled-up badge

**Files:**
- Modify: `go/internal/web/console/src/components/providers/ProviderHub.tsx:230`
- Modify: `go/internal/web/console/tests/smoke.test.mjs`

**Interfaces:**
- Consumes: `entry.instance_status_counts` (object of status → count) and `entry.instances` from Task 1.

- [ ] **Step 1: Write the failing source assertion**

```javascript
test("provider tiles show instance composition rather than one rolled-up status", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  assert.match(hub, /instance_status_counts/);
  assert.match(hub, /instance\{.*=== 1 \? "" : "s"\}/);
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd go/internal/web/console && npm test
```

Expected: FAIL.

- [ ] **Step 3: Render the composition**

In the card's `<dl>`, replace the `Connection` definition's neighbour by adding an instances row. Insert before the `Protocol` block:

```tsx
<div><dt>Instances</dt><dd>{instanceCount} instance{instanceCount === 1 ? "" : "s"}{compositionText ? ` · ${compositionText}` : ""}</dd></div>
```

and derive both above the `return`, inside the `groupEntries.map((entry) => {` body:

```tsx
          const statusCounts = asRecord(entry.instance_status_counts);
          const instanceCount = asList(entry.instances).length;
          // Composition, not a rollup: a tile is an aggregator, so it reports what
          // its instances are rather than adopting one instance's status.
          const compositionText = Object.entries(statusCounts)
            .map(([state, count]) => `${numberValue(count)} ${state}`)
            .join(" · ");
```

- [ ] **Step 4: Run the console gate**

```bash
cd go/internal/web/console && npm run lint && npm test
```

Expected: both pass.

- [ ] **Step 5: Rebuild dist and commit**

```bash
cd go/internal/web/console && npm run check:dist
cd ../../../.. && git add go/internal/web/console/src/components/providers/ProviderHub.tsx go/internal/web/console/tests/smoke.test.mjs go/internal/web/console/dist
git commit -m "feat(console): show provider instance composition on the tile"
```

---

### Task 4: Allow adding a second instance of a configured type

**Files:**
- Modify: `go/internal/web/console/src/components/providers/ProviderHub.tsx:33,78`
- Modify: `go/internal/web/console/tests/smoke.test.mjs`

**Interfaces:**
- Consumes: nothing new.
- Produces: `ConnectDialog` accepts an optional `mode` prop — `"create"` (default) or `"edit"`. In create mode the provider-ID field is editable and pre-filled with a non-colliding suggestion.

`disabled={configured}` is the hard stop: once one instance of a type exists, the tile's dialog pins the ID and can only edit.

- [ ] **Step 1: Write the failing source assertion**

```javascript
test("connect dialog allows creating an additional instance of a configured type", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // The ID field must lock only when editing an existing instance, never merely
  // because the type already has one.
  assert.doesNotMatch(hub, /disabled=\{configured\}/);
  assert.match(hub, /mode === "edit"/);
  assert.match(hub, /Add instance/);
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd go/internal/web/console && npm test
```

Expected: FAIL — `disabled={configured}` is still present.

- [ ] **Step 3: Add the mode prop and a non-colliding default id**

Change the `ConnectDialog` signature:

```tsx
export function ConnectDialog({ entry, onClose, onConfigured, mode = "create" }: { entry: JSONRecord; onClose: () => void; onConfigured: () => Promise<void>; mode?: "create" | "edit" }) {
```

Replace the `providerID` initial state so a create on an already-configured type suggests a free id:

```tsx
  const existingIDs = configuredProviderIDs(entry);
  const suggestedID = (() => {
    const base = stringValue(entry.default_provider_id, stringValue(entry.id));
    if (!existingIDs.includes(base)) return base;
    for (let n = 2; ; n += 1) {
      const candidate = `${base}-${n}`;
      if (!existingIDs.includes(candidate)) return candidate;
    }
  })();
  const [providerID, setProviderID] = useState(mode === "edit" ? stringValue(providerConfig.id) : suggestedID);
```

Change the ID input so it locks only when editing:

```tsx
<label>Gateway provider ID<input value={providerID} disabled={mode === "edit"} onInput={(event) => setProviderID((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>
```

Update the dialog heading and submit label to key off `mode` rather than `configured`:

```tsx
<h2 id="connect-provider-title">{mode === "edit" ? "Edit" : "Connect"} {label}</h2>
```

```tsx
{mode === "edit" ? "Save configuration" : "Connect provider"}
```

- [ ] **Step 4: Add the "Add instance" action to the card**

In the configured-admin branch of the card footer's `provider-actions`, add as the first button:

```tsx
<button class="button button--secondary" type="button" title="Configure another instance of this integration" onClick={() => connect(entry, "create")}><Plug size={15} /> Add instance</button>
```

Thread the mode through the state that opens the dialog. At `ProviderHub.tsx:135` the
entry is held in `const [connectEntry, setConnectEntry] = useState<JSONRecord | null>(null);`
— carry the mode with it rather than adding a parallel state that can drift:

```tsx
  const [connectEntry, setConnectEntry] = useState<{ entry: JSONRecord; mode: "create" | "edit" } | null>(null);
```

Update the `connect` helper at line 180 to take the mode, defaulting to create:

```tsx
  const connect = (entry: JSONRecord, mode: "create" | "edit" = "create") => {
```

and its `setConnectEntry` call near line 198:

```tsx
    setConnectEntry({ entry: { ...entry, provider_config: configuredProviderConfig(entry, data) }, mode });
```

and the render site at line 233:

```tsx
      {connectEntry ? <ConnectDialog entry={connectEntry.entry} mode={connectEntry.mode} onClose={() => setConnectEntry(null)} onConfigured={onChanged} /> : null}
```

The existing "Edit"-intent call site must now pass `"edit"` explicitly; every other
`connect(entry)` call keeps the create default. Run `tsc --noEmit` to confirm no call
site was missed.

- [ ] **Step 5: Run the console gate**

```bash
cd go/internal/web/console && npm run lint && npm test
```

Expected: both pass.

- [ ] **Step 6: Rebuild dist and commit**

```bash
cd go/internal/web/console && npm run check:dist
cd ../../../.. && git add go/internal/web/console/src/components/providers/ProviderHub.tsx go/internal/web/console/tests/smoke.test.mjs go/internal/web/console/dist
git commit -m "feat(console): allow adding another instance of a configured provider type"
```

---

### Task 5: Credentials and lifecycle target an explicit instance

**Files:**
- Modify: `go/internal/web/console/src/components/providers/shared.ts:79`
- Modify: `go/internal/web/console/src/components/providers/ProviderHub.tsx:96`
- Modify: `go/internal/web/console/tests/smoke.test.mjs`

**Interfaces:**
- Consumes: `entry.instances`.
- Produces: `PrivateAPIKeyDialog` takes a required `providerID: string` prop instead of resolving `[0]` internally. `runLifecycle`'s `providerIDOverride` becomes required for multi-instance tiles.

Two silent-wrong-target bugs: a credential always lands on the first instance, and grid-level Test/Sync/**Remove** act on the first instance — so "Remove" on a two-instance tile deletes one and leaves the other.

- [ ] **Step 1: Write the failing source assertion**

```javascript
test("credential and lifecycle actions name an explicit instance", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.ts"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // The dialog must be TOLD its target. A caller that has already established
  // the tile has exactly one instance may still index [0] at the call site —
  // what must not survive is the dialog silently choosing for itself.
  assert.match(hub, /function PrivateAPIKeyDialog\(\{ entry, providerID/);
  assert.doesNotMatch(hub, /const providerID = configuredProviderIDs\(entry\)\[0\]/);
  assert.doesNotMatch(shared, /providerIDs\[0\] \?\? stringValue\(entry\.id\)/);
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd go/internal/web/console && npm test
```

Expected: FAIL — both `[0]` patterns are present.

- [ ] **Step 3: Make the credential dialog take its target**

```tsx
export function PrivateAPIKeyDialog({ entry, providerID, onClose, onConfigured }: { entry: JSONRecord; providerID: string; onClose: () => void; onConfigured: () => Promise<void> }) {
```

Delete the internal `const providerID = configuredProviderIDs(entry)[0] ?? "";` line.

The dialog is opened from `privateKeyEntry` state, which currently holds only the entry.
Carry the target id with it, the same way Task 4 carries the mode:

```tsx
  const [privateKeyEntry, setPrivateKeyEntry] = useState<{ entry: JSONRecord; providerID: string } | null>(null);
```

and at the render site (`ProviderHub.tsx:234`):

```tsx
      {privateKeyEntry ? <PrivateAPIKeyDialog entry={privateKeyEntry.entry} providerID={privateKeyEntry.providerID} onClose={() => setPrivateKeyEntry(null)} onConfigured={onChanged} /> : null}
```

Every site that opens this dialog must now name the instance it is acting on. Where the
tile has exactly one instance, pass `configuredProviderIDs(entry)[0]` **at the call
site** — that is a legitimate use, because the caller has established there is only one.
Where the tile has several, the action belongs on the detail page's per-instance row,
which already knows its own id. Run `tsc --noEmit` to confirm no call site was missed.

- [ ] **Step 4: Make lifecycle refuse an ambiguous target**

In `shared.ts`, change the resolution so a multi-instance tile cannot silently pick one:

```ts
  const runLifecycle = async (entry: JSONRecord, operation: LifecycleOperation, providerIDOverride?: string, model?: string) => {
    const providerIDs = configuredProviderIDs(entry);
    // Never guess which instance the operator meant: acting on [0] silently
    // tested, synced, or DELETED the wrong upstream on a multi-instance tile.
    const providerID = providerIDOverride ?? (providerIDs.length === 1 ? providerIDs[0] : "");
    if (!providerID) return;
```

- [ ] **Step 5: Route grid actions through the detail page for multi-instance tiles**

In `ProviderHub.tsx`, the card's Test / Sync / Remove buttons must not fire without a target. Where `instanceCount > 1`, render a single button opening the detail page instead:

```tsx
{instanceCount > 1
  ? <button class="button button--secondary" type="button" onClick={(event) => openDetail(event, id)}>Manage {instanceCount} instances</button>
  : <>{/* existing Check / Sync / Remove buttons, unchanged */}</>}
```

- [ ] **Step 6: Run the console gate**

```bash
cd go/internal/web/console && npm run lint && npm test
```

Expected: both pass.

- [ ] **Step 7: Rebuild dist and commit**

```bash
cd go/internal/web/console && npm run check:dist
cd ../../../.. && git add go/internal/web/console/src go/internal/web/console/tests/smoke.test.mjs go/internal/web/console/dist
git commit -m "fix(console): target an explicit instance for credential and lifecycle actions"
```

---

### Task 6: Instances whose id differs from the registry id stay under their tile

**Files:**
- Modify: `go/internal/web/console/src/components/providers/ProviderHub.tsx:152-160`
- Modify: `go/internal/web/console/tests/smoke.test.mjs`

**Interfaces:**
- Consumes: `provider_statuses` rows keyed by registry id.

`statusByID.get(stringValue(entry.id))` merges status onto a curated tile by **registry id**. Any configured instance named something else (`vertex-prod`) fails that merge and reappears as an orphan "custom" tile — which is exactly what Task 4 now lets operators create.

- [ ] **Step 1: Write the failing source assertion**

```javascript
test("configured instances are not orphaned into custom tiles by their id", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // Custom tiles are those with no registry entry of their own — not merely
  // those whose provider id differs from the registry id.
  assert.doesNotMatch(hub, /statuses\.filter\(\(status\) => !registryIDs\.has\(stringValue\(status\.id\)\)\)/);
  assert.match(hub, /registry_id/);
});
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd go/internal/web/console && npm test
```

Expected: FAIL.

- [ ] **Step 3: Key the merge on registry id, not tile id**

```tsx
  const entries = useMemo(() => {
    const registryIDs = new Set(registry.map((entry) => stringValue(entry.id)));
    const curated = registry.map((entry) => ({
      ...entry,
      ...statusByID.get(stringValue(entry.id)),
    }));
    // A tile owns every configured instance that claims its registry id. Keying
    // "custom" on the provider id instead orphaned any instance not named after
    // its registry entry — which is every second instance an operator adds.
    const custom = statuses.filter((status) => {
      const claimed = stringValue(status.registry_id, stringValue(status.id));
      return !registryIDs.has(claimed);
    });
    return [...curated, ...custom];
  }, [data.provider_registry, data.provider_statuses]);
```

- [ ] **Step 4: Confirm the server sends `registry_id` on custom rows**

```bash
cd go && grep -n 'registryID\|"registry_id"' internal/api/provider_status.go | sed -n '1,20p'
```

If the non-registry snapshot loop (around `for _, snapshot := range configured`) does not already emit `"registry_id": snapshot.registryID`, add it to that row, then re-run `go test ./internal/api/...`.

- [ ] **Step 5: Run both gates**

```bash
cd go && go build ./... && go vet ./... && go test ./...
cd internal/web/console && npm run lint && npm test
```

Expected: all pass.

- [ ] **Step 6: Rebuild dist and commit**

```bash
cd go/internal/web/console && npm run check:dist
cd ../../../.. && git add go/internal/api/provider_status.go go/internal/web/console/src go/internal/web/console/tests/smoke.test.mjs go/internal/web/console/dist
git commit -m "fix(console): keep additional instances under their registry tile"
```

---

### Task 7: Live verification

**Files:** none — this task produces evidence, not code.

This is where Tasks 2–6 actually get proven, because the console test tooling cannot execute behaviour.

- [ ] **Step 1: Build and run locally**

```bash
cd go && go build -o ../llmgw-local ./cmd/llmgw && LLMGW_CONFIG=./config.local.yaml ../llmgw-local
```

- [ ] **Step 2: Verify each fix against the running console**

Confirm, in order:
1. A configured provider tile shows an **Add instance** action.
2. Adding a second instance of the same type succeeds and the ID field is editable, pre-filled with a non-colliding suggestion.
3. Both instances appear under **one** tile — not one tile plus an orphan "custom" tile.
4. The tile shows a composition (`2 instances · …`), not a single status badge.
5. The detail table shows **different** model counts per instance where the upstreams differ.
6. Adding a credential to the second instance attaches it to that instance, not the first.
7. On a two-instance tile the grid offers **Manage N instances** rather than Test/Sync/Remove.

- [ ] **Step 3: Record the result**

Note any step that fails, with what was observed. A failure here means a task's implementation is wrong even though its assertions passed — fix it before moving to workstream B.

- [ ] **Step 4: Final gate before handing off**

```bash
cd go && go build ./... && go vet ./... && go test ./...
cd internal/web/console && npm ci && npm run lint && npm test && npm run check:dist
git log -1 --format='%an <%ae> / %cn <%ce>'
```

Confirm the identity line is the intended public one before this reaches the PR.
