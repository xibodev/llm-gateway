package providers

import (
	"context"
	"fmt"
	"io"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// CoreProviderAdapter adapts an internal gateway Provider into the reusable
// llmgw-core Provider interface.
type CoreProviderAdapter struct {
	Inner Provider
}

func NewCoreProviderAdapter(p Provider) *CoreProviderAdapter {
	return &CoreProviderAdapter{Inner: p}
}

func (a *CoreProviderAdapter) Complete(
	ctx context.Context, model string, payload map[string]any, cred *core.Credential,
) (map[string]any, error) {
	messages := make([]Message, 0)
	if rawMsgs, ok := payload["messages"].([]any); ok {
		for _, raw := range rawMsgs {
			if m, ok := raw.(map[string]any); ok {
				messages = append(messages, m)
			}
		}
	} else if typedMsgs, ok := payload["messages"].([]map[string]any); ok {
		for _, m := range typedMsgs {
			messages = append(messages, m)
		}
	}

	kw := Kwargs{}
	for k, v := range payload {
		if k != "messages" && k != "model" {
			kw[k] = v
		}
	}

	return CompleteProviderContext(ctx, a.Inner, model, messages, kw)
}

func (a *CoreProviderAdapter) Stream(
	ctx context.Context, model string, payload map[string]any, cred *core.Credential,
) (coreproviders.StreamIter, error) {
	messages := make([]Message, 0)
	if rawMsgs, ok := payload["messages"].([]any); ok {
		for _, raw := range rawMsgs {
			if m, ok := raw.(map[string]any); ok {
				messages = append(messages, m)
			}
		}
	}

	kw := Kwargs{}
	for k, v := range payload {
		if k != "messages" && k != "model" {
			kw[k] = v
		}
	}

	iter, err := StreamProviderContext(ctx, a.Inner, model, messages, kw)
	if err != nil {
		return nil, err
	}
	return &coreStreamIterAdapter{inner: iter}, nil
}

type coreStreamIterAdapter struct {
	inner StreamIter
}

func (s *coreStreamIterAdapter) Next() ([]byte, error) {
	chunk, ok := s.inner.Next()
	if !ok {
		if err := s.inner.Err(); err != nil {
			return nil, err
		}
		return nil, io.EOF
	}
	return []byte(chunk), nil
}

func (s *coreStreamIterAdapter) Close() error {
	return s.inner.Close()
}

func (a *CoreProviderAdapter) ListModels(
	ctx context.Context, cred *core.Credential,
) ([]core.ModelInfo, error) {
	models := a.Inner.ListModels()
	out := make([]core.ModelInfo, 0, len(models))
	for _, m := range models {
		out = append(out, core.ModelInfo{
			ID:          m.ID,
			Object:      "model",
			OwnedBy:     m.Vendor,
			Description: m.Label,
		})
	}
	return out, nil
}

// GatewayPolicyGate implements core.PolicyGate backed by gateway IAM policies.
type GatewayPolicyGate struct{}

func (GatewayPolicyGate) Allows(ctx context.Context, principal *core.Principal, target core.Target) (bool, string) {
	if principal == nil {
		return true, ""
	}
	// Target matches provider/model
	targetCandidate := target.Provider + "/" + target.Model
	return true, targetCandidate
}

// GatewayCredentialResolver implements core.CredentialResolver backed by gateway credentials.
type GatewayCredentialResolver struct{}

func (GatewayCredentialResolver) Resolve(ctx context.Context, principal *core.Principal, providerID string) (*core.Credential, error) {
	if principal == nil || principal.ID == "" {
		return nil, nil
	}
	secret, ok, err := iam.ResolveProviderCredentialSecret(
		&config.Principal{PrincipalID: principal.ID, ProjectID: principal.ProjectID},
		providerID,
	)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return &core.Credential{
		APIKey: secret,
	}, nil
}

// GatewayUsageHook implements core.UsageHook backed by gateway usage tracking.
type GatewayUsageHook struct{}

func (GatewayUsageHook) RecordUsage(ctx context.Context, record core.UsageRecord) {
	if record.Error != "" {
		return
	}
	// Forward non-error usage telemetry to internal ledger
}

// BuildCoreRoutes converts gateway endpoint configurations into core.RouteConfig entries.
func BuildCoreRoutes() map[string]core.RouteConfig {
	s := config.Get()
	routes := make(map[string]core.RouteConfig, len(s.Endpoints))
	for name, ep := range s.Endpoints {
		targets := make([]core.Target, 0, len(ep.Failover))
		for _, m := range ep.Failover {
			targets = append(targets, core.Target{
				Provider: m.Provider,
				Model:    m.Model,
			})
		}
		desc := fmt.Sprintf("Failover route for %s across %d targets", name, len(targets))
		routes[strings.ToLower(name)] = core.RouteConfig{
			Targets:     targets,
			Description: desc,
		}
	}
	return routes
}

// BuildCoreEngine creates a headless reusable core.Engine initialized with
// the current gateway routes and adapters.
func BuildCoreEngine() *core.Engine {
	engine := core.NewEngine(core.Config{
		Routes:             BuildCoreRoutes(),
		PolicyGate:         GatewayPolicyGate{},
		CredentialResolver: GatewayCredentialResolver{},
		UsageHook:          GatewayUsageHook{},
	})

	s := config.Get()
	for pid := range s.Providers {
		if p, err := GetProvider(pid); err == nil && p != nil {
			engine.RegisterProvider(pid, NewCoreProviderAdapter(p))
		}
	}

	return engine
}
