package fingerprint

import (
	"strings"
	"testing"
)

// TestGatherDevicePosture runs the real checks on this machine; it only
// verifies they complete and fill in the basics.
func TestGatherDevicePosture(t *testing.T) {
	withCacheDir(t)
	fp, postures, _ := gatherDevicePosture()
	if fp.Platform != "linux" || fp.Hostname == "" || fp.KernelVersion == "" || fp.PlatformFingerprint == "" {
		t.Fatalf("incomplete fingerprint: %+v", fp)
	}
	t.Logf("fingerprint: %+v", *fp)
	t.Logf("postures: %+v", *postures)
}

const cpuinfoSMT = `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 140
model name	: 11th Gen Intel(R) Core(TM) i7-1165G7 @ 2.80GHz
stepping	: 1
siblings	: 8
cpu cores	: 4

processor	: 1
vendor_id	: GenuineIntel
siblings	: 8
`

func cpuinfoWith(siblings, cores string) string {
	return strings.NewReplacer("siblings	: 8", "siblings	: "+siblings, "cpu cores	: 4", "cpu cores	: "+cores).Replace(cpuinfoSMT)
}

// Audit #53: the first values are pinned, so the hash follows the CPU model and
// not the number of CPUs online; and the first computation equals the legacy one.
func TestCPUFingerprintPinsOnlineDependentFields(t *testing.T) {
	first, store := cpuFingerprintFrom(strings.NewReader(cpuinfoSMT), nil)
	want := "vendor=genuineintel|model_name=11th gen intel(r) core(tm) i7-1165g7 @ 2.80ghz|family=6|model=140|stepping=1|cores=4|siblings=8"
	if first != want {
		t.Fatalf("first fingerprint differs from the legacy form:\n got %s\nwant %s", first, want)
	}
	if store == nil {
		t.Fatal("topology not stored on first use")
	}

	// Booted with nosmt: siblings 4, same machine.
	again, store2 := cpuFingerprintFrom(strings.NewReader(cpuinfoWith("4", "4")), store)
	if again != first || store2 != nil {
		t.Fatalf("online-CPU change altered the fingerprint: %s (store=%v)", again, store2)
	}

	// A different CPU model is a different machine: re-pinned.
	other := strings.Replace(cpuinfoSMT, "i7-1165G7", "i9-12900K", 1)
	changed, store3 := cpuFingerprintFrom(strings.NewReader(other), store)
	if changed == first || store3 == nil {
		t.Fatalf("CPU model change not reflected: %s", changed)
	}
}

func TestCPUFingerprintWithoutTopologyFields(t *testing.T) {
	arm := "processor\t: 0\nCPU implementer\t: 0x41\nCPU part\t: 0xd08\nCPU revision\t: 3\n"
	fp, store := cpuFingerprintFrom(strings.NewReader(arm), nil)
	if fp != "implementer=0x41|part=0xd08|revision=3" || store == nil {
		t.Fatalf("fp=%q store=%v", fp, store)
	}
	if empty, store := cpuFingerprintFrom(strings.NewReader(""), nil); empty != "" || store != nil {
		t.Fatalf("empty cpuinfo: %q %v", empty, store)
	}
}

