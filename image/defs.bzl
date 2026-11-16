# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Rules that turn a Go program into a bootable NetBSD appliance image.

    netbsd_go_appliance(
        name = "server_image",
        binary = ":server",
        release = "@netbsd_11_1_amd64//:release",
        args = ["-listen=:8080"],
    )

defines

    :server_image.kernel   CLOUD kernel (unless kernel = "GENERIC" or a label)
    :server_image.rootfs   FFS root file system
    :server_image.raw      hybrid BIOS/UEFI raw disk image
    :server_image.qemu     `bazel run` target that boots it under QEMU
    :server_image_boot_test  QEMU boot tests (BIOS/UEFI, virtio/NVMe), when boot_test is set
    :server_image.publish_ec2      `bazel run` target that publishes an AMI
    :server_image_ec2_smoke_test   manual test that boots the AMI on EC2
"""

load("//kernel:defs.bzl", "netbsd_kernel")
load("//netbsd:release.bzl", "NetBSDReleaseInfo")
load(":transitions.bzl", "netbsd_transition")

# Default boot loader and kernel console per architecture. On arm64 the
# console comes from ACPI SPCR or the device tree, so none is forced.
_DEFAULT_CONSOLE = {"aarch64": "", "amd64": "com0"}

# QEMU's name for each architecture.
_QEMU_ARCH = {"aarch64": "aarch64", "amd64": "x86_64"}

def _console(ctx):
    return _DEFAULT_CONSOLE[ctx.attr.arch] if ctx.attr.console == "auto" else ctx.attr.console

_OTEL_KEYS = [
    "aws_log_group",
    "aws_log_stream",
    "aws_metrics_namespace",
    "azure_connection_string",
    "exporter",
    "gcp_log_name",
    "host_metrics",
    "otlp_endpoint",
    "service_name",
    "user_data_env",
]

def _otel_config(ctx):
    """Turns the otel string_dict into init's "otel" JSON section."""
    otel = {}
    headers = {}
    for k, v in ctx.attr.otel.items():
        if k.startswith("otlp_header."):
            headers[k[len("otlp_header."):]] = v
        elif k in ("host_metrics", "user_data_env"):
            if v not in ("true", "false"):
                fail("otel: %s must be \"true\" or \"false\"" % k)
            otel[k] = v == "true"
        elif k in _OTEL_KEYS:
            otel[k] = v
        else:
            fail("otel: unknown key %r; expected one of %s or otlp_header.<name>" % (k, _OTEL_KEYS))
    if headers:
        otel["otlp_headers"] = headers
    if ctx.file.otel_config:
        otel["config_file"] = "/etc/otelcol/config.yaml"
    return otel

def _single_executable(target):
    exe = target[DefaultInfo].files_to_run.executable
    if exe == None:
        fail("%s is not executable" % target.label)
    return exe

