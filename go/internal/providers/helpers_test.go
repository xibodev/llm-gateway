package providers

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func setupCodexProviderTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	oldKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	oldPolicies := config.Get().Policies
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
		config.Update(func(s *config.Settings) {
			s.CredentialEncryptionKey = oldKey
			s.Providers = oldProviders
			s.Policies = oldPolicies
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
}

func copilotTransportBody(payload map[string]any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
	return strings.TrimRight(buffer.String(), "\n")
}

func drainZenStream(t *testing.T, stream StreamIter, err error) ([]string, error) {
	t.Helper()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, ok := stream.Next()
		if !ok {
			return chunks, stream.Err()
		}
		chunks = append(chunks, chunk)
	}
}
