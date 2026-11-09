package inject

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/benmaguire/onit-ai-testenv/simulations/controller/pkg/catalog"

	simv1 "github.com/benmaguire/onit-ai-testenv/simulations/controller/api/v1alpha1"
)

// Mokka (nvml-mock) runtime control. nvml-mock-ctl runs in the node-agent
// container of the nvml-mock pod on the target node and writes an overrides
// file that every mock NVML consumer on that node re-reads within ~1s.
// See https://github.com/NVIDIA/k8s-test-infra/blob/main/docs/nvml-mock-ctl.md
const (
	mokkaContainer = "node-agent"
	mokkaCtl       = "/usr/local/bin/nvml-mock-ctl"
)

var (
	gpuTargets   = []string{"node", "nodeSelector", "gpu", "podSelector", "graph", "component", "namespace", "count"}
	refMokkaCtl  = "https://github.com/NVIDIA/k8s-test-infra/blob/main/docs/nvml-mock-ctl.md"
	refXid       = "https://docs.nvidia.com/deploy/xid-errors/"
	requireMokka = "Mokka (nvml-mock) installed — the nvidia-sim mokka component"
)

// gpuSim is a GPU simulation built from nvml-mock-ctl commands.
type gpuSim struct {
	entry    catalog.Entry
	validate func(p params) error
	// inject returns the nvml-mock-ctl argument lists to apply to one GPU target.
	inject func(ctx context.Context, env *Env, t gpuTarget, p params) ([][]string, error)
	// recover returns the argument lists that revert one GPU target.
	recover func(gpu string) [][]string
}

func (g *gpuSim) Entry() catalog.Entry { return g.entry }

func (g *gpuSim) Validate(sim *simv1.Simulation) error {
	if err := checkKnownParams(g.entry, sim); err != nil {
		return err
	}
	if gpu := sim.Spec.Target.GPU; gpu != "" && gpu != "all" && !strings.HasPrefix(gpu, "GPU-") {
		if _, err := strconv.Atoi(gpu); err != nil {
			return fmt.Errorf("target.gpu %q must be an index, \"all\" or a GPU UUID", gpu)
		}
	}
	if g.validate != nil {
		return g.validate(newParams(g.entry, sim))
	}
	return nil
}

func (g *gpuSim) Inject(ctx context.Context, env *Env, sim *simv1.Simulation) (Result, error) {
	p := newParams(g.entry, sim)
	targets, err := resolveGPUs(ctx, env, sim)
	if err != nil {
		return Result{}, err
	}
	var res Result
	for _, t := range targets {
		cmds, err := g.inject(ctx, env, t, p)
		if err != nil {
			return res, err
		}
		res.Targets = append(res.Targets, simv1.AffectedTarget{
			Kind: "GPU", Namespace: t.Mokka.Namespace, Name: t.Mokka.Name, Node: t.Node, GPU: t.GPU, Detail: t.Via,
		})
		for _, args := range cmds {
			if err := runCtl(ctx, env, t.Mokka.Namespace, t.Mokka.Name, args); err != nil {
				return res, err
			}
		}
	}
	res.Message = fmt.Sprintf("injected on %s", describeGPUs(res.Targets))
	return res, nil
}