def _netbsd_rootfs_impl(ctx):
    rel = ctx.attr.release[NetBSDReleaseInfo]
    if rel.arch != ctx.attr.arch:
        fail("release is for %s but arch is %s" % (rel.arch, ctx.attr.arch))

    app = _single_executable(ctx.attr.binary[0])
    init = _single_executable(ctx.attr.init[0])
    exec_name = ctx.attr.exec_name or ctx.attr.binary[0].label.name
    exec_path = "/app/" + exec_name

    config = {"exec": exec_path}
    if ctx.attr.args:
        config["args"] = ctx.attr.args
    if ctx.attr.env:
        config["env"] = ctx.attr.env
    if not ctx.attr.user_data_env:
        config["user_data_env"] = False
    if ctx.attr.network != "dhcp":
        config["network"] = ctx.attr.network
    if ctx.attr.hostname:
        config["hostname"] = ctx.attr.hostname
    if ctx.attr.on_exit != "restart":
        config["on_exit"] = ctx.attr.on_exit
    if ctx.attr.timesync != "auto":
        config["timesync"] = ctx.attr.timesync
    if ctx.attr.time_servers:
        config["time_servers"] = ctx.attr.time_servers
    otel = _otel_config(ctx)
    if otel:
        config["otel"] = otel

    kernel = ctx.file.kernel or rel.kernel
    entries = [
        {"path": "/netbsd", "type": "file", "src": kernel.path, "mode": 0o444},
        {"path": "/sbin/init", "type": "file", "src": init.path, "mode": 0o555},
        {"path": exec_path, "type": "file", "src": app.path, "mode": 0o555},
        {"path": "/etc/appliance.json", "type": "file", "content": json.encode_indent(config) + "\n", "mode": 0o444},
    ]
    inputs = [kernel, init, app]
    if rel.boot:
        # x86 BIOS boot: bootxx_ffsv2 in the partition loads /boot.
        entries.append({"path": "/boot", "type": "file", "src": rel.boot.path, "mode": 0o444})
        inputs.extend([rel.boot, rel.bootxx])

    certs = []
    if ctx.attr.ca_certs:
        certs = [ctx.file.ca_bundle] if ctx.file.ca_bundle else rel.ca_certs.to_list()
    if certs:
        entries.append({
            "path": "/etc/openssl/certs/ca-certificates.crt",
            "type": "file",
            "srcs": sorted([f.path for f in certs]),
            "mode": 0o444,
        })
        inputs.extend(certs)

    if ctx.file.otel_config:
        entries.append({"path": "/etc/otelcol/config.yaml", "type": "file", "src": ctx.file.otel_config.path, "mode": 0o400})
        inputs.append(ctx.file.otel_config)

    for target, dest in ctx.attr.files.items():
        files = target.files.to_list()
        if len(files) != 1:
            fail("files: %s must produce exactly one file, got %d" % (target.label, len(files)))
        entries.append({"path": dest, "type": "file", "src": files[0].path, "mode": 0o444})
        inputs.append(files[0])

    manifest = ctx.actions.declare_file(ctx.label.name + ".manifest.json")
    ctx.actions.write(manifest, json.encode_indent({"arch": ctx.attr.arch, "entries": entries}))

    out = ctx.actions.declare_file(ctx.label.name + ".ffs")
    spec = ctx.actions.declare_file(ctx.label.name + ".mtree")
    args = ctx.actions.args()
    args.add("rootfs")
    args.add("--manifest", manifest)
    args.add("--makefs", rel.makefs.executable)
    if rel.bootxx:
        args.add("--installboot", rel.installboot.executable)
        args.add("--bootxx", rel.bootxx)
    args.add("--console", _console(ctx))
    args.add("--size", str(ctx.attr.size))
    args.add("--out", out)
    args.add("--spec-out", spec)
    ctx.actions.run(
        executable = ctx.executable._imgtool,
        arguments = [args],
        inputs = inputs + [manifest],
        tools = [rel.makefs, rel.installboot],
        outputs = [out, spec],
        mnemonic = "NetBSDRootfs",
        progress_message = "Building NetBSD root file system %{label}",
    )
    return [
        DefaultInfo(files = depset([out])),
        OutputGroupInfo(spec = depset([spec]), manifest = depset([manifest])),
    ]

