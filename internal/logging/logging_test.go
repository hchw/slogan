package logging

import (
	"bytes"
	"strings"
	"testing"
)

func TestSensitiveFieldsAreRedacted(t *testing.T) {
	var out bytes.Buffer
	log := NewWithWriter("test", &out)
	log.Info("provider configured", "api_key", "sk-secret-value", "Authorization", "Bearer secret", "prompt", "private body", "provider_id", 4)
	got := out.String()
	for _, leaked := range []string{"sk-secret-value", "Bearer secret", "private body"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("log leaked sensitive value %q: %s", leaked, got)
		}
	}
	if strings.Count(got, "[REDACTED]") < 3 || !strings.Contains(got, `"provider_id":4`) {
		t.Fatalf("unexpected redacted log: %s", got)
	}
}
