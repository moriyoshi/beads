#!/bin/sh
# Copyright 2026 Moriyoshi Koizumi
# SPDX-License-Identifier: Apache-2.0
#
# Builds and boot-tests an appliance with beads as a non-root module.
# Run from anywhere; meant for CI. The first run builds the NetBSD tools for
# this workspace's own output base (about 10 minutes).
set -eu
cd "$(dirname "$0")"
exec bazel test //:app_image_boot_test "$@"
