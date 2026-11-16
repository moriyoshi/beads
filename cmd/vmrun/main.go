// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command vmrun boots a disk image under QEMU, either interactively (serial
// console on the terminal) or as a test that waits for the init shim to come
// up, probes an HTTP endpoint inside the guest, and checks for a clean ACPI
// poweroff.
//
// Every flag has an environment-variable default (VMRUN_*), which is how the
// Bazel rules configure it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moriyoshi/beads/internal/otlpsink"
	"github.com/moriyoshi/beads/internal/runfiles"
	"github.com/moriyoshi/beads/internal/sntp"
)

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

type options struct {
	arch     string
	image    string
	firmware string
	disk     string
	memory   string
	smp      string
	forward  string
	userData string // file passed as opt/beads/user-data
	test     bool
	ready    string
	httpPort int
	httpPath string
	httpWant string
	shutdown []string
	timesync bool
	logs     []string // lines that must arrive over OTLP
	spans    []string // span names that must arrive
	metrics  []string // metric names that must arrive
	timeout  time.Duration
	qemu     string
	accel    string
}

func parseFlags() *options {
	o := &options{}
	flag.StringVar(&o.arch, "arch", envOr("VMRUN_ARCH", "x86_64"), "guest architecture: x86_64 or aarch64")
	flag.StringVar(&o.image, "image", envOr("VMRUN_IMAGE", ""), "raw disk image (path or runfiles location)")
	flag.StringVar(&o.firmware, "firmware", envOr("VMRUN_FIRMWARE", "bios"), "bios or uefi")
	flag.StringVar(&o.disk, "disk", envOr("VMRUN_DISK", "virtio"), "disk interface: virtio, nvme or ahci")
	flag.StringVar(&o.memory, "memory", envOr("VMRUN_MEMORY", "512"), "guest memory in MiB")
	flag.StringVar(&o.smp, "smp", envOr("VMRUN_SMP", "2"), "guest CPUs")
	flag.StringVar(&o.forward, "forward", envOr("VMRUN_FORWARD", ""), "comma-separated host:guest TCP port forwards (interactive mode)")
	flag.StringVar(&o.userData, "user-data", envOr("VMRUN_USER_DATA", ""), "file to pass as instance user-data through fw_cfg (path or runfiles location)")
	flag.BoolVar(&o.test, "test", envOr("VMRUN_TEST", "") == "1", "run as a boot test")
	flag.StringVar(&o.ready, "ready", envOr("VMRUN_READY", "nbinit: ready"), "console line marking a booted guest")
	port, _ := strconv.Atoi(envOr("VMRUN_HTTP_PORT", "0"))
	flag.IntVar(&o.httpPort, "http-port", port, "guest TCP port to probe with HTTP GET (0 = skip)")
	flag.StringVar(&o.httpPath, "http-path", envOr("VMRUN_HTTP_PATH", "/"), "path to GET")
	flag.StringVar(&o.httpWant, "http-expect", envOr("VMRUN_HTTP_EXPECT", ""), "substring the response body must contain")
	flag.BoolVar(&o.timesync, "timesync-check", envOr("VMRUN_TIMESYNC_CHECK", "") == "1", "boot with the RTC in 2001 and require init to correct it from vmrun's NTP server")
	spansExpect := flag.String("spans-expect", envOr("VMRUN_SPANS_EXPECT", ""), "|-separated span names the guest must ship over OTLP")
	metricsExpect := flag.String("metrics-expect", envOr("VMRUN_METRICS_EXPECT", ""), "|-separated metric names the guest must ship over OTLP")
	logsExpect := flag.String("logs-expect", envOr("VMRUN_LOGS_EXPECT", ""), "|-separated lines the guest must ship over OTLP to vmrun's receiver")
	shutdown := flag.String("shutdown-expect", envOr("VMRUN_SHUTDOWN_EXPECT", ""), "|-separated console lines that must appear after the ACPI power button is pressed")
	timeout, _ := time.ParseDuration(envOr("VMRUN_TIMEOUT", "0"))
	flag.DurationVar(&o.timeout, "timeout", timeout, "overall test deadline (default: from TEST_TIMEOUT, else 5m)")
	flag.StringVar(&o.qemu, "qemu", envOr("QEMU", ""), "qemu-system-x86_64 binary")
	flag.StringVar(&o.accel, "accel", envOr("VMRUN_ACCEL", ""), "QEMU accelerator (default: kvm or hvf when usable, else tcg)")
	flag.Parse()
	split := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, "|") {
			if s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	o.spans, o.metrics = split(*spansExpect), split(*metricsExpect)
	for _, s := range strings.Split(*logsExpect, "|") {
		if s != "" {
			o.logs = append(o.logs, s)
		}
	}
	for _, s := range strings.Split(*shutdown, "|") {
		if s != "" {
			o.shutdown = append(o.shutdown, s)
		}
	}
	if o.timeout == 0 {
		if s, err := strconv.Atoi(os.Getenv("TEST_TIMEOUT")); err == nil && s > 15 {
			o.timeout = time.Duration(s-10) * time.Second
		} else {
			o.timeout = 5 * time.Minute
		}
	}
	return o
}