netbsd_rootfs = rule(
    implementation = _netbsd_rootfs_impl,
    attrs = {
        "args": attr.string_list(doc = "Arguments passed to the program."),
        "arch": attr.string(default = "amd64", values = ["amd64", "aarch64"]),
        "binary": attr.label(
            mandatory = True,
            executable = True,
            cfg = netbsd_transition,
            doc = "The Go program to run. Built for NetBSD in pure mode.",
        ),
        "ca_bundle": attr.label(
            allow_single_file = True,
            doc = "PEM bundle to install at /etc/openssl/certs/ca-certificates.crt instead of the release's Mozilla certificates, e.g. \"@curl_ca_bundle//:cacert.pem\".",
        ),
        "ca_certs": attr.bool(default = True, doc = "Install a CA bundle at /etc/openssl/certs/ca-certificates.crt: ca_bundle if set, else the release's copy of Mozilla's roots."),
        "console": attr.string(default = "auto", doc = "Boot loader and kernel console device; \"auto\" is com0 on amd64 and the firmware's choice on aarch64."),
        "env": attr.string_dict(doc = "Environment variables for the program."),
        "exec_name": attr.string(doc = "File name of the program under /app. Defaults to the binary's target name."),
        "files": attr.label_keyed_string_dict(
            allow_files = True,
            cfg = netbsd_transition,
            doc = "Extra files to install, mapped to absolute destination paths.",
        ),
        "hostname": attr.string(doc = "Hostname; by default the one offered by DHCP, if any."),
        "kernel": attr.label(
            allow_single_file = True,
            doc = "Kernel to install; defaults to the release's GENERIC kernel.",
        ),
        "otel": attr.string_dict(doc = """Telemetry (logs, metrics, traces) through the OpenTelemetry Collector
embedded in init. Keys: exporter (auto, none, otlp, aws, gcp, azure),
service_name, host_metrics ("true"/"false"), otlp_endpoint,
otlp_header.<name>, aws_log_group, aws_log_stream, aws_metrics_namespace,
gcp_log_name, azure_connection_string, user_data_env ("true"/"false": pass
the program's user-data variables to the collector too, for otel_config to
reference as ${env:NAME}). With exporter auto (the default)
init picks OTLP if an endpoint is known, else the platform's own services
when the instance has credentials for them."""),
        "otel_config": attr.label(
            allow_single_file = [".yaml", ".yml"],
            doc = "A complete collector configuration to use instead of the generated one. It may reference ${env:BEADS_INSTANCE_ID}, ${env:BEADS_REGION}, ${env:BEADS_ZONE}, ${env:BEADS_PLATFORM}, ${env:BEADS_HOSTNAME}, ${env:BEADS_SERVICE_NAME}, ${env:BEADS_OTLP_ENDPOINT}, and, with otel user_data_env, variables from user-data. Logs from init arrive over OTLP/HTTP on 127.0.0.1:4318.",
        ),
        "user_data_env": attr.bool(
            default = True,
            doc = "Let instance user-data written as a JSON object, a JSON array of {\"key\", \"value\"} pairs, or KEY=VALUE lines set program environment variables, overriding env. PATH, HOME, TMPDIR, APPLIANCE_* and OTEL_* cannot be set this way.",
        ),
        "time_servers": attr.string_list(doc = "NTP servers (host or host:port) init keeps the clock in step with, instead of the platform's time service and DHCP-provided servers."),
        "timesync": attr.string(default = "auto", values = ["auto", "off"], doc = "Whether init synchronizes the clock."),
        "init": attr.label(
            default = "//guest/init",
            executable = True,
            cfg = netbsd_transition,
        ),
        "network": attr.string(default = "dhcp", values = ["dhcp", "none"]),
        "on_exit": attr.string(
            default = "restart",
            values = ["restart", "poweroff", "reboot", "halt"],
            doc = "What init does when the program exits.",
        ),
        "release": attr.label(mandatory = True, providers = [NetBSDReleaseInfo]),
        "size": attr.int(default = 0, doc = "Fixed root file system size in bytes; 0 sizes it to the contents plus 10%."),
        "_imgtool": attr.label(default = "//cmd/imgtool", executable = True, cfg = "exec"),
    },
    doc = "Builds a read-only FFSv2 root file system: kernel, boot loader, Go init shim and the program.",
)

