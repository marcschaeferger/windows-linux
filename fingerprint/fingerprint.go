package fingerprint

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"runtime"
	"strings"
)

type Fingerprint struct {
	Username            string `json:"username"`
	Hostname            string `json:"hostname"`
	Platform            string `json:"platform"`
	OSVersion           string `json:"osVersion"`
	KernelVersion       string `json:"kernelVersion"`
	Architecture        string `json:"arch"`
	DeviceModel         string `json:"deviceModel"`
	SerialNumber        string `json:"serialNumber"`
	PlatformFingerprint string `json:"platformFingerprint"`
}

func GatherFingerprintInfo() *Fingerprint {
	fp, _, _ := gatherDevicePosture()
	return fp
}

func GatherPostureChecks() *PostureChecks {
	_, postures, _ := gatherDevicePosture()
	return postures
}

func (p *Fingerprint) ToMap() map[string]any {
	b, err := json.Marshal(p)
	if err != nil {
		return map[string]any{}
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}

	return m
}

func (p *PostureChecks) ToMap() map[string]any {
	b, err := json.Marshal(p)
	if err != nil {
		return map[string]any{}
	}

	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return map[string]any{}
	}

	return m
}

func computePlatformFingerprint(serialNumber string) string {
	return platformFingerprintHash(runtime.GOOS, runtime.GOARCH, cpuFingerprint(), dmiFingerprint(),
		serialNumber, machineUniqueID)
}

// platformFingerprintHash hashes the fingerprint inputs.
//
// A machine with a real hardware serial gets exactly the hash earlier versions
// produced, so devices already registered keep their fingerprint. Only when
// there is no usable serial (empty or an OEM placeholder), where identical
// machines would otherwise hash alike, a per-install identifier is mixed in.
// machineID is only called then.
func platformFingerprintHash(goos, goarch, cpu, dmi, serialNumber string, machineID func() string) string {
	parts := []string{goos, goarch, cpu, dmi}

	if isUsefulSerial(serialNumber) {
		parts = append(parts, "serial="+normalize(serialNumber))
	} else if id := machineID(); id != "" {
		parts = append(parts, "machine="+id)
	}

	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}

	raw := strings.Join(out, "|")
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

func isUsefulSerial(serial string) bool {
	serial = strings.TrimSpace(serial)
	if serial == "" {
		return false
	}

	switch normalize(serial) {
	case "to be filled by o.e.m.",
		"to be filled by oem",
		"to be filled by o.e.m",
		"default string",
		"none",
		"not specified",
		"not applicable",
		"not available",
		"n/a",
		"na",
		"unknown",
		"o.e.m.",
		"oem",
		"system serial number",
		"serial number",
		"chassis serial number",
		"base board serial number",
		"0123456789",
		"123456789",
		"1234567890",
		"00000000":
		return false
	}

	// A run of one repeated character ("0000000000", "FFFFFFFF", "xxxxxxxx")
	// is a filler, not a serial.
	if len(serial) >= 4 && strings.Trim(serial, serial[:1]) == "" {
		return false
	}

	return true
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Join(strings.Fields(s), " ")
}

// fingerprintAppID keys the machine identifier hash. Like systemd's
// app-specific machine ids, it keeps the raw /etc/machine-id (or MachineGuid)
// out of anything sent to the server.
const fingerprintAppID = "pangolin-client platform fingerprint"

// appSpecificID derives the value that goes into the fingerprint from a raw
// machine identifier.
func appSpecificID(raw string) string {
	mac := hmac.New(sha256.New, []byte(strings.ToLower(strings.TrimSpace(raw))))
	mac.Write([]byte(fingerprintAppID))
	return hex.EncodeToString(mac.Sum(nil))
}

// machineUniqueID returns a stable identifier of this OS install, or "" if
// none can be had. It prefers the identifier the OS keeps (so it survives
// reinstalling Pangolin) and falls back to a random one stored next to the
// serial cache.
func machineUniqueID() string {
	if raw := osMachineID(); raw != "" {
		return appSpecificID(raw)
	}
	if id := loadOrCreateInstallID(newInstallID); id != "" {
		return appSpecificID(id)
	}
	return ""
}

func newInstallID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}