func findExecutable(name string) (string, error) {
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	// Bazel tests run with a minimal PATH; look where package managers put
	// QEMU.
	for _, d := range []string{"/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/run/current-system/sw/bin"} {
		p := filepath.Join(d, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s not found; install QEMU or set $QEMU", name)
}

// ovmf locates UEFI firmware code and a variable-store template for arch.
func ovmf(arch string) (code, vars string, err error) {
	if c := os.Getenv("OVMF_CODE"); c != "" {
		return c, os.Getenv("OVMF_VARS"), nil
	}
	if arch == "aarch64" {
		return findPair([][2]string{
			{"/opt/homebrew/share/qemu/edk2-aarch64-code.fd", "/opt/homebrew/share/qemu/edk2-arm-vars.fd"},
			{"/usr/local/share/qemu/edk2-aarch64-code.fd", "/usr/local/share/qemu/edk2-arm-vars.fd"},
			{"/usr/share/AAVMF/AAVMF_CODE.fd", "/usr/share/AAVMF/AAVMF_VARS.fd"},
			{"/usr/share/edk2/aarch64/QEMU_EFI-pflash.raw", "/usr/share/edk2/aarch64/vars-template-pflash.raw"},
			{"/usr/share/qemu/edk2-aarch64-code.fd", "/usr/share/qemu/edk2-arm-vars.fd"},
		})
	}
	return findPair([][2]string{
		{"/opt/homebrew/share/qemu/edk2-x86_64-code.fd", "/opt/homebrew/share/qemu/edk2-i386-vars.fd"},
		{"/usr/local/share/qemu/edk2-x86_64-code.fd", "/usr/local/share/qemu/edk2-i386-vars.fd"},
		{"/usr/share/OVMF/OVMF_CODE_4M.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
		{"/usr/share/OVMF/OVMF_CODE.fd", "/usr/share/OVMF/OVMF_VARS.fd"},
		{"/usr/share/edk2/ovmf/OVMF_CODE.fd", "/usr/share/edk2/ovmf/OVMF_VARS.fd"},
		{"/usr/share/qemu/edk2-x86_64-code.fd", "/usr/share/qemu/edk2-i386-vars.fd"},
	})
}

func findPair(pairs [][2]string) (code, vars string, err error) {
	for _, p := range pairs {
		if _, err := os.Stat(p[0]); err == nil {
			if _, err := os.Stat(p[1]); err == nil {
				return p[0], p[1], nil
			}
		}
	}
	return "", "", errors.New("UEFI firmware (OVMF) not found; set $OVMF_CODE and $OVMF_VARS")
}

// hostArch maps GOARCH to QEMU's architecture names.
func hostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	}
	return runtime.GOARCH
}

// defaultAccel picks hardware virtualization when the guest architecture
// matches the host's and it is usable, and TCG emulation otherwise.
func defaultAccel(arch string) string {
	if arch == hostArch() {
		switch runtime.GOOS {
		case "linux":
			if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err == nil {
				f.Close()
				return "kvm"
			}
		case "darwin":
			return "hvf"
		}
	}
	return "tcg,thread=multi"
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// guestHost is the host as seen from a QEMU user-mode network guest.
const guestHost = "10.0.2.2"

// skewedRTC is where the timesync check starts the guest's clock.
const skewedRTC = "2001-01-01T00:00:00"

// startNTP serves SNTP with the host's clock on a free loopback port, which
// the guest reaches as guestHost:port.
func startNTP() (port int, stop func(), err error) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, err
	}
	go sntp.Serve(conn, time.Now)
	return conn.LocalAddr().(*net.UDPAddr).Port, func() { conn.Close() }, nil
}

