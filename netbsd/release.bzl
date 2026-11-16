# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""The netbsd_release rule: everything the image rules take from one NetBSD release."""

NetBSDReleaseInfo = provider(
    doc = "Files and tools of one NetBSD release for one architecture.",
    fields = {
        "arch": "NetBSD port name, e.g. amd64",
        "boot": "File: x86 secondary boot loader (/boot), or None",
        "bootxx": "File: x86 FFSv2 primary bootstrap, or None",
        "ca_certs": "depset of File: CA certificates in PEM form",
        "efi_name": "file name of the UEFI loader under EFI/BOOT on the ESP",
        "efiboot": "File: UEFI boot loader (bootx64.efi, bootaa64.efi)",
        "gptmbr": "File: GPT protective-MBR boot code, or None for UEFI-only images",
        "installboot": "FilesToRunProvider: installboot(8) built for the host",
        "kernel": "File: kernel",
        "makefs": "FilesToRunProvider: makefs(8) built for the host",
        "nbconfig": "FilesToRunProvider: config(1) built for the host",
        "nbmake": "FilesToRunProvider: the cross make wrapper (nbmake-<arch>)",
        "src_stamp": "File: digest of the kernel sources next to the tools",
        "version": "release version, e.g. 11.1",
    },
)

def _netbsd_release_impl(ctx):
    return [NetBSDReleaseInfo(
        arch = ctx.attr.arch,
        boot = ctx.file.boot,
        bootxx = ctx.file.bootxx,
        ca_certs = depset(ctx.files.ca_certs),
        efi_name = ctx.attr.efi_name,
        efiboot = ctx.file.efiboot,
        gptmbr = ctx.file.gptmbr,
        installboot = ctx.attr.installboot[DefaultInfo].files_to_run,
        kernel = ctx.file.kernel,
        makefs = ctx.attr.makefs[DefaultInfo].files_to_run,
        nbconfig = ctx.attr.nbconfig[DefaultInfo].files_to_run,
        nbmake = ctx.attr.nbmake[DefaultInfo].files_to_run,
        src_stamp = ctx.file.src_stamp,
        version = ctx.attr.version,
    )]

netbsd_release = rule(
    implementation = _netbsd_release_impl,
    attrs = {
        "arch": attr.string(mandatory = True),
        "boot": attr.label(allow_single_file = True),
        "bootxx": attr.label(allow_single_file = True),
        "ca_certs": attr.label_list(allow_files = [".pem"]),
        "efi_name": attr.string(mandatory = True),
        "efiboot": attr.label(allow_single_file = True, mandatory = True),
        "gptmbr": attr.label(allow_single_file = True),
        "installboot": attr.label(allow_single_file = True, executable = True, cfg = "exec", mandatory = True),
        "kernel": attr.label(allow_single_file = True, mandatory = True),
        "makefs": attr.label(allow_single_file = True, executable = True, cfg = "exec", mandatory = True),
        "nbconfig": attr.label(allow_single_file = True, executable = True, cfg = "exec", mandatory = True),
        "nbmake": attr.label(allow_single_file = True, executable = True, cfg = "exec", mandatory = True),
        "src_stamp": attr.label(allow_single_file = True, mandatory = True),
        "version": attr.string(mandatory = True),
    },
    provides = [NetBSDReleaseInfo],
    doc = "Bundles a NetBSD release's kernel, boot loaders and host tools. Created by the netbsd module extension.",
)
