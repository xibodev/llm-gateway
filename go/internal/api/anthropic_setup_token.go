package api

import (
	"fmt"
	"strings"
	"unicode"
)

const AnthropicSetupTokenPrefix = "sk-ant-oat01-"

func ValidateAnthropicSetupToken(token string) error {
	if token != strings.TrimSpace(token) {
		return fmt.Errorf("Anthropic setup token must not contain surrounding whitespace")
	}
	if !strings.HasPrefix(token, AnthropicSetupTokenPrefix) {
		return fmt.Errorf("Anthropic setup token must start with %q", AnthropicSetupTokenPrefix)
	}
	if len(token) < 80 {
		return fmt.Errorf("Anthropic setup token is too short")
	}
	suffix := strings.TrimPrefix(token, AnthropicSetupTokenPrefix)
	for _, char := range suffix {
		if char > unicode.MaxASCII || !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return fmt.Errorf("Anthropic setup token contains invalid characters")
		}
	}
	return nil
}

func DetectAnthropicKind(key string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(key), AnthropicSetupTokenPrefix) {
		return "setup_token", nil
	}
	return "api_key", nil
}
