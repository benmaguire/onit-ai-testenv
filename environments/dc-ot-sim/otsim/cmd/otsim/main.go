// Command otsim simulates a data centre's OT plant (power, cooling, water)
// and exposes every device over industry protocols: Modbus TCP, SNMPv2c and
// Redfish, plus Prometheus metrics and an admin/fault-injection API.
//
//	otsim serve            --config site.yaml [--profiles-dir dir] ...
//	otsim validate         --config site.yaml        # check config + profiles
//	otsim telegraf-config  --config site.yaml        # DCIM-style collector config
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/api"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/modbus"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/profile"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/redfish"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/site"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/snmp"
	"github.com/benmaguire/onit-ai-testenv/environments/dc-ot-sim/otsim/internal/telegraf"
)

var version = "dev"

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("otsim "+cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "/etc/otsim/site.yaml", "site description")
	profDir := fs.String("profiles-dir", "", "extra/override profiles (regions/, modbus/, snmp/)")
	region := fs.String("region", "", "override the site's region")
	admin := fs.String("admin-addr", ":8080", "admin API + /metrics")
	rf := fs.String("redfish-addr", ":8443", "Redfish service (plain HTTP)")
	mbGW := fs.String("modbus-gateway", "tcp://0.0.0.0:502", "Modbus TCP gateway (unit ID = device index + 1); empty disables")
	mbBase := fs.Int("modbus-base-port", 15000, "per-device Modbus TCP port = base + device index (0 disables)")
	snmpAddr := fs.String("snmp-addr", ":161", "shared SNMP port (community = device id); empty disables")
	snmpBase := fs.Int("snmp-base-port", 16000, "per-device SNMP port = base + device index (0 disables)")
	community := fs.String("community", "public", "community for per-device SNMP ports")
	hostTpl := fs.String("host-template", "otsim-{id}", "telegraf-config: device host name template")
	rfURL := fs.String("redfish-url", "http://otsim:8443", "telegraf-config: Redfish base URL")
	out := fs.String("out", "", "telegraf-config: write to this file instead of stdout")
	_ = fs.Parse(args)

	cfg, err := site.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	set, err := profile.Load(*profDir)
	if err != nil {
		log.Fatal(err)
	}
	if *region != "" {
		cfg.Site.Region = *region
	}
	reg, ok := set.Regions[cfg.Site.Region]
	if !ok {
		log.Fatalf("unknown region %q (have %s)", cfg.Site.Region, strings.Join(profile.Names(set.Regions), ", "))
	}
	st := site.New(cfg, reg, time.Now())
	if err := set.Validate(st); err != nil {
		log.Fatal(err)
	}

	switch cmd {
	case "validate":
		fmt.Printf("ok: site %s, region %s, %d devices\n", cfg.Site.Name, reg.Name, len(cfg.Devices))
		return
	case "telegraf-config":
		conf := telegraf.Generate(st, set, telegraf.Options{HostTemplate: *hostTpl, ModbusPort: 502, SNMPPort: 161,
			Community: *community, RedfishURL: *rfURL})
		if *out != "" {
			if err := os.WriteFile(*out, []byte(conf), 0o644); err != nil {
				log.Fatal(err)
			}
			return
		}
		fmt.Print(conf)
		return
	case "serve":
	default:
		log.Fatalf("unknown command %q (serve, validate, telegraf-config)", cmd)
	}

	promReg := prometheus.NewRegistry()
	promReg.MustRegister(collectors.NewGoCollector())
	reqs := api.NewMetrics(promReg, st)
	count := func(d, p string) { reqs.WithLabelValues(d, p).Inc() }
	modbus.Requests, snmp.Requests, redfish.Requests = count, count, count

	stop := make(chan struct{})
	go st.Run(stop)

	if _, err := modbus.Serve(st, set, *mbGW, "0.0.0.0", *mbBase); err != nil {
		log.Fatal(err)
	}
	agent := snmp.New(st, set, *community)
	if *snmpAddr != "" {
		if err := agent.Serve(*snmpAddr, ""); err != nil {
			log.Fatalf("snmp %s: %v", *snmpAddr, err)
		}
	}
	if *snmpBase > 0 {
		for i, d := range st.Devices() {
			if _, ok := d.Protocols["snmp"]; ok {
				if err := agent.Serve(fmt.Sprintf(":%d", *snmpBase+i), d.ID); err != nil {
					log.Fatalf("snmp %s: %v", d.ID, err)
				}
			}
		}
	}
	go func() {
		log.Fatal((&http.Server{Addr: *rf, Handler: &redfish.Handler{Site: st}, ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
	}()
	go func() {
		srv := &api.Server{Site: st, Profiles: set, Requests: reqs}
		log.Fatal((&http.Server{Addr: *admin, Handler: srv.Handler(promReg), ReadHeaderTimeout: 10 * time.Second}).ListenAndServe())
	}()
	log.Printf("otsim %s: site %s, region %s (%s, %.0f/%.0f V, %.0f Hz), %d devices; admin %s, redfish %s, modbus %s (+%d), snmp %s (+%d)",
		version, cfg.Site.Name, reg.Name, reg.Description, reg.VoltageLN, reg.VoltageLL, reg.FrequencyHz, len(cfg.Devices),
		*admin, *rf, *mbGW, *mbBase, *snmpAddr, *snmpBase)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	close(stop)
}