// qemuArgs builds the command line. scratch holds the writable UEFI
// variable store; ntpPort and otlpPort, if non-zero, are passed to the
// guest through fw_cfg as its time server and its OTLP logs endpoint.
func qemuArgs(o *options, image, scratch string, forwards [][2]int, qmpPort, ntpPort, otlpPort int) ([]string, error) {
	accel := o.accel
	if accel == "" {
		accel = defaultAccel(o.arch)
	}
	var args []string
	switch o.arch {
	case "x86_64":
		args = []string{"-machine", "q35"}
	case "aarch64":
		cpu := "max"
		if !strings.HasPrefix(accel, "tcg") {
			cpu = "host"
		}
		args = []string{"-machine", "virt", "-cpu", cpu}
		if o.firmware != "uefi" {
			return nil, fmt.Errorf("aarch64 guests boot with UEFI only (got firmware %q)", o.firmware)
		}
	default:
		return nil, fmt.Errorf("unknown arch %q", o.arch)
	}
	args = append(args,
		"-accel", accel,
		"-m", o.memory, "-smp", o.smp,
		"-nodefaults", "-no-user-config",
		"-drive", "file="+image+",format=raw,if=none,id=disk0,snapshot=on",
	)
	switch o.disk {
	case "virtio":
		args = append(args, "-device", "virtio-blk-pci,drive=disk0")
	case "nvme":
		args = append(args, "-device", "nvme,drive=disk0,serial=vmrun0")
	case "ahci":
		args = append(args, "-device", "ahci,id=ahci0", "-device", "ide-hd,drive=disk0,bus=ahci0.0")
	default:
		return nil, fmt.Errorf("unknown disk interface %q", o.disk)
	}
	netdev := "user,id=net0"
	for _, f := range forwards {
		netdev += fmt.Sprintf(",hostfwd=tcp:127.0.0.1:%d-:%d", f[0], f[1])
	}
	args = append(args, "-netdev", netdev, "-device", "virtio-net-pci,netdev=net0")
	if o.userData != "" {
		// QEMU option values escape commas by doubling them.
		args = append(args, "-fw_cfg", "name=opt/beads/user-data,file="+strings.ReplaceAll(o.userData, ",", ",,"))
	}
	if ntpPort != 0 {
		args = append(args, "-fw_cfg", fmt.Sprintf("name=opt/beads/time-servers,string=%s:%d", guestHost, ntpPort))
	}
	if otlpPort != 0 {
		args = append(args, "-fw_cfg", fmt.Sprintf("name=opt/beads/otlp-endpoint,string=http://%s:%d", guestHost, otlpPort))
	}
	if o.timesync {
		args = append(args, "-rtc", "base="+skewedRTC)
	}

	switch o.firmware {
	case "bios":
	case "uefi":
		code, varsTmpl, err := ovmf(o.arch)
		if err != nil {
			return nil, err
		}
		vars := filepath.Join(scratch, "efivars.fd")
		b, err := os.ReadFile(varsTmpl)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(vars, b, 0o644); err != nil {
			return nil, err
		}
		args = append(args,
			"-drive", "if=pflash,format=raw,unit=0,readonly=on,file="+code,
			"-drive", "if=pflash,format=raw,unit=1,file="+vars)
	default:
		return nil, fmt.Errorf("unknown firmware %q", o.firmware)
	}

	if o.test {
		args = append(args, "-display", "none", "-serial", "stdio",
			"-qmp", fmt.Sprintf("tcp:127.0.0.1:%d,server=on,wait=off", qmpPort))
	} else {
		args = append(args, "-nographic", "-serial", "mon:stdio")
	}
	return args, nil
}

