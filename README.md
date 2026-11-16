# beads

<img src="./assets/daemon.webp" width="512" />

Bazel rules that package a Go program as a bootable NetBSD appliance image:
a NetBSD kernel, a small Go init, and your program. Nothing else is in the
image: no shell, no libc, no rc scripts.

This works because Go on NetBSD makes raw system calls. A pure-Go
`GOOS=netbsd` binary is fully static. The init shim (`//guest/init`) does the
jobs rc(8) would otherwise do:
- mounts tmpfs on `/tmp` and `/var`;
- configures `lo0`;
- runs DHCP over bpf(4) and writes `/etc/resolv.conf`;
- keeps the clock in step with the platform's time service (SNTP);
- ships logs, metrics and traces through an OpenTelemetry Collector embedded in init;
- runs and restarts the program;
- shuts down cleanly on signals and on the power button.

Images can be built for **amd64** and **aarch64** (arm64).

Status:
- **Milestone 1 (QEMU MVP): done.**
- **Milestone 2 (self-built `CLOUD` kernel): done.**
- **Milestone 3 (EC2): done.** Verified on `t3.micro` and `t3.nano`.
- **arm64: done** for QEMU; EC2 Graviton (`t4g`) is wired up.
- Next: GCE and Azure.

## Usage

```starlark
# MODULE.bazel
bazel_dep(name = "beads", version = "0.0.0")

netbsd = use_extension("@beads//netbsd:extensions.bzl", "netbsd")
netbsd.release(version = "11.1", arch = "amd64")
netbsd.release(version = "11.1", arch = "aarch64")   # if you want arm64 images
use_repo(netbsd, "netbsd_11_1_amd64", "netbsd_11_1_aarch64")
```

```starlark
# BUILD.bazel
load("@rules_go//go:def.bzl", "go_binary")
load("@beads//image:defs.bzl", "netbsd_go_appliance")

go_binary(name = "server", srcs = ["main.go"])

netbsd_go_appliance(
    name = "server_image",
    binary = ":server",            # rebuilt for netbsd/amd64, pure Go
    release = "@netbsd_11_1_amd64//:release",
    args = ["-listen=:8080"],
    http_port = 8080,              # forwarded by .qemu, probed by the boot tests
    http_expect = "hello",
)

netbsd_go_appliance(
    name = "server_arm64_image",
    arch = "aarch64",              # rebuilt for netbsd/arm64; UEFI-only image
    binary = ":server",
    release = "@netbsd_11_1_aarch64//:release",
    args = ["-listen=:8080"],
)
```

| Target | What it is |
|---|---|
| `:server_image.kernel` | the `CLOUD` kernel built from source (pass `kernel = "GENERIC"` for the stock kernel, or a label of your own `netbsd_kernel`) |
| `:server_image.rootfs` | FFSv2 root file system (`.ffs`); output group `spec` has its mtree spec |
| `:server_image.raw` | raw GPT disk. amd64 images boot under BIOS and UEFI; aarch64 images boot under UEFI only. AWS accepts it as-is (`import-snapshot --disk-container Format=RAW`, or the `.publish_ec2` target) |
| `:server_image.qemu` | `bazel run` boots the image with the serial console on your terminal (Ctrl-A X quits) |
| `:server_image_boot_test` | boots under SeaBIOS and OVMF (amd64) or AAVMF (aarch64) with virtio and NVMe disks, waits for init, GETs `http_port`, presses the ACPI power button, and checks that the program was stopped and the guest powered off |

The example in `e2e/` is a working instance:

```sh
bazel build //e2e:hello_image.raw
bazel run   //e2e:hello_image.qemu      # then: curl http://127.0.0.1:8080/
bazel test  //e2e:hello_image_boot_test
```

### Requirements

- **Host**: macOS or Linux, with a C/C++ compiler.
  - On first use, the extension downloads the NetBSD sources (about 430 MB)
    and runs `build.sh tools` to get `makefs`, `installboot` and the cross
    toolchain. This takes about 10 minutes and is not repeated after that.
