# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Module extension that declares NetBSD release repositories.

    netbsd = use_extension("@beads//netbsd:extensions.bzl", "netbsd")
    netbsd.release(version = "11.1", arch = "amd64")
    netbsd.release(version = "11.1", arch = "aarch64")
    use_repo(netbsd, "netbsd_11_1_amd64", "netbsd_11_1_aarch64")

Each release tag creates `netbsd_<version>_<arch>` (kernel, boot loaders and
the `:release` target the image rules take) and `netbsd_<version>_<arch>_tools`
(makefs, installboot and the cross toolchain, built from source on first use).

The extension also defines `curl_ca_bundle`, Mozilla's CA bundle as
extracted by curl (https://curl.se/docs/caextract.html), pinned in
versions.bzl. Use it with `ca_bundle = "@curl_ca_bundle//:cacert.pem"`. The
root module can pin another dated bundle:

    netbsd.curl_ca_bundle(date = "2026-11-05", sha256 = "...")
"""

load(":ca_repository.bzl", "curl_ca_bundle_repository")
load(":repositories.bzl", "PORTS", "netbsd_release_repository")
load(":tools_repository.bzl", "netbsd_tools_repository")
load(":versions.bzl", "CURL_CA_BUNDLE", "MIRROR", "RELEASES")

_release = tag_class(attrs = {
    "arch": attr.string(default = "amd64", values = PORTS.keys()),
    "version": attr.string(mandatory = True),
})

_curl_ca_bundle = tag_class(attrs = {
    "date": attr.string(mandatory = True, doc = "Date of a cacert-YYYY-MM-DD.pem on https://curl.se/ca/."),
    "sha256": attr.string(mandatory = True, doc = "Its SHA-256, as published in the matching .sha256 file."),
})

def repo_name(version, arch):
    return "netbsd_%s_%s" % (version.replace(".", "_"), arch)

def _netbsd_impl(mctx):
    seen = {}
    for mod in mctx.modules:
        for tag in mod.tags.release:
            name = repo_name(tag.version, tag.arch)
            if name in seen:
                continue
            seen[name] = True
            pinned = RELEASES.get(tag.version)
            if pinned == None or tag.arch not in pinned["sets"]:
                fail("NetBSD %s/%s is not pinned in netbsd/versions.bzl; run //tools/pin" % (tag.version, tag.arch))
            netbsd_tools_repository(
                name = name + "_tools",
                arch = tag.arch,
                mirror = MIRROR,
                sources = pinned["source"],
                version = tag.version,
            )
            netbsd_release_repository(
                name = name,
                arch = tag.arch,
                mirror = MIRROR,
                sets = pinned["sets"][tag.arch],
                tools_repo = name + "_tools",
                version = tag.version,
            )
    ca = CURL_CA_BUNDLE
    for mod in mctx.modules:
        if mod.is_root and mod.tags.curl_ca_bundle:
            tag = mod.tags.curl_ca_bundle[-1]
            ca = {
                "date": tag.date,
                "sha256": tag.sha256,
                "url": "https://curl.se/ca/cacert-%s.pem" % tag.date,
            }
    curl_ca_bundle_repository(name = "curl_ca_bundle", **ca)
    return mctx.extension_metadata(reproducible = True)

netbsd = module_extension(
    implementation = _netbsd_impl,
    tag_classes = {"curl_ca_bundle": _curl_ca_bundle, "release": _release},
)