func TestValidMachineID(t *testing.T) {
	good := "0123456789abcdef0123456789ABCDEF"
	if validMachineID(good+"\n") != strings.ToLower(good) {
		t.Error("valid id rejected")
	}
	for _, bad := range []string{"", "uninitialized", strings.Repeat("0", 32), "abc", strings.Repeat("g", 32)} {
		if validMachineID(bad) != "" {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Audit #54 fixtures. Docker/libvirt-style rulesets have NAT and forward chains
// but filter nothing inbound.
const nftDockerOnly = `{"nftables":[{"metainfo":{"version":"1.0.9"}},
{"table":{"family":"ip","name":"nat","handle":1}},
{"chain":{"family":"ip","table":"nat","name":"POSTROUTING","type":"nat","hook":"postrouting","prio":100,"policy":"accept"}},
{"rule":{"family":"ip","table":"nat","chain":"POSTROUTING","expr":[{"match":{}},{"masquerade":null}]}},
{"chain":{"family":"ip","table":"filter","name":"FORWARD","type":"filter","hook":"forward","prio":0,"policy":"drop"}},
{"rule":{"family":"ip","table":"filter","chain":"FORWARD","expr":[{"jump":{"target":"DOCKER-USER"}}]}},
{"chain":{"family":"ip","table":"filter","name":"DOCKER-USER"}},
{"rule":{"family":"ip","table":"filter","chain":"DOCKER-USER","expr":[{"drop":null}]}},
{"chain":{"family":"ip","table":"filter","name":"INPUT","type":"filter","hook":"input","prio":0,"policy":"accept"}},
{"rule":{"family":"ip","table":"filter","chain":"INPUT","expr":[{"match":{}},{"accept":null}]}}]}`

const nftDropPolicy = `{"nftables":[{"chain":{"family":"inet","table":"filter","name":"input","type":"filter","hook":"input","prio":0,"policy":"drop"}}]}`

const nftJumpToDrop = `{"nftables":[
{"chain":{"family":"inet","table":"fw","name":"input","type":"filter","hook":"input","prio":0,"policy":"accept"}},
{"rule":{"family":"inet","table":"fw","chain":"input","expr":[{"jump":{"target":"zones"}}]}},
{"chain":{"family":"inet","table":"fw","name":"zones"}},
{"rule":{"family":"inet","table":"fw","chain":"zones","expr":[{"match":{}},{"counter":{}},{"reject":{"type":"icmpx"}}]}}]}`

const nftDropOnlyInForward = `{"nftables":[
{"chain":{"family":"inet","table":"fw","name":"input","type":"filter","hook":"input","prio":0,"policy":"accept"}},
{"chain":{"family":"inet","table":"fw","name":"other"}},
{"rule":{"family":"inet","table":"fw","chain":"other","expr":[{"drop":null}]}}]}`

func TestNftInputFilters(t *testing.T) {
	tests := []struct {
		name, in      string
		filters, okay bool
	}{
		{"docker only", nftDockerOnly, false, true},
		{"drop policy", nftDropPolicy, true, true},
		{"jump to reject", nftJumpToDrop, true, true},
		{"drop in an unreachable chain", nftDropOnlyInForward, false, true},
		{"empty ruleset", `{"nftables":[{"metainfo":{}}]}`, false, true},
		{"not json", `table ip nat {}`, false, false},
	}
	for _, tc := range tests {
		filters, ok := nftInputFilters(tc.in)
		if filters != tc.filters || ok != tc.okay {
			t.Errorf("%s: got (%v,%v) want (%v,%v)", tc.name, filters, ok, tc.filters, tc.okay)
		}
	}
}

func TestIptablesInputFilters(t *testing.T) {
	tests := []struct {
		name, in string
		want     bool
	}{
		{"docker", "-P INPUT ACCEPT\n-P FORWARD DROP\n-P OUTPUT ACCEPT\n-N DOCKER\n-N DOCKER-USER\n" +
			"-A FORWARD -j DOCKER-USER\n-A FORWARD -o docker0 -j DOCKER\n-A DOCKER ! -i docker0 -o docker0 -j DROP\n", false},
		{"libvirt", "-P INPUT ACCEPT\n-N LIBVIRT_INP\n-A INPUT -j LIBVIRT_INP\n" +
			"-A LIBVIRT_INP -i virbr0 -p udp -m udp --dport 53 -j ACCEPT\n", false},
		{"drop policy", "-P INPUT DROP\n", true},
		{"direct reject", "-P INPUT ACCEPT\n-A INPUT -p tcp --dport 23 -j REJECT --reject-with icmp-port-unreachable\n", true},
		{"jump chain drop", "-P INPUT ACCEPT\n-N f\n-A INPUT -j f\n-A f -m comment --comment \"a -j b\" -j DROP\n", true},
		{"jump loop terminates", "-N a\n-N b\n-A INPUT -j a\n-A a -j b\n-A b -j a\n", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		if got := iptablesInputFilters(tc.in); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}