- **Boot tests and `.qemu` targets**: `qemu-system-x86_64` and/or
  `qemu-system-aarch64`, plus edk2 UEFI firmware (OVMF for x86, AAVMF for
  arm64).
  - `vmrun` searches `PATH`, then `/opt/homebrew`, `/usr/local` and `/usr`.
  - Set `$QEMU`, `$OVMF_CODE` and `$OVMF_VARS` to override what it finds.
  - It uses hardware virtualization when the guest architecture matches the
    host (KVM on Linux, HVF on macOS), and TCG emulation otherwise.
  - On an Apple Silicon Mac, arm64 guests run under HVF and amd64 guests
    under TCG. A boot test takes about 10–20 s either way.
- **Tools build per architecture:** each architecture builds its own NetBSD
  cross toolchain, about 10 minutes each the first time.

## Amazon EC2

```sh
# Publish an AMI (prints its ID as JSON). Flags after `--` go to the tool.
AWS_PROFILE=my-profile bazel run //e2e:hello_image.publish_ec2 -- --region ap-northeast-1

# Publish a temporary AMI, boot it, check it, and delete everything.
AWS_PROFILE=my-profile AWS_REGION=ap-northeast-1 \
  bazel test --config=cloud //e2e:hello_image_ec2_smoke_test
```

- **Upload path:** the raw image goes straight into an EBS snapshot through
  the EBS direct APIs (StartSnapshot, PutSnapshotBlock, CompleteSnapshot).
  - All-zero blocks are skipped, so a 66 MB image uploads in seconds.
  - No S3 bucket or `vmimport` role is needed.
- **AMI settings:**
  - registered with `EnaSupport`, boot mode `uefi-preferred` and IMDSv2-only
    metadata;
  - Nitro instance types only. The default test type is `t3.nano` for amd64
    images and `t4g.nano` for aarch64 images (registered as `arm64`, boot
    mode `uefi`).
- **Before acting**, the tool prints the AWS account and region it is about to
  use.
- **Smoke test steps:**
  1. creates a temporary security group that admits the probed port only
     from this machine's public IP;
  2. launches the instance in the default VPC (or `CLOUD_EC2_SUBNET`),
     with the appliance's `user_data` file as its user-data if one is set;
  3. waits for `nbinit: ready` in the serial console output;
  4. GETs `http_port` on the instance's public IP;
  5. terminates the instance and deletes the security group, AMI and
     snapshot, even when the test fails.
  - The console log is saved as an undeclared test output
    (`ec2-console.log`).
- **IAM permissions needed:**
  - EBS direct API: `ebs:StartSnapshot`, `ebs:PutSnapshotBlock`,
    `ebs:CompleteSnapshot`;
  - EC2: `ec2:DescribeSnapshots`, `RegisterImage`, `DescribeImages`,
    `DeregisterImage`, `DeleteSnapshot`;
  - smoke test only: `ec2:DescribeVpcs`, `DescribeSubnets`,
    `CreateSecurityGroup`, `AuthorizeSecurityGroupIngress`,
    `DeleteSecurityGroup`, `RunInstances`, `DescribeInstances`,
    `GetConsoleOutput`, `TerminateInstances`, `CreateTags`;
  - plus `sts:GetCallerIdentity`.

