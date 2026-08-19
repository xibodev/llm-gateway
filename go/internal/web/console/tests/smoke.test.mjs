import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import test from "node:test";

const root = resolve(import.meta.dirname, "..");

test("console source keeps its local asset base", () => {
  const vite = readFileSync(resolve(root, "vite.config.ts"), "utf8");
  assert.match(vite, /base: "\/console\/"/);
});

test("console styles retain the approved neutral enterprise direction", () => {
  const styles = readFileSync(resolve(root, "src/styles/base.css"), "utf8").toLowerCase();
  for (const forbidden of ["linear-gradient", "radial-gradient", "neon", "glow"]) {
    assert.equal(styles.includes(forbidden), false, `unexpected ${forbidden}`);
  }
});

test("console provides local light and dark themes without changing the professional direction", () => {
  const index = readFileSync(resolve(root, "index.html"), "utf8");
  const shell = readFileSync(resolve(root, "src/components/AppShell.tsx"), "utf8");
  const styles = readFileSync(resolve(root, "src/styles/base.css"), "utf8");
  assert.match(index, /color-scheme" content="light dark"/);
  assert.match(index, /llmgw\.console\.theme/);
  assert.match(shell, /Moon/);
  assert.match(shell, /Sun/);
  assert.match(shell, /Use light theme/);
  assert.match(shell, /Use dark theme/);
  assert.match(styles, /:root\[data-theme="dark"\]/);
});

test("mode-aware API client names only local API roots", () => {
  const mode = readFileSync(resolve(root, "src/lib/mode.ts"), "utf8");
  assert.match(mode, /"\/user\/api"/);
  assert.match(mode, /"\/admin\/api"/);
});


test("revoked keys have no console enable action", () => {
  const keys = readFileSync(resolve(root, "src/pages/ApiKeys.tsx"), "utf8");
  assert.match(keys, /const revoked = status === "revoked"/);
  assert.match(keys, /Permanently revoked/);
  assert.match(keys, /!revoked \? <button class="button button--secondary"/);
});


test("static admin console authentication is session-only and admin-scoped", () => {
  const api = readFileSync(resolve(root, "src/lib/api.ts"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const shell = readFileSync(resolve(root, "src/components/AppShell.tsx"), "utf8");
  assert.match(api, /sessionStorage/);
  assert.doesNotMatch(api, /localStorage/);
  assert.match(api, /mode === "admin"/);
  assert.match(api, /Authorization/);
  assert.match(app, /Administrator sign-in required/);
  assert.match(app, /cause.status === 401/);
  assert.match(app, /Invalid administrator key/);
  assert.match(app, /clearStaticAdminKey\(\)/);
  assert.match(shell, /Sign out/);
});

test("console redirects browser-auth failures to the correct protected entrypoint", () => {
  const api = readFileSync(resolve(root, "src/lib/api.ts"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  assert.match(api, /authentication_redirect/);
  assert.match(api, /response\.type === "opaqueredirect"/);
  assert.match(api, /redirect: init\.redirect \?\? "manual"/);
  assert.match(api, /startBrowserAuthentication\(mode\)/);
  assert.match(api, /finalURL\.origin !== window\.location\.origin/);
  assert.match(api, /unexpected non-JSON response/);
  assert.match(app, /cause\.code === authenticationRedirectCode/);
  assert.match(app, /mode === "portal" \? "\/portal" : "\/admin"/);
});

test("portal shell shows the signed-in human without exposing identity controls to admin mode", () => {
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const shell = readFileSync(resolve(root, "src/components/AppShell.tsx"), "utf8");
  assert.match(app, /data\?\.principal/);
  assert.match(app, /identityName=\{identityName\}/);
  assert.match(shell, /Signed in as \$\{identityName\}/);
  assert.match(shell, /mode === "portal" && identityName/);
});

test("all modal dialogs share focus trapping, Escape close, and focus return", () => {
  const focus = readFileSync(resolve(root, "src/components/useDialogFocus.ts"), "utf8");
  const oauth = readFileSync(resolve(root, "src/components/providers/OAuthConnectDialog.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const keys = readFileSync(resolve(root, "src/pages/ApiKeys.tsx"), "utf8");
  assert.match(focus, /event.key === "Escape"/);
  assert.match(focus, /event.key !== "Tab"/);
  assert.match(focus, /previousFocus\.focus\(\)/);
  assert.match(oauth, /useDialogFocus\(closeDialog\)/);
  assert.equal((hub.match(/useDialogFocus\(onClose\)/g) ?? []).length, 2);
  assert.match(keys, /useDialogFocus\(onDismiss, returnFocus\)/);
  assert.match(keys, /returnFocus=\{createButtonRef\.current\}/);
});

test("admin console exposes complete alert-rule management", () => {
  const navigation = readFileSync(resolve(root, "src/lib/navigation.ts"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const alerts = readFileSync(resolve(root, "src/pages/Alerts.tsx"), "utf8");
  assert.match(navigation, /id: "alerts"/);
  assert.doesNotMatch(navigation, /\["overview", "providers", "models", "playground", "keys", "usage", "alerts"/);
  assert.match(app, /case "alerts": content = <Alerts/);
  assert.match(alerts, /"\/alerts\/status"/);
  assert.match(alerts, /"\/alerts\/evaluate"/);
  assert.match(alerts, /`\/alerts\/\$\{encodeURIComponent\(id\)\}`/);
  assert.match(alerts, /Create rule/);
});


test("portal provider actions use the current principal private connections", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  assert.match(hub, /data\.provider_connections/);
  assert.match(hub, /hasPrivateConnection/);
  assert.match(hub, /Add or replace account/);
  assert.match(hub, /Administrator setup required/);
  assert.match(hub, /supportsOAuth/);
});

test("provider hub includes configured advanced providers outside the curated registry", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  assert.match(hub, /const registryIDs = new Set/);
  assert.match(hub, /statuses\.filter\(\(status\) => \{/);
  assert.match(hub, /!registryIDs\.has\(claimed\)/);
  assert.match(hub, /return \[\.\.\.curated, \.\.\.custom\]/);
});

test("configured instances are not orphaned into custom tiles by their id", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // Custom tiles are those with no registry entry of their own — not merely
  // those whose provider id differs from the registry id.
  assert.doesNotMatch(hub, /statuses\.filter\(\(status\) => !registryIDs\.has\(stringValue\(status\.id\)\)\)/);
  assert.match(hub, /registry_id/);
});

test("overview next step is state-aware and navigates to the recommended workflow", () => {
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const overview = readFileSync(resolve(root, "src/pages/Overview.tsx"), "utf8");
  assert.match(app, /<Overview data=\{data\} mode=\{mode\} onNavigate=\{navigate\}/);
  assert.match(overview, /function nextStepFor/);
  assert.match(overview, /title: "Review provider readiness"/);
  assert.match(overview, /title: "Create a fallback route"/);
  assert.match(overview, /title: "Create an API key"/);
  assert.match(overview, /title: "Run a gateway request"/);
  assert.match(overview, /onClick=\{\(\) => onNavigate\(nextStep\.page\)\}/);
  assert.match(overview, /Review providers/);
});

test("official OAuth onboarding auto-opens, polls, tests, and reports the result", () => {
  const oauth = readFileSync(
    resolve(root, "src/components/providers/OAuthConnectDialog.tsx"),
    "utf8",
  );
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  assert.match(oauth, /window\.open\("about:blank"/);
  assert.match(oauth, /window\.setTimeout/);
  assert.match(oauth, /Automatic check/);
  assert.match(oauth, /testConnection/);
  assert.match(oauth, /Connected and tested/);
  assert.match(oauth, /Try in playground/);
  assert.match(oauth, /const closePopup = useCallback/);
  assert.match(oauth, /useEffect\(\(\) => closePopup/);
  assert.match(oauth, /closePopup\(\);\s+setStage\("error"\)/);
  assert.match(oauth, /let refreshed = true/);
  assert.match(oauth, /Reload the console to refresh the provider card/);
  assert.doesNotMatch(oauth, /> Check authorization</);
  assert.match(app, /const refresh = \(\) => load\(false\)/);
  assert.match(app, /<Providers data=\{data\} mode=\{mode\} detail=\{route.detail\} onChanged=\{refresh\} onNavigate=\{navigate\}/);
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  assert.match(hub, /Add account/);
  assert.match(hub, /Add or replace account/);
});


test("admin catalog actions select an owner principal", () => {
  const routes = readFileSync(resolve(root, "src/pages/Routes.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  assert.match(routes, /principal_id/);
  assert.match(routes, /Catalog owner/);
  assert.match(shared, /principal_id/);
  assert.match(hub, /useProviderLifecycle\(ownerID, onChanged\)/);
  assert.match(hub, /Catalog owner/);
  assert.match(hub, /supportsOAuth && !ownerID/);
});

test("provider onboarding renders required setup fields and editable configuration", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  const oauth = readFileSync(resolve(root, "src/components/providers/OAuthConnectDialog.tsx"), "utf8");
  const styles = readFileSync(resolve(root, "src/styles/base.css"), "utf8");
  assert.match(hub, /Google Cloud project ID/);
  assert.match(hub, /fields\.has\("location"\)/);
  assert.match(hub, /leave blank to keep the current key/);
  assert.match(detail, /Edit configuration/);
  assert.match(oauth, /OAuth client ID/);
  assert.match(oauth, /client_id: clientID\.trim\(\)/);
  assert.match(styles, /\.playground-settings \{ position: relative; z-index: 10; padding: 0; overflow: visible; \}/);
  assert.match(styles, /\.playground-settings select, \.playground-settings input, \.model-filters select, \.model-filters input, \.model-combo input \{ min-height: 44px; \}/);
});

test("admin model catalogs stay scoped to the selected principal", () => {
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  const models = readFileSync(resolve(root, "src/pages/ModelsEndpoints.tsx"), "utf8");
  assert.match(models, /principal_id=\$\{encodeURIComponent\(principalID\)\}/);
  assert.match(playground, /query\.set\("principal_id", scopedPrincipalID\)/);
  assert.match(playground, /project_id: projectID/);
  assert.match(playground, /eligibleProjects/);
  assert.match(playground, /\[catalogPath\]/);
  assert.match(models, /Catalog owner/);
  assert.match(app, /catalogPrincipalID/);
  assert.match(app, /<ModelsEndpoints.*principalID=\{catalogPrincipalID\}/);
  assert.match(app, /<Playground.*principalID=\{catalogPrincipalID\}/);
});

test("access page manages principals, projects, and memberships over the IAM API", () => {
  const access = readFileSync(resolve(root, "src/pages/Access.tsx"), "utf8");
  const navigation = readFileSync(resolve(root, "src/lib/navigation.ts"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  assert.match(navigation, /id: "access"/);
  assert.match(app, /case "access": content = <Access/);
  assert.match(access, /"\/principals"/);
  assert.match(access, /"\/memberships"/);
  assert.match(access, /\/\$\{target\}\/status/);
  assert.match(access, /option value="human"/);
  assert.match(access, /option value="owner"/);
  assert.match(access, /useDialogFocus\(onClose\)/);
});

test("access page is admin-only and excluded from the portal shell", () => {
  const navigation = readFileSync(resolve(root, "src/lib/navigation.ts"), "utf8");
  const access = readFileSync(resolve(root, "src/pages/Access.tsx"), "utf8");
  assert.doesNotMatch(navigation, /\["overview", "providers", "models", "playground", "keys", "access"/);
  assert.match(access, /mode !== "admin"/);
  assert.match(access, /Administrator area/);
});

test("access page protects the built-in system principal from status changes", () => {
  const access = readFileSync(resolve(root, "src/pages/Access.tsx"), "utf8");
  assert.match(access, /stringValue\(principal.kind\) === "system"/);
  assert.match(access, /Built-in/);
});

test("console routes support a detail segment and provider detail pages", () => {
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  const providers = readFileSync(resolve(root, "src/pages/Providers.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  assert.match(app, /function routeFromHash/);
  assert.match(app, /decodeURIComponent\(rest.join\("\/"\)\)/);
  assert.match(app, /const navigate = \(next: PageID, detail\?: string\)/);
  assert.match(providers, /<ProviderDetail entryID=\{detail\}/);
  assert.match(detail, /Back to providers|Providers<\/button>/);
  assert.match(detail, /Models from this provider/);
  assert.match(detail, /Private connections/);
});

test("provider cards and overview metrics navigate to their destinations", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const overview = readFileSync(resolve(root, "src/pages/Overview.tsx"), "utf8");
  assert.match(hub, /provider-card--clickable/);
  assert.match(hub, /closest\("button, a, select, input, label"\)/);
  assert.match(hub, /onKeyDown/);
  assert.match(overview, /metric-card--clickable/);
  assert.match(overview, /onOpen=\{\(\) => onNavigate\("providers"\)\}/);
  assert.match(overview, /onOpen=\{\(\) => onNavigate\("keys"\)\}/);
});

test("provider detail scopes its catalog to the selected owner and explains empty states", () => {
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  assert.match(detail, /principal_id=\$\{encodeURIComponent\(ownerID\)\}/);
  assert.match(detail, /Select a catalog owner/);
  assert.match(detail, /create one on the Access page/);
  assert.match(detail, /No models synced yet/);
});

test("provider status vocabulary distinguishes configured, synced, and verified", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  assert.match(shared, /configured: "Configured · unverified"/);
  assert.match(shared, /catalog_synced: "Catalog synced"/);
  assert.match(shared, /verified: "Verified"/);
  assert.match(shared, /check_failed: "Check failed"/);
  assert.doesNotMatch(shared, /ready: "Ready"/);
});

test("lifecycle verbs say what they do and verify runs a real completion", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  assert.match(shared, /test: "reachability check"/);
  assert.match(shared, /repair: "cache reset"/);
  assert.match(shared, /verify: "test completion"/);
  assert.match(hub, /Check reachability/);
  assert.match(hub, /Sync catalog/);
  assert.doesNotMatch(hub, />\s*Repair</);
  assert.match(detail, /Run test completion/);
  assert.match(detail, /Clear cache &amp; retry/);
  assert.match(detail, /cannot fix a wrong credential or endpoint|cannot repair credentials or endpoints/);
  assert.match(detail, /Never — run a test completion/);
});

test("routes render as clickable tiles with a detail page and end-to-end test runner", () => {
  const routes = readFileSync(resolve(root, "src/pages/Routes.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/RouteDetail.tsx"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  assert.match(app, /<Routes data=\{data\} mode=\{mode\} detail=\{route.detail\}/);
  assert.match(routes, /route-list--grid/);
  assert.match(routes, /<RouteDetail routeName=\{detail\}/);
  assert.match(routes, /closest\("button, a, select, input, label"\)/);
  assert.match(detail, /"\/playground"/);
  assert.match(detail, /model: routeName/);
  assert.match(detail, /Fallback trace/);
  assert.match(detail, /Create them on the Access page first/);
});

test("settings page has real project policy controls instead of shell copy", () => {
  const settings = readFileSync(resolve(root, "src/pages/Settings.tsx"), "utf8");
  assert.match(settings, /ProjectPolicyEditor/);
  assert.match(settings, /\/policy`/);
  assert.match(settings, /allowed_models/);
  assert.match(settings, /Save project policy/);
  assert.match(settings, /managed on the Access page/);
  assert.match(settings, /onNavigate\("access"\)/);
  assert.doesNotMatch(settings, /grouped here rather than mixed into provider and routing workflows/);
});

test("overview shows an evidence-driven first-run guide for admins", () => {
  const guide = readFileSync(resolve(root, "src/components/GetStartedGuide.tsx"), "utf8");
  const overview = readFileSync(resolve(root, "src/pages/Overview.tsx"), "utf8");
  assert.match(overview, /<GetStartedGuide data=\{data\} onNavigate=\{onNavigate\}/);
  assert.match(overview, /\{!isPortal \? <GetStartedGuide/);
  assert.match(guide, /Create a human owner/);
  assert.match(guide, /hasProjectOwner/);
  assert.match(guide, /role === "owner" \|\| role === "admin"/);
  assert.match(guide, /activeHumanIDs\.has/);
  assert.match(guide, /activeProjectIDs\.has/);
  assert.match(guide, /Verify inference with a test completion/);
  assert.match(guide, /Mint a project API key/);
  assert.match(guide, /last_verified_at/);
  assert.match(guide, /localStorage/);
  assert.match(guide, /llmgw\.console\.setup-guide-dismissed/);
});

test("providers can be disabled and re-enabled without losing configuration", () => {
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  assert.match(detail, /\/enabled`/);
  assert.match(detail, /disabled_provider_ids/);
  assert.match(detail, /\{disabledProviderIDs.has\(providerID\) \? "Enable" : "Disable"\}/);
  assert.match(detail, /without deleting its configuration or credentials/);
  assert.match(shared, /disabled: "Disabled"/);
});

test("key creation exposes the acting principal so OAuth-backed keys work", () => {
  const keys = readFileSync(resolve(root, "src/pages/ApiKeys.tsx"), "utf8");
  assert.match(keys, /Acts as/);
  assert.match(keys, /payload.principal_id = principalID/);
  assert.match(keys, /New service identity \(shared credentials only\)/);
  assert.match(keys, /inherits that human's private provider connections/);
  assert.match(keys, /Copilot or Codex OAuth subscription/);
});

test("model pickers cascade provider then capability before the model list", () => {
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  const models = readFileSync(resolve(root, "src/pages/ModelsEndpoints.tsx"), "utf8");
  const routes = readFileSync(resolve(root, "src/pages/Routes.tsx"), "utf8");
  assert.match(picker, /export function ModelFilters/);
  assert.match(picker, /All providers/);
  assert.match(picker, /All capabilities/);
  assert.match(picker, /capability: "all"/);
  assert.match(models, /<ModelFilters models=\{models\}/);
  assert.match(routes, /<ModelFilters models=\{memberModels\}/);
  assert.match(routes, /includeCategories=\{false\}/);
});

test("capability detection reads catalog metadata and refuses to fake chat", () => {
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  assert.match(picker, /export function capabilitiesFor/);
  assert.match(picker, /\/audio\/speech/);
  assert.match(picker, /\/audio\/transcriptions/);
  assert.match(picker, /out.delete\("chat"\)/);
});

test("playground is a chat surface with multi-turn history", () => {
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  assert.match(playground, /function ChatThread/);
  assert.match(playground, /chat-composer/);
  assert.match(playground, /Enter to send, Shift\+Enter/);
  assert.match(playground, /event.key !== "Enter" \|\| event.shiftKey/);
  assert.match(playground, /sendChat\(event, \(event.currentTarget as HTMLTextAreaElement\).value\)/);
  assert.match(playground, /messages: history.map/);
  assert.match(playground, /Clear conversation/);
  assert.match(playground, /Raw gateway response/);
});

test("playground switches surface by model capability", () => {
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  assert.match(playground, /function modeFor/);
  assert.match(playground, /"\/playground\/speech"/);
  assert.match(playground, /"\/playground\/transcription"/);
  assert.match(playground, /surface === "tts"/);
  assert.match(playground, /surface === "transcription"/);
  assert.match(playground, /<audio controls src=\{audioURL\}/);
  assert.match(playground, /playground-settings__toggle/);
  assert.match(playground, /accept="audio\/\*"/);
});

test("form data uploads keep their own multipart boundary", () => {
  const api = readFileSync(resolve(root, "src/lib/api.ts"), "utf8");
  assert.match(api, /!\(init.body instanceof FormData\)/);
});

test("long catalogs paginate instead of dumping every row", () => {
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  assert.match(picker, /export function Pager/);
  assert.match(picker, /export const defaultPageSize = 20/);
  assert.match(detail, /<Pager total=\{matchedModels.length\}/);
  assert.match(detail, /pagedModels.map/);
});

test("model selection is a type-ahead, not a list of hundreds", () => {
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  assert.match(picker, /export function ModelCombo/);
  assert.match(picker, /Type to search models/);
  assert.match(picker, /event.key === "ArrowDown"/);
  assert.match(playground, /<ModelCombo models=\{voiceModels\}/);
});

test("provider models hand off to the playground and back", () => {
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  const providers = readFileSync(resolve(root, "src/pages/Providers.tsx"), "utf8");
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  const app = readFileSync(resolve(root, "src/App.tsx"), "utf8");
  assert.match(detail, /onOpenPlayground\(stringValue\(row.id\)\)/);
  assert.match(providers, /onNavigate\("playground", modelID\)/);
  assert.match(app, /preset=\{route.detail\}/);
  assert.match(playground, /Back to provider/);
});

test("speech surface offers a language filter over locale-keyed voices", () => {
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  assert.match(playground, /function localeOf/);
  assert.match(playground, /All languages/);
  assert.match(playground, /voiceModels/);
});

test("setup guide points each step at the provider that still needs it", () => {
  const guide = readFileSync(resolve(root, "src/components/GetStartedGuide.tsx"), "utf8");
  assert.match(guide, /needsCatalog/);
  assert.match(guide, /needsVerify/);
  assert.doesNotMatch(guide, /firstConfigured/);
});

test("playground exercises image and video as their own surfaces", () => {
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  assert.match(picker, /image: "Image generation"/);
  assert.match(picker, /video: "Video generation"/);
  assert.match(picker, /\/images\/generations/);
  assert.match(picker, /\/videos\/generations/);
  assert.match(playground, /surface === "image"/);
  assert.match(playground, /surface === "video"/);
  assert.match(playground, /"\/playground\/image"/);
  assert.match(playground, /"\/playground\/video"/);
  assert.match(playground, /<img src=\{imageURL\}/);
  assert.match(playground, /<video controls src=\{videoURL\}/);
});

test("video polling is cancellable and does not block the request", () => {
  const playground = readFileSync(resolve(root, "src/pages/Playground.tsx"), "utf8");
  assert.match(playground, /const attempt = \+\+videoPoll.current/);
  assert.match(playground, /if \(attempt !== videoPoll.current\) return/);
  assert.match(playground, /operation/);
  assert.match(playground, /navigating away stops the polling but not the job/);
});

test("generation-only models never offer a chat composer", () => {
  const picker = readFileSync(resolve(root, "src/components/ModelPicker.tsx"), "utf8");
  assert.match(picker, /out.has\("image"\) \|\| out.has\("video"\)/);
  assert.match(picker, /out.delete\("chat"\)/);
});

test("provider detail rows render each instance's own catalog data", () => {
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  // Rows must iterate instance records, not bare ids paired with aggregate fields.
  assert.match(detail, /asList\(entry\.instances\)/);
  assert.doesNotMatch(detail, /<td>\{numberValue\(entry\.model_count\)\}/);
});

test("provider tiles show instance composition rather than one rolled-up status", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  assert.match(hub, /instance_status_counts/);
  assert.match(hub, /instance\{.*=== 1 \? "" : "s"\}/);
});

test("credential and lifecycle actions name an explicit instance", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // The dialog must be TOLD its target. A caller that has already established
  // the tile has exactly one instance may still index [0] at the call site —
  // what must not survive is the dialog silently choosing for itself.
  assert.match(hub, /function PrivateAPIKeyDialog\(\{ entry, providerID/);
  assert.doesNotMatch(hub, /const providerID = configuredProviderIDs\(entry\)\[0\]/);
  assert.doesNotMatch(shared, /providerIDs\[0\] \?\? stringValue\(entry\.id\)/);
});

test("official OAuth account binding also names an explicit instance", () => {
  const oauth = readFileSync(resolve(root, "src/components/providers/OAuthConnectDialog.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  // Same rule as the private-key dialog: OAuthConnectDialog must be TOLD its
  // target. It has no legitimate reason to resolve configuredProviderIDs
  // itself — every caller passes a resolved providerID prop instead.
  assert.match(oauth, /function OAuthConnectDialog\(\{\s*entry,\s*providerID,/);
  assert.doesNotMatch(oauth, /configuredProviderIDs/);
  assert.match(hub, /<OAuthConnectDialog entry=\{oauthEntry\.entry\} providerID=\{oauthEntry\.providerID\}/);
  assert.match(detail, /<OAuthConnectDialog entry=\{entry\} providerID=\{oauthProviderID\}/);
});

test("connect dialog allows creating an additional instance of a configured type", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // The ID field must lock only when editing an existing instance, never merely
  // because the type already has one.
  assert.doesNotMatch(hub, /disabled=\{configured\}/);
  assert.match(hub, /mode === "edit"/);
  assert.match(hub, /Add instance/);
});

test("instance identity is never inferred from a bare configured flag", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  // Every configured row — curated tile or custom provider — carries the ids of
  // its instances. The fallback that invented the tile id as an instance made
  // configuredProviderIDs and entry.instances disagree, so the lifecycle table
  // rendered no rows at all for a custom provider.
  assert.doesNotMatch(shared, /boolValue\(entry\.configured\)/);
  assert.match(shared, /return asList\(entry\.configured_provider_ids\)/);
});

test("a suggested instance id never names a provider configured elsewhere", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  // The create route is an upsert keyed on the id alone, so a default that is
  // merely free under this tile can still overwrite another tile's provider.
  assert.match(hub, /takenIDs = \[\]/);
  assert.match(hub, /const taken = new Set\(\[\.\.\.configuredProviderIDs\(entry\), \.\.\.takenIDs\]/);
  assert.match(hub, /takenIDs=\{allProviderIDs\}/);
  assert.match(detail, /takenIDs=\{allProviderIDs\}/);
  assert.doesNotMatch(hub, /existingIDs/);
});

test("add instance appears only where a credential-based create can succeed", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // Gate on capability, not on a hardcoded list of ids: an OAuth-only tile fell
  // through to the account flow and a custom tile posts its provider id as a
  // registry id, which the server answers with a 400.
  assert.match(hub, /const CREDENTIAL_AUTH_METHODS = new Set/);
  assert.match(hub, /const canAddInstance = registryIDs\.has\(id\) && methods\.some/);
  assert.match(hub, /\{canAddInstance \? <button[^>]*title="Configure another instance/);
});

test("the create dialog never claims a key from another instance", () => {
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  // provider_config carries the first configured instance's record. A new
  // instance has no stored key, so the "keep the current key" affordance and
  // the guard it disables belong to edit mode only.
  assert.match(hub, /const apiKeySet = mode === "edit" && boolValue\(providerConfig\.api_key_set\)/);
});

test("per-instance status is rendered and the tile pill follows the composition", () => {
  const shared = readFileSync(resolve(root, "src/components/providers/shared.tsx"), "utf8");
  const hub = readFileSync(resolve(root, "src/components/providers/ProviderHub.tsx"), "utf8");
  const detail = readFileSync(resolve(root, "src/pages/ProviderDetail.tsx"), "utf8");
  // The server emits every instance's own status; nothing read it, so a tile
  // whose instance had failed its check wore the same grey pill as a verified
  // one and the operator had no way to find out which instance failed.
  assert.match(detail, /<th>Status<\/th>/);
  assert.match(detail, /<StatusBadge status=\{stringValue\(instance\.status/);
  assert.match(shared, /export function tileStatus/);
  assert.match(shared, /instances_attention/);
  assert.match(hub, /const status = tileStatus\(entry\)/);
  assert.doesNotMatch(hub, /const status = stringValue\(entry\.status/);
});
