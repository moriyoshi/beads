# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0

"""Configuration transition that builds Go targets for NetBSD."""

_PURE = str(Label("@rules_go//go/config:pure"))
_PLATFORMS = "//command_line_option:platforms"

def _netbsd_transition_impl(_settings, attr):
    return {
        _PLATFORMS: [str(Label("//platforms:netbsd_" + attr.arch))],
        # Pure Go: NetBSD binaries then need no libc, ld.elf_so or C toolchain.
        _PURE: True,
    }

netbsd_transition = transition(
    implementation = _netbsd_transition_impl,
    inputs = [],
    outputs = [_PLATFORMS, _PURE],
)