def _netbsd_disk_image_impl(ctx):
    rel = ctx.attr.release[NetBSDReleaseInfo]
    out = ctx.actions.declare_file(ctx.label.name)
    args = ctx.actions.args()
    args.add("disk")
    args.add("--rootfs", ctx.file.rootfs)
    args.add("--efiboot", rel.efiboot)
    args.add("--efi-name", rel.efi_name)
    inputs = [ctx.file.rootfs, rel.efiboot]
    if rel.gptmbr:
        args.add("--gptmbr", rel.gptmbr)
        inputs.append(rel.gptmbr)
    args.add("--console", _DEFAULT_CONSOLE[rel.arch] if ctx.attr.console == "auto" else ctx.attr.console)
    args.add("--esp-size-mib", str(ctx.attr.esp_size_mib))
    args.add("--min-size", str(ctx.attr.min_size))
    args.add("--out", out)
    ctx.actions.run(
        executable = ctx.executable._imgtool,
        arguments = [args],
        inputs = inputs,
        outputs = [out],
        mnemonic = "NetBSDDiskImage",
        progress_message = "Assembling disk image %{label}",
    )
    return [DefaultInfo(files = depset([out]))]

netbsd_disk_image = rule(
    implementation = _netbsd_disk_image_impl,
    attrs = {
        "console": attr.string(default = "auto"),
        "esp_size_mib": attr.int(default = 40),
        "min_size": attr.int(default = 0, doc = "Pad the disk to at least this many bytes."),
        "release": attr.label(mandatory = True, providers = [NetBSDReleaseInfo]),
        "rootfs": attr.label(mandatory = True, allow_single_file = [".ffs"]),
        "_imgtool": attr.label(default = "//cmd/imgtool", executable = True, cfg = "exec"),
    },
    doc = "Assembles a raw GPT disk: BIOS and UEFI bootable on amd64, UEFI on aarch64.",
)

def _rlocation(ctx, f):
    if f.short_path.startswith("../"):
        return f.short_path[3:]
    return ctx.workspace_name + "/" + f.short_path

def _vm_impl(ctx, test):
    vmrun = ctx.attr._vmrun[DefaultInfo]
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = exe, target_file = vmrun.files_to_run.executable, is_executable = True)
    env = {
        "VMRUN_ARCH": _QEMU_ARCH[ctx.attr.arch],
        "VMRUN_IMAGE": _rlocation(ctx, ctx.file.image),
        "VMRUN_FIRMWARE": ctx.attr.firmware,
        "VMRUN_DISK": ctx.attr.disk,
        "VMRUN_MEMORY": str(ctx.attr.memory),
    }
    data = [ctx.file.image]
    if ctx.file.user_data:
        env["VMRUN_USER_DATA"] = _rlocation(ctx, ctx.file.user_data)
        data.append(ctx.file.user_data)
    if test:
        env["VMRUN_TEST"] = "1"
        if ctx.attr.http_port:
            env["VMRUN_HTTP_PORT"] = str(ctx.attr.http_port)
            env["VMRUN_HTTP_PATH"] = ctx.attr.http_path
            env["VMRUN_HTTP_EXPECT"] = ctx.attr.http_expect
        env["VMRUN_SHUTDOWN_EXPECT"] = "|".join(ctx.attr.shutdown_expect)
        if ctx.attr.timesync_check:
            env["VMRUN_TIMESYNC_CHECK"] = "1"
        if ctx.attr.logs_expect:
            env["VMRUN_LOGS_EXPECT"] = "|".join(ctx.attr.logs_expect)
        if ctx.attr.spans_expect:
            env["VMRUN_SPANS_EXPECT"] = "|".join(ctx.attr.spans_expect)
        if ctx.attr.metrics_expect:
            env["VMRUN_METRICS_EXPECT"] = "|".join(ctx.attr.metrics_expect)
    elif ctx.attr.forward:
        env["VMRUN_FORWARD"] = ",".join(ctx.attr.forward)
    runfiles = ctx.runfiles(files = data).merge(vmrun.default_runfiles)
    return [
        DefaultInfo(executable = exe, runfiles = runfiles),
        RunEnvironmentInfo(environment = env),
    ]