On boot, init reads SMBIOS to find out which platform it is on. On EC2 it also
reads IMDSv2. It writes what it learns to `/var/run/appliance/`
(`platform`, `instance-id`, `region`, `zone`, `local-ipv4`, `user-data`) and
passes `APPLIANCE_PLATFORM`, `APPLIANCE_INSTANCE_ID` and `APPLIANCE_REGION` to
the program. User-data written as `KEY=VALUE` lines also sets program
environment variables; see [Environment from user-data](#environment-from-user-data).

## Environment from user-data

Instance user-data (EC2 IMDS, or `opt/beads/user-data` on QEMU) that
defines variables becomes part of the program's environment, so one image
can be configured per launch. Three forms are accepted, told apart by the
first non-blank character:

```json
{"LISTEN_ADDR": ":8080", "WORKERS": 4, "DEBUG": false}
```

```json
[
  {"key": "LISTEN_ADDR", "value": ":8080"},
  {"key": "DATABASE_URL", "value": "postgres://db.internal/app"}
]
```

```sh
LISTEN_ADDR=:8080
export DATABASE_URL="postgres://db.internal/app"
GREETING='literal $text # not a comment'
```

```sh
aws ec2 run-instances --image-id ami-... --user-data file://env.json ...
```

- **JSON object** (`{`): each member is a variable.
- **JSON array** (`[`): elements are `{"key": ..., "value": ...}`. Field
  names match case-insensitively, so AWS-style `Key`/`Value` works too.
  Any other field is an error.
- **JSON values:** strings, numbers or booleans. Numbers and booleans
  become their JSON text (`4`, `false`). `null`, objects and arrays are
  errors.
- **Dotenv** (anything else): blank lines and `#` comments; `KEY=VALUE` with an optional
  `export `; keys are `[A-Za-z_][A-Za-z0-9_]*`. Unquoted values end at
  ` #`. Single quotes are literal. Double quotes take `\n \r \t \" \\ \$`.
  There is no variable expansion, and values can't span lines.
- **All or nothing:** if anything doesn't parse, as with a shell script, a
  `#cloud-config` document or invalid JSON, init uses none of it for the
  environment and says so on the console. The raw bytes are still written to
  `/var/run/appliance/user-data`.
- **Precedence:** user-data overrides the image's `env`, which overrides
  what init discovers about the instance. User-data can't set `PATH`,
  `HOME`, `TMPDIR`, `APPLIANCE_*` or `OTEL_*`; init ignores those and logs
  their names.
- **Secrets:** the console log shows variable names, never values. Anyone
  who can read the instance's user-data (on EC2: `ec2:DescribeInstanceAttribute`,
  or any process on the instance) can read the values.