func parseForwards(s string) ([][2]int, error) {
	var out [][2]int
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		h, g, ok := strings.Cut(f, ":")
		if !ok {
			g = h
		}
		hp, err1 := strconv.Atoi(h)
		gp, err2 := strconv.Atoi(g)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad forward %q (want host:guest)", f)
		}
		out = append(out, [2]int{hp, gp})
	}
	return out, nil
}

func distinct(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}

func main() {
	o := parseFlags()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "vmrun:", err)
		os.Exit(1)
	}
}

func run(o *options) error {
	if o.image == "" {
		return errors.New("no image given (-image or $VMRUN_IMAGE)")
	}
	image, err := runfiles.Resolve(o.image)
	if err != nil {
		return err
	}
	if o.userData != "" {
		if o.userData, err = runfiles.Resolve(o.userData); err != nil {
			return err
		}
	}
	qemu := o.qemu
	if qemu == "" {
		if qemu, err = findExecutable("qemu-system-" + o.arch); err != nil {
			return err
		}
	}
	scratch, err := os.MkdirTemp(os.Getenv("TEST_TMPDIR"), "vmrun-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	if !o.test {
		forwards, err := parseForwards(o.forward)
		if err != nil {
			return err
		}
		// Give the guest a time server, as clouds do.
		ntpPort, stopNTP, err := startNTP()
		if err != nil {
			return err
		}
		defer stopNTP()
		args, err := qemuArgs(o, image, scratch, forwards, 0, ntpPort, 0)
		if err != nil {
			return err
		}
		for _, f := range forwards {
			fmt.Fprintf(os.Stderr, "vmrun: forwarding 127.0.0.1:%d -> guest:%d\n", f[0], f[1])
		}
		fmt.Fprintln(os.Stderr, "vmrun: serial console attached; press Ctrl-A X to quit")
		cmd := exec.Command(qemu, args...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		return cmd.Run()
	}
	return bootTest(o, qemu, image, scratch)
}

func bootTest(o *options, qemu, image, scratch string) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()

	var forwards [][2]int
	hostPort := 0
	if o.httpPort != 0 {
		p, err := freePort()
		if err != nil {
			return err
		}
		hostPort = p
		forwards = append(forwards, [2]int{p, o.httpPort})
	}
	qmpPort, err := freePort()
	if err != nil {
		return err
	}
	ntpPort, stopNTP, err := startNTP()
	if err != nil {
		return err
	}
	defer stopNTP()
	// Receive the guest's logs, as a cloud log service would.
	sink, err := otlpsink.Start("127.0.0.1:0")
	if err != nil {
		return err
	}
	defer sink.Close()
	args, err := qemuArgs(o, image, scratch, forwards, qmpPort, ntpPort, sink.Port())
	if err != nil {
		return err
	}
	fmt.Printf("vmrun: %s %s\n", qemu, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, qemu, args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var logw io.Writer = os.Stdout
	if d := os.Getenv("TEST_UNDECLARED_OUTPUTS_DIR"); d != "" {
		if f, err := os.Create(filepath.Join(d, "serial-"+o.firmware+"-"+o.disk+".log")); err == nil {
			defer f.Close()
			logw = io.MultiWriter(os.Stdout, f)
		}
	}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	ready := make(chan struct{})
	var (
		mu       sync.Mutex
		afterOff []string // console lines after the power button press
		pressed  bool
		stepped  bool // init reported setting the clock
	)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		seen := false
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			fmt.Fprintln(logw, line)
			mu.Lock()
			if pressed {
				afterOff = append(afterOff, line)
			}
			if strings.Contains(line, "nbinit: time: stepped") {
				stepped = true
			}
			mu.Unlock()
			if !seen && strings.Contains(line, o.ready) {
				seen = true
				close(ready)
			}
		}
		exited <- cmd.Wait()
	}()

	select {
	case <-ready:
		fmt.Printf("vmrun: guest ready after %v\n", time.Since(start).Round(time.Second))
		mu.Lock()
		ok := stepped
		mu.Unlock()
		if o.timesync && !ok {
			cmd.Process.Kill()
			return fmt.Errorf("guest booted with its clock at %s but init did not step it from the NTP server before reporting ready", skewedRTC)
		}
	case err := <-exited:
		return fmt.Errorf("QEMU exited before the guest was ready: %v", err)
	case <-ctx.Done():
		return fmt.Errorf("guest not ready within %v", o.timeout)
	}

	if o.httpPort != 0 {
		if err := probe(ctx, hostPort, o); err != nil {
			cmd.Process.Kill()
			return err
		}
	}

	mu.Lock()
	pressed = true
	mu.Unlock()
	if err := qmpPowerdown(qmpPort); err != nil {
		cmd.Process.Kill()
		return fmt.Errorf("QMP: %w", err)
	}
	select {
	case err := <-exited:
		if err != nil {
			return fmt.Errorf("QEMU did not exit cleanly after powerdown: %w", err)
		}
	case <-ctx.Done():
		return errors.New("guest did not power off after ACPI powerdown")
	}
	fmt.Printf("vmrun: OTLP receiver got %s\n", sink.Summary())
	if names := distinct(sink.Metrics()); len(names) > 0 {
		fmt.Printf("vmrun: metrics: %s\n", strings.Join(names, " "))
	}
	if names := distinct(sink.Spans()); len(names) > 0 {
		fmt.Printf("vmrun: spans: %s\n", strings.Join(names, " "))
	}
	if missing := sink.Missing(o.logs); len(missing) > 0 {
		return fmt.Errorf("the guest never shipped log lines containing %q over OTLP (got %s)", missing, sink.Summary())
	}
	if missing := otlpsink.MissingNames(sink.Spans(), o.spans); len(missing) > 0 {
		return fmt.Errorf("the guest never shipped spans named %q over OTLP (got %s)", missing, sink.Summary())
	}
	if missing := otlpsink.MissingNames(sink.Metrics(), o.metrics); len(missing) > 0 {
		return fmt.Errorf("the guest never shipped metrics named %q over OTLP (got %s)", missing, sink.Summary())
	}
	log := strings.Join(afterOff, "\n")
	for _, want := range o.shutdown {
		if !strings.Contains(log, want) {
			return fmt.Errorf("after the power button press the console never showed %q", want)
		}
	}
	fmt.Printf("vmrun: PASS (%s firmware, %s disk, %v)\n", o.firmware, o.disk, time.Since(start).Round(time.Second))
	return nil
}