_VM_ATTRS = {
    "arch": attr.string(default = "amd64", values = ["amd64", "aarch64"]),
    "disk": attr.string(default = "virtio", values = ["virtio", "nvme", "ahci"], doc = "Disk interface the image is attached through."),
    "firmware": attr.string(default = "bios", values = ["bios", "uefi"]),
    "image": attr.label(mandatory = True, allow_single_file = True),
    "memory": attr.int(default = 512, doc = "Guest memory in MiB."),
    "user_data": attr.label(allow_single_file = True, doc = "File passed to the guest as instance user-data, through fw_cfg as opt/beads/user-data."),
    "_vmrun": attr.label(default = "//cmd/vmrun", executable = True, cfg = "target"),
}

def _netbsd_qemu_impl(ctx):
    return _vm_impl(ctx, test = False)

def _netbsd_qemu_test_impl(ctx):
    return _vm_impl(ctx, test = True)

netbsd_qemu = rule(
    implementation = _netbsd_qemu_impl,
    attrs = dict(_VM_ATTRS, forward = attr.string_list(doc = "TCP forwards, as \"host_port:guest_port\".")),
    executable = True,
    doc = "`bazel run` target that boots an image under QEMU with the serial console on the terminal.",
)

netbsd_qemu_test = rule(
    implementation = _netbsd_qemu_test_impl,
    attrs = dict(
        _VM_ATTRS,
        http_expect = attr.string(doc = "Substring the HTTP response body must contain."),
        http_path = attr.string(default = "/"),
        http_port = attr.int(doc = "Guest TCP port to probe with HTTP GET once init is ready; 0 skips the probe."),
        shutdown_expect = attr.string_list(
            default = ["nbinit: stopping for poweroff", "nbinit: poweroff"],
            doc = "Console lines that must appear after the ACPI power button press.",
        ),
        metrics_expect = attr.string_list(doc = "Metric names the guest must ship to vmrun's OTLP receiver."),
        spans_expect = attr.string_list(doc = "Span names the guest must ship to vmrun's OTLP receiver."),
        logs_expect = attr.string_list(
            doc = "Log lines the guest must ship, through its embedded OpenTelemetry collector, to the OTLP receiver vmrun provides through fw_cfg.",
        ),
        timesync_check = attr.bool(
            default = True,
            doc = "Boot with the guest clock in 2001 and require init to correct it from an NTP server vmrun provides through fw_cfg.",
        ),
    ),
    test = True,
    doc = "Boots an image under QEMU, waits for init, optionally probes HTTP, then checks for a clean ACPI poweroff.",
)

def _cloud_impl(ctx):
    tool = ctx.attr._cloud[DefaultInfo]
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = exe, target_file = tool.files_to_run.executable, is_executable = True)
    env = {
        "CLOUD_COMMAND": ctx.attr.command,
        "CLOUD_IMAGE": _rlocation(ctx, ctx.file.image),
        "CLOUD_NAME": ctx.attr.image_name,
    }
    if ctx.attr.http_port:
        env["CLOUD_HTTP_PORT"] = str(ctx.attr.http_port)
        env["CLOUD_HTTP_PATH"] = ctx.attr.http_path
        env["CLOUD_HTTP_EXPECT"] = ctx.attr.http_expect
    data = [ctx.file.image]
    if ctx.file.user_data:
        env["CLOUD_USER_DATA"] = _rlocation(ctx, ctx.file.user_data)
        data.append(ctx.file.user_data)
    env.update(ctx.attr.env)
    runfiles = ctx.runfiles(files = data).merge(tool.default_runfiles)
    return [
        DefaultInfo(executable = exe, runfiles = runfiles),
        RunEnvironmentInfo(environment = env),
    ]

_CLOUD_ATTRS = {
    "command": attr.string(mandatory = True, values = ["ec2-publish", "ec2-smoke"]),
    "env": attr.string_dict(doc = "Extra CLOUD_* settings, e.g. CLOUD_AWS_REGION or CLOUD_EC2_INSTANCE_TYPE."),
    "http_expect": attr.string(),
    "http_path": attr.string(default = "/"),
    "http_port": attr.int(),
    "image": attr.label(mandatory = True, allow_single_file = True),
    "image_name": attr.string(doc = "Name prefix for published images."),
    "user_data": attr.label(allow_single_file = True, doc = "File the smoke test launches the instance with as user-data."),
    "_cloud": attr.label(default = "//cmd/cloud", executable = True, cfg = "target"),
}

