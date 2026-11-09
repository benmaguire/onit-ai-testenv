package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-logr/logr"

	apic "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/client"
)

// Registrar makes an agent announce itself to a hub and keep doing so as a
// heartbeat, so hubs need no static configuration for agents that can reach them.
type Registrar struct {
	Hub      *apic.Client // hub URL + registration token
	Request  apic.RegisterRequest
	Interval time.Duration
	Log      logr.Logger
}

// Start runs until ctx is done (manager.Runnable).
func (r *Registrar) Start(ctx context.Context) error {
	interval := r.Interval
	if interval == 0 {
		interval = time.Minute
	}
	ok := false
	for {
		err := r.Hub.Do(ctx, http.MethodPost, r.Hub.BaseURL+"/api/v1/clusters/register", r.Request, nil)
		switch {
		case err != nil:
			r.Log.Error(err, "registering with hub failed; will retry", "hub", r.Hub.BaseURL)
			ok = false
		case !ok:
			r.Log.Info("registered with hub", "hub", r.Hub.BaseURL, "as", r.Request.Name, "endpoint", r.Request.Endpoint)
			ok = true
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

// NeedLeaderElection: only the leader registers.
func (r *Registrar) NeedLeaderElection() bool { return true }
