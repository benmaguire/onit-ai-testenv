package api

import (
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/types"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
	"github.com/benmaguire/onit-ai-testenv/simulations/controller/internal/observe"
	apic "github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/client"
)

func (s *Server) observer() *observe.Observer {
	if s.Observer == nil {
		s.Observer = &observe.Observer{Profiles: observe.Builtin}
	}
	return s.Observer
}

func (s *Server) getSim(w http.ResponseWriter, r *http.Request) *simv1.Simulation {
	sim := &simv1.Simulation{}
	key := types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := s.Client.Get(r.Context(), key, sim); err != nil {
		writeErr(w, statusFor(err), err)
		return nil
	}
	return sim
}

// report: the simulation's timeline and measurements. While the observation
// is not complete the measurements are computed live (not stored).
func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim(w, r)
	if sim == nil {
		return
	}
	o := s.observer()
	rep := apic.Report{
		Cluster: s.ClusterName, Namespace: sim.Namespace, Name: sim.Name, Type: sim.Spec.Type,
		Phase: sim.Status.Phase, Message: sim.Status.Message, Targets: sim.Status.Targets, Data: sim.Status.Data,
		InjectedAt: sim.Status.InjectedAt, RecoveryStartedAt: sim.Status.RecoveryStartedAt, RecoveredAt: sim.Status.RecoveredAt,
	}
	for _, wdw := range observe.Windows(sim, time.Now()) {
		rep.Windows = append(rep.Windows, apic.ReportWindow{Name: wdw.Name, Start: wdw.StartStr, End: wdw.EndStr})
	}
	switch {
	case sim.Status.Observation != nil && sim.Status.Observation.Complete:
		rep.Observation = sim.Status.Observation
	case sim.Spec.Observe != nil:
		rep.Observation = o.Finalize(r.Context(), sim)
		rep.Live = true
	}
	writeJSON(w, http.StatusOK, rep)
}

// metricsExport: raw series and probe samples, as JSON or CSV.
func (s *Server) metricsExport(w http.ResponseWriter, r *http.Request) {
	sim := s.getSim(w, r)
	if sim == nil {
		return
	}
	var step time.Duration
	if v := r.URL.Query().Get("step"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		step = d
	}
	ex := s.observer().Export(r.Context(), sim, step)
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename="+sim.Name+".csv")
		_ = ex.WriteCSV(w)
		return
	}
	writeJSON(w, http.StatusOK, ex)
}

func (s *Server) profiles(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.observer().Profiles)
}
