package updater

import (
	"strings"
	"testing"
)

func TestManualUpdateFor(t *testing.T) {
	defer func(orig func() bool) { imageBased = orig }(imageBased)

	imageBased = func() bool { return true }
	instructions, command := manualUpdateFor(formatRPM)
	if !strings.Contains(instructions, "ostree") || !strings.HasPrefix(command, "rpm-ostree install") {
		t.Errorf("rpm on an image-based system: %q %q", instructions, command)
	}
	// The format is only about rpm; other packages are unaffected.
	if instructions, _ := manualUpdateFor(formatDeb); instructions != "" {
		t.Errorf("deb on an image-based system got instructions: %q", instructions)
	}

	imageBased = func() bool { return false }
	if instructions, command := manualUpdateFor(formatRPM); instructions != "" || command != "" {
		t.Errorf("rpm on a regular system should update in place, got %q %q", instructions, command)
	}
	if instructions, _ := manualUpdateFor(formatAUR); instructions == "" {
		t.Error("AUR installs should still be told to use their helper")
	}
	if instructions, _ := manualUpdateFor(formatNone); instructions == "" {
		t.Error("hand-installed builds should still get download instructions")
	}
}
