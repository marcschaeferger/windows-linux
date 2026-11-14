package managers

import (
	"errors"
	"sync"
	"testing"
)

func TestCleanupStaleDNS(t *testing.T) {
	// Reset the package state for this test.
	staleDNSCleanupDone = make(chan struct{})
	staleDNSCleanupDoneOnce = sync.Once{}

	var got string
	cleanupStaleDNS(func(name string) error {
		got = name
		return errors.New("nmcli failed")
	})
	if got != "Pangolin" {
		t.Errorf("cleaned %q, want the tunnel interface Pangolin", got)
	}
	select {
	case <-staleDNSCleanupDone:
	default:
		t.Fatal("cleanup did not release waiting tunnel starts after an error")
	}
	waitForStaleDNSCleanup() // returns at once
}
