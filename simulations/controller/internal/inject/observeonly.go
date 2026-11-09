package inject

import (
	"context"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"
)

// observeOnly injects nothing: it gives any action outside the controller (a
// deploy, a load test, a manual change) the same baseline / during / after
// measurement as a fault.
type observeOnly struct{}

func (observeOnly) Entry() catalog.Entry {
	return catalog.Entry{
		Type: "observe-only", Category: "observe", Summary: "Measure a window without injecting anything",
		Effect:   "Nothing is changed. With spec.observe, the baseline / \"fault\" / after windows are measured around whatever happens meanwhile — run your own action (deploy, load test, failover) while it is Active, then recover it.",
		Recovery: "Nothing to revert; recovered immediately when duration elapses or recovery is requested.",
		Targets:  []string{"namespace", "podSelector"},
	}
}

func (o observeOnly) Validate(sim *simv1.Simulation) error { return checkKnownParams(o.Entry(), sim) }

func (observeOnly) Inject(context.Context, *Env, *simv1.Simulation) (Result, error) {
	return Result{Message: "observing (nothing injected)"}, nil
}

func (observeOnly) Recover(context.Context, *Env, *simv1.Simulation) (string, error) {
	return "observation window closed", nil
}

func (observeOnly) Verify(context.Context, *Env, *simv1.Simulation) (bool, string, error) {
	return true, "nothing to verify", nil
}