netbsd_cloud_run = rule(
    implementation = _cloud_impl,
    attrs = _CLOUD_ATTRS,
    executable = True,
    doc = "`bazel run` target that publishes an image to a cloud. Extra flags go after `--`.",
)

netbsd_cloud_test = rule(
    implementation = _cloud_impl,
    attrs = _CLOUD_ATTRS,
    test = True,
    doc = "Publishes an image, boots it in a cloud, checks it, and cleans up. Needs credentials; see README.",
)

def netbsd_go_appliance(
        name,
        binary,
        release,
        arch = "amd64",
        kernel = None,
        args = [],
        env = {},
        files = {},
        hostname = None,
        on_exit = "restart",
        ca_certs = True,
        ca_bundle = None,
        http_port = 0,
        http_path = "/",
        http_expect = "",
        shutdown_expect = None,
        logs_expect = [],
        spans_expect = [],
        metrics_expect = [],
        otel = {},
        otel_config = None,
        user_data = None,
        boot_test = True,
        boot_tests = None,
        clouds = ["ec2"],
        tags = [],
        visibility = None,
        **kwargs):
    """Builds a NetBSD appliance image for a Go program.

    Args:
      name: base name of the generated targets.
      binary: the go_binary to run; it is rebuilt for NetBSD.
      release: a netbsd_release, e.g. "@netbsd_11_1_amd64//:release".
      arch: "amd64" or "aarch64"; must match the release.
      kernel: None builds the CLOUD kernel for arch (virtio, NVMe, ENA,
        and Hyper-V on amd64);
        "GENERIC" uses the release's stock kernel; anything else is a label
        of a kernel file.
      args: program arguments.
      env: program environment.
      files: extra files, label -> absolute path in the image.
      hostname: fixed hostname; defaults to `name`.
      on_exit: init's policy when the program exits.
      ca_certs: install a CA bundle at /etc/openssl/certs/ca-certificates.crt.
      ca_bundle: the PEM bundle to install, e.g.
        "@curl_ca_bundle//:cacert.pem" for curl's extract of Mozilla's store;
        defaults to the copy of Mozilla's roots shipped with the release.
      http_port: guest port the program serves HTTP on. It is forwarded by the
        .qemu target and probed by the boot tests.
      http_path: path the boot tests GET.
      http_expect: substring the boot tests expect in the response.
      clouds: clouds to define publish targets and smoke tests for
        (currently "ec2"). The smoke tests are tagged manual: they create
        real resources and need credentials.
      shutdown_expect: console lines the boot tests require after the ACPI
        power button press; by default init's own shutdown messages. Add
        the program's shutdown log line to check it got SIGTERM.
      logs_expect: log lines the boot tests require the guest to ship over
        OTLP, through its embedded collector, to vmrun's receiver.
      spans_expect: span names the boot tests require over OTLP.
      metrics_expect: metric names the boot tests require over OTLP.
      otel: telemetry settings; see netbsd_rootfs.otel.
      otel_config: your own collector configuration; see
        netbsd_rootfs.otel_config.
      user_data: a file the .qemu target, the boot tests and the cloud smoke
        tests launch the instance with as user-data. It is not part of the
        image. Written as JSON or KEY=VALUE lines, it sets program
        environment variables (see netbsd_rootfs.user_data_env).
      boot_test: whether to define the QEMU boot tests.
      boot_tests: "firmware/disk" combinations to boot-test, with firmware
        bios or uefi and disk virtio, nvme or ahci. Defaults to BIOS and
        UEFI on amd64 and UEFI only on aarch64.
      tags: tags for every generated target.
      visibility: visibility of the image targets.
      **kwargs: passed to netbsd_rootfs (e.g. size, console, network).
    """
    if kernel == None:
        netbsd_kernel(
            name = name + ".kernel",
            config = Label("//kernel:configs/CLOUD." + arch),
            release = release,
            tags = tags,
            visibility = visibility,
        )
        kernel = name + ".kernel"
    elif kernel == "GENERIC":
        kernel = None
    netbsd_rootfs(
        name = name + ".rootfs",
        arch = arch,
        binary = binary,
        kernel = kernel,
        release = release,
        args = args,
        env = env,
        files = files,
        hostname = hostname if hostname != None else name.replace("_", "-"),
        on_exit = on_exit,
        ca_certs = ca_certs,
        ca_bundle = ca_bundle,
        otel = otel,
        otel_config = otel_config,
        tags = tags,
        visibility = visibility,
        **kwargs
    )
    netbsd_disk_image(
        name = name + ".raw",
        rootfs = name + ".rootfs",
        release = release,
        tags = tags,
        visibility = visibility,
    )
    netbsd_qemu(
        name = name + ".qemu",
        arch = arch,
        firmware = "uefi" if arch == "aarch64" else "bios",
        image = name + ".raw",
        forward = ["%d:%d" % (http_port, http_port)] if http_port else [],
        user_data = user_data,
        tags = tags,
        visibility = visibility,
    )
    if "ec2" in clouds:
        netbsd_cloud_run(
            name = name + ".publish_ec2",
            command = "ec2-publish",
            env = {"CLOUD_ARCH": arch},
            image = name + ".raw",
            image_name = name.replace("_", "-"),
            tags = tags,
            visibility = visibility,
        )
        smoke_env = {"CLOUD_ARCH": arch}
        if logs_expect or metrics_expect or spans_expect:
            # Check that the instance's telemetry reaches CloudWatch and
            # X-Ray through init's collector, under a temporary IAM role.
            service = otel.get("service_name", native.package_relative_label(binary).name)
            smoke_env.update({
                "CLOUD_LOGS_EXPECT": "|".join(logs_expect),
                "CLOUD_METRICS_EXPECT": "|".join(metrics_expect),
                "CLOUD_SERVICE": service,
                "CLOUD_TELEMETRY_CHECK": "1",
                "CLOUD_TRACES_CHECK": "1" if spans_expect else "",
            })
        netbsd_cloud_test(
            name = name + "_ec2_smoke_test",
            command = "ec2-smoke",
            env = smoke_env,
            image = name + ".raw",
            image_name = name.replace("_", "-"),
            http_port = http_port,
            http_path = http_path,
            http_expect = http_expect,
            user_data = user_data,
            size = "large",
            # Longer than the tool's own deadlines (publishing plus 10
            # minutes for the instance), so the tool fails and cleans up
            # before Bazel would kill it.
            timeout = "eternal",
            tags = tags + ["manual", "external", "requires-network", "ec2"],
        )
    if boot_tests == None:
        boot_tests = ["uefi/virtio", "uefi/nvme"] if arch == "aarch64" else ["bios/virtio", "uefi/virtio", "uefi/nvme"]
    if boot_test:
        tests = []
        for combo in boot_tests:
            fw, disk = combo.split("/")
            test_name = "%s_boot_test_%s_%s" % (name, fw, disk)
            netbsd_qemu_test(
                name = test_name,
                arch = arch,
                image = name + ".raw",
                firmware = fw,
                disk = disk,
                http_port = http_port,
                http_path = http_path,
                http_expect = http_expect,
                shutdown_expect = shutdown_expect,
                logs_expect = logs_expect,
                spans_expect = spans_expect,
                metrics_expect = metrics_expect,
                user_data = user_data,
                size = "medium",
                tags = tags + ["qemu"],
            )
            tests.append(":" + test_name)
        native.test_suite(name = name + "_boot_test", tests = tests)
