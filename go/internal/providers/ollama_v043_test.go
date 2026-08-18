package providers

import "testing"

func TestV043OllamaMapsDeveloperRoleToSystem(t *testing.T) {
	messages := ollamaNormalizeMessages([]Message{
		{"role": "developer", "content": "developer policy"},
		{"role": "user", "content": "hello"},
	})
	if len(messages) != 2 || messages[0]["role"] != "system" ||
		messages[0]["content"] != "developer policy" {
		t.Fatalf("messages=%+v", messages)
	}
}
