package api

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	translate "github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
)

const transportModeHeader = "X-LLMGW-Transport-Mode"

const transportCatalogFreshness = time.Hour

func requestedTransportMode(r *http.Request) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(r.Header.Get(transportModeHeader)))
	if mode == "" {
		return "", nil
	}
	if mode != "transparent" {
		return "", errors.New("X-LLMGW-Transport-Mode must be transparent when present")
	}
	return mode, nil
}

func exactNativeTransparentTarget(model, surface string, resolution router.Resolution, principal *config.Principal) (router.Target, error) {
	exact := isExactProviderModelResolution(model, resolution)
	if !exact {
		return router.Target{}, errors.New("transparent mode requires an exact provider/model target; endpoints and aliases are not accepted")
	}
	target := resolution.Targets[0]
	if config.Get().Providers[target.Provider] == nil {
		return router.Target{}, errors.New("transparent mode requires a configured provider")
	}
	modelInfo, ok := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, principal)
	if !ok {
		return router.Target{}, errors.New("transparent mode requires a catalog-confirmed native surface")
	}
	interfaces, err := providerTransportInterfaces(target.Provider, target.Model, principal, modelInfo.SupportedSurfaces)
	if err != nil {
		return router.Target{}, errors.New("transparent mode could not resolve the provider interface")
	}
	plan := planTargetTransport(
		modelInfo, providers.CatalogRefreshedAtForPrincipal(target.Provider, principal), interfaces,
		surface, exact, core.TransportRequirementTransparent, false, translate.Report{}, time.Now(),
	)
	if plan.Disposition == core.TransportNative {
		return target, nil
	}
	switch plan.Reason {
	case core.TransportRejectExactTargetRequired:
		return router.Target{}, errors.New("transparent mode requires an exact provider/model target; endpoints and aliases are not accepted")
	case core.TransportRejectNativeUnconfirmed:
		anonymousZen, _ := providers.AnonymousZenForPrincipal(target.Provider, principal)
		if anonymousZen {
			return router.Target{}, errors.New("transparent mode is unavailable for OpenCode Zen anonymous adaptation")
		}
		if !hasNativeInterface(interfaces, modelSurface(surface)) {
			return router.Target{}, errors.New("the requested client surface is not native for this provider/model target")
		}
		return router.Target{}, errors.New("transparent mode requires fresh, catalog-confirmed native capability evidence")
	default:
		return router.Target{}, errors.New("the requested client surface is not native for this provider/model target")
	}
}

func hasNativeInterface(interfaces []core.TransportInterface, surface core.ModelSurface) bool {
	for _, current := range interfaces {
		if current.Surface == surface && current.Native == core.SupportSupported {
			return true
		}
	}
	return false
}

func targetTransportMode(target router.Target, principal *config.Principal, surface string) string {
	if model, found := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, principal); found {
		interfaces, err := providerTransportInterfaces(target.Provider, target.Model, principal, model.SupportedSurfaces)
		if err == nil {
			plan := planTargetTransport(
				model, providers.CatalogRefreshedAtForPrincipal(target.Provider, principal), interfaces,
				surface, true, core.TransportRequirementAny, true, translate.Report{}, time.Now(),
			)
			if plan.Disposition == core.TransportNative {
				return "native"
			}
			return "translated"
		}
	}
	provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
	if err != nil {
		return "translated"
	}
	if providers.PreservesWireNativeSurface(provider, target.Model, modelSurface(surface)) {
		return "native"
	}
	return "translated"
}

func planTargetTransport(
	model providers.ModelInfo,
	refreshedAt time.Time,
	interfaces []core.TransportInterface,
	surface string,
	exact bool,
	requirement core.TransportRequirement,
	translationEvaluated bool,
	translation translate.Report,
	evaluatedAt time.Time,
) core.TransportPlan {
	capabilities := providers.AdaptModelCapabilities(
		model.Capabilities, model.SupportedSurfaces, refreshedAt, time.Time{},
	)
	if model.TypedCapabilities != nil {
		copy := *model.TypedCapabilities
		capabilities = &copy
	}
	if !refreshedAt.IsZero() {
		discoveredAt := refreshedAt.UTC()
		expiresAt := discoveredAt.Add(transportCatalogFreshness)
		capabilities.Freshness.DiscoveredAt = &discoveredAt
		capabilities.Freshness.ExpiresAt = &expiresAt
	} else {
		capabilities.Freshness = core.ModelCapabilityFreshness{}
	}
	return core.PlanTransport(core.TransportPlanRequest{
		Operation:            core.ModelOperationChat,
		Surface:              modelSurface(surface),
		EvaluatedAt:          evaluatedAt,
		ExactTarget:          exact,
		Capabilities:         *capabilities,
		Interfaces:           interfaces,
		Requirement:          requirement,
		TranslationEvaluated: translationEvaluated,
		Translation:          translation,
	})
}

func providerTransportInterfaces(providerID, model string, principal *config.Principal, surfaces []string) ([]core.TransportInterface, error) {
	provider, err := providers.GetProviderForPrincipal(providerID, principal)
	if err != nil {
		return nil, err
	}
	interfaces := make([]core.TransportInterface, 0, len(surfaces))
	for _, candidate := range surfaces {
		surface := modelSurface(candidate)
		switch surface {
		case core.ModelSurfaceChatCompletions:
		case core.ModelSurfaceResponses:
		case core.ModelSurfaceMessages:
		default:
			continue
		}
		native := core.SupportUnsupported
		if providers.PreservesWireNativeSurface(provider, model, surface) {
			native = core.SupportSupported
		}
		interfaces = append(interfaces, core.TransportInterface{Surface: surface, Native: native})
	}
	return interfaces, nil
}

func modelTransportSurfaces(
	providerID string,
	principal *config.Principal,
	model providers.ModelInfo,
	refreshedAt time.Time,
	capabilities map[string]any,
	surfaces []string,
	evaluatedAt time.Time,
) ([]string, []string, []string) {
	interfaces, err := providerTransportInterfaces(providerID, model.ID, principal, surfaces)
	if err != nil {
		interfaces = nil
	}

	chat, _ := capabilities["chat"].(bool)
	for _, surface := range surfaces {
		if modelSurface(surface) != "" {
			chat = true
			break
		}
	}
	if !chat {
		return nil, nil, nil
	}

	model.Capabilities = capabilities
	model.SupportedSurfaces = surfaces
	native := []string{}
	emulated := []string{}
	unknown := []string{}
	for _, surface := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		plan := planTargetTransport(
			model, refreshedAt, interfaces, surface, true,
			core.TransportRequirementAny, true, translate.Report{}, evaluatedAt,
		)
		switch plan.Disposition {
		case core.TransportNative:
			native = append(native, surface)
		case core.TransportAdapted:
			emulated = append(emulated, surface)
		default:
			unknown = append(unknown, surface)
		}
	}
	return native, emulated, unknown
}

func modelSurface(surface string) core.ModelSurface {
	switch strings.ToLower(strings.TrimPrefix(strings.TrimSpace(surface), "/v1")) {
	case "/chat/completions":
		return core.ModelSurfaceChatCompletions
	case "/responses":
		return core.ModelSurfaceResponses
	case "/messages":
		return core.ModelSurfaceMessages
	default:
		return ""
	}
}

func rejectTransparentStream(w http.ResponseWriter, stream bool) bool {
	if !stream {
		return false
	}
	writeError(w, http.StatusBadRequest, "Transparent streaming is not supported yet; use native mode or a non-streaming transparent request.")
	return true
}
