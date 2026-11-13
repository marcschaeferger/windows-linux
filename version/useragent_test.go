package version

import (
	"runtime"
	"testing"
)

func TestArchDoesNotPanic(t *testing.T) {
	if got := Arch(); got == "" {
		t.Fatalf("Arch() on %s returned an empty string", runtime.GOARCH)
	}
}
