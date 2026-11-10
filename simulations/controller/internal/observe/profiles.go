package observe

import (
	"fmt"
	"os"
	"sort"

	"sigs.k8s.io/yaml"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// Profiles maps a profile name to its queries.
type Profiles map[string][]simv1.MetricQuery

// Builtin profiles. Queries are templates (see ObserveSpec.Queries) and must
// aggregate to one series.
var Builtin = Profiles{
	"kubernetes": {
		{Name: "ready_pods", Query: `sum(kube_pod_status_ready{namespace="{{.Namespace}}",condition="true"})`, Unit: "pods", HigherIsBetter: true},
		{Name: "container_restarts", Query: `sum(kube_pod_container_status_restarts_total{namespace="{{.Namespace}}"})`, Unit: "restarts"},
		{Name: "target_cpu_cores", Query: `sum(rate(container_cpu_usage_seconds_total{namespace="{{.Namespace}}",pod=~"{{.Pods}}",container!=""}[{{.Rate}}]))`, Unit: "cores"},
		{Name: "target_cpu_throttled_ratio", Query: `sum(rate(container_cpu_cfs_throttled_periods_total{namespace="{{.Namespace}}",pod=~"{{.Pods}}"}[{{.Rate}}])) / clamp_min(sum(rate(container_cpu_cfs_periods_total{namespace="{{.Namespace}}",pod=~"{{.Pods}}"}[{{.Rate}}])), 1)`, Unit: "ratio"},
		{Name: "target_memory_bytes", Query: `sum(container_memory_working_set_bytes{namespace="{{.Namespace}}",pod=~"{{.Pods}}",container!=""})`, Unit: "bytes"},
		{Name: "target_net_rx_bytes_per_s", Query: `sum(rate(container_network_receive_bytes_total{namespace="{{.Namespace}}",pod=~"{{.Pods}}"}[{{.Rate}}]))`, Unit: "B/s"},
		{Name: "target_net_tx_bytes_per_s", Query: `sum(rate(container_network_transmit_bytes_total{namespace="{{.Namespace}}",pod=~"{{.Pods}}"}[{{.Rate}}]))`, Unit: "B/s"},
	},
	"gpu": {
		{Name: "gpu_util_avg", Query: `avg(DCGM_FI_DEV_GPU_UTIL)`, Unit: "%"},
		{Name: "gpu_temp_max", Query: `max(DCGM_FI_DEV_GPU_TEMP)`, Unit: "°C"},
		{Name: "gpu_power_total", Query: `sum(DCGM_FI_DEV_POWER_USAGE)`, Unit: "W"},
		{Name: "gpu_fb_used", Query: `sum(DCGM_FI_DEV_FB_USED)`, Unit: "MiB"},
		{Name: "gpu_xid_last", Query: `max(DCGM_FI_DEV_XID_ERRORS)`, Unit: "xid"},
		{Name: "gpus_allocatable", Query: `sum(kube_node_status_allocatable{resource="nvidia_com_gpu"})`, Unit: "GPUs", HigherIsBetter: true},
	},
	"dynamo": {
		{Name: "dynamo_requests_per_s", Query: `sum(rate(dynamo_frontend_requests_total[{{.Rate}}]))`, Unit: "req/s", HigherIsBetter: true},
		{Name: "dynamo_errors_per_s", Query: `sum(rate(dynamo_frontend_requests_total{status!="success"}[{{.Rate}}]))`, Unit: "req/s"},
		{Name: "dynamo_latency_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(dynamo_frontend_request_duration_seconds_bucket[{{.Rate}}])))`, Unit: "s"},
		{Name: "dynamo_ttft_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(dynamo_frontend_time_to_first_token_seconds_bucket[{{.Rate}}])))`, Unit: "s"},
		{Name: "dynamo_itl_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(dynamo_frontend_inter_token_latency_seconds_bucket[{{.Rate}}])))`, Unit: "s"},
		{Name: "dynamo_inflight", Query: `sum(dynamo_frontend_inflight_requests)`, Unit: "requests"},
	},
	"vllm": {
		{Name: "vllm_running", Query: `sum(vllm:num_requests_running{namespace="{{.Namespace}}"})`, Unit: "requests"},
		{Name: "vllm_waiting", Query: `sum(vllm:num_requests_waiting{namespace="{{.Namespace}}"})`, Unit: "requests"},
		{Name: "vllm_kv_cache_usage_max", Query: `max(vllm:kv_cache_usage_perc{namespace="{{.Namespace}}"})`, Unit: "ratio"},
		{Name: "vllm_success_per_s", Query: `sum(rate(vllm:request_success_total{namespace="{{.Namespace}}"}[{{.Rate}}]))`, Unit: "req/s", HigherIsBetter: true},
		{Name: "vllm_ttft_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(vllm:time_to_first_token_seconds_bucket{namespace="{{.Namespace}}"}[{{.Rate}}])))`, Unit: "s"},
		{Name: "vllm_itl_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(vllm:inter_token_latency_seconds_bucket{namespace="{{.Namespace}}"}[{{.Rate}}])))`, Unit: "s"},
	},
	"triton": {
		{Name: "triton_success_per_s", Query: `sum(rate(nv_inference_request_success[{{.Rate}}]))`, Unit: "req/s", HigherIsBetter: true},
		{Name: "triton_failure_per_s", Query: `sum(rate(nv_inference_request_failure[{{.Rate}}]))`, Unit: "req/s"},
		{Name: "triton_request_ms", Query: `sum(rate(nv_inference_request_duration_us[{{.Rate}}])) / clamp_min(sum(rate(nv_inference_request_success[{{.Rate}}])), 1e-9) / 1000`, Unit: "ms"},
		{Name: "triton_queue_ms", Query: `sum(rate(nv_inference_queue_duration_us[{{.Rate}}])) / clamp_min(sum(rate(nv_inference_request_success[{{.Rate}}])), 1e-9) / 1000`, Unit: "ms"},
	},
	"kai": {
		{Name: "pending_pods", Query: `sum(kube_pod_status_phase{namespace="{{.Namespace}}",phase="Pending"})`, Unit: "pods"},
		{Name: "running_pods", Query: `sum(kube_pod_status_phase{namespace="{{.Namespace}}",phase="Running"})`, Unit: "pods", HigherIsBetter: true},
		{Name: "ready_virtual_nodes", Query: `sum(kube_node_status_condition{condition="Ready",status="true"} * on(node) group_left() kube_node_labels{label_type="kwok"})`, Unit: "nodes", HigherIsBetter: true},
	},
	"network": {
		{Name: "router_in_pkts_per_s", Query: `sum(rate(gnmic_interfaces_interface_state_counters_in_pkts[{{.Rate}}]))`, Unit: "pkt/s"},
		{Name: "router_out_pkts_per_s", Query: `sum(rate(gnmic_interfaces_interface_state_counters_out_pkts[{{.Rate}}]))`, Unit: "pkt/s"},
		{Name: "router_in_errors_per_s", Query: `sum(rate(gnmic_interfaces_interface_state_counters_in_errors[{{.Rate}}]))`, Unit: "err/s"},
		{Name: "router_bgp_established", Query: `count(gnmic_network_instances_network_instance_protocols_protocol_bgp_neighbors_neighbor_state_session_state{session_state="ESTABLISHED"}) or vector(0)`, Unit: "sessions", HigherIsBetter: true},
	},
	"ot": {
		{Name: "ot_pue", Query: `max(otsim_site{kpi="pue"})`, Unit: "ratio"},
		{Name: "ot_it_kw", Query: `max(otsim_site{kpi="it_kw"})`, Unit: "kW", HigherIsBetter: true},
		{Name: "ot_facility_kw", Query: `max(otsim_site{kpi="facility_kw"})`, Unit: "kW"},
		{Name: "ot_rack_inlet_c", Query: `max(otsim_site{kpi="rack_inlet_c"})`, Unit: "°C"},
		{Name: "ot_cdu_return_max_c", Query: `max(otsim_site{kpi="cdu_return_max_c"})`, Unit: "°C"},
		{Name: "ot_ups_charge_min_pct", Query: `min(otsim_point{type="ups",point="battery_charge_pct"})`, Unit: "%", HigherIsBetter: true},
		{Name: "ot_alarms_critical", Query: `max(otsim_site{kpi="alarms_critical"})`, Unit: "alarms"},
		{Name: "ot_water_l_per_h", Query: `max(otsim_site{kpi="water_l_per_h"})`, Unit: "L/h"},
		{Name: "dcim_scrape_errors", Query: `sum(rate(internal_gather_errors{input=~"modbus|snmp|http"}[{{.Rate}}])) or vector(0)`, Unit: "err/s"},
	},
	"probes": {
		{Name: "probe_success_ratio", Query: `sum(rate(sim_probe_total{simulation="{{.Simulation}}",result="success"}[{{.Rate}}])) / clamp_min(sum(rate(sim_probe_total{simulation="{{.Simulation}}"}[{{.Rate}}])), 1e-9)`, Unit: "ratio", HigherIsBetter: true},
		{Name: "probe_latency_p95", Query: `histogram_quantile(0.95, sum by (le) (rate(sim_probe_duration_seconds_bucket{simulation="{{.Simulation}}"}[{{.Rate}}])))`, Unit: "s"},
	},
}

// LoadProfiles returns the builtin profiles overlaid with a YAML file
// (profile name -> list of {name, query, unit, higherIsBetter}). Empty path = builtins.
func LoadProfiles(path string) (Profiles, error) {
	out := Profiles{}
	for k, v := range Builtin {
		out[k] = v
	}
	if path == "" {
		return out, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	var extra Profiles
	if err := yaml.Unmarshal(b, &extra); err != nil {
		return nil, fmt.Errorf("metrics profiles %s: %w", path, err)
	}
	for k, v := range extra {
		out[k] = v
	}
	return out, nil
}

// Names lists profile names.
func (p Profiles) Names() []string {
	var n []string
	for k := range p {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}
