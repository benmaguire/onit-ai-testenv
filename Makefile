# Local test environments on a Mac: kind, k3d or k3s (Colima).
#
#   make help                                  # this list
#   make up ENV=nvidia-triton-cpu              # deploy an environment (checks + offers to install/create a cluster)
#   make sim-suite ENV=nvidia-triton-cpu       # run its simulations, reports in reports/
#   make down ENV=nvidia-triton-cpu            # tear it down
#
# Variables (all optional except ENV where noted):
#   ENV           environment (make envs)
#   CLUSTER       auto | kind | k3d | k3s      (auto: use the current local cluster, else the env's recommended type)
#   CLUSTER_NAME  name of the cluster this Makefile creates (default testenv)
#   AUTO_INSTALL  ask | yes | no               (installs via Homebrew; ask needs a terminal)
#   CPUS MEMORY DISK                           VM size for Colima (default 6 CPU, 16 GiB, 100 GiB)
#   STATE         up | down                    (make up STATE=down scales workloads to 0)
#   HARDWARE      simulated | real             (default: env.yaml; real = onto real NVIDIA GPUs, needs ALLOW_REMOTE=1)
#   FRONTEND      inference-gateway | dynamo   (nvidia-vllm only; default: its values.yaml)
#   TYPE ARGS     simulation for `make sim`, e.g. TYPE=net-latency ARGS="--target-namespace x --pod-selector app=y"
#   NAME          simulation name for sim-report / sim-metrics
#   ONLY          regex: run only matching suite entries
#   WAIT=0 FORCE=1 PURGE=1 ALLOW_REMOTE=1 DRY_RUN=1 SKIP_PREFLIGHT=1

SHELL := /bin/bash
T := scripts/local/testenv.sh
export ENV CLUSTER CLUSTER_NAME AUTO_INSTALL CPUS MEMORY DISK STATE HARDWARE FRONTEND TYPE ARGS ONLY WAIT FORCE PURGE \
       ALLOW_REMOTE DRY_RUN SKIP_PREFLIGHT WAIT_TIMEOUT SIMULATOR_IMAGE KNE_RUNNER_IMAGE OTSIM_IMAGE SIM_WAIT SUITE_TIMEOUT

.DEFAULT_GOAL := help
.PHONY: help envs tools cluster-install cluster-create cluster-delete check up down restart render status ui argocd \
        simulator simulator-delete sim sim-suite sim-catalog sim-list sim-report sim-metrics sim-recover-all test

help: ## Show targets
	@awk 'BEGIN {FS = ":.*## "} /^##@/ {printf "\n\033[1m%s\033[0m\n", substr($$0, 5)} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)
	@echo
	@echo "  Environments: $$(ls environments | tr '\n' ' ')"
	@echo "  Example:      make up ENV=nvidia-triton-cpu && make sim-suite ENV=nvidia-triton-cpu"

##@ Cluster

envs: ## List environments with their local requirements
	@$(T) envs

tools: ## Install kubectl, helm, yq and the CLUSTER tool (kind / k3d / colima) with Homebrew
	@$(T) tools

cluster-install: ## Install tools + container runtime and create a local cluster (CLUSTER=kind|k3d|k3s, or ENV's recommendation)
	@$(T) cluster-install

cluster-create: ## Create (or start) the local cluster CLUSTER_NAME
	@$(T) cluster-create

cluster-delete: ## Delete the local cluster CLUSTER_NAME (asks first)
	@$(T) cluster-delete

check: ## Preflight for ENV: tools, cluster reachable / local / suitable, resources, dependencies
	@$(T) check

##@ Environments

up: ## Deploy ENV from the working tree (dependencies first)
	@$(T) up

down: ## Tear down ENV (PURGE=1 also removes CRDs and stuck finalizers)
	@$(T) down

restart: ## down + up ENV
	@$(T) down && $(T) up

render: ## Render ENV's manifests without applying (CLUSTER= selects the overrides)
	@$(T) render

status: ## Environments up in the current cluster (ENV= shows its pods)
	@$(T) status

ui: ## Port-forward Grafana (observability) to http://localhost:3000
	@$(T) ui

argocd: ## Optional: install Argo CD + the ApplicationSet (deploys from GitHub main)
	@$(T) argocd

##@ Simulations

simulator: ## Build the simulation controller image, load it into the cluster and deploy it
	@$(T) simulator

simulator-delete: ## Remove the simulation controller (recovers active simulations first)
	@$(T) simulator-delete

sim: ## Run one simulation: TYPE=<type> ARGS="<simctl inject args>" (waits for the report)
	@$(T) sim

sim-suite: ## Run simulations/ENV/suite.txt; reports + CSVs in reports/ENV/<run>/
	@$(T) sim-suite

sim-catalog: ## List simulation types (TYPE= for one type's details)
	@$(T) simctl catalog $(TYPE)

sim-list: ## List simulations
	@$(T) simctl list

sim-report: ## Report for NAME=<simulation>
	@$(T) simctl report $(NAME)

sim-metrics: ## Raw metrics CSV for NAME=<simulation> (stdout)
	@$(T) simctl metrics $(NAME)

sim-recover-all: ## Emergency stop: recover every active simulation
	@$(T) simctl recover-all

##@ Development

test: ## Lint charts and scripts, run the controller's tests
	@for c in $$(find environments -name Chart.yaml -exec dirname {} \;); do helm lint $$c >/dev/null || { helm lint $$c; exit 1; }; done; echo "charts ok"
	@for f in scripts/local/*.sh; do bash -n $$f || exit 1; done; echo "scripts ok"
	@$(MAKE) --no-print-directory -C simulations/controller test
