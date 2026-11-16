# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Repository rule that builds NetBSD's host tools and cross toolchain.

Kept free of load()s on purpose: Bazel refetches a repository whenever the
.bzl file defining its rule (or anything it loads) changes, and this one takes
about ten minutes to build. Checksums come in as attributes instead.
"""

# build.sh machine flags, MACHINE, and the cross make wrapper name per architecture.
_MACHINES = {
    "aarch64": (["-m", "evbarm", "-a", "aarch64"], "evbarm", "nbmake-evbarm"),
    "amd64": (["-m", "amd64"], "amd64", "nbmake-amd64"),
}

_TOOLS = ["nbmakefs", "nbinstallboot", "nbgpt", "nbmtree", "nbconfig"]

_BUILD = """\
package(default_visibility = ["//visibility:public"])

exports_files(["src.stamp"])

{rules}

# The machine-independent name for the cross make wrapper.
alias(name = "nbmake", actual = "tooldir/bin/{nbmake}")

filegroup(
    name = "tooldir",
    srcs = glob(["tooldir/**"]),
)
"""

def _netbsd_tools_impl(rctx):
    version, arch = rctx.attr.version, rctx.attr.arch
    if arch not in _MACHINES:
        fail("unsupported arch %s" % arch)
    flags, machine, nbmake = _MACHINES[arch]
    for name in sorted(rctx.attr.sources.keys()):
        rctx.report_progress("Fetching NetBSD %s %s" % (version, name))
        rctx.download_and_extract(
            url = rctx.attr.mirror + "/NetBSD-%s/source/sets/%s" % (version, name),
            integrity = rctx.attr.sources[name],
            output = "src",
            stripPrefix = "usr/src",
        )

    rctx.report_progress("Building NetBSD %s host tools for %s (takes a while)" % (version, arch))
    script = """
set -e
jobs=$(getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
cd src
# -M (MAKEOBJDIRPREFIX), not -O: build.sh turns -O into
# MAKEOBJDIR=${{.CURDIR:C,^$TOP,<objdir>,}}, a regex over the source path, and
# Bazel's canonical repository names contain '+' ("beads++netbsd+..." when
# beads is a dependency), which breaks the regex or silently stops it from
# matching.
if ! ./build.sh -U -u {flags} -j "$jobs" -T {tooldir} -M {objdir} tools > ../build.log 2>&1; then
    tail -n 60 ../build.log >&2
    exit 1
fi
# Every tool that includes tools/Makefile.nbincludes runs
# "rm -f machine && install -l s -r $MACHINE machine" in nbinclude, and with
# -j several do so at once. install(1) stat()s its target, so when another job
# recreates machine in between, the link lands inside the directory instead:
# $MACHINE/$MACHINE -> $MACHINE, a loop that makes the BUILD file's glob fail.
nb={tooldir}/include/nbinclude
if [ -L "$nb/{machine}/{machine}" ]; then
    rm "$nb/{machine}/{machine}"
fi
""".format(flags = " ".join(flags), machine = machine, tooldir = rctx.path("tooldir"), objdir = rctx.path("obj"))
    res = rctx.execute(["/bin/sh", "-c", script], timeout = 7200, quiet = True)
    if res.return_code != 0:
        fail("build.sh tools failed (log: %s):\n%s" % (rctx.path("build.log"), res.stderr))

    # The tools are self-contained; keep the kernel sources for
    # netbsd_kernel but drop the object tree.
    rctx.delete("obj")

    # Actions that read src/ directly (netbsd_kernel) take this file as an
    # input, so their cache keys change whenever the sources do.
    rctx.file("src.stamp", "".join([
        "%s %s\n" % (name, rctx.attr.sources[name])
        for name in sorted(rctx.attr.sources.keys())
    ]) + "arch %s\n" % arch)

    rules = ['alias(name = "{t}", actual = "tooldir/bin/{t}")'.format(t = t) for t in _TOOLS]
    rctx.file("BUILD.bazel", _BUILD.format(rules = "\n".join(rules), nbmake = nbmake))

netbsd_tools_repository = repository_rule(
    implementation = _netbsd_tools_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
        "mirror": attr.string(mandatory = True),
        "sources": attr.string_dict(mandatory = True, doc = "Source set file name -> integrity."),
        "version": attr.string(mandatory = True),
    },
    doc = "Builds NetBSD's cross tools (makefs, installboot, the toolchain) with build.sh on the host.",
)
