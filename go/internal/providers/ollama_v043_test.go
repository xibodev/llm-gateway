package providers

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestV043OllamaMapsDeveloperRoleToSystem(t *testing.T) {
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	daemon := ollamaDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	})
	_, err := ollamaFixture(t, daemon.URL).Complete("model", []Message{
		{"role": "developer", "content": "developer policy"},
		{"role": "user", "content": "hello"},
	}, nil)
	if err != nil || len(body.Messages) != 2 || body.Messages[0]["role"] != "system" ||
		body.Messages[0]["content"] != "developer policy" {
		t.Fatalf("messages=%+v err=%v", body.Messages, err)
	}
}
