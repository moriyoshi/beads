# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Repository rule that fetches curl's extract of Mozilla's CA bundle."""

_BUILD = """\
package(default_visibility = ["//visibility:public"])

exports_files(["cacert.pem"])

# The bundle's date, for reference in images and logs.
filegroup(
    name = "date",
    srcs = ["DATE"],
)
"""

def _curl_ca_bundle_impl(rctx):
    rctx.download(url = rctx.attr.url, output = "cacert.pem", sha256 = rctx.attr.sha256)
    rctx.file("DATE", rctx.attr.date + "\n")
    rctx.file("BUILD.bazel", _BUILD)

curl_ca_bundle_repository = repository_rule(
    implementation = _curl_ca_bundle_impl,
    attrs = {
        "date": attr.string(mandatory = True),
        "sha256": attr.string(mandatory = True),
        "url": attr.string(mandatory = True),
    },
    doc = "Downloads a dated cacert-YYYY-MM-DD.pem from https://curl.se/docs/caextract.html.",
)
