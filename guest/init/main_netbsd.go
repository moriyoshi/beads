// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command init is PID 1 of a NetBSD appliance image. It replaces init(8) and
// rc(8): it mounts the scratch file systems, configures networking with
// DHCP, then runs and supervises the one program the image exists for.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path"
	"strings"
	"sync"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	"golang.org/x/sys/unix"

	"github.com/moriyoshi/beads/guest/internal/dhcp"
	"github.com/moriyoshi/beads/guest/internal/logship"
	"github.com/moriyoshi/beads/guest/internal/metadata"
	"github.com/moriyoshi/beads/guest/internal/nbsys"
	"github.com/moriyoshi/beads/guest/internal/otelagent"
	"github.com/moriyoshi/beads/guest/internal/otelconfig"
	"github.com/moriyoshi/beads/internal/sntp"
)

// shipper sends log records to the embedded collector; nil when log
// shipping is off.
var shipper *logship.Shipper

func logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "nbinit: %s\n", msg)
	shipper.Emit("nbinit", otellog.SeverityInfo, msg)
}

// setupConsole points stdin at /dev/null and stdout/stderr at the console.
// The kernel starts init with no open descriptors.
func setupConsole() {
	if fd, err := unix.Open("/dev/console", unix.O_RDWR|unix.O_NOCTTY, 0); err == nil {
		unix.Dup2(fd, 1)
		unix.Dup2(fd, 2)
		if fd > 2 {
			unix.Close(fd)
		}
	}
	if fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0); err == nil {
		unix.Dup2(fd, 0)
		if fd > 2 {
			unix.Close(fd)
		}
	}
}

