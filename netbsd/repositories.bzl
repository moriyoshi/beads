# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Repository rule that fetches a NetBSD release's kernel and boot loaders."""

# Per-architecture layout of a release: its directory on the mirror, the
# kernel set, and the boot files the image rules use. x86 images boot from
# both BIOS and UEFI; arm64 images from UEFI only.
PORTS = {
    "aarch64": struct(
        dir = "evbarm-aarch64",
        kernel_set = "kern-GENERIC64.tar.xz",
        efiboot = "bootaa64.efi",
        efi_name = "BOOTAA64.EFI",
        bios = False,
    ),
    "amd64": struct(
        dir = "amd64",
        kernel_set = "kern-GENERIC.tar.xz",
        efiboot = "bootx64.efi",
        efi_name = "BOOTX64.EFI",
        bios = True,
    ),
}

# What the image rules take from the base set. Everything else is deleted
# after extraction to keep the repository small.
_BASE_KEEP = {
    "usr": {
        "mdec": None,
        "share": {"certs": None},
    },
}

def _prune(rctx, root, keep):
    # Starlark has no recursion; walk with an explicit work list.
    work = [(root, keep)]
    for _ in range(64):
        if not work:
            return
        path, keep = work.pop()
        for child in path.readdir():
            if child.basename not in keep:
                rctx.delete(child)
            elif keep[child.basename] != None:
                work.append((child, keep[child.basename]))
    fail("_prune: keep tree too deep")

_RELEASE_BUILD = """\
load("{release_bzl}", "netbsd_release")

netbsd_release(
    name = "release",
    arch = "{arch}",
{bios}    ca_certs = glob(["base/usr/share/certs/mozilla/server/*.pem"]),
    efi_name = "{efi_name}",
    efiboot = "base/usr/mdec/{efiboot}",
    installboot = "{tools}//:nbinstallboot",
    kernel = "kern/netbsd",
    makefs = "{tools}//:nbmakefs",
    nbconfig = "{tools}//:nbconfig",
    nbmake = "{tools}//:nbmake",
    src_stamp = "{tools}//:src.stamp",
    version = "{version}",
    visibility = ["//visibility:public"],
)

exports_files(glob(["kern/**", "base/**"]))
"""

_BIOS_ATTRS = """\
    boot = "base/usr/mdec/boot",
    bootxx = "base/usr/mdec/bootxx_ffsv2",
    gptmbr = "base/usr/mdec/gptmbr.bin",
"""

def _netbsd_release_impl(rctx):
    version, arch = rctx.attr.version, rctx.attr.arch
    port = PORTS[arch]
    url = rctx.attr.mirror + "/NetBSD-%s/%s/binary/sets/" % (version, port.dir)
    rctx.report_progress("Fetching NetBSD %s/%s kernel" % (version, arch))
    rctx.download_and_extract(url = url + port.kernel_set, integrity = rctx.attr.sets[port.kernel_set], output = "kern")
    rctx.report_progress("Fetching NetBSD %s/%s base set" % (version, arch))
    rctx.download_and_extract(url = url + "base.tar.xz", integrity = rctx.attr.sets["base.tar.xz"], output = "base")
    _prune(rctx, rctx.path("base"), _BASE_KEEP)
    rctx.file("BUILD.bazel", _RELEASE_BUILD.format(
        arch = arch,
        bios = _BIOS_ATTRS if port.bios else "",
        efi_name = port.efi_name,
        efiboot = port.efiboot,
        release_bzl = str(Label("//netbsd:release.bzl")),
        tools = "@" + rctx.attr.tools_repo,
        version = version,
    ))

netbsd_release_repository = repository_rule(
    implementation = _netbsd_release_impl,
    attrs = {
        "arch": attr.string(mandatory = True, values = PORTS.keys()),
        "mirror": attr.string(mandatory = True),
        "sets": attr.string_dict(mandatory = True, doc = "Binary set file name -> integrity."),
        "tools_repo": attr.string(mandatory = True, doc = "Apparent name of the matching netbsd_tools_repository."),
        "version": attr.string(mandatory = True),
    },
    doc = "Fetches the GENERIC kernel and boot loaders of a NetBSD release.",
)
