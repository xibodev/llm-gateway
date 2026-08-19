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
  data,
  mode,
  onClose,
  onComplete,
}: {
  entry: JSONRecord;
  providerID: string;
  data: JSONRecord;
  mode: ConsoleMode;
  onClose: () => void;
  onComplete: () => Promise<void>;
}) {
  const owners = asList(data.principals)
    .map(asRecord)
    .filter(
      (principal) =>
        stringValue(principal.kind) === "human" &&
        stringValue(principal.status, "active") === "active",
    );
  const [ownerID, setOwnerID] = useState(stringValue(owners[0]?.id));
  const [connectionName, setConnectionName] = useState("personal");
  const [clientID, setClientID] = useState("");
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

  const poll = useCallback(async () => {
    if (!flow || polling.current || stage !== "waiting") return;
    polling.current = true;
    try {
      const response = await sendJSON<JSONRecord>(mode, endpoint("poll"), "POST", {
        device_code: flow.deviceCode,
        connection_name: connectionName.trim() || "personal",
      });
      const status = stringValue(response.status, "error");
      if (status === "authorized") {
        closePopup();
        setStage("testing");
        setMessage("Authorization complete. Testing the connection and syncing its model catalog.");
        let result: JSONRecord;
        try {
          const connection = asRecord(response.connection);
          result = await testConnection(
            stringValue(connection.provider_id, flow.providerID),
          );
        } catch (cause) {
          result = {
            success: false,
            details:
              cause instanceof Error
                ? cause.message
                : "The connection was saved, but its catalog test could not run.",
          };
        }
        setProbe(result);
        let refreshed = true;
        try {
          await onComplete();
        } catch {
          refreshed = false;
        }
        setStage("success");
        setMessage(
          `${boolValue(result.success)
            ? "The private connection is ready."
            : "The private connection was saved, but its catalog test needs attention."}${
            refreshed ? "" : " Reload the console to refresh the provider card."
          }`,
        );
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
  }, [closePopup, connectionName, endpoint, flow, mode, onComplete, stage, testConnection]);

  useEffect(() => {
    if (!flow || stage !== "waiting") return;
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
        requiresClientID && mode === "admin" ? { client_id: clientID.trim() } : {},
      );
      const interval = Math.max(1, numberValue(response.interval, 5));
      const expiresAt =
        numberValue(response.expires_at) * 1000 ||
        Date.now() + Math.max(60, numberValue(response.expires_in, 900)) * 1000;
      const verificationURI = stringValue(response.verification_uri);
      const nextFlow = {
        providerID: stringValue(response.provider_id, providerID),
        deviceCode: stringValue(response.device_code),
        userCode: stringValue(response.user_code),
        verificationURI,
        interval,
        expiresAt,
      };
      if (!nextFlow.deviceCode || !nextFlow.userCode || !verificationURI) {
        throw new Error("The provider returned incomplete device authorization data.");
      }
      setFlow(nextFlow);
      setNextPollAt(Date.now() + interval * 1000);
      setNow(Date.now());
      setStage("waiting");
      setMessage("Waiting for authorization in the provider tab. This page checks automatically.");
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
    setStage("intro");
  };

  const openPlayground = () => {
    closePopup();
    window.location.hash = "playground";
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
              <label class="owner-select">
                Human owner
                <select
                  value={ownerID}
                  onInput={(event) =>
                    setOwnerID((event.currentTarget as HTMLSelectElement).value)
                  }
                >
                  <option value="">Select owner</option>
                  {owners.map((owner) => (
                    <option value={stringValue(owner.id)} key={stringValue(owner.id)}>
                      {stringValue(
                        owner.display_name,
                        stringValue(owner.email, stringValue(owner.id)),
                      )}
                    </option>
                  ))}
                </select>
              </label>
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
                  Enter only an OAuth client ID you are authorized to operate. OpenAI
                  does not currently document third-party Codex client registration,
                  and the gateway does not embed the official Codex CLI client ID.
                </small>
              </label>
            ) : null}
            <div class="oauth-disclosure">
              <ShieldCheck size={18} />
              <div>
                <strong>What happens next</strong>
                <p>
                  The official provider page opens in a new tab. Enter the temporary code,
                  approve access, and return here. The console checks automatically, stores
                  tokens server-side, then tests the provider catalog.
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
                <span>Automatic check in {Math.max(0, Math.ceil((nextPollAt - now) / 1000))}s</span>
              </div>
            </div>
            <p class="oauth-flow__instruction">
              Enter this one-time code on the official verification page.
            </p>
            <div class="oauth-code">
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
            </div>
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
          <div class={`oauth-result ${probePassed ? "oauth-result--success" : "oauth-result--warning"}`}>
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
              <h3>Connection was not completed</h3>
              <p>{message}</p>
            </div>
          </div>
        ) : null}

        <footer class="oauth-dialog__footer">
          {stage === "error" ? (
            <button class="button button--primary" type="button" onClick={reset}>
              <RefreshCw size={16} /> Try again
            </button>
          ) : null}
          {stage === "success" ? (
            <>
              <button class="button button--secondary" type="button" onClick={closeDialog}>
                Done
              </button>
              <button class="button button--primary" type="button" onClick={openPlayground}>
                Try in playground
              </button>
            </>
          ) : (
            <button class="button button--secondary" type="button" onClick={closeDialog}>
              Cancel
            </button>
          )}
        </footer>
      </section>
    </div>
  );
}
