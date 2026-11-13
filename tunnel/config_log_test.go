package tunnel

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fosrl/windows-linux/logging"
)

func TestConfigFormattingNeverShowsCredentials(t *testing.T) {
	cfg := Config{
		Name:               "pangolin",
		Endpoint:           "https://pangolin.example.com",
		ID:                 "olm-id-1",
		Secret:             "olm-secret-VALUE-123456",
		UserToken:          "session-token-VALUE-654321",
		InitialFingerprint: json.RawMessage(`{"serial":"SN-0001"}`),
		InitialPostures:    json.RawMessage(`{"a":1}`),
	}
	for _, format := range []string{"%v", "%+v", "%s", "%#v"} {
		for _, arg := range []any{cfg, &cfg} {
			out := fmt.Sprintf(format, arg)
			for _, leak := range []string{"olm-secret-VALUE-123456", "session-token-VALUE-654321", "SN-0001"} {
				if strings.Contains(out, leak) {
					t.Errorf("%s of %T leaked %q: %s", format, arg, leak, out)
				}
			}
			if !strings.Contains(out, "olm-id-1") || !strings.Contains(out, "pangolin.example.com") {
				t.Errorf("%s of %T dropped non-secret fields: %s", format, arg, out)
			}
		}
	}
	// The real value is untouched, since the IPC and the config file use JSON.
	b, err := json.Marshal(cfg)
	if err != nil || !strings.Contains(string(b), "olm-secret-VALUE-123456") {
		t.Errorf("JSON encoding must keep the secret, got %s (%v)", b, err)
	}
}

func TestRegisterSecretsScrubsRawValues(t *testing.T) {
	cfg := Config{Secret: "raw-secret-ABCDEFGH", UserToken: "raw-token-12345678"}
	cfg.RegisterSecrets()
	got := logging.Redact("olm dump: {Secret:raw-secret-ABCDEFGH UserToken:raw-token-12345678} again raw-token-12345678")
	if strings.Contains(got, "raw-secret-ABCDEFGH") || strings.Contains(got, "raw-token-12345678") {
		t.Errorf("registered secrets leaked: %s", got)
	}
}
