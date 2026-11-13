package logging

import (
	"fmt"
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "Go %+v struct dump",
			in:   "Starting tunnel with config: {Endpoint:https://p.example.com ID:olm-1 Secret:s3cr3tValue99 UserToken:tok_abcdef123456 MTU:1280}",
			want: "Starting tunnel with config: {Endpoint:https://p.example.com ID:olm-1 Secret:[REDACTED] UserToken:[REDACTED] MTU:1280}",
		},
		{
			name: "JSON body",
			in:   `Requesting token with body: {"olmId":"abc","secret":"hunter2hunter2","userToken":"u.t.k\"en","orgId":"o1"}`,
			want: `Requesting token with body: {"olmId":"abc","secret":"[REDACTED]","userToken":"[REDACTED]","orgId":"o1"}`,
		},
		{
			name: "JSON inside a %q string",
			in:   `system query raw output: "{\"serialNumber\":\"PF2ABCDE\",\"diskEncrypted\":true}"`,
			want: `system query raw output: "{\"serialNumber\":\"[REDACTED]\",\"diskEncrypted\":true}"`,
		},
		{
			name: "key=value and quoted value",
			in:   `login token=abc123def456 serial="XYZ-0001" password: pw!1`,
			want: `login token=[REDACTED] serial="[REDACTED]" password: [REDACTED]`,
		},
		{
			name: "map dump",
			in:   "headers map[Authorization:Bearer abc.def.ghi1234 X-Other:1]",
			want: "headers map[Authorization:[REDACTED] X-Other:1]",
		},
		{
			name: "bearer anywhere",
			in:   "sent header Bearer eyJhbGciOi.payload.sig",
			want: "sent header Bearer [REDACTED]",
		},
		{
			name: "flags stay readable",
			in:   "GetUserSecrets (userId=u1, hasSessionToken=true, hasOlmId=true, hasOlmSecret=false)",
			want: "GetUserSecrets (userId=u1, hasSessionToken=true, hasOlmId=true, hasOlmSecret=false)",
		},
		{
			name: "empty and nil values stay",
			in:   "Secret: UserToken:<nil> token=null",
			want: "Secret: UserToken:<nil> token=null",
		},
		{
			name: "ordinary line untouched",
			in:   "Auth: active account found (userId=u1), loading session",
			want: "Auth: active account found (userId=u1), loading session",
		},
		{
			name: "single quotes",
			in:   "connecting with secret='abcdefgh1234' to host",
			want: "connecting with secret='[REDACTED]' to host",
		},
		{
			name: "quoted value with spaces is masked whole",
			in:   `password: "my long pass phrase" next=1`,
			want: `password: "[REDACTED]" next=1`,
		},
		{
			name: "JSON array and number values",
			in:   `{"secret":["abcdefg","hijklmn"],"serialNumber": 12345678,"x":1}`,
			want: `{"secret":"[REDACTED]","serialNumber": "[REDACTED]","x":1}`,
		},
		{
			name: "quoted JSON whose value has escapes",
			in:   fmt.Sprintf("%q", `{"secret":"a\"b\\c\nd","orgId":"o1"}`),
			want: `"{\"secret\":\"[REDACTED]\",\"orgId\":\"o1\"}"`,
		},
		{
			name: "wireguard private key",
			in:   "interface privateKey=aGVsbG8gd29ybGQ= up, psk: AAAABBBBCCCC",
			want: "interface privateKey=[REDACTED] up, psk: [REDACTED]",
		},
		{
			name: "error text after token keeps its meaning",
			in:   "Failed to open thread token: Access is denied",
			want: "Failed to open thread token: Access is denied",
		},
		{
			name: "several secrets in one line",
			in:   "olmSecret=aaaaaaaa newtSecret=bbbbbbbb",
			want: "olmSecret=[REDACTED] newtSecret=[REDACTED]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Redact(tt.in)
			if got != tt.want {
				t.Errorf("Redact(%q)\n got %q\nwant %q", tt.in, got, tt.want)
			}
			if again := Redact(got); again != got {
				t.Errorf("Redact is not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestRegisterSecrets(t *testing.T) {
	RegisterSecrets("short", "  longsecretvalue1  ", "another-secret-2")
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.values, registry.replacer = nil, nil
		registry.mu.Unlock()
	})

	got := Redact("config dump (longsecretvalue1) and another-secret-2 and short")
	want := "config dump ([REDACTED]) and [REDACTED] and short"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRegisterSecretsBounded(t *testing.T) {
	t.Cleanup(func() {
		registry.mu.Lock()
		registry.values, registry.replacer = nil, nil
		registry.mu.Unlock()
	})
	for i := 0; i < maxRegisteredSecrets+10; i++ {
		RegisterSecrets(strings.Repeat("x", minSecretLen) + string(rune('A'+i%26)) + strings.Repeat("y", i))
	}
	registry.mu.RLock()
	n := len(registry.values)
	registry.mu.RUnlock()
	if n > maxRegisteredSecrets {
		t.Errorf("registry grew to %d", n)
	}
}
