#!/usr/bin/env python3
"""Build the current native Linux Desktop + Agent; no installation or VM launch."""
import argparse
import datetime
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from image import MACHINES, REPO, file_record, go_identity, source_inventory, target_os, validate_elf


def native_build_command(package, binary, ldflags):
    # Go deliberately omits -ldflags from embedded build settings with trimpath.
    # The packager must inspect the immutable companion identity, so retain it.
    # Explicit false also overrides an inherited GOFLAGS=-trimpath default.
    return ["go", "build", "-buildvcs=false", "-trimpath=false", "-tags", "gtk3,meadow_vm_lab",
            "-ldflags", ldflags, "-o", str(binary), package]


def source_identity(explicit_commit=None, explicit_dirty=None):
    git = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, text=True, capture_output=True)
    if git.returncode == 0:
        commit = git.stdout.strip()
        if explicit_commit and explicit_commit != commit:
            raise ValueError("provided source commit differs from checkout HEAD")
        dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=REPO, text=True))
        origin = "git"
    else:
        if not explicit_commit or explicit_dirty is None:
            raise ValueError("Git metadata is unavailable; pass --source-commit and --source-dirty/--no-source-dirty explicitly")
        commit, dirty, origin = explicit_commit, explicit_dirty, "explicit_builder_input"
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("source commit must be a full Git SHA")
    sources, digest = source_inventory()
    return {"commit": commit, "dirty": dirty, "commit_origin": origin,
            "source_inventory_sha256": digest, "source_files": sources}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=MACHINES, required=True)
    parser.add_argument("--build-version", required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--source-commit", help="full host Git SHA when .git is outside a container mount")
    parser.add_argument("--source-dirty", action=argparse.BooleanOptionalAction, default=None,
                        help="explicit host dirty status; required with externally supplied Git identity")
    args = parser.parse_args()
    try:
        if sys.platform != "linux":
            raise ValueError("build inside the target Linux image/container, not on the host Mac")
        target_os(Path("/"))
        actual = subprocess.check_output(["dpkg", "--print-architecture"], text=True).strip()
        if actual != args.arch:
            raise ValueError("native GTK builds require a container/VM of the target architecture")
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", args.build_version):
            raise ValueError("invalid build version")
        if args.output.exists():
            raise ValueError("output exists; choose a fresh build directory")
        versions = {name: subprocess.check_output(command, text=True).strip() for name, command in {
            "go": ["go", "version"], "cc": ["cc", "--version"],
            "gtk": ["pkg-config", "--modversion", "gtk+-3.0"],
            "webkit": ["pkg-config", "--modversion", "webkit2gtk-4.1"],
        }.items()}
        before = source_identity(args.source_commit, args.source_dirty)
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        pkg = "github.com/hey-meadow/meadow-code/go/core/buildinfo."
        identity = {"DesktopProductName": "MeadowOS", "DesktopVersion": "0.1-dev",
                    "DesktopBuild": args.build_version, "DesktopPackageMode": "portable",
                    "DesktopVariant": "vm-lab", "Version": "meadowos-dev",
                    "Commit": before["commit"], "BuildDate": stamp}
        ldflags = " ".join(f"-X {pkg}{key}={value}" for key, value in identity.items())
        output = args.output.absolute()
        output.mkdir(parents=True)
        env = dict(os.environ, CGO_ENABLED="1", GOOS="linux", GOARCH=args.arch)
        for package, relative in [("./cmd/meadow-agentd", "libexec/meadow-agentd"),
                                  ("./desktop", "bin/meadow-desktop")]:
            binary = output / relative
            binary.parent.mkdir(parents=True, exist_ok=True)
            subprocess.run(native_build_command(package, binary, ldflags),
                           cwd=REPO / "go", env=env, check=True)
            validate_elf(binary, args.arch)
        after = source_identity(args.source_commit, args.source_dirty)
        if before != after:
            raise ValueError("source changed during build; binaries are not a coherent publishable payload")
        records = {relative: {**file_record(output / relative), "identity": go_identity(output / relative)}
                   for relative in ["bin/meadow-desktop", "libexec/meadow-agentd"]}
        if records["bin/meadow-desktop"]["identity"] != records["libexec/meadow-agentd"]["identity"]:
            raise ValueError("native companion build identities differ")
        (output / "build-provenance.json").write_text(json.dumps({
            "schema_version": 1, "development_only": True, "arch": args.arch,
            "build_identity": identity, "source": before, "tool_versions": versions,
            "files": records, "product_ready": False,
            "acceptance": "compile_only; no VM boot, model, SDK or public release acceptance",
        }, indent=2) + "\n")
        print(json.dumps({"output": str(output), "arch": args.arch, "product_ready": False}))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"MeadowOS build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