func (g *gpuSim) Recover(ctx context.Context, env *Env, sim *simv1.Simulation) (string, error) {
	var errs []error
	for _, t := range sim.Status.Targets {
		if t.Kind != "GPU" {
			continue
		}
		// The nvml-mock pod may have been replaced since injection.
		mp, err := mokkaPodOn(ctx, env, t.Node)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, args := range g.recover(t.GPU) {
			if err := runCtl(ctx, env, mp.Namespace, mp.Name, args); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return "reverted on " + describeGPUs(sim.Status.Targets), errors.Join(errs...)
}

// Verify checks the mock NVML answers for the GPU again.
func (g *gpuSim) Verify(ctx context.Context, env *Env, sim *simv1.Simulation) (bool, string, error) {
	for _, t := range sim.Status.Targets {
		if t.Kind != "GPU" {
			continue
		}
		mp, err := mokkaPodOn(ctx, env, t.Node)
		if err != nil {
			return false, err.Error(), nil
		}
		cmd := []string{"nvidia-smi", "--query-gpu=index,name", "--format=csv,noheader"}
		if t.GPU != "all" {
			cmd = append(cmd, "-i", t.GPU)
		}
		if _, _, err := env.Exec.Exec(ctx, mp.Namespace, mp.Name, mokkaContainer, cmd); err != nil {
			return false, fmt.Sprintf("GPU %s on %s not healthy yet: %v", t.GPU, t.Node, err), nil
		}
	}
	return true, "nvidia-smi reports the GPUs healthy", nil
}

func runCtl(ctx context.Context, env *Env, ns, pod string, args []string) error {
	cmd := append([]string{mokkaCtl}, args...)
	env.Log.V(1).Info("nvml-mock-ctl", "pod", ns+"/"+pod, "args", args)
	_, _, err := env.Exec.Exec(ctx, ns, pod, mokkaContainer, cmd)
	return err
}

func describeGPUs(ts []simv1.AffectedTarget) string {
	var parts []string
	for _, t := range ts {
		if t.Kind == "GPU" {
			parts = append(parts, fmt.Sprintf("%s/gpu%s", t.Node, t.GPU))
		}
	}
	return strings.Join(parts, ", ")
}

func one(args ...string) [][]string { return [][]string{args} }

func resetGPU(gpu string) [][]string { return one("reset", "--gpu", gpu) }

func failHealthy(gpu string) [][]string { return one("fail", "--gpu", gpu, "--mode", "healthy") }

// failSim builds the device-failure types (fail --mode ...).
func failSim(typ, summary, effect, mode, defaultXid string) *gpuSim {
	ps := []catalog.Param{
		{Name: "xid", Description: "Xid code surfaced through NVML events (and the kernel log when Mokka's nodeAgent.kernelLog is on). Empty for none.", Default: defaultXid},
		{Name: "afterCalls", Description: "Trip after N guarded NVML calls instead of immediately."},
	}
	if mode == "" {
		ps = append([]catalog.Param{{Name: "mode", Description: "Failure mode: ecc_uncorrectable, lost or fallen_off_bus.", Default: "ecc_uncorrectable"}}, ps...)
		ps[1].Required = true
	}
	return &gpuSim{
		entry: catalog.Entry{
			Type: typ, Category: "gpu", Summary: summary, Effect: effect,
			Recovery:   "`nvml-mock-ctl fail --mode healthy` removes the failure; recovered once nvidia-smi answers for the GPU. Kernel-log Xid lines are never retracted, as on real hardware.",
			Targets:    gpuTargets,
			Params:     ps,
			Requires:   []string{requireMokka},
			References: []string{refMokkaCtl, refXid},
		},
		validate: func(p params) error {
			m := mode
			if m == "" {
				m = p.str("mode")
			}
			if err := oneOf("mode", m, "ecc_uncorrectable", "lost", "fallen_off_bus"); err != nil {
				return err
			}
			if p.str("xid") != "" {
				if _, err := p.int("xid", 1, 1000); err != nil {
					return err
				}
			}
			if p.str("afterCalls") != "" {
				if _, err := p.int("afterCalls", 0, 1<<30); err != nil {
					return err
				}
			}
			return nil
		},
		inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
			m := mode
			if m == "" {
				m = p.str("mode")
			}
			args := []string{"fail", "--gpu", t.GPU, "--mode", m}
			if x := p.str("xid"); x != "" {
				args = append(args, "--xid", x)
			}
			if n := p.str("afterCalls"); n != "" {
				args = append(args, "--after-calls", n)
			}
			return [][]string{args}, nil
		},
		recover: failHealthy,
	}
}

// GPUInjectors returns every Mokka-backed GPU simulation.
func GPUInjectors() []Injector {
	return []Injector{
		failSim("gpu-xid", "Raise a GPU Xid error with a chosen failure mode",
			"The GPU trips the chosen failure mode and NVML delivers the Xid event; device plugin / DCGM / health monitors react as to a real Xid.",
			"", "79"),
		failSim("gpu-lost", "GPU becomes lost (NVML_ERROR_GPU_IS_LOST)",
			"Guarded NVML calls, handle lookups and event waits return GPU_IS_LOST. The device plugin marks the GPU unhealthy; pods using it typically fail health checks.",
			"lost", ""),
		failSim("gpu-fallen-off-bus", "GPU falls off the PCIe bus (Xid 79)",
			"Same NVML surface as lost, modelling a GPU that fell off the bus, with Xid 79 by default.",
			"fallen_off_bus", "79"),
		failSim("gpu-ecc-uncorrectable", "Uncorrectable (double-bit) ECC errors (Xid 48)",
			"The device stays addressable but uncorrectable ECC counters climb, with Xid 48 by default — the signal for retiring pages / draining the GPU.",
			"ecc_uncorrectable", "48"),

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-overheat", Category: "gpu", Summary: "GPU overheats and thermally throttles",
				Effect:   "Reported GPU temperature is pinned high and, by default, the hw_thermal_slowdown throttle reason is raised. dcgm-exporter / alerting see the temperature and throttle.",
				Recovery: "`nvml-mock-ctl reset --gpu` returns the GPU to its profile (this clears every override on that GPU).",
				Targets:  gpuTargets,
				Params: []catalog.Param{
					{Name: "temperature", Description: "Temperature in °C (0-200).", Default: "95"},
					{Name: "throttle", Description: "Also raise the thermal throttle reason.", Default: "true"},
				},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				if _, err := p.int("temperature", 0, 200); err != nil {
					return err
				}
				_, err := p.bool("throttle")
				return err
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				cmds := [][]string{{"temp", "--gpu", t.GPU, p.str("temperature")}}
				if b, _ := p.bool("throttle"); b {
					cmds = append(cmds, []string{"throttle", "--gpu", t.GPU, "thermal"})
				}
				return cmds, nil
			},
			recover: resetGPU,
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-power-throttle", Category: "gpu", Summary: "GPU hits its power cap and throttles",
				Effect:   "Power draw is pinned (clamped to the profile's limits) and the sw_power_cap throttle reason raised; clocks can be pinned low too.",
				Recovery: "`nvml-mock-ctl reset --gpu` (clears every override on that GPU).",
				Targets:  gpuTargets,
				Params: []catalog.Param{
					{Name: "watts", Description: "Reported power draw in W.", Default: "700"},
					{Name: "clocksMHz", Description: "Optionally pin SM/graphics clocks to this MHz."},
				},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				if _, err := p.int("watts", 0, 100000); err != nil {
					return err
				}
				if p.str("clocksMHz") != "" {
					_, err := p.int("clocksMHz", 0, 100000)
					return err
				}
				return nil
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				cmds := [][]string{{"power", "--gpu", t.GPU, p.str("watts")}, {"throttle", "--gpu", t.GPU, "power"}}
				if c := p.str("clocksMHz"); c != "" {
					cmds = append(cmds, []string{"clocks", "--gpu", t.GPU, c})
				}
				return cmds, nil
			},
			recover: resetGPU,
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-throttle", Category: "gpu", Summary: "Set active clock-throttle reasons",
				Effect:   "nvidia-smi / NVML report the given clocks_throttle_reasons (authoritative: other reasons cleared).",
				Recovery: "`nvml-mock-ctl throttle --gpu <g> none`.",
				Targets:  gpuTargets,
				Params: []catalog.Param{{Name: "reasons", Default: "thermal",
					Description: "Comma-separated: thermal, sw_thermal, power, power_brake, hw_slowdown, sync_boost, idle, app_clocks, display_clocks."}},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				for _, r := range p.list("reasons") {
					if err := oneOf("reasons", r, "thermal", "sw_thermal", "power", "power_brake", "hw_slowdown", "sync_boost", "idle", "app_clocks", "display_clocks"); err != nil {
						return err
					}
				}
				if len(p.list("reasons")) == 0 {
					return fmt.Errorf("param \"reasons\" is empty")
				}
				return nil
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				return [][]string{append([]string{"throttle", "--gpu", t.GPU}, p.list("reasons")...)}, nil
			},
			recover: func(gpu string) [][]string { return one("throttle", "--gpu", gpu, "none") },
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-clocks-degraded", Category: "gpu", Summary: "SM/graphics clocks drop",
				Effect:   "Reported SM and graphics clocks are pinned to a low value, optionally with a P-state.",
				Recovery: "`nvml-mock-ctl reset --gpu` (clears every override on that GPU).",
				Targets:  gpuTargets,
				Params: []catalog.Param{
					{Name: "mhz", Description: "Clock in MHz.", Default: "300"},
					{Name: "pstate", Description: "Optional P-state 0-15 to report, e.g. 8."},
				},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				if _, err := p.int("mhz", 0, 100000); err != nil {
					return err
				}
				if p.str("pstate") != "" {
					_, err := p.int("pstate", 0, 15)
					return err
				}
				return nil
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				cmds := [][]string{{"clocks", "--gpu", t.GPU, p.str("mhz")}}
				if s := p.str("pstate"); s != "" {
					cmds = append(cmds, []string{"pstate", "--gpu", t.GPU, s})
				}
				return cmds, nil
			},
			recover: resetGPU,
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-utilization", Category: "gpu", Summary: "Pin GPU and memory utilization",
				Effect:   "GPU and memory utilization report a fixed percent (e.g. 100 for a saturated GPU, 0 for an idle one). Useful for autoscaling and alerting tests.",
				Recovery: "`nvml-mock-ctl reset --gpu` (clears every override on that GPU).",
				Targets:  gpuTargets,
				Params:   []catalog.Param{{Name: "percent", Description: "Utilization 0-100.", Default: "100"}},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error { _, err := p.int("percent", 0, 100); return err },
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				return one("util", "--gpu", t.GPU, p.str("percent")), nil
			},
			recover: resetGPU,
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-memory-pressure", Category: "gpu", Summary: "GPU framebuffer reported nearly full",
				Effect:   "memory.used/free are set so the GPU reports the given percent of its framebuffer in use (values are reported, nothing is allocated).",
				Recovery: "`nvml-mock-ctl reset --gpu` (clears every override on that GPU).",
				Targets:  gpuTargets,
				Params:   []catalog.Param{{Name: "usedPercent", Description: "Percent of total memory reported used (0-100).", Default: "95"}},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error { _, err := p.float("usedPercent", 0, 100); return err },
			inject: func(ctx context.Context, env *Env, t gpuTarget, p params) ([][]string, error) {
				pct, _ := p.float("usedPercent", 0, 100)
				total, err := gpuMemoryTotal(ctx, env, t)
				if err != nil {
					return nil, err
				}
				used := uint64(float64(total) * pct / 100)
				return one("set", "--gpu", t.GPU,
					fmt.Sprintf("memory.used_bytes=%d", used),
					fmt.Sprintf("memory.free_bytes=%d", total-used)), nil
			},
			recover: resetGPU,
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-nvlink-errors", Category: "gpu", Summary: "NVLink data-link errors climb",
				Effect:   "NVLink DL replay/recovery/CRC counters climb at the given rate on the GPU's active links (what DCGM_HEALTH_WATCH_NVLINK reads). Needs a profile with NVLink.",
				Recovery: "`nvml-mock-ctl nvlink-error --gpu <g> 0`.",
				Targets:  gpuTargets,
				Params: []catalog.Param{
					{Name: "rate", Description: "Errors per second.", Default: "100"},
					{Name: "links", Description: "Comma-separated link ids; empty = all active links."},
				},
				Requires: []string{requireMokka, "a GPU profile with NVLink (h100, b200, gb200, gb300)"}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				_, err := p.float("rate", 0, 1e9)
				return err
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				args := []string{"nvlink-error", "--gpu", t.GPU}
				if l := p.list("links"); len(l) > 0 {
					args = append(args, "--links", strings.Join(l, ","))
				}
				return [][]string{append(args, p.str("rate"))}, nil
			},
			recover: func(gpu string) [][]string { return one("nvlink-error", "--gpu", gpu, "0") },
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-sram-ecc", Category: "gpu", Summary: "On-die SRAM ECC errors",
				Effect:   "ecc.sram counters (volatile and aggregate) report the given count; optionally raises SRAM Threshold Exceeded, the driver's 'needs servicing' signal.",
				Recovery: "`nvml-mock-ctl sram-ecc --gpu <g> 0`.",
				Targets:  gpuTargets,
				Params: []catalog.Param{
					{Name: "count", Description: "Error count.", Default: "4"},
					{Name: "type", Description: "correctable, parity or secded.", Default: "secded"},
					{Name: "source", Description: "Unit for uncorrectable errors: l2, sm, microcontroller, pcie, other.", Default: "sm"},
					{Name: "thresholdExceeded", Description: "Raise SRAM Threshold Exceeded.", Default: "true"},
				},
				Requires: []string{requireMokka}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				if _, err := p.int("count", 0, 1e9); err != nil {
					return err
				}
				if err := oneOf("type", p.str("type"), "correctable", "parity", "secded"); err != nil {
					return err
				}
				if err := oneOf("source", p.str("source"), "l2", "sm", "microcontroller", "pcie", "other"); err != nil {
					return err
				}
				_, err := p.bool("thresholdExceeded")
				return err
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				args := []string{"sram-ecc", "--gpu", t.GPU, "--type", p.str("type")}
				if p.str("type") != "correctable" {
					args = append(args, "--source", p.str("source"))
				}
				if b, _ := p.bool("thresholdExceeded"); b {
					args = append(args, "--threshold-exceeded")
				}
				return [][]string{append(args, p.str("count"))}, nil
			},
			recover: func(gpu string) [][]string { return one("sram-ecc", "--gpu", gpu, "0") },
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-fabric-degraded", Category: "gpu", Summary: "NVLink fabric health degraded",
				Effect:   "nvidia-smi -q Fabric → Health reports the given conditions (authoritative). Summary becomes Unhealthy / Limited Capacity.",
				Recovery: "`nvml-mock-ctl fabric-health --gpu <g> healthy`.",
				Targets:  gpuTargets,
				Params: []catalog.Param{{Name: "conditions", Default: "degraded_bandwidth",
					Description: "Comma-separated: degraded_bandwidth, route_recovery, route_unhealthy, access_timeout_recovery, no_partition, insufficient_nvlinks, incompatible_gpu_fw, invalid_location, incorrect_sysguid, incorrect_chassis_sn, gpu_state_invalid."}},
				Requires: []string{requireMokka, "a GPU profile with an NVLink fabric (gb200, gb300)"}, References: []string{refMokkaCtl},
			},
			validate: func(p params) error {
				cs := p.list("conditions")
				if len(cs) == 0 {
					return fmt.Errorf("param \"conditions\" is empty")
				}
				for _, c := range cs {
					if err := oneOf("conditions", c, "degraded_bandwidth", "route_recovery", "route_unhealthy", "access_timeout_recovery",
						"no_partition", "insufficient_nvlinks", "incompatible_gpu_fw", "invalid_location", "incorrect_sysguid",
						"incorrect_chassis_sn", "gpu_state_invalid"); err != nil {
						return err
					}
				}
				return nil
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				return [][]string{append([]string{"fabric-health", "--gpu", t.GPU}, p.list("conditions")...)}, nil
			},
			recover: func(gpu string) [][]string { return one("fabric-health", "--gpu", gpu, "healthy") },
		},

		&gpuSim{
			entry: catalog.Entry{
				Type: "gpu-set", Category: "gpu", Summary: "Write arbitrary mock NVML fields (escape hatch)",
				Effect:   "Runs `nvml-mock-ctl set --gpu <g> key.path=value ...` for any field in Mokka's config schema; unknown fields are rejected by nvml-mock-ctl.",
				Recovery: "`nvml-mock-ctl reset --gpu` (clears every override on that GPU).",
				Targets:  gpuTargets,
				Params: []catalog.Param{{Name: "fields", Required: true,
					Description: "Comma-separated key.path=value pairs, e.g. \"ecc.mode_current=false,pstate=8\"."}},
				Requires:   []string{requireMokka},
				References: []string{refMokkaCtl, "https://github.com/NVIDIA/k8s-test-infra/blob/main/docs/configuration.md"},
			},
			validate: func(p params) error {
				for _, f := range p.list("fields") {
					if !strings.Contains(f, "=") {
						return fmt.Errorf("param \"fields\": %q is not key.path=value", f)
					}
				}
				return nil
			},
			inject: func(_ context.Context, _ *Env, t gpuTarget, p params) ([][]string, error) {
				return [][]string{append([]string{"set", "--gpu", t.GPU}, p.list("fields")...)}, nil
			},
			recover: resetGPU,
		},
	}
}

// gpuMemoryTotal returns the target GPU's total memory in bytes, as the mock reports it.
func gpuMemoryTotal(ctx context.Context, env *Env, t gpuTarget) (uint64, error) {
	cmd := []string{"nvidia-smi", "--query-gpu=index,memory.total", "--format=csv,noheader,nounits"}
	stdout, _, err := env.Exec.Exec(ctx, t.Mokka.Namespace, t.Mokka.Name, mokkaContainer, cmd)
	if err != nil {
		return 0, fmt.Errorf("read GPU memory on %s: %w", t.Node, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		f := strings.Split(line, ",")
		if len(f) != 2 {
			continue
		}
		if t.GPU == "all" || strings.TrimSpace(f[0]) == t.GPU {
			mib, err := strconv.ParseUint(strings.TrimSpace(f[1]), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse memory.total %q: %w", f[1], err)
			}
			return mib << 20, nil
		}
	}
	return 0, fmt.Errorf("GPU %s not found on %s", t.GPU, t.Node)
}
