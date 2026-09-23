import { useCallback, useEffect, useRef, useState } from "preact/hooks";
import {
  Check,
  CheckCircle2,
  CircleAlert,
  Clipboard,
  ExternalLink,
  LoaderCircle,
  RefreshCw,
  ShieldCheck,
  X,
} from "lucide-preact";
import { sendJSON, type JSONRecord } from "../../lib/api";
import type { ConsoleMode } from "../../lib/mode";
import { asList, asRecord, numberValue, stringValue } from "../../lib/records";
import { useDialogFocus } from "../useDialogFocus";

function boolValue(value: unknown): boolean {
  return value === true;
}

type DeviceFlow = {
  providerID: string;
  deviceCode: string;
  userCode: string;
  verificationURI: string;
  interval: number;
  expiresAt: number;
  browser: boolean;
  manual: boolean;
};

type OAuthStage = "intro" | "starting" | "waiting" | "testing" | "success" | "error";

function formatRemaining(expiresAt: number, now: number): string {
  const seconds = Math.max(0, Math.ceil((expiresAt - now) / 1000));
  const minutes = Math.floor(seconds / 60);
  const remainder = seconds % 60;
  return minutes > 0 ? `${minutes}m ${remainder}s` : `${remainder}s`;
}

export function OAuthConnectDialog({
  entry,
  providerID,
  ownerID,
  data,
  mode,
  onClose,
  onComplete,
  onOpenPlayground,
}: {
  entry: JSONRecord;
  providerID: string;
  ownerID: string;
  data: JSONRecord;
  mode: ConsoleMode;
  onClose: () => void;
  onComplete: () => Promise<void>;
  onOpenPlayground?: (modelID: string, ownerID: string) => void;
}) {
  const owners = asList(data.principals)
    .map(asRecord)
    .filter(
      (principal) =>
        stringValue(principal.kind) === "human" &&
        stringValue(principal.status, "active") === "active",
    );
  const [connectionName, setConnectionName] = useState("personal");
  const [clientID, setClientID] = useState("");
  const [clientSecret, setClientSecret] = useState("");
  const [clientMode, setClientMode] = useState("public");
  const [redirectURI, setRedirectURI] = useState("");
  const [oauthProfile, setOAuthProfile] = useState("gateway_callback");
  const [codexFlow, setCodexFlow] = useState("browser");
  const [authorizationResponse, setAuthorizationResponse] = useState("");
  const [flow, setFlow] = useState<DeviceFlow | null>(null);
  const [stage, setStage] = useState<OAuthStage>("intro");
  const [message, setMessage] = useState("");
  const [nextPollAt, setNextPollAt] = useState(0);
  const [now, setNow] = useState(Date.now());
  const [copied, setCopied] = useState(false);
  const [probe, setProbe] = useState<JSONRecord | null>(null);
  const polling = useRef(false);
  const popup = useRef<Window | null>(null);
  const requiresClientID = asList(entry.onboarding_fields).map(String).includes("client_id");
  const supportsConsumerManual = stringValue(entry.id) === "google_antigravity";
  const supportsCodexFlows = stringValue(entry.id) === "openai_codex";

  const closePopup = useCallback(() => {
    try {
      if (popup.current && !popup.current.closed) popup.current.close();
    } catch {
      // A cross-origin provider tab can reject property access while it is
      // navigating. It remains safe to forget the reference.
    }
    popup.current = null;
  }, []);
  const closeDialog = useCallback(() => {
    closePopup();
    onClose();
  }, [closePopup, onClose]);
  const dialogRef = useDialogFocus(closeDialog);

  useEffect(() => closePopup, [closePopup]);

  const label = stringValue(entry.label, "Provider");
  const endpoint = useCallback(
    (suffix: string) =>
      mode === "portal"
        ? `/connections/${encodeURIComponent(providerID)}/oauth/${suffix}`
        : `/principals/${encodeURIComponent(ownerID)}/connections/${encodeURIComponent(providerID)}/oauth/${suffix}`,
    [mode, ownerID, providerID],
  );

  useEffect(() => {
    if (stage !== "waiting") return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [stage]);

  const testConnection = useCallback(async (providerID: string) => {
    const path =
      mode === "portal"
        ? `/providers/${encodeURIComponent(providerID)}/test`
        : `/providers/${encodeURIComponent(providerID)}/test?principal_id=${encodeURIComponent(ownerID)}`;
    return sendJSON<JSONRecord>(mode, path, "POST", {});
  }, [mode, ownerID]);

  const finishAuthorization = useCallback(async (response: JSONRecord, activeFlow: DeviceFlow) => {
    closePopup();
    setStage("testing");
    setMessage("Authorization complete. Testing the connection and syncing its model catalog.");
    let result: JSONRecord;
    try {
      const connection = asRecord(response.connection);
      result = await testConnection(stringValue(connection.provider_id, activeFlow.providerID));
    } catch (cause) {
      result = {
        success: false,
        details: cause instanceof Error ? cause.message : "The connection was saved, but its catalog test could not run.",
      };
    }
    setProbe(result);
    let refreshed = true;
    try { await onComplete(); }
    catch { refreshed = false; }
    const catalogReady = boolValue(result.success) && numberValue(result.model_count) > 0;
    setStage(catalogReady ? "success" : "error");
    setMessage(`${catalogReady
      ? "The private connection is ready."
      : "The private connection was saved, but no usable model catalog was returned."}${
      refreshed ? "" : " Reload the console to refresh the provider card."
    }`);
  }, [closePopup, onComplete, testConnection]);

  const poll = useCallback(async () => {
    if (!flow || polling.current || stage !== "waiting") return;
    polling.current = true;
    try {
      const response = await sendJSON<JSONRecord>(mode, endpoint("poll"), "POST", {
        ...(flow.browser ? { flow_id: flow.deviceCode } : { device_code: flow.deviceCode }),
        connection_name: connectionName.trim() || "personal",
      });
      const status = stringValue(response.status, "error");
      if (status === "authorized") {
        await finishAuthorization(response, flow);
        return;
      }
      if (status === "pending" || status === "slow_down") {
        const interval = status === "slow_down" ? flow.interval + 5 : flow.interval;
        setFlow({ ...flow, interval });
        setNextPollAt(Date.now() + interval * 1000);
        setMessage(
          status === "slow_down"
            ? "The provider asked us to slow down. Automatic checking will continue."
            : "Waiting for authorization in the provider tab. No action is required here.",
        );
        return;
      }
      closePopup();
      setStage("error");
      setMessage(
        stringValue(
          response.error,
          status === "expired"
            ? "The device code expired. Start a new sign-in."
            : `Authorization ended with status: ${status}.`,
        ),
      );
    } catch (cause) {
      closePopup();
      setStage("error");
      setMessage(cause instanceof Error ? cause.message : "OAuth polling failed.");
    } finally {
      polling.current = false;
    }
  }, [closePopup, connectionName, endpoint, finishAuthorization, flow, mode, stage]);

  useEffect(() => {
    if (!flow || flow.manual || stage !== "waiting") return;
    if (flow.expiresAt <= Date.now()) {
      closePopup();
      setStage("error");
      setMessage("The device code expired. Start a new sign-in.");
      return;
    }
    const timer = window.setTimeout(
      () => void poll(),
      Math.max(250, nextPollAt - Date.now()),
    );
    return () => window.clearTimeout(timer);
  }, [closePopup, flow, nextPollAt, poll, stage]);

  const start = async () => {
    if (mode === "admin" && !ownerID) {
      setMessage("Select an active human owner first.");
      return;
    }
    setStage("starting");
    setMessage("Starting the official provider sign-in.");
    setProbe(null);
    setCopied(false);

    // Opening a blank tab synchronously keeps the provider hand-off from being
    // blocked after the device-code request completes.
    popup.current = window.open("about:blank", "llmgw-provider-oauth");
    try {
      const response = await sendJSON<JSONRecord>(
        mode,
        endpoint("start"),
        "POST",
        {
          ...(requiresClientID && mode === "admin" ? { client_id: clientID.trim() } : {}),
          ...(oauthProfile === "consumer_manual" ? {
            profile: "consumer_manual",
            ...(mode === "admin" && clientID.trim() ? {
              client_id: clientID.trim(), client_secret: clientSecret,
              client_mode: clientMode, redirect_uri: redirectURI.trim(),
            } : {}),
          } : {}),
          connection_name: connectionName.trim() || "personal",
          ...(supportsCodexFlows ? { flow: codexFlow } : {}),
        },
      );
      const interval = Math.max(1, numberValue(response.interval, 5));
      const expiresAt =
        numberValue(response.expires_at) * 1000 ||
        Date.now() + Math.max(60, numberValue(response.expires_in, 900)) * 1000;
      const browser = stringValue(response.flow) === "browser";
	  const manual = ["consumer_manual", "browser_pkce"].includes(stringValue(response.flow));
      const verificationURI = stringValue(response.authorization_url, stringValue(response.verification_uri));
      const nextFlow = {
        providerID: stringValue(response.provider_id, providerID),
        deviceCode: stringValue(response.flow_id, stringValue(response.device_code)),
        userCode: stringValue(response.user_code),
        verificationURI,
        interval,
        expiresAt,
        browser,
        manual,
      };
      if (!nextFlow.deviceCode || (!browser && !manual && !nextFlow.userCode) || !verificationURI) {
        throw new Error("The provider returned incomplete authorization data.");
      }
      setFlow(nextFlow);
      setNextPollAt(manual ? 0 : Date.now() + interval * 1000);
      setNow(Date.now());
      setStage("waiting");
      setMessage(manual
        ? "After approval, paste the authorization code or the full redirect URL below."
        : "Waiting for authorization in the provider tab. This page checks automatically.");
      try {
        if (popup.current && !popup.current.closed) {
          popup.current.location.replace(verificationURI);
        } else {
          popup.current = window.open(verificationURI, "llmgw-provider-oauth");
        }
      } catch {
        popup.current = window.open(verificationURI, "llmgw-provider-oauth");
      }
    } catch (cause) {
      closePopup();
      setStage("error");
      setMessage(cause instanceof Error ? cause.message : "Could not start official OAuth.");
    }
  };

  const completeManual = async () => {
    if (!flow || !flow.manual || !authorizationResponse.trim()) return;
    setMessage("Exchanging the authorization code securely.");
    try {
      const response = await sendJSON<JSONRecord>(mode, endpoint("complete"), "POST", {
        flow_id: flow.deviceCode,
        authorization_response: authorizationResponse.trim(),
      });
      if (stringValue(response.status) !== "authorized") {
        setMessage(stringValue(response.error, "Manual authorization failed."));
        return;
      }
      await finishAuthorization(response, flow);
    } catch (cause) {
      setMessage(cause instanceof Error ? cause.message : "Manual authorization failed.");
    }
  };

  const copyCode = async () => {
    if (!flow?.userCode) return;
    try {
      await navigator.clipboard.writeText(flow.userCode);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setMessage("Copy was blocked by the browser. Select the code and copy it manually.");
    }
  };

  const reset = () => {
    closePopup();
    setFlow(null);
    setProbe(null);
    setMessage("");
    setNextPollAt(0);
    setAuthorizationResponse("");
    setStage("intro");
  };

  const openPlayground = () => {
    closePopup();
    const model = stringValue(asList(probe?.sample)[0]);
    onOpenPlayground?.(model.includes("/") ? model : `${providerID}/${model}`, ownerID);
    onClose();
  };

  const probePassed = boolValue(probe?.success);
  const probeStatus = stringValue(probe?.status, "saved");
  const displayProbeStatus = probeStatus
    ? probeStatus[0].toUpperCase() + probeStatus.slice(1)
    : "Saved";

  return (
    <div class="dialog-backdrop" role="presentation">
      <section
        ref={dialogRef}
        class="dialog oauth-dialog"
        role="dialog"
        aria-modal="true"
        aria-labelledby="oauth-provider-title"
        tabIndex={-1}
      >
        <header>
          <div>
            <p class="eyebrow">Official provider sign-in</p>
            <h2 id="oauth-provider-title">Connect {label}</h2>
          </div>
          <button class="icon-button" type="button" aria-label="Close dialog" onClick={closeDialog}>
            <X size={18} />
          </button>
        </header>

        <ol class="oauth-progress" aria-label="Connection progress">
          <li class={stage === "intro" || stage === "starting" ? "is-active" : "is-done"}>
            <span>1</span> Start
          </li>
          <li class={stage === "waiting" ? "is-active" : stage === "intro" || stage === "starting" ? "" : "is-done"}>
            <span>2</span> Authorize
          </li>
          <li class={stage === "testing" ? "is-active" : stage === "success" ? "is-done" : ""}>
            <span>3</span> Verify
          </li>
        </ol>

        {stage === "intro" || stage === "starting" ? (
          <div class="oauth-flow">
            {mode === "admin" ? (
              <p class="oauth-flow__notice">
                Human owner: {stringValue(owners.find((owner) => stringValue(owner.id) === ownerID)?.display_name, ownerID)}
              </p>
            ) : (
              <p class="oauth-flow__notice">
                This subscription remains encrypted and private to the signed-in human.
              </p>
            )}
            <label class="owner-select">
              Connection name
              <input
                value={connectionName}
                onInput={(event) =>
                  setConnectionName((event.currentTarget as HTMLInputElement).value)
                }
                placeholder="personal"
              />
            </label>
            {supportsConsumerManual ? <label class="owner-select">
              OAuth profile
              <select value={oauthProfile} onChange={(event) => setOAuthProfile((event.currentTarget as HTMLSelectElement).value)}>
                <option value="gateway_callback">Gateway callback</option>
                <option value="consumer_manual">Manual code</option>
              </select>
              <small class="form-help">Manual code uses a fixed caller-configured redirect URI and never requires the browser to reach this gateway.</small>
            </label> : null}
            {supportsConsumerManual && oauthProfile === "consumer_manual" && mode === "admin" ? <>
              <p class="oauth-flow__notice">Leave these fields blank to use the runtime or previously encrypted admin profile.</p>
              <label class="owner-select">OAuth client ID<input value={clientID} onInput={(event) => setClientID((event.currentTarget as HTMLInputElement).value)} autoComplete="off" /></label>
              <label class="owner-select">Client mode<select value={clientMode} onChange={(event) => setClientMode((event.currentTarget as HTMLSelectElement).value)}><option value="public">Public</option><option value="confidential">Confidential</option></select></label>
              {clientMode === "confidential" ? <label class="owner-select">OAuth client secret<input type="password" value={clientSecret} onInput={(event) => setClientSecret((event.currentTarget as HTMLInputElement).value)} autoComplete="new-password" /></label> : null}
              <label class="owner-select">Fixed redirect URI<input value={redirectURI} onInput={(event) => setRedirectURI((event.currentTarget as HTMLInputElement).value)} placeholder="https://localhost.example/callback" autoComplete="off" /></label>
            </> : null}
            {supportsCodexFlows ? <label class="owner-select">
              Sign-in flow
              <select value={codexFlow} onChange={(event) => setCodexFlow((event.currentTarget as HTMLSelectElement).value)}>
                <option value="browser">Browser sign-in</option>
                <option value="device_code">Device code</option>
              </select>
              <small class="form-help">Browser sign-in matches the official Codex CLI profile. Device authorization remains available for headless environments.</small>
            </label> : null}
            {requiresClientID && mode === "admin" ? (
              <label class="owner-select">
                OAuth client ID
                <input
                  value={clientID}
                  onInput={(event) =>
                    setClientID((event.currentTarget as HTMLInputElement).value)
                  }
                  placeholder="Required the first time"
                  autoComplete="off"
                />
                <small class="form-help">
                  Leave blank to use the verified public Codex client. Enter a different
                  client ID only when you are authorized to operate it.
                </small>
              </label>
            ) : null}
            <div class="oauth-disclosure">
              <ShieldCheck size={18} />
              <div>
                <strong>What happens next</strong>
                <p>
                  The official provider page opens in a new tab. Approve access and return
                  here. Manual-code profiles ask you to paste the code or full redirect URL;
                  callback and device profiles are checked automatically. Tokens stay server-side.
                </p>
              </div>
            </div>
            <button
              class="button button--primary"
              type="button"
              disabled={stage === "starting" || (mode === "admin" && !ownerID)}
              onClick={() => void start()}
            >
              {stage === "starting" ? (
                <LoaderCircle class="spin" size={16} />
              ) : (
                <ExternalLink size={16} />
              )}
              Continue with {label}
            </button>
          </div>
        ) : null}

        {stage === "waiting" && flow ? (
          <div class="oauth-flow">
            <div class="oauth-waiting-heading">
              <LoaderCircle class="spin" size={20} />
              <div>
                <strong>Approve access in the provider tab</strong>
                <span>{flow.manual ? "Waiting for the returned authorization code" : `Automatic check in ${Math.max(0, Math.ceil((nextPollAt - now) / 1000))}s`}</span>
              </div>
            </div>
            <p class="oauth-flow__instruction">
              {flow.manual ? "Complete sign-in, then paste the returned code or redirect URL here." : flow.browser ? "Complete sign-in on the official provider page." : "Enter this one-time code on the official verification page."}
            </p>
            {!flow.browser && !flow.manual ? <div class="oauth-code">
              <button
                class="oauth-code__value technical"
                type="button"
                onClick={() => void copyCode()}
                aria-label={`Copy device code ${flow.userCode}`}
              >
                {flow.userCode}
                {copied ? <Check size={16} /> : <Clipboard size={16} />}
              </button>
              <a href={flow.verificationURI} target="_blank" rel="noreferrer">
                Open verification <ExternalLink size={14} />
              </a>
            </div> : <a class="button button--secondary" href={flow.verificationURI} target="_blank" rel="noreferrer">Open provider sign-in <ExternalLink size={14} /></a>}
            {flow.manual ? <label class="owner-select">
              Authorization code or full redirect URL
              <textarea value={authorizationResponse} onInput={(event) => setAuthorizationResponse((event.currentTarget as HTMLTextAreaElement).value)} rows={3} autoComplete="off" />
              <button class="button button--primary" type="button" disabled={!authorizationResponse.trim()} onClick={() => void completeManual()}>Complete authorization</button>
            </label> : null}
            <div class="oauth-waiting-meta">
              <span>Expires in {formatRemaining(flow.expiresAt, now)}</span>
              <span>Tokens never enter browser state</span>
            </div>
            <p class="oauth-message" role="status">{message}</p>
          </div>
        ) : null}

        {stage === "testing" ? (
          <div class="oauth-result oauth-result--working" aria-live="polite">
            <LoaderCircle class="spin" size={28} />
            <div>
              <h3>Connection authorized</h3>
              <p>{message}</p>
            </div>
          </div>
        ) : null}

        {stage === "success" ? (
          <div class={`oauth-result ${probePassed ? "oauth-result--success" : "oauth-result--warning"}`} role="status" aria-live="polite">
            {probePassed ? <CheckCircle2 size={30} /> : <CircleAlert size={30} />}
            <div>
              <h3>{probePassed ? "Connected and tested" : "Connected with a warning"}</h3>
              <p>{message}</p>
              <dl class="oauth-test-summary">
                <div><dt>Catalog</dt><dd>{numberValue(probe?.model_count)} models</dd></div>
                <div><dt>Latency</dt><dd>{numberValue(probe?.latency_ms)} ms</dd></div>
                <div><dt>Status</dt><dd>{displayProbeStatus}</dd></div>
              </dl>
            </div>
          </div>
        ) : null}

        {stage === "error" ? (
          <div class="oauth-result oauth-result--error" role="alert">
            <CircleAlert size={30} />
            <div>
              <h3>{probe ? "Connection saved, catalog unavailable" : "Connection was not completed"}</h3>
              <p>{message}</p>
              {probe ? <dl class="oauth-test-summary">
                <div><dt>Catalog</dt><dd>{numberValue(probe.model_count)} models</dd></div>
                <div><dt>Failure code</dt><dd class="technical">{stringValue(probe.failure_code, "catalog_failed")}</dd></div>
                <div><dt>Detail</dt><dd>{stringValue(probe.details, "The provider catalog could not be verified.")}</dd></div>
              </dl> : null}
            </div>
          </div>
        ) : null}

        <footer class="oauth-dialog__footer">
          {stage === "error" ? probe ? (
            <button class="button button--secondary" type="button" onClick={closeDialog}>
              Done
            </button>
          ) : (
            <button class="button button--primary" type="button" onClick={reset}>
              <RefreshCw size={16} /> Try again
            </button>
          ) : null}
          {stage === "success" ? (
            <>
              <button class="button button--secondary" type="button" onClick={closeDialog}>
                Done
              </button>
              <button class="button button--primary" type="button" onClick={openPlayground} disabled={!onOpenPlayground || !stringValue(asList(probe?.sample)[0])}>
                Try in playground
              </button>
            </>
          ) : stage !== "error" || !probe ? (
            <button class="button button--secondary" type="button" onClick={closeDialog}>
              Cancel
            </button>
          ) : null}
        </footer>
      </section>
    </div>
  );
}