- **The collector:** these variables go to the program only. With
  `otel = {"user_data_env": "true"}`, the embedded collector gets them too,
  for a custom `otel_config` to reference; see
  [Telemetry](#telemetry-the-embedded-opentelemetry-collector).
- **Opting out:** `user_data_env = False` on `netbsd_go_appliance` /
  `netbsd_rootfs` makes init ignore user-data for the environment, for the
  program and the collector alike.
- **Testing:** `user_data = "file"` on `netbsd_go_appliance` passes the file
  as user-data to the `.qemu` target, the boot tests and the EC2 smoke test.
  It is not put into the image.
  - `//e2e:hello_image` (dotenv) and `//e2e:hello_arm64_image` (JSON pairs)
    use it to change the greeting they serve. `hello_image`'s EC2 smoke test
    checks this through IMDS.
  - `//e2e:hello_custom_otel_image` uses it to give the collector a variable
    its `otel_config` needs.

## Time synchronization

Init sets the clock before it starts the program, so TLS certificate checks
work from the first request. Afterwards it polls every 64–1024 s. The first
sync, and any later offset over 0.5 s, steps the clock; smaller offsets are
slewed with adjtime(2), so time never jumps backwards under a running
program.

| Platform | Time source |
|---|---|
| EC2 | Amazon Time Sync Service, `169.254.169.123` |
| GCE | metadata server, `169.254.169.254` |
| Azure | `hvtimesync(4)` in the kernel follows the Hyper-V host; init does nothing |
| QEMU | the `opt/beads/time-servers` fw_cfg entry (`vmrun` serves NTP on the host and passes it) |
| any | NTP servers offered by DHCP (option 42), added to the above |

- **Overrides** (`netbsd_go_appliance` / `netbsd_rootfs` arguments):
  - `time_servers = ["time.example", "10.0.0.1:123"]` replaces the automatic
    choice;
  - `timesync = "off"` disables synchronization.
- **Status:** each attempt is written to `/var/run/appliance/timesync` and
  logged on the console (`nbinit: time: stepped +2m3s from 169.254.169.123 ...`).
- **Boot tests:** each boot test starts the guest with its RTC in 2001 and
  requires init to step the clock before it reports ready.

## Telemetry: the embedded OpenTelemetry Collector

The OpenTelemetry Collector is compiled into the init executable
(`//guest/internal/otelagent`, release 0.165.0). Init runs it as a supervised
child process, `/sbin/init otelcol`, rather than in PID 1, so a panic in a
Collector component cannot take down init and with it the kernel.

- **Logs, from program output:** the program's stdout and stderr are pipes.
  Every line still goes to the serial console, and also becomes a log record
  tagged `log.iostream=stdout|stderr`. Init's own messages are shipped too,
  under the scope `nbinit`.
- **Traces and metrics, from the program's own OpenTelemetry SDK:** init gives
  the program the standard variables, so an instrumented program needs no
  configuration:
  - `OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318`;
  - `OTEL_EXPORTER_OTLP_PROTOCOL`;
  - `OTEL_SERVICE_NAME`;
  - `OTEL_RESOURCE_ATTRIBUTES`: the instance ID, cloud provider, platform,
    region and zone.

  The Collector receives OTLP over HTTP (`:4318`) and gRPC (`:4317`) on
  loopback. `e2e/hello` shows the pattern: one span per request and a
  request counter.
- **Host metrics:** `otel = {"host_metrics": "true"}` adds the `host_metrics`
  receiver (every 60 s). On NetBSD that covers the scrapers that produce
  data: `cpu`, `memory`, `filesystem`, `paging`.
  - gopsutil doesn't implement `disk` or `load` on NetBSD.
  - `network` needs netstat(1), which the image lacks.
- **Shutdown:** init stops the program (whose SDK flushes on SIGTERM), waits
  for its output, flushes, then stops the Collector, which flushes its
  exporters. All of this happens before power-off.

**Built-in components:**
- receivers: `otlp`, `host_metrics`;
- processors: `memory_limiter`, `batch`, `resource`;
- exporters: `otlp`, `otlp_http`, `debug`, `awscloudwatchlogs`, `awsemf`,
  `awsxray`, `googlecloud`, `azuremonitor`.

`resourcedetection` is left out: its detectors add about 65 MiB, and init
already supplies the same attributes. With the Collector, init is about
40 MiB.

**Destinations** (`otel = {...}` on `netbsd_go_appliance` / `netbsd_rootfs`;
`exporter` defaults to `auto`):

| `exporter` | Logs | Metrics | Traces | `auto` picks it when |
|---|---|---|---|---|
| `otlp` | `otlp_http` | `otlp_http` | `otlp_http` | `otlp_endpoint` is set (with `otlp_header.<name>`), or given through fw_cfg on QEMU |
| `aws` | CloudWatch Logs `aws_log_group` (default `/beads/<service>`), stream `aws_log_stream` (default the instance ID) | CloudWatch metrics via EMF, namespace `aws_metrics_namespace` (default `beads/<service>`), events in `/beads/<service>/metrics` | X-Ray | on EC2 with an instance profile |
| `gcp` | Cloud Logging (`gcp_log_name`) | Cloud Monitoring | Cloud Trace | on GCE |
| `azure` | Azure Monitor | Azure Monitor | Azure Monitor | on Azure with `azure_connection_string` |
| `none` | console only; no Collector process | | | nothing else applies |

- **Custom config:** `otel_config = "collector.yaml"` installs a complete
  Collector configuration instead. It can reference
  `${env:BEADS_INSTANCE_ID}`, `${env:BEADS_REGION}`, `${env:BEADS_ZONE}`,
  `${env:BEADS_PLATFORM}`, `${env:BEADS_HOSTNAME}`,
  `${env:BEADS_SERVICE_NAME}` and `${env:BEADS_OTLP_ENDPOINT}` (the
  `otlp_endpoint` setting, or the fw_cfg one on QEMU). Variables without a
  value are left unset, so `${env:NAME:-default}` falls back.
- **User-data in a custom config:** with `otel = {"user_data_env": "true"}`,
  the collector also gets the variables user-data gives the program (see
  [Environment from user-data](#environment-from-user-data)). A deployment
  can then rename log groups without rebuilding the image:

  ```yaml
  log_group_name: ${env:APP_LOG_GROUP:-/app/logs}
  ```

  - The base (`HOME`, `PATH`, `TMPDIR`) and `BEADS_*` can't be overridden
    this way. Variables that user-data doesn't define stay unset.
  - It is opt-in because the collector also reads ordinary variables such as
    `HTTPS_PROXY`, `AWS_*` and `GODEBUG`. A value meant for the program
    would otherwise change how telemetry is sent.
  - `//e2e:hello_custom_otel_image` boots with a config that is valid only
    when a user-data variable reaches the collector.
- **Credentials:** the AWS and Google exporters take credentials from the
  instance (IMDS role, metadata-server service account). On AWS the role
  needs:
  - `logs:CreateLogGroup`, `logs:CreateLogStream`, `logs:PutLogEvents`,
    `logs:DescribeLogStreams`;
  - `xray:PutTraceSegments`, `xray:PutTelemetryRecords`,
    `xray:GetSamplingRules`, `xray:GetSamplingTargets`.
- **Secrets:** an Azure connection string set in BUILD files ends up in the
  image. Prefer an `otel_config` that takes such values from user-data
  (`${env:NAME}` with `user_data_env`).
- **QEMU boot tests:** `vmrun` runs an OTLP receiver on the host.
  `logs_expect`, `spans_expect` and `metrics_expect` list what must reach it
  through the guest's Collector.
- **EC2 smoke test:** when those lists are set, the smoke test also:
  1. creates a temporary IAM role and instance profile with the permissions
     above, and launches the instance with it;
  2. after the instance terminates (when init flushes everything), requires
     the log lines in CloudWatch Logs, the metric names in the EMF group,
     and a trace for the service in X-Ray;
  3. deletes the log streams, empty log groups, and the role and profile.
     X-Ray traces can't be deleted; they expire after 30 days.

## CA certificates

Go on NetBSD reads trusted roots from `/etc/openssl/certs/ca-certificates.crt`.
By default that file holds the copy of Mozilla's roots shipped with the
NetBSD release.

To use the bundle curl extracts from Mozilla's store
(https://curl.se/docs/caextract.html) instead:

```starlark
use_repo(netbsd, "curl_ca_bundle")

netbsd_go_appliance(
    ...
    ca_bundle = "@curl_ca_bundle//:cacert.pem",
)
```

- **Pinning:** `curl_ca_bundle` is a dated, immutable `cacert-YYYY-MM-DD.pem`,
  pinned by SHA-256 in `netbsd/versions.bzl`.
  - `go run ./tools/pin ...` moves the pin to curl's newest bundle.
  - A root module can pin its own with
    `netbsd.curl_ca_bundle(date = "...", sha256 = "...")`.
- **Your own roots:** `ca_bundle` accepts any PEM file label.
- **None at all:** `ca_certs = False` installs no bundle.

## QEMU instance metadata

On QEMU, init reads `opt/beads/*` entries from the fw_cfg device, the way it
reads IMDS on EC2:
- `opt/beads/user-data` becomes `/var/run/appliance/user-data` (and, as
  JSON or `KEY=VALUE` lines, the program's environment);
- `opt/beads/time-servers` lists NTP servers;
- `opt/beads/otlp-endpoint` is an OTLP/HTTP endpoint for logs.

```sh
qemu-system-x86_64 ... -fw_cfg name=opt/beads/user-data,file=user-data.txt
```

## The CLOUD kernel

`kernel/configs/CLOUD.amd64` is one kernel for every supported platform:
- **Platform:** ACPI, PCIe, a 16550 serial console.
- **QEMU and GCE:** virtio (blk, net, scsi, rng, balloon).
- **EC2 Nitro and GCE:** NVMe.
- **EC2:** ENA.
- **Azure:** Hyper-V VMBus (hvn, hvs, heartbeat, shutdown, timesync).
- **Local testing:** AHCI.

Graphics, keyboards, USB, audio and the module loader are left out. Binary
compatibility is kept only at `COMPAT_100`, because Go's runtime still calls
NetBSD 10 system calls such as `__kevent50`. The result is about 9 MB,
against 30 MB for GENERIC.

`kernel/configs/CLOUD.aarch64` is the arm64 counterpart, for QEMU `virt` and
Graviton:
- **Platform:** ACPI and FDT, PCIe, GICv2/v3, the generic timer, PL011 and
  16550 consoles, and ACPI GED for the power button.
- **Devices:** virtio, NVMe and ENA.

`netbsd_kernel` builds any config with the release's cross toolchain:

```starlark
load("@beads//kernel:defs.bzl", "netbsd_kernel")

netbsd_kernel(
    name = "my_kernel",
    config = "MYKERNEL.amd64",   # may `include "arch/amd64/conf/std.amd64"` etc.
    release = "@netbsd_11_1_amd64//:release",
)
```

- **Reproducible:** builds use `MKREPRO` with source and object paths mapped,
  so the same sources and config give a byte-identical kernel. That makes the
  result safe to share through `--disk_cache` or a remote cache.
- **Not sandboxed:** the action reads the source tree in place and is never
  executed remotely. Its cache key covers the config and a stamp of the source
  archives' checksums.

## Image layout

```
LBA 0        protective MBR + gptmbr.bin        (BIOS; amd64 only)
1 MiB        ESP, FAT32: EFI/BOOT/BOOTX64.EFI or BOOTAA64.EFI, boot.cfg   (UEFI)
41 MiB       NetBSD FFSv2 root, attrs bootme (+ biosboot and bootxx_ffsv2 on amd64)
               /netbsd  /boot (amd64)  /boot.cfg  /sbin/init  /app/<program>
               /etc/appliance.json  /etc/openssl/certs/ca-certificates.crt
               /dev/* (static nodes)  /tmp /var (tmpfs mount points)
end          backup GPT
```

- **Root file system**: mounted read-only. Writable scratch space is the tmpfs
  on `/tmp` and `/var`.
- **`/etc/resolv.conf`**: a symlink into `/var/run`.
- **`/etc/appliance.json`**: written by the rules and read by init. It holds
  `exec`, `args`, `env`, `user_data_env`, `network`, `hostname`, `timesync`,
  `time_servers`, `otel`, `on_exit` (`restart` / `poweroff` / `reboot` /
  `halt`), `restart_delay_sec` and `stop_timeout_sec`.
- **Shutdown**: the program gets SIGTERM, then SIGKILL after
  `stop_timeout_sec`. Init then syncs and powers off, reboots or halts.
  Triggers:
  - sending SIGTERM, SIGINT or SIGUSR1 to init;
  - a power switch press read from `/dev/power`: the ACPI power button,
    which clouds use for "stop instance", the ACPI GED button on arm64, or a
    Hyper-V shutdown request. Holding `/dev/power` open keeps the kernel
    from powering off on its own.

## Repository layout

```
netbsd/      module extension, repository rules, pinned checksums (versions.bzl).
             tools_repository.bzl has no loads, so re-pinning does not rebuild the tools
image/       netbsd_rootfs, netbsd_disk_image, netbsd_qemu(_test), netbsd_go_appliance
platforms/   netbsd_amd64 and netbsd_aarch64 platforms
guest/init/  the PID 1 shim; guest/internal/{dhcp,nbsys} are its DHCP client and syscalls
cmd/imgtool/ build-time helper: mtree + makefs + installboot, GPT/FAT32 assembly
cmd/vmrun/   QEMU launcher used by .qemu targets and boot tests
internal/    gpt, fat32, rootfs, disk: the libraries behind imgtool
tools/pin/   regenerates netbsd/versions.bzl: `go run ./tools/pin 11.1/amd64 11.1/aarch64 >| netbsd/versions.bzl`
e2e/         example appliances, their boot tests, and checks on image contents
tests/consumer/  a workspace that uses beads as a non-root module (see below)
```

## Testing as a dependency

`tests/consumer` depends on beads through `local_path_override`, so beads'
repositories get the canonical names a real consumer sees
(`beads++netbsd+...`). Root-module builds never see those names, and they
once broke `build.sh`. Run it in CI next to `bazel test //...`:

```sh
tests/consumer/test.sh
```

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
