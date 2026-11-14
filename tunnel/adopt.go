package tunnel

import "sync/atomic"

// adoptCheck asks the manager whether a running tunnel may be adopted by
// this UI: not when another user on the computer started it.
var adoptCheck atomic.Pointer[func() bool]

// SetAdoptCheck sets the check AdoptRunningTunnel makes before adopting.
func SetAdoptCheck(fn func() bool) {
	adoptCheck.Store(&fn)
}

func mayAdopt() bool {
	fn := adoptCheck.Load()
	return fn == nil || *fn == nil || (*fn)()
}
