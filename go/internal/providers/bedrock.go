package providers

import (
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// bedrockBaseURL templates the OpenAI-compatible Bedrock endpoint for a region.
func bedrockBaseURL(region string) string {
	return config.BedrockBaseURL(region)
}

// buildBedrockProvider builds an OpenAI-standard provider bound to Bedrock. An
// explicit base URL wins (bedrock-mantle / VPC); otherwise it is region-templated.
func buildBedrockProvider(
	region, baseURL, apiKey string,
	observation *iam.ProviderAccountObservation,
	timeout float64,
) OpenAIProvider {
	url := strings.TrimSpace(baseURL)
	if url == "" {
		url = bedrockBaseURL(region)
	}
	return OpenAIProvider{auth: bearerAuth{
		base: strings.TrimRight(url, "/"), apiKey: apiKey, observation: observation,
	}, Timeout: timeout}
}
