// Package inject implements the simulation types: each Injector knows how to
// inject one kind of failure, revert it, and verify the target recovered.
package inject

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/kube"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/pkg/catalog"
)

// Env is what injectors get to act on the cluster.
type Env struct {
	Client client.Client
	Exec   kube.Executor
	Log    logr.Logger
	// MokkaNamespace is where the nvml-mock DaemonSet runs.
	MokkaNamespace string
	// MokkaSelector selects nvml-mock DaemonSet pods.
	MokkaSelector map[string]string
	// DefaultGraph / DefaultGraphNamespace are used by dynamo-* types when the
	// target names no graph.
	DefaultGraph          string
	DefaultGraphNamespace string
	// Now is the clock (overridable in tests).
	Now func() time.Time
}

// Result is what Inject did, persisted in the Simulation status so Recover
// can undo exactly that.
type Result struct {
	Targets []simv1.AffectedTarget
	Data    map[string]string
	Message string
}

// Injector implements one simulation type.
type Injector interface {
	// Entry describes the type for the catalog.
	Entry() catalog.Entry
	// Validate checks spec before anything is done.
	Validate(sim *simv1.Simulation) error
	// Inject applies the fault. On error it returns whatever it already
	// changed so the controller can attempt recovery.
	Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error)
	// Recover reverts the fault using sim.Status.Targets / Data.
	Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error)
	// Verify reports whether the target is healthy again. Called repeatedly
	// after Recover until done or the recovery timeout.
	Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (done bool, msg string, err error)
}

// Registry maps simulation types to injectors.
type Registry struct{ m map[string]Injector }

// NewRegistry returns a registry with the given injectors.
func NewRegistry(injectors ...Injector) *Registry {
	r := &Registry{m: map[string]Injector{}}
	for _, i := range injectors {
		t := i.Entry().Type
		if _, dup := r.m[t]; dup {
			panic("duplicate simulation type " + t)
		}
		r.m[t] = i
	}
	return r
}

// Get returns the injector for a type.
func (r *Registry) Get(t string) (Injector, bool) { i, ok := r.m[t]; return i, ok }

// Catalog lists every type, sorted by category then type.
func (r *Registry) Catalog() []catalog.Entry {
	out := make([]catalog.Entry, 0, len(r.m))
	for _, i := range r.m {
		out = append(out, i.Entry())
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Category != out[b].Category {
			return out[a].Category < out[b].Category
		}
		return out[a].Type < out[b].Type
	})
	return out
}

// --- parameter helpers -------------------------------------------------------

// params resolves spec.params against the entry's defaults.
type params struct {
	vals map[string]string
}

func newParams(e catalog.Entry, sim *simv1.Simulation) params {
	p := params{vals: map[string]string{}}
	for _, d := range e.Params {
		if d.Default != "" {
			p.vals[d.Name] = d.Default
		}
	}
	for k, v := range sim.Spec.Params {
		p.vals[k] = v
	}
	return p
}

func (p params) str(k string) string { return strings.TrimSpace(p.vals[k]) }

func (p params) int(k string, min, max int) (int, error) {
	s := p.str(k)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("param %q: %q is not an integer", k, s)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("param %q: %d out of range [%d, %d]", k, n, min, max)
	}
	return n, nil
}

func (p params) float(k string, min, max float64) (float64, error) {
	s := p.str(k)
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("param %q: %q is not a number", k, s)
	}
	if f < min || f > max {
		return 0, fmt.Errorf("param %q: %v out of range [%v, %v]", k, f, min, max)
	}
	return f, nil
}

func (p params) bool(k string) (bool, error) {
	s := p.str(k)
	if s == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("param %q: %q is not a boolean", k, s)
	}
	return b, nil
}

func (p params) list(k string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(p.str(k), func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func oneOf(k, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("param %q: %q must be one of %s", k, v, strings.Join(allowed, ", "))
}

// checkKnownParams rejects params the entry does not declare (catches typos).
func checkKnownParams(e catalog.Entry, sim *simv1.Simulation) error {
	known := map[string]bool{}
	for _, p := range e.Params {
		known[p.Name] = true
	}
	for k := range sim.Spec.Params {
		if !known[k] {
			return fmt.Errorf("unknown param %q for type %s", k, e.Type)
		}
	}
	for _, p := range e.Params {
		if p.Required && p.Default == "" && strings.TrimSpace(sim.Spec.Params[p.Name]) == "" {
			return fmt.Errorf("param %q is required for type %s", p.Name, e.Type)
		}
	}
	return nil
}
