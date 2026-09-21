package api

import (
	"errors"
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

const transportModeHeader = "X-LLMGW-Transport-Mode"

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
	if !isExactProviderModelResolution(model, resolution) {
		return router.Target{}, errors.New("transparent mode requires an exact provider/model target; endpoints and aliases are not accepted")
	}
	target := resolution.Targets[0]
	cfg := config.Get().Providers[target.Provider]
	if cfg == nil {
		return router.Target{}, errors.New("transparent mode requires a configured provider")
	}
	if anonymous, err := providers.AnonymousZenForPrincipal(target.Provider, principal); err != nil {
		return router.Target{}, errors.New("transparent mode could not resolve the provider connection mode")
	} else if anonymous {
		return router.Target{}, errors.New("transparent mode is unavailable for OpenCode Zen anonymous adaptation")
	}
	modelInfo, ok := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, principal)
	if !ok {
		return router.Target{}, errors.New("transparent mode requires a catalog-confirmed native surface")
	}
	for _, candidate := range modelInfo.SupportedSurfaces {
		if strings.EqualFold(strings.TrimSpace(candidate), surface) ||
			strings.EqualFold(strings.TrimPrefix(strings.TrimSpace(candidate), "/v1"), strings.TrimPrefix(surface, "/v1")) {
			return target, nil
		}
	}
	return router.Target{}, errors.New("the requested client surface is not native for this provider/model target")
}

func rejectTransparentStream(w http.ResponseWriter, stream bool) bool {
	if !stream {
		return false
	}
	writeError(w, http.StatusBadRequest, "Transparent streaming is not supported yet; use native mode or a non-streaming transparent request.")
	return true
}