// probe GETs the forwarded guest port until it answers or ctx ends; the
// program may start listening some time after init reports ready.
func probe(ctx context.Context, hostPort int, o *options) error {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, o.httpPath)
	client := &http.Client{Timeout: 10 * time.Second}
	var last error
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.Contains(string(body), o.httpWant) {
				fmt.Printf("vmrun: GET guest:%d%s -> %d %q\n", o.httpPort, o.httpPath, resp.StatusCode, strings.TrimSpace(string(body)))
				return nil
			}
			last = fmt.Errorf("GET %s: status %d, body %q (want %q)", o.httpPath, resp.StatusCode, body, o.httpWant)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("HTTP probe of guest port %d failed: %v", o.httpPort, last)
		case <-time.After(2 * time.Second):
		}
	}
}

// qmpPowerdown asks QEMU to press the ACPI power button.
func qmpPowerdown(port int) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 10*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(30 * time.Second))
	dec := json.NewDecoder(c)
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil { // greeting
		return err
	}
	for _, cmd := range []string{"qmp_capabilities", "system_powerdown"} {
		if _, err := fmt.Fprintf(c, `{"execute":%q}`+"\n", cmd); err != nil {
			return err
		}
		for {
			msg = nil
			if err := dec.Decode(&msg); err != nil {
				return err
			}
			if e, ok := msg["error"]; ok {
				return fmt.Errorf("%s: %v", cmd, e)
			}
			if _, ok := msg["return"]; ok {
				break
			}
			// Asynchronous events may arrive before the reply.
		}
	}
	return nil
}
