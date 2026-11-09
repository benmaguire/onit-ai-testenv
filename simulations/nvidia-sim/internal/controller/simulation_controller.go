// Package controller reconciles Simulations: inject → hold → recover → verify.
package controller

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/nvidia-sim/internal/inject"
)

const (
	// Finalizer guarantees an injected fault is reverted before the
	// Simulation disappears.
	Finalizer = "sim.testenv.onit.ai/recover"

	condInjected  = "Injected"
	condRecovered = "Recovered"

	defaultRecoveryTimeout = 5 * time.Minute
	verifyInterval         = 3 * time.Second
)

// SimulationReconciler drives Simulation objects.
type SimulationReconciler struct {
	Client   client.Client
	Env      *inject.Env
	Registry *inject.Registry
	Recorder record.EventRecorder
	Now      func() time.Time
}

func (r *SimulationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// SetupWithManager registers the controller. Simulations are processed one at
// a time: nvml-mock-ctl edits a shared per-node overrides file.
func (r *SimulationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&simv1.Simulation{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// +kubebuilder:rbac:groups=sim.testenv.onit.ai,resources=simulations;simulations/status;simulations/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sim.testenv.onit.ai,resources=simulationclusters;simulationclusters/status,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create;get
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete

// Reconcile advances one Simulation through its phases.
func (r *SimulationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	sim := &simv1.Simulation{}
	if err := r.Client.Get(ctx, req.NamespacedName, sim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	inj, known := r.Registry.Get(sim.Spec.Type)

	// Deletion: revert first if the fault is (or may be) in place.
	if !sim.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(sim, Finalizer) {
			if known && needsRecovery(sim.Status.Phase) {
				if msg, err := inj.Recover(ctx, r.Env, sim); err != nil {
					logger.Error(err, "recovery on delete failed; removing finalizer anyway", "msg", msg)
					r.event(sim, "Warning", "RecoverFailed", err.Error())
				} else {
					r.event(sim, "Normal", "Recovered", "reverted on delete: "+msg)
				}
			}
			controllerutil.RemoveFinalizer(sim, Finalizer)
			return ctrl.Result{}, r.Client.Update(ctx, sim)
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(sim, Finalizer) {
		controllerutil.AddFinalizer(sim, Finalizer)
		if err := r.Client.Update(ctx, sim); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !known {
		return r.fail(ctx, sim, fmt.Sprintf("unknown simulation type %q (see GET /api/v1/catalog)", sim.Spec.Type))
	}
	now := r.now()

	switch sim.Status.Phase {
	case "", simv1.PhasePending, simv1.PhaseWaiting:
		if err := inj.Validate(sim); err != nil {
			return r.fail(ctx, sim, "invalid: "+err.Error())
		}
		if sim.Spec.Recover {
			sim.Status.Phase = simv1.PhaseRecovered
			sim.Status.Message = "cancelled before injection"
			sim.Status.RecoveredAt = &metav1.Time{Time: now}
			return r.finish(ctx, sim)
		}
		if d := sim.Spec.Delay; d != nil {
			start := sim.CreationTimestamp.Add(d.Duration)
			if now.Before(start) {
				if sim.Status.Phase != simv1.PhaseWaiting {
					sim.Status.Phase = simv1.PhaseWaiting
					sim.Status.Message = "injecting at " + start.UTC().Format(time.RFC3339)
					if err := r.status(ctx, sim); err != nil {
						return ctrl.Result{}, err
					}
				}
				return ctrl.Result{RequeueAfter: start.Sub(now)}, nil
			}
		}
		return r.inject(ctx, sim, inj, now)

	case simv1.PhaseActive:
		due := sim.Status.RecoverAt != nil && !now.Before(sim.Status.RecoverAt.Time)
		if sim.Spec.Recover || due {
			return r.recover(ctx, sim, inj, now)
		}
		if sim.Status.RecoverAt != nil {
			return ctrl.Result{RequeueAfter: sim.Status.RecoverAt.Sub(now)}, nil
		}
		return ctrl.Result{}, nil // held until spec.recover or delete

	case simv1.PhaseRecovering:
		done, msg, err := inj.Verify(ctx, r.Env, sim)
		if err != nil {
			logger.Error(err, "verify")
			msg = err.Error()
		}
		if done {
			sim.Status.Phase = simv1.PhaseRecovered
			sim.Status.RecoveredAt = &metav1.Time{Time: now}
			if sim.Status.Data == nil {
				sim.Status.Data = map[string]string{}
			}
			if sim.Status.InjectedAt != nil {
				sim.Status.Data["timeToRecover"] = now.Sub(sim.Status.InjectedAt.Time).Round(time.Second).String()
			}
			if sim.Status.RecoveryStartedAt != nil {
				sim.Status.Data["recoveryDuration"] = now.Sub(sim.Status.RecoveryStartedAt.Time).Round(time.Second).String()
			}
			sim.Status.Message = msg
			setCond(sim, condRecovered, metav1.ConditionTrue, "Verified", msg)
			r.event(sim, "Normal", "Recovered", msg)
			return r.finish(ctx, sim)
		}
		timeout := defaultRecoveryTimeout
		if sim.Spec.RecoveryTimeout != nil {
			timeout = sim.Spec.RecoveryTimeout.Duration
		}
		if sim.Status.RecoveryStartedAt != nil && now.Sub(sim.Status.RecoveryStartedAt.Time) > timeout {
			return r.fail(ctx, sim, fmt.Sprintf("not recovered within %s: %s", timeout, msg))
		}
		if sim.Status.Message != msg {
			sim.Status.Message = msg
			if err := r.status(ctx, sim); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: verifyInterval}, nil

	case simv1.PhaseRecovered, simv1.PhaseFailed:
		return r.ttl(ctx, sim, now)
	}
	return ctrl.Result{}, nil
}

func (r *SimulationReconciler) inject(ctx context.Context, sim *simv1.Simulation, inj inject.Injector, now time.Time) (ctrl.Result, error) {
	res, err := inj.Inject(ctx, r.Env, sim)
	sim.Status.Targets = res.Targets
	sim.Status.Data = res.Data
	sim.Status.InjectedAt = &metav1.Time{Time: now}
	if err != nil {
		// Undo whatever was applied before the error.
		if len(res.Targets) > 0 {
			if _, rerr := inj.Recover(ctx, r.Env, sim); rerr != nil {
				err = fmt.Errorf("%w (and partial rollback failed: %v)", err, rerr)
			}
		}
		setCond(sim, condInjected, metav1.ConditionFalse, "InjectFailed", err.Error())
		return r.fail(ctx, sim, "inject failed: "+err.Error())
	}
	sim.Status.Phase = simv1.PhaseActive
	sim.Status.Message = res.Message
	if sim.Spec.Duration != nil {
		sim.Status.RecoverAt = &metav1.Time{Time: now.Add(sim.Spec.Duration.Duration)}
	}
	setCond(sim, condInjected, metav1.ConditionTrue, "Injected", res.Message)
	r.event(sim, "Normal", "Injected", res.Message)
	if err := r.status(ctx, sim); err != nil {
		return ctrl.Result{}, err
	}
	if sim.Status.RecoverAt != nil {
		return ctrl.Result{RequeueAfter: sim.Status.RecoverAt.Sub(now)}, nil
	}
	return ctrl.Result{}, nil
}

func (r *SimulationReconciler) recover(ctx context.Context, sim *simv1.Simulation, inj inject.Injector, now time.Time) (ctrl.Result, error) {
	msg, err := inj.Recover(ctx, r.Env, sim)
	if err != nil {
		r.event(sim, "Warning", "RecoverFailed", err.Error())
		sim.Status.Message = "recover failed, retrying: " + err.Error()
		if serr := r.status(ctx, sim); serr != nil {
			return ctrl.Result{}, serr
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	sim.Status.Phase = simv1.PhaseRecovering
	sim.Status.RecoveryStartedAt = &metav1.Time{Time: now}
	sim.Status.Message = msg
	setCond(sim, condRecovered, metav1.ConditionFalse, "Verifying", msg)
	r.event(sim, "Normal", "Recovering", msg)
	if err := r.status(ctx, sim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *SimulationReconciler) fail(ctx context.Context, sim *simv1.Simulation, msg string) (ctrl.Result, error) {
	sim.Status.Phase = simv1.PhaseFailed
	sim.Status.Message = msg
	r.event(sim, "Warning", "Failed", msg)
	return r.finish(ctx, sim)
}

func (r *SimulationReconciler) finish(ctx context.Context, sim *simv1.Simulation) (ctrl.Result, error) {
	if err := r.status(ctx, sim); err != nil {
		return ctrl.Result{}, err
	}
	return r.ttl(ctx, sim, r.now())
}

func (r *SimulationReconciler) ttl(ctx context.Context, sim *simv1.Simulation, now time.Time) (ctrl.Result, error) {
	if sim.Spec.TTLAfterFinished == nil {
		return ctrl.Result{}, nil
	}
	end := sim.Status.RecoveredAt
	if end == nil {
		end = &sim.CreationTimestamp
		if sim.Status.InjectedAt != nil {
			end = sim.Status.InjectedAt
		}
	}
	at := end.Add(sim.Spec.TTLAfterFinished.Duration)
	if now.Before(at) {
		return ctrl.Result{RequeueAfter: at.Sub(now)}, nil
	}
	return ctrl.Result{}, client.IgnoreNotFound(r.Client.Delete(ctx, sim))
}

func (r *SimulationReconciler) status(ctx context.Context, sim *simv1.Simulation) error {
	sim.Status.ObservedGeneration = sim.Generation
	err := r.Client.Status().Update(ctx, sim)
	if apierrors.IsConflict(err) {
		return fmt.Errorf("status conflict, will retry: %w", err)
	}
	return err
}

func (r *SimulationReconciler) event(sim *simv1.Simulation, typ, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(sim, typ, reason, msg)
	}
}

// needsRecovery: phases in which a fault may be applied.
func needsRecovery(p simv1.Phase) bool {
	return p == simv1.PhaseActive || p == simv1.PhaseRecovering
}

func setCond(sim *simv1.Simulation, typ string, st metav1.ConditionStatus, reason, msg string) {
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	meta.SetStatusCondition(&sim.Status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: sim.Generation,
	})
}
