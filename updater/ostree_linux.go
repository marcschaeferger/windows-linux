package updater

import "os"

// imageBased reports whether this is an image-based system (Fedora Silverblue,
// Kinoite and other ostree deployments). Its root filesystem is read-only, so
// "dnf install" of the update fails; the package has to be layered with
// rpm-ostree and takes effect after a reboot. A variable, so tests can set it.
var imageBased = func() bool {
	_, err := os.Stat("/run/ostree-booted")
	return err == nil
}

// ostreeUpdateInstructions is what ManualUpdate returns on an image-based
// system for an rpm install.
func ostreeUpdateInstructions() (instructions, command string) {
	return "This is an image-based system (ostree), where Pangolin can't replace its own files. " +
			"Download the new rpm from https://github.com/fosrl/windows-linux/releases?q=linux-&expanded=true, " +
			"layer it, then reboot:",
		"rpm-ostree install --idempotent pangolin-client-*.rpm"
}
