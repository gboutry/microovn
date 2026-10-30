package poc

import (
	"context"
	"sync"
	"time"
)

// Controller serializes explicit apply/cleanup with compatibility reconciliation.
// The worker lives for the daemon's lifetime and stops before cleanup begins.
type Controller struct {
	Runtime   Runtime
	Report    func(error)
	lifecycle sync.Mutex
	apply     sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
}

func (c *Controller) Enable(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	desired, err := c.Runtime.Config(ctx)
	if err != nil {
		return err
	}
	c.apply.Lock()
	err = c.Runtime.enable(ctx, desired)
	c.apply.Unlock()
	if desired.Neutron != nil && c.cancel == nil {
		worker, cancel := context.WithCancel(context.Background())
		c.cancel = cancel
		c.done = make(chan struct{})
		go func() {
			defer close(c.done)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-worker.Done():
					return
				case <-ticker.C:
					ctx, cancel := context.WithTimeout(worker, 30*time.Second)
					c.apply.Lock()
					err := c.Runtime.Enable(ctx)
					c.apply.Unlock()
					cancel()
					if err != nil && worker.Err() == nil && c.Report != nil {
						c.Report(err)
					}
				}
			}
		}()
	}
	return err
}

func (c *Controller) Disable(ctx context.Context) error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	if c.cancel != nil {
		c.cancel()
		<-c.done
		c.cancel = nil
	}
	c.apply.Lock()
	defer c.apply.Unlock()
	return c.Runtime.Disable(ctx)
}
