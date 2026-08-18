import { useEffect, useMemo, useState } from "preact/hooks";
import { AppShell } from "./components/AppShell";
import { ErrorState, LoadingState } from "./components/PageState";
import { APIError, authenticationRedirectCode, clearStaticAdminKey, getJSON, hasStaticAdminKey, storeStaticAdminKey, type JSONRecord } from "./lib/api";
import { modeForPath, type ConsoleMode } from "./lib/mode";
import { asList, asRecord, stringValue } from "./lib/records";
import { navigationFor, type PageID } from "./lib/navigation";
import { Access } from "./pages/Access";
import { ApiKeys } from "./pages/ApiKeys";
import { Alerts } from "./pages/Alerts";
import { ModelsEndpoints } from "./pages/ModelsEndpoints";
import { Overview } from "./pages/Overview";
import { Playground } from "./pages/Playground";
import { Providers } from "./pages/Providers";
import { Routes } from "./pages/Routes";
import { Settings } from "./pages/Settings";
import { UsageQuotas } from "./pages/UsageQuotas";

type ConsoleRoute = { page: PageID; detail: string };

function routeFromHash(mode: ConsoleMode): ConsoleRoute {
  const raw = window.location.hash.replace(/^#\/?/, "");
  const [candidate, ...rest] = raw.split("/");
  const page = navigationFor(mode).some((item) => item.id === candidate) ? (candidate as PageID) : "overview";
  return { page, detail: page === candidate ? decodeURIComponent(rest.join("/")) : "" };
}

function StaticAdminSignIn({ error, onSignedIn }: { error: string | null; onSignedIn: () => void }) {
  const [key, setKey] = useState("");
  const [message, setMessage] = useState("");
  const submit = (event: Event) => {
    event.preventDefault();
    if (!key.trim()) { setMessage("Enter the gateway administrator key."); return; }
    storeStaticAdminKey(key);
    setKey("");
    setMessage("");
    onSignedIn();
  };
  return <section class="state-panel state-panel--error"><div><h2>Administrator sign-in required</h2><p>{error || "Enter the local gateway administrator key to continue. The key is kept only for this browser session."}</p><form onSubmit={submit}><label>Gateway administrator key<input type="password" autoComplete="current-password" value={key} onInput={(event) => setKey((event.currentTarget as HTMLInputElement).value)} /></label>{message ? <p role="status">{message}</p> : null}<button class="button button--primary" type="submit">Sign in</button></form></div></section>;
}

export function App() {
  const mode = modeForPath();
  const isAdmin = mode === "admin";
  const navigation = useMemo(() => navigationFor(mode), [mode]);
  const [route, setRoute] = useState<ConsoleRoute>(() => routeFromHash(mode));
  const page = route.page;
  const [data, setData] = useState<JSONRecord | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [adminSignInRequired, setAdminSignInRequired] = useState(false);
  const [staticAdminKeyActive, setStaticAdminKeyActive] = useState(() => isAdmin && hasStaticAdminKey());
  const [catalogPrincipalID, setCatalogPrincipalID] = useState("");

  const load = async (showLoading = true) => {
    if (showLoading) setLoading(true);
    setError(null);
    try {
      const endpoint = mode === "portal" ? "/me" : "/state";
      setData(await getJSON<JSONRecord>(mode, endpoint));
      if (mode === "admin") setAdminSignInRequired(false);
    } catch (cause) {
      const message = cause instanceof Error ? cause.message : "The console could not load gateway state.";
      setError(message);
      if (cause instanceof APIError && cause.code === authenticationRedirectCode) {
        window.location.assign(mode === "portal" ? "/portal" : "/admin");
        return;
      }
      if (mode === "admin" && cause instanceof APIError && cause.status === 401) {
        const rejectedStaticKey = hasStaticAdminKey();
        setAdminSignInRequired(true);
        if (rejectedStaticKey) {
          clearStaticAdminKey();
          setStaticAdminKeyActive(false);
          setError("Invalid administrator key. Check the value and try again.");
        }
      }
    } finally {
      if (showLoading) setLoading(false);
    }
  };
  const refresh = () => load(false);

  useEffect(() => { void load(true); }, [mode]);
  useEffect(() => {
    if (!isAdmin || !data) {
      setCatalogPrincipalID("");
      return;
    }
    const ownerIDs = asList(data.principals).map(asRecord)
      .filter((principal) => stringValue(principal.kind) === "human" && stringValue(principal.status, "active") === "active")
      .map((principal) => stringValue(principal.id)).filter(Boolean);
    setCatalogPrincipalID((current) => ownerIDs.includes(current) ? current : (ownerIDs[0] ?? ""));
  }, [data, isAdmin]);
  useEffect(() => {
    const syncHash = () => setRoute(routeFromHash(mode));
    window.addEventListener("hashchange", syncHash);
    return () => window.removeEventListener("hashchange", syncHash);
  }, [mode]);

  const navigate = (next: PageID, detail?: string) => {
    window.location.hash = detail ? `${next}/${encodeURIComponent(detail)}` : next;
    setRoute({ page: next, detail: detail ?? "" });
  };
  const signedInWithStaticAdminKey = () => {
    setStaticAdminKeyActive(true);
    setAdminSignInRequired(false);
    void load(true);
  };
  const signOutStaticAdminKey = () => {
    clearStaticAdminKey();
    setStaticAdminKeyActive(false);
    setData(null);
    setError(null);
    setAdminSignInRequired(true);
  };

  let content = <LoadingState />;
  if (!loading && isAdmin && adminSignInRequired) {
    content = <StaticAdminSignIn error={error} onSignedIn={signedInWithStaticAdminKey} />;
  } else if (!loading && error) {
    content = <ErrorState title="Workspace state is unavailable" detail={error} action={<button class="button button--primary" type="button" onClick={() => void load(true)}>Retry</button>} />;
  } else if (!loading && data) {
    switch (page) {
      case "providers": content = <Providers data={data} mode={mode} detail={route.detail} onChanged={refresh} onNavigate={navigate} />; break;
      case "routes": content = <Routes data={data} mode={mode} detail={route.detail} onChanged={refresh} onNavigate={navigate} />; break;
      case "models": content = <ModelsEndpoints data={data} mode={mode} principalID={catalogPrincipalID} onPrincipalIDChange={setCatalogPrincipalID} />; break;
      case "playground": content = <Playground data={data} mode={mode} principalID={catalogPrincipalID} onPrincipalIDChange={setCatalogPrincipalID} preset={route.detail} onPresetConsumed={() => setRoute({ page: "playground", detail: route.detail })} onBack={route.detail ? () => navigate("providers", route.detail.split("/")[0]) : undefined} />; break;
      case "keys": content = <ApiKeys data={data} mode={mode} onChanged={refresh} />; break;
      case "access": content = <Access data={data} mode={mode} onChanged={refresh} />; break;
      case "usage": content = <UsageQuotas data={data} mode={mode} />; break;
      case "alerts": content = <Alerts data={data} />; break;
      case "settings": content = <Settings data={data} mode={mode} onNavigate={navigate} />; break;
      default: content = <Overview data={data} mode={mode} onNavigate={navigate} />;
    }
  }

  const principal = mode === "portal" ? asRecord(data?.principal) : {};
  const identityName = stringValue(principal.display_name, stringValue(principal.email));
  const identityDetail = stringValue(principal.email);

  return <AppShell mode={mode} navigation={navigation} currentPage={page} onNavigate={navigate} identityName={identityName} identityDetail={identityDetail} staticAdminKeyActive={isAdmin && staticAdminKeyActive} onStaticAdminSignOut={isAdmin ? signOutStaticAdminKey : undefined}>{content}</AppShell>;
}
