package tunnel

import "testing"

func TestAdoptRunningTunnelAsksManager(t *testing.T) {
	asked := false
	SetAdoptCheck(func() bool { asked = true; return false })
	t.Cleanup(func() { SetAdoptCheck(nil) })
	tm := &Manager{}
	if tm.AdoptRunningTunnel("") {
		t.Fatal("adopted a tunnel the manager says belongs to another user")
	}
	if !asked {
		t.Fatal("the manager was not asked")
	}
}

func TestMayAdoptWithoutCheck(t *testing.T) {
	SetAdoptCheck(nil)
	if !mayAdopt() {
		t.Fatal("without a manager check, adoption is refused")
	}
}
