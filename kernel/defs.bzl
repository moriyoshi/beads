# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""netbsd_kernel: build a NetBSD kernel from a config file.

    netbsd_kernel(
        name = "kernel",
        config = "@beads//kernel:configs/CLOUD.amd64",
        release = "@netbsd_11_1_amd64//:release",
    )

The build uses the sources and cross toolchain of the release's tools
repository. It runs outside the sandbox because those sources are a large
tree that is not declared file by file. The source stamp and the config are
inputs, so the action key still changes whenever either does. Builds are
reproducible (MKREPRO, with source and object paths mapped), so the result
can be cached locally or remotely; it is never executed remotely.
"""

load("//netbsd:release.bzl", "NetBSDReleaseInfo")

_BUILD_SCRIPT = """\
set -eu
config="$1" ident="$2" stamp="$3" nbconfig="$4" nbmake="$5" out="$6"
execroot="$(pwd)"
# The physical path: the nbmake wrapper exports NETBSDSRCDIR as recorded when
# the tools were built, and the source prefix map only strips that spelling.
src="$(cd "$execroot/$(dirname "$stamp")/src" && pwd -P)"
work="$(mktemp -d "${TMPDIR:-/tmp}/netbsd-kernel.XXXXXX")"
trap 'rm -rf "$work"' EXIT
# Resolve symlinks (macOS: /var -> /private/var) so the path matches what the
# compiler records and NETBSDOBJDIR's prefix map strips it.
work="$(cd "$work" && pwd -P)"
mkdir "$work/conf"
cp "$config" "$work/conf/$ident"
# config(1) names the kernel after the build directory, hence $ident.
(cd "$work" && "$execroot/$nbconfig" -b "$work/$ident" -s "$src/sys" "conf/$ident" > config.log 2>&1) || {
    cat "$work/config.log" >&2
    exit 1
}
jobs="$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"
cd "$work/$ident"
if ! NETBSDSRCDIR="$src" NETBSDOBJDIR="$work/$ident" \\
    "$execroot/$nbmake" -j "$jobs" MKREPRO=yes MKREPRO_TIMESTAMP=0 MKCTF=no > "$work/make.log" 2>&1; then
    grep -E -B5 'error|Error|\\*\\*\\*' "$work/make.log" | tail -n 60 >&2
    exit 1
fi
cp netbsd "$execroot/$out"
"""

def _netbsd_kernel_impl(ctx):
    rel = ctx.attr.release[NetBSDReleaseInfo]
    ident = ctx.attr.ident or ctx.file.config.basename.split(".")[0]
    out = ctx.actions.declare_file(ctx.label.name + "/netbsd")
    ctx.actions.run_shell(
        command = _BUILD_SCRIPT,
        arguments = [
            ctx.file.config.path,
            ident,
            rel.src_stamp.path,
            rel.nbconfig.executable.path,
            rel.nbmake.executable.path,
            out.path,
        ],
        inputs = [ctx.file.config, rel.src_stamp],
        tools = [rel.nbconfig, rel.nbmake],
        outputs = [out],
        mnemonic = "NetBSDKernel",
        progress_message = "Building NetBSD kernel %s for %%{label}" % ident,
        execution_requirements = {
            "no-remote-exec": "1",
            "no-sandbox": "1",
        },
    )
    return [DefaultInfo(files = depset([out]))]

netbsd_kernel = rule(
    implementation = _netbsd_kernel_impl,
    attrs = {
        "config": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "Kernel config file. Includes resolve against sys/ of the release sources.",
        ),
        "ident": attr.string(doc = "Kernel name; defaults to the config file name up to the first dot."),
        "release": attr.label(mandatory = True, providers = [NetBSDReleaseInfo]),
    },
    doc = "Builds a NetBSD kernel with the release's cross toolchain.",
)
