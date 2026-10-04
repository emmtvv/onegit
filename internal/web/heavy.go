package web

import (
	"context"
	"errors"
	"runtime"
	"time"
)

// errBusy means every slot for heavy git work (code search, blame, the
// tree's last commits) stayed taken: such requests queue for a while and
// then give up, instead of piling up git processes until the host stalls.
var errBusy = errors.New("the server is busy, try again in a moment")

// heavySlots bounds concurrent heavy git operations per replica.
var heavySlots = max(2, runtime.NumCPU())

// heavyWait is how long a request waits for a slot.
const heavyWait = 10 * time.Second

// acquireHeavy takes a slot for heavy git work, waiting up to wait.
func (w *Web) acquireHeavy(ctx context.Context, wait time.Duration) (release func(), err error) {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case w.heavy <- struct{}{}:
		return func() { <-w.heavy }, nil
	case <-t.C:
		return nil, errBusy
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
