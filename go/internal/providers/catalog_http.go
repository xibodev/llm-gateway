package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Catalog validation is separate from inference decoding: a bad discovery
// response must not replace a usable cache with an apparently empty catalog.
func decodeCatalogResponse(resp *http.Response, field string, identities ...string) (map[string]any, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := "catalog_http_error"
		detail := fmt.Sprintf("Provider catalog returned HTTP %d.", resp.StatusCode)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			code = "catalog_authentication_failed"
			detail = "Provider credential was rejected by the catalog API."
		}
		return nil, catalogError(code, detail, resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, catalogError("catalog_transport_error", "Provider catalog response could not be read.", resp.StatusCode)
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return nil, catalogError("catalog_invalid_json", "Provider catalog response was not valid JSON.", resp.StatusCode)
	}
	items, ok := body[field].([]any)
	if !ok {
		return nil, catalogError("catalog_invalid_shape", "Provider catalog response did not contain a "+field+" array.", resp.StatusCode)
	}
	// Validate every row before filtering capabilities or publishing any page.
	// Only identity fields are required; optional provider extensions stay open.
	for _, item := range items {
		row, ok := item.(map[string]any)
		valid := false
		if ok {
			for _, key := range identities {
				value, exists := row[key]
				if !exists {
					continue
				}
				id, ok := value.(string)
				if !ok {
					return nil, catalogError("catalog_invalid_shape", "Provider catalog response contained an invalid model row.", resp.StatusCode)
				}
				valid = valid || strings.TrimSpace(id) != ""
			}
		}
		if !valid {
			return nil, catalogError("catalog_invalid_shape", "Provider catalog response contained an invalid model row.", resp.StatusCode)
		}
	}
	return body, nil
}