func mountFilesystems() error {
	if err := nbsys.MountTmpfs("/tmp", 0o1777, 0); err != nil {
		return err
	}
	if err := nbsys.MountTmpfs("/var", 0o755, 0); err != nil {
		return err
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{"/var/run", 0o755}, {"/var/log", 0o755}, {"/var/db", 0o755}, {"/var/tmp", 0o777 | os.ModeSticky}} {
		if err := os.Mkdir(d.path, d.mode.Perm()); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	// kernfs is a convenience for debugging; its absence is not fatal.
	if err := nbsys.MountKernfs("/kern"); err != nil {
		logf("warning: %v", err)
	}
	return nil
}

func configureLoopback() error {
	if err := nbsys.AddAddr("lo0", net.IPv4(127, 0, 0, 1), net.CIDRMask(8, 32), nil); err != nil {
		return err
	}
	return nbsys.IfUp("lo0")
}

func pickInterface(name string) (*net.Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for i := range ifs {
		ifc := &ifs[i]
		if name != "" {
			if ifc.Name == name {
				return ifc, nil
			}
			continue
		}
		if ifc.Flags&net.FlagLoopback == 0 && len(ifc.HardwareAddr) == 6 {
			return ifc, nil
		}
	}
	if name != "" {
		return nil, fmt.Errorf("interface %s not found", name)
	}
	return nil, errors.New("no Ethernet interface found")
}

func randomXID() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// applyLease configures the interface, routes, resolver and hostname.
func applyLease(ifc *net.Interface, l *dhcp.Lease, hostname string) error {
	if l.MTU >= 576 && l.MTU != ifc.MTU {
		if err := nbsys.SetMTU(ifc.Name, l.MTU); err != nil {
			logf("warning: %v", err)
		}
	}
	if err := nbsys.AddAddr(ifc.Name, l.Addr, l.Mask, l.Broadcast); err != nil {
		return err
	}
	subnet := net.IPNet{IP: l.Addr.Mask(l.Mask), Mask: l.Mask}
	for _, r := range l.Routes {
		// With a /32 lease (as on GCE) the gateway lies outside the subnet,
		// so it needs an on-link host route before routes through it.
		if !r.Gateway.IsUnspecified() && !subnet.Contains(r.Gateway) && !coveredOnLink(l.Routes, r.Gateway) {
			host := net.IPNet{IP: r.Gateway, Mask: net.CIDRMask(32, 32)}
			if err := nbsys.AddRoute(host, nil, ifc.Index); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
		}
		if err := nbsys.AddRoute(r.Dst, r.Gateway, ifc.Index); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	// EC2, GCE and Azure all serve instance metadata at the link-local
	// 169.254.169.254, reached through the gateway rather than ARPed for
	// directly. Say so explicitly instead of relying on the default route.
	for _, r := range l.Routes {
		if ones, _ := r.Dst.Mask.Size(); ones == 0 && !r.Gateway.IsUnspecified() {
			md := net.IPNet{IP: net.IPv4(169, 254, 169, 254).To4(), Mask: net.CIDRMask(32, 32)}
			if err := nbsys.AddRoute(md, r.Gateway, ifc.Index); err != nil && !errors.Is(err, unix.EEXIST) {
				logf("warning: route to metadata service: %v", err)
			}
			break
		}
	}
	if err := os.WriteFile("/var/run/resolv.conf", []byte(resolvConf(l.DNS, l.Domain, l.Search)), 0o644); err != nil {
		return err
	}
	if hostname == "" {
		hostname = l.Hostname
	}
	if hostname != "" {
		if err := nbsys.SetHostname(hostname); err != nil {
			logf("warning: %v", err)
		}
	}
	return nil
}

func coveredOnLink(routes []dhcp.Route, ip net.IP) bool {
	for _, r := range routes {
		if r.Gateway.IsUnspecified() && r.Dst.Contains(ip) {
			return true
		}
	}
	return false
}

// configureNetwork brings up lo0 and, unless networking is off, the first
// Ethernet interface by DHCP. It returns the lease, or nil without one.
func configureNetwork(cfg *Config) (*dhcp.Lease, error) {
	if err := configureLoopback(); err != nil {
		return nil, err
	}
	if cfg.Hostname != "" {
		nbsys.SetHostname(cfg.Hostname)
	}
	if cfg.Network == "none" {
		return nil, nil
	}
	ifc, err := pickInterface(cfg.Interface)
	if err != nil {
		return nil, err
	}
	if err := nbsys.IfUp(ifc.Name); err != nil {
		return nil, err
	}
	// NetBSD runs IPv4 duplicate address detection on new addresses and
	// keeps them unusable ("tentative") for about three seconds. A DHCP
	// lease is already ours, and the delay made the first metadata and NTP
	// requests time out.
	if err := nbsys.SetSysctlInt("net.inet.ip.dad_count", 0); err != nil {
		logf("warning: %v", err)
	}
	link, err := nbsys.OpenBPF(ifc.Name, ifc.HardwareAddr)
	if err != nil {
		return nil, err
	}
	client := &dhcp.Client{Link: link, XID: randomXID(), Logf: func(f string, a ...any) { logf("dhcp %s: "+f, append([]any{ifc.Name}, a...)...) }}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	lease, err := client.Acquire(ctx)
	if err != nil {
		link.Close()
		return nil, fmt.Errorf("dhcp on %s: %w", ifc.Name, err)
	}
	if err := applyLease(ifc, lease, cfg.Hostname); err != nil {
		link.Close()
		return nil, err
	}
	logf("network: %s %s", ifc.Name, lease)
	go renewLoop(client, ifc, lease)
	return lease, nil
}

// renewLoop re-runs DHCP at half the lease time. A changed address is only
// logged: moving a running service to a new address is left to the platform.
func renewLoop(c *dhcp.Client, ifc *net.Interface, l *dhcp.Lease) {
	for l.LeaseTime > 0 {
		time.Sleep(l.LeaseTime / 2)
		ctx, cancel := context.WithTimeout(context.Background(), l.LeaseTime/4)
		nl, err := c.Acquire(ctx)
		cancel()
		if err != nil {
			logf("dhcp renew on %s: %v", ifc.Name, err)
			continue
		}
		if !nl.Addr.Equal(l.Addr) {
			logf("dhcp renew on %s: address changed from %s to %s; keeping the old one", ifc.Name, l.Addr, nl.Addr)
		}
		l = nl
	}
}

// discoverInstance identifies the platform from SMBIOS, reads its metadata
// service, and publishes the result under /var/run/appliance and in the
// program's environment.
func discoverInstance(cfg *Config) *metadata.Info {
	vendor, _ := unix.Sysctl("machdep.dmi.system-vendor")
	product, _ := unix.Sysctl("machdep.dmi.system-product")
	info := &metadata.Info{Platform: metadata.Detect(vendor, product)}
	if info.Platform == metadata.QEMU {
		if dev, err := nbsys.OpenFWCfg(); err == nil {
			if entries, err := metadata.ReadFWCfg(dev); err != nil {
				logf("metadata: %v", err)
			} else {
				info = metadata.QEMUInfo(entries)
			}
			dev.Close()
		}
	}
	if info.Platform == metadata.EC2 && cfg.Network != "none" {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		fetched, err := (&metadata.EC2Client{}).Fetch(ctx)
		cancel()
		if err != nil {
			logf("metadata: %v", err)
		} else {
			info = fetched
		}
	}
	logf("platform: %s %s", info.Platform, info.InstanceID)
	if err := os.MkdirAll(metadataDir, 0o755); err != nil {
		logf("metadata: %v", err)
		return info
	}
	for name, data := range info.Files() {
		if err := os.WriteFile(metadataDir+"/"+name, data, 0o644); err != nil {
			logf("metadata: %v", err)
		}
	}
	cfg.addPlatformEnv(info.Env())
	applyUserDataEnv(cfg, info.UserData)
	return info
}

// applyUserDataEnv adds the variables user-data defines, if it is written
// as JSON or KEY=VALUE lines, to the program's environment. Only names are logged:
// values may be secrets, and the console is often readable by others.
func applyUserDataEnv(cfg *Config, ud []byte) {
	if len(ud) == 0 || !cfg.useUserDataEnv() {
		return
	}
	names, refused, err := cfg.setUserEnv(ud)
	if err != nil {
		logf("user-data: not an environment (%v); not using it", err)
		return
	}
	if len(refused) > 0 {
		logf("user-data: ignoring reserved variables %s", strings.Join(refused, " "))
	}
	if len(names) > 0 {
		logf("user-data: environment %s", strings.Join(names, " "))
	}
}

// startTimesync sets the clock from the configured or platform time servers
// before the program starts, then keeps it in step in the background.
// Progress goes to the console and to /var/run/appliance/timesync.
func startTimesync(cfg *Config, info *metadata.Info, lease *dhcp.Lease) {
	platform := info.Platform
	writeStatus := func(s string) {
		if err := os.WriteFile(metadataDir+"/timesync", []byte(s+"\n"), 0o644); err != nil {
			logf("time: %v", err)
		}
	}
	var dhcpNTP []net.IP
	if lease != nil {
		dhcpNTP = lease.NTP
	}
	servers := cfg.timeServers(string(platform), info.TimeServers, dhcpNTP)
	switch {
	case cfg.Timesync == "off":
		writeStatus("off")
		return
	case len(servers) == 0 && platform == metadata.Azure:
		writeStatus("hvtimesync") // the kernel follows the Hyper-V host
		return
	case len(servers) == 0:
		logf("time: no time servers known; the clock is not synchronized")
		writeStatus("not synchronized: no time servers")
		return
	}
	s := &sntp.Syncer{
		Servers:  servers,
		Clock:    nbsys.SystemClock{},
		Logf:     logf,
		OnStatus: func(st sntp.Status) { writeStatus(st.String()) },
	}
	// Quick attempts at boot: the first packet is often lost while the
	// gateway's ARP entry resolves. Give up after ~15s and let the
	// background loop keep trying.
	s.QueryTimeout = 1500 * time.Millisecond
	for attempt := 0; attempt < 8; attempt++ {
		if st := s.SyncOnce(context.Background()); st.Err == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.QueryTimeout = 0
	go s.Run(context.Background())
}

// halt powers off, reboots or halts the machine after syncing disks.
func halt(how string) {
	howto := nbsys.RBPowerdown
	switch how {
	case "reboot":
		howto = nbsys.RBAutoboot
	case "halt":
		howto = nbsys.RBHalt
	}
	logf("%s", how)
	flushLogs()
	// Let the console finish printing everything init wrote before the
	// kernel starts printing its own shutdown messages, which bypass the tty
	// queue and would otherwise interleave character by character with ours.
	drainConsole()
	unix.Sync()
	if err := nbsys.Reboot(howto); err != nil {
		logf("reboot(2): %v", err)
	}
	select {}
}

// drainConsole waits, up to a second, until output queued on the console has
// been transmitted (tcdrain(3)).
func drainConsole() {
	done := make(chan struct{})
	go func() {
		unix.Syscall(unix.SYS_IOCTL, uintptr(unix.Stdout), unix.TIOCDRAIN, 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

// fatal reports an unrecoverable error and powers off: PID 1 must never exit,
// or the kernel panics.
func fatal(err error) {
	logf("fatal: %v", err)
	time.Sleep(3 * time.Second)
	halt("poweroff")
}

type exitEvent struct {
	pid    int
	status unix.WaitStatus
}

// reap collects every exited child, as PID 1 inherits all orphans, and
// reports each one on ch.
func reap(sigchld <-chan os.Signal, ch chan<- exitEvent) {
	for range sigchld {
		for {
			var ws unix.WaitStatus
			pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			ch <- exitEvent{pid, ws}
		}
	}
}

// appOutput tracks the goroutines copying the running program's output;
// halt waits for them so the last lines are shipped.
var appOutput sync.WaitGroup

// start runs the program. When logs are shipped, its stdout and stderr are
// pipes whose lines go both to the console and to the collector; otherwise
// it writes to the console directly.
func start(cfg *Config) (*os.Process, error) {
	argv := append([]string{cfg.Exec}, cfg.Args...)
	attr := &os.ProcAttr{
		Dir:   "/",
		Env:   cfg.environ(),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &unix.SysProcAttr{Setsid: true},
	}
	if shipper == nil {
		return os.StartProcess(cfg.Exec, argv, attr)
	}
	type stream struct {
		name string
		sev  otellog.Severity
		r, w *os.File
	}
	streams := []*stream{{name: "stdout", sev: otellog.SeverityInfo}, {name: "stderr", sev: otellog.SeverityInfo}}
	for _, st := range streams {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		st.r, st.w = r, w
	}
	attr.Files = []*os.File{os.Stdin, streams[0].w, streams[1].w}
	p, err := os.StartProcess(cfg.Exec, argv, attr)
	for _, st := range streams {
		st.w.Close() // the child has its own copy
		if err != nil {
			st.r.Close()
		}
	}
	if err != nil {
		return nil, err
	}
	for _, st := range streams {
		c := &logship.Capture{Console: os.Stdout, Shipper: shipper, Scope: "app", Stream: st.name, Severity: st.sev}
		appOutput.Add(1)
		go func() {
			defer appOutput.Done()
			io.Copy(c, st.r)
			c.Flush()
			st.r.Close()
		}()
	}
	return p, nil
}

// The collector embedded in this executable, run as a child process.
const collectorConfig = "/var/run/otelcol/config.yaml"

var (
	collectorMu   sync.Mutex
	collectorProc *os.Process
	collectorArgs []string
	collectorEnv  []string
)

func startCollector() error {
	p, err := os.StartProcess("/sbin/init", append([]string{"/sbin/init", "otelcol"}, collectorArgs...), &os.ProcAttr{
		Dir:   "/",
		Env:   collectorEnv,
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &unix.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return err
	}
	collectorMu.Lock()
	collectorProc = p
	collectorMu.Unlock()
	return nil
}

func collectorPid() int {
	collectorMu.Lock()
	defer collectorMu.Unlock()
	if collectorProc == nil {
		return 0
	}
	return collectorProc.Pid
}

// startLogging decides where logs go, writes the collector configuration,
// starts the collector and points init's log pipeline at it.
func startLogging(cfg *Config, info *metadata.Info, lease *dhcp.Lease) {
	host, _ := os.Hostname()
	in := otelconfig.Instance{
		Platform:   string(info.Platform),
		InstanceID: info.InstanceID,
		Region:     info.Region,
		Zone:       info.Zone,
		Hostname:   host,
		// GCE instances normally run with a service account.
		HasCredentials: info.HasCredentials || info.Platform == metadata.GCE,
		OTLPEndpoint:   info.OTLPEndpoint,
	}
	oc := cfg.Otel
	oc.ServiceName = cfg.serviceName()
	choice := otelconfig.Resolve(&oc, in)
	logf("telemetry: %s", otelconfig.Describe(choice, &oc, in))
	if choice == otelconfig.None {
		return
	}
	uri := "file:" + oc.ConfigFile
	if choice != otelconfig.Custom {
		yaml, err := otelconfig.CollectorConfig(choice, &oc, in)
		if err == nil {
			err = os.MkdirAll(path.Dir(collectorConfig), 0o755)
		}
		if err == nil {
			err = os.WriteFile(collectorConfig, []byte(yaml), 0o600)
		}
		if err != nil {
			logf("logs: %v; not shipping", err)
			return
		}
		uri = "file:" + collectorConfig
	}
	collectorArgs = []string{uri}
	var userNames []string
	collectorEnv, userNames = cfg.collectorEnviron(otelconfig.Env(&oc, in))
	if len(userNames) > 0 {
		logf("telemetry: collector environment from user-data %s", strings.Join(userNames, " "))
	}
	if err := startCollector(); err != nil {
		logf("logs: starting the collector: %v; not shipping", err)
		return
	}
	logf("started the OpenTelemetry collector (pid %d)", collectorPid())
	s, err := logship.New(context.Background(), "http://"+otelconfig.HTTPListen, logship.Resource(oc.ServiceName, in))
	if err != nil {
		logf("logs: %v; not shipping", err)
		return
	}
	shipper = s
	// Point the program's own OpenTelemetry SDK, if it has one, at the
	// collector: its traces and metrics follow the same route as the logs.
	cfg.addPlatformEnv(otelconfig.AppEnv(&oc, in))
}

// flushLogs ships what the program last wrote, then stops the collector so
// that it exports everything it holds.
func flushLogs() {
	if shipper == nil {
		return
	}
	done := make(chan struct{})
	go func() { appOutput.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := shipper.Shutdown(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "nbinit: logs: flushing: %v\n", err)
	}
	cancel()
	shipper = nil

	collectorMu.Lock()
	p := collectorProc
	collectorProc = nil // stop restarting it
	collectorMu.Unlock()
	if p == nil {
		return
	}
	p.Signal(unix.SIGTERM)
	// reap() collects it; once it has, signal 0 fails with ESRCH.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := unix.Kill(p.Pid, 0); errors.Is(err, unix.ESRCH) {
			return
		}
	}
	fmt.Fprintf(os.Stderr, "nbinit: logs: the collector did not stop; killing it\n")
	p.Kill()
}

// runCollector is "/sbin/init otelcol URI...": the embedded collector. It
// handles SIGTERM itself, shutting down gracefully with a fresh context.
func runCollector(uris []string) {
	if err := otelagent.Run(context.Background(), uris...); err != nil {
		fmt.Fprintf(os.Stderr, "otelcol: %v\n", err)
		os.Exit(1)
	}
}

func describe(ws unix.WaitStatus) string {
	if ws.Signaled() {
		return "killed by " + ws.Signal().String()
	}
	return fmt.Sprintf("exit status %d", ws.ExitStatus())
}

// watchPower takes over /dev/power so that the ACPI power button, a Hyper-V
// shutdown request and similar switches reach init instead of making the
// kernel power off on its own, and reports each press on the returned
// channel ("poweroff" or "reboot"). Clouds use the power button for "stop
// instance", so this is what lets the program shut down cleanly.
func watchPower() <-chan string {
	ch := make(chan string, 1)
	p, err := nbsys.OpenPower()
	if err != nil {
		logf("warning: %v; the kernel will handle the power button", err)
		return ch
	}
	go func() {
		for {
			ev, err := p.Next()
			if err != nil {
				logf("warning: reading /dev/power: %v", err)
				return
			}
			name := ev.Name
			if name == "" {
				name = "power switch"
			}
			how := "poweroff"
			if ev.Reset {
				how = "reboot"
			}
			logf("%s pressed", name)
			select {
			case ch <- how:
			default: // a shutdown is already pending
			}
		}
	}()
	return ch
}

// supervise runs the program, applies the exit policy, and turns signals
// sent to init and power button presses into an orderly shutdown.
func supervise(cfg *Config, power <-chan string) {
	sigchld := make(chan os.Signal, 1)
	signal.Notify(sigchld, unix.SIGCHLD)
	exits := make(chan exitEvent, 16)
	go reap(sigchld, exits)

	shutdown := make(chan os.Signal, 1)
	// SIGTERM: power off; SIGINT: reboot; SIGUSR1: halt (as with init(8)).
	signal.Notify(shutdown, unix.SIGTERM, unix.SIGINT, unix.SIGUSR1)

	var proc *os.Process
	restart := time.NewTimer(0)
	for {
		select {
		case <-restart.C:
			p, err := start(cfg)
			if err != nil {
				logf("start %s: %v", cfg.Exec, err)
				if cfg.OnExit != "restart" {
					fatal(err)
				}
				restart.Reset(cfg.restartDelay())
				continue
			}
			proc = p
			logf("started %s (pid %d)", cfg.Exec, p.Pid)

		case ev := <-exits:
			if pid := collectorPid(); pid != 0 && ev.pid == pid {
				logf("the OpenTelemetry collector (pid %d) exited: %s; restarting", ev.pid, describe(ev.status))
				time.AfterFunc(2*time.Second, func() {
					if collectorPid() == pid { // not stopped meanwhile
						if err := startCollector(); err != nil {
							logf("logs: restarting the collector: %v", err)
						}
					}
				})
				continue
			}
			if proc == nil || ev.pid != proc.Pid {
				continue // an orphan
			}
			logf("%s (pid %d) exited: %s", cfg.Exec, ev.pid, describe(ev.status))
			proc = nil
			if cfg.OnExit != "restart" {
				halt(cfg.OnExit)
			}
			restart.Reset(cfg.restartDelay())

		case sig := <-shutdown:
			how := map[os.Signal]string{unix.SIGTERM: "poweroff", unix.SIGINT: "reboot", unix.SIGUSR1: "halt"}[sig]
			logf("received %v; stopping", sig)
			restart.Stop()
			stop(proc, exits, cfg.stopTimeout())
			halt(how)

		case how := <-power:
			logf("stopping for %s", how)
			restart.Stop()
			stop(proc, exits, cfg.stopTimeout())
			halt(how)
		}
	}
}

func stop(proc *os.Process, exits <-chan exitEvent, timeout time.Duration) {
	if proc == nil {
		return
	}
	logf("sending SIGTERM to pid %d", proc.Pid)
	proc.Signal(unix.SIGTERM)
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-exits:
			if ev.pid == proc.Pid {
				logf("pid %d exited: %s", ev.pid, describe(ev.status))
				return
			}
		case <-deadline:
			logf("pid %d did not exit after %v; killing", proc.Pid, timeout)
			proc.Kill()
			return
		}
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "otelcol" {
		runCollector(os.Args[2:])
		return
	}
	setupConsole()
	logf("starting (pid %d)", os.Getpid())
	cfg, err := loadConfig()
	if err != nil {
		fatal(err)
	}
	if err := mountFilesystems(); err != nil {
		fatal(err)
	}
	// Before anything slow (DHCP can take a while), so an early press is
	// queued for supervise rather than handled abruptly by the kernel.
	power := watchPower()
	lease, err := configureNetwork(cfg)
	if err != nil {
		// Run the program anyway; it may not need the network, and the
		// console log says what went wrong.
		logf("network: %v", err)
	}
	info := discoverInstance(cfg)
	startTimesync(cfg, info, lease)
	startLogging(cfg, info, lease)
	logf("ready")
	supervise(cfg, power)
}
