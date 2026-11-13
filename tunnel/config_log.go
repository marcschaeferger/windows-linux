package tunnel

import (
	"fmt"

	"github.com/fosrl/windows-linux/logging"
)

// String formats the config for logs with the olm secret and the user token
// masked. Every %v and %+v of a Config, or a pointer to one, goes through it,
// so a log line can't leak the credentials. The device fingerprint and
// posture snapshots are summarized by size only.
func (c Config) String() string {
	type plain Config // no methods, so Sprintf doesn't call String again
	p := plain(c)
	p.Secret = maskSecret(c.Secret)
	p.UserToken = maskSecret(c.UserToken)
	p.InitialFingerprint = nil
	p.InitialPostures = nil
	return fmt.Sprintf("%+v initialFingerprint:%dB initialPostures:%dB",
		p, len(c.InitialFingerprint), len(c.InitialPostures))
}

// GoString keeps %#v from printing the credentials too.
func (c Config) GoString() string { return "tunnel.Config" + c.String() }

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return logging.Redacted
}

// RegisterSecrets tells the log redaction about this config's credentials, so
// they are masked in every line this process writes, including lines olm logs
// with its own copy of the config.
func (c Config) RegisterSecrets() {
	logging.RegisterSecrets(c.Secret, c.UserToken)
}
