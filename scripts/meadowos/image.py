#!/usr/bin/env python3
"""Render and stage a development-only Ubuntu MeadowOS payload. Never boots a VM."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import struct
import subprocess
import sys

REPO = Path(__file__).resolve().parents[2]
PACKAGING = REPO / "packaging/meadowos"
MACHINES = {"amd64": 62, "arm64": 183}
THUNAR_TRIPLETS = {"amd64": "x86_64-linux-gnu", "arm64": "aarch64-linux-gnu"}
FOLDER_RESOURCE = "folder_materials/libmeadow-folder-materials.so"


def folder_extension_path(arch):
    return f"usr/lib/{THUNAR_TRIPLETS[arch]}/thunarx-3/libmeadow-folder-materials.so"


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def file_record(path):
    return {"sha256": sha256(path), "size": path.stat().st_size}



def source_inventory():
    """Hash the complete native input tree, independent of Git credentials."""
    excluded = {"__pycache__", "node_modules", ".git", ".cache"}
    sources = {}
    for directory in ("go", "scripts/meadowos", "packaging/meadowos"):
        for path in (REPO / directory).rglob("*"):
            relative = path.relative_to(REPO)
            if excluded.intersection(relative.parts) or path.suffix in (".pyc", ".pyo"):
                continue
            if path.is_symlink():
                raise ValueError(f"native source must not contain symlinks: {relative}")
            if path.is_file():
                sources[str(relative)] = file_record(path)
    sources = dict(sorted(sources.items()))
    if not sources:
        raise ValueError("native source inventory is empty")
    digest = hashlib.sha256(json.dumps(sources, sort_keys=True).encode()).hexdigest()
    return sources, digest


def verified_source_identity(provenance):
    """Admit archive builds only with coherent, rehashed build provenance."""
    source = provenance.get("source", {})
    if (not isinstance(source, dict)
            or not isinstance(source.get("commit"), str)
            or not re.fullmatch(r"[0-9a-f]{40}", source["commit"])
            or type(source.get("dirty")) is not bool
            or source.get("commit_origin") not in ("git", "explicit_builder_input")):
        raise ValueError("build provenance has no valid source identity")
    sources, digest = source_inventory()
    if source.get("source_files") != sources or source.get("source_inventory_sha256") != digest:
        raise ValueError("native source differs from build provenance")
    git = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, text=True, capture_output=True)
    if git.returncode == 0:
        commit = git.stdout.strip()
        dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=REPO, text=True))
        if commit != source["commit"] or dirty != source["dirty"]:
            raise ValueError("Git source identity differs from build provenance")
    else:
        # The builder required an explicit full SHA and dirty declaration. The
        # SHA identifies the caller's source revision; hashes bind these bytes.
        commit, dirty = source["commit"], source["dirty"]
    return commit, dirty


def target_os(root):
    path = root / "etc/os-release"
    if not path.is_file():
        raise ValueError("target is missing /etc/os-release")
    values = {}
    for line in path.read_text().splitlines():
        if "=" in line:
            key, value = line.split("=", 1)
            values[key] = value.strip().strip('"')
    if values.get("ID") != "ubuntu" or values.get("VERSION_ID") != "26.04":
        raise ValueError("target must be Ubuntu 26.04; refusing host/other OS installation")


def validate_elf(path, arch):
    if path.is_symlink() or not path.is_file():
        raise ValueError(f"missing regular Linux binary: {path}")
    data = path.read_bytes()[:64]
    if len(data) < 64 or data[:6] != b"\x7fELF\x02\x01":
        raise ValueError(f"not a 64-bit little-endian Linux ELF: {path}")
    if struct.unpack_from("<H", data, 18)[0] != MACHINES[arch]:
        raise ValueError(f"binary architecture does not match {arch}: {path}")
    if not os.access(path, os.X_OK):
        raise ValueError(f"binary must be executable: {path}")


def go_identity(path):
    result = subprocess.run(["go", "version", "-m", str(path)], capture_output=True, text=True, check=True)
    if "meadow_vm_lab" not in result.stdout:
        raise ValueError(f"binary is not a compile-time VM development build: {path}")
    fields = {}
    pkg = "github.com/hey-meadow/meadow-code/go/core/buildinfo."
    for name in ["DesktopPackageMode", "DesktopVariant", "DesktopBuild", "Commit", "BuildDate"]:
        match = re.search(re.escape(pkg + name) + r"=([^\s'\"]+)", result.stdout)
        if not match:
            raise ValueError(f"binary lacks build identity {name}: {path}")
        fields[name] = match[1]
    if fields["DesktopPackageMode"] != "portable" or fields["DesktopVariant"] != "vm-lab":
        raise ValueError("MeadowOS foundation requires portable vm-lab composition")
    return fields


def safe_target(root, relative):
    target = root / relative
    for component in [*reversed(target.parents), target]:
        if component.is_symlink():
            raise ValueError(f"refusing symlink in install path: {component}")
    return target


def normalize_root(root):
    # Canonicalize before the live-root guard, including /tmp/.. and // aliases.
    absolute = root.absolute()
    for component in [*reversed(absolute.parents), absolute]:
        if component.is_symlink():
            raise ValueError(f"refusing symlink in target root: {component}")
    result = absolute.resolve(strict=True)
    if not result.is_dir():
        raise ValueError("target root must be an existing directory")
    return result


def copy_regular(source, target, executable=False):
    if source.is_symlink() or not source.is_file():
        raise ValueError(f"payload must contain regular files: {source}")
    target.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(source, target)
    target.chmod(0o755 if executable else 0o644)


WINDOW_RESOURCE_SOURCES = {
    "desktop/glass/meadow-glass-capture": ("desktop/glass/capture.c",),
    "window_manager/libmeadow-hover-chrome.so": ("window_manager/meadow_hover_chrome.c",),
    "folder_materials/libmeadow-folder-materials.so": (
        "window_manager/meadow_folder_materials.c",
        "window_manager/meadow_folder_paint.c",
        "window_manager/meadow_folder_paint.h",
    ),
}
LEGACY_WINDOW_RESOURCES = frozenset(list(WINDOW_RESOURCE_SOURCES)[:2])


def window_resources(directory, arch):
    """Accept known native resources with every contributing source hash."""
    if directory is None:
        return None
    receipt_path = safe_target(directory, "window-resources.json")
    receipt = json.loads(receipt_path.read_text())
    names = set(receipt.get("files", {}))
    if names not in (LEGACY_WINDOW_RESOURCES, set(WINDOW_RESOURCE_SOURCES)):
        raise ValueError("invalid native window resource manifest")
    sources = {source for name in names for source in WINDOW_RESOURCE_SOURCES[name]}
    if (receipt.get("schema_version") != 1 or receipt.get("arch") != arch
            or set(receipt.get("source_files", {})) != sources):
        raise ValueError("invalid native window resource manifest")
    for relative in names:
        path = safe_target(directory, relative)
        validate_elf(path, arch)
        if file_record(path) != receipt["files"][relative]:
            raise ValueError("native window resource changed after compilation")
    for source in sources:
        if file_record(PACKAGING / source) != receipt["source_files"][source]:
            raise ValueError("native window resource source differs from checkout")
    return receipt


def render(args):
    subprocess.run([sys.executable, str(PACKAGING / "desktop/sidebar_styles.py"), "--check"], check=True)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,63}", args.build_version):
        raise ValueError("invalid build version")
    output = args.output.absolute()
    if output.exists():
        raise ValueError("output already exists; render into a new directory")
    for path in [args.desktop_binary, args.agent_binary]:
        validate_elf(path, args.arch)
    desktop_identity, agent_identity = [go_identity(path) for path in [args.desktop_binary, args.agent_binary]]
    if desktop_identity != agent_identity or agent_identity["DesktopBuild"] != args.build_version:
        raise ValueError("Desktop and Agent must have the exact same build identity/version")
    provenance_path = args.desktop_binary.parent.parent / "build-provenance.json"
    provenance = json.loads(provenance_path.read_text())
    for relative, path in [("bin/meadow-desktop", args.desktop_binary), ("libexec/meadow-agentd", args.agent_binary)]:
        if provenance["files"][relative]["sha256"] != sha256(path):
            raise ValueError("binary differs from its build provenance")
        if provenance["files"][relative]["identity"] != agent_identity:
            raise ValueError("build provenance identity differs")
    commit, dirty = verified_source_identity(provenance)
    if agent_identity["Commit"] != commit:
        raise ValueError("binary commit differs from source provenance")
    resource_dir = getattr(args, "window_resources", None)
    resources = window_resources(resource_dir, args.arch)
    desktop = PACKAGING / "desktop"
    shell = PACKAGING / "shell"
    voiceos = PACKAGING / "voiceos"
    for relative in ["bin/meadow-welcome", "bin/meadow-theme", "bin/meadow-settings", "bin/meadow-quicklook",
                     "quicklook.py", "file_preview.py",
                     "share/applications/meadow-welcome.desktop", "share/applications/meadow-settings.desktop",
                     "icons/Meadow/index.theme", "picom.conf"]:
        if not (desktop / relative).is_file():
            raise ValueError(f"missing native desktop asset: {relative}")
    for path in [shell / "meadow_shell.py", PACKAGING / "os_session.py", PACKAGING / "session/meadow.desktop"]:
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"missing regular Meadow Xsession asset: {path}")
    for path in [shell / "voiceos_surface.py", shell / "voice_transport.py", voiceos / "web/index.html"]:
        if path.is_symlink() or not path.is_file():
            raise ValueError(f"missing regular VoiceOS asset: {path}")
    # Validate every input before creating the output or copying executable code.
    assets = list(desktop.rglob("*")) + list(shell.rglob("*")) + list(voiceos.rglob("*")) + list((PACKAGING / "window_manager").rglob("*"))
    if any(path.is_symlink() for path in assets):
        raise ValueError("desktop assets must not contain symlinks")
    output.mkdir(parents=True)
    payload = output / "payload"
    prefix = payload / "opt/meadow"
    share = prefix / "share/meadow-os"
    copy_regular(args.desktop_binary, prefix / "bin/meadow-desktop", True)
    copy_regular(args.agent_binary, prefix / "libexec/meadow-agentd", True)
    shutil.copytree(desktop, share / "desktop", ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.pyo", "tests", "meadow-glass-capture", "*.so"))
    shutil.copytree(shell, share / "shell", ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.pyo", "tests"))
    shutil.copytree(voiceos, share / "voiceos", ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.pyo", "tests"))
    shutil.copytree(PACKAGING / "window_manager", share / "window_manager",
                    ignore=shutil.ignore_patterns("__pycache__", "*.pyc", "*.so", "tests"))
    if resources:
        for relative in resources["files"]:
            copy_regular(resource_dir / relative, share / relative, True)
        if FOLDER_RESOURCE in resources["files"]:
            # Ubuntu Thunarx can disable THUNARX_DIRS at compile time. Install
            # the provider into its canonical architecture-specific directory.
            copy_regular(resource_dir / FOLDER_RESOURCE, payload / folder_extension_path(args.arch), True)
        copy_regular(resource_dir / "window-resources.json", share / "window-resources.json")
    copy_regular(PACKAGING / "runtime.py", share / "bin/runtime.py", True)
    copy_regular(PACKAGING / "os_session.py", share / "bin/os_session.py")
    for name, command in [("meadow-session", '"$@"'), ("meadow-first-login", 'first-login "$@"'),
                          ("meadow-doctor", 'doctor "$@"'), ("meadow-os-session", 'os-session "$@"')]:
        path = share / "bin" / name
        path.write_text('#!/bin/sh\nset -eu\nexec /usr/bin/python3 /opt/meadow/share/meadow-os/bin/runtime.py ' + command + '\n')
        path.chmod(0o755)
    copy_regular(PACKAGING / "meadow-agent.service", payload / "usr/lib/systemd/user/meadow-agent.service")
    # Meadow owns its session components; do not autostart the old Welcome app.
    copy_regular(PACKAGING / "session/meadow.desktop", payload / "usr/share/xsessions/meadow.desktop")
    copy_regular(PACKAGING / "packages.json", share / "packages.json")
    copy_regular(provenance_path, share / "build-provenance.json")
    for path in (desktop / "share/applications").glob("*.desktop"):
        copy_regular(path, payload / "usr/share/applications" / path.name)
    if (desktop / "themes").is_dir():
        shutil.copytree(desktop / "themes", payload / "usr/share/themes")
    shutil.copytree(desktop / "icons/Meadow", payload / "usr/share/icons/Meadow")
    shutil.copytree(desktop / "icons/Meadow-Cursors", payload / "usr/share/icons/Meadow-Cursors")
    for name in ["meadow-welcome", "meadow-theme", "meadow-settings", "meadow-quicklook"]:
        # A wrapper avoids source-tree symlinks and has a stable installed path.
        path = prefix / "bin" / name
        path.write_text(f'#!/bin/sh\nset -eu\ntask_bindir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)\nexec "$task_bindir/../share/meadow-os/desktop/bin/{name}" "$@"\n')
        path.chmod(0o755)
    for name in ["image.py", "provision.sh"]:
        copy_regular(Path(__file__).parent / name, output / name, True)
    files = {str(path.relative_to(payload)): {**file_record(path), "mode": oct(path.stat().st_mode & 0o777)}
             for path in sorted(payload.rglob("*")) if path.is_file()}
    manifest = {
        "schema_version": 1, "product": "meadow-os-foundation", "development_only": True,
        "os": "ubuntu", "version_id": "26.04", "arch": args.arch,
        "build_version": args.build_version, "binary_identity": agent_identity,
        "source_commit": commit, "source_dirty": dirty,
        "source_files": {str(path.relative_to(REPO)): file_record(path) for path in
                         [PACKAGING / "runtime.py", PACKAGING / "packages.json", Path(__file__),
                          PACKAGING / "os_session.py", PACKAGING / "session/meadow.desktop", desktop / "picom.conf",
                          Path(__file__).parent / "provision.sh"]},
        "files": files, "product_ready": False,
        "native_window_resources": resources,
        "optional_services": json.loads((PACKAGING / "packages.json").read_text())["optional_services"],
        "session": {"name": "meadow", "window_manager": "xfwm4", "compositor": "xfwm4",
                    "requested_geometry": [1920, 1080], "native_backdrop_blur": False,
                    "material": "native alpha composition; backdrop blur is not enabled",
                    "experimental_compositor": {"opt_in": "MEADOW_EXPERIMENTAL_GLASS=1",
                        "name": "picom", "backend": "glx", "config": "desktop/picom.conf",
                        "acceptance": "rejected: intermittent blank first map of preconstructed hidden GTK surfaces",
                        "start": "after current Shell final geometry first-paint receipt",
                        "shell_recovery": "rebuild only owned compositor after replacement Shell first paint"},
                    "compositor_failure": "retain WM and Agent; opaque GTK fallback",
                    "restores_xfce_saved_sessions": False},
        "pending_acceptance": ["vm_boot", "meadow_xsession", "1920x1080_display", "model", "sdk_control", "private_url", "update_rollback"],
    }
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    # This cloud-init snippet intentionally carries no payload URL, passwords, user keys or credentials.
    (output / "cloud-init.yaml").write_text("#cloud-config\n# Copy this verified bundle into the dedicated dev guest, then run provision.sh.\n# No runcmd: a new guest must not fetch unpinned scripts or API credentials.\nssh_deletekeys: true\n")
    print(json.dumps({"bundle": str(output), "arch": args.arch, "product_ready": False}))


def verify_bundle(bundle):
    manifest = json.loads((bundle / "manifest.json").read_text())
    if manifest.get("product") != "meadow-os-foundation" or manifest.get("development_only") is not True:
        raise ValueError("not a MeadowOS development foundation bundle")
    if manifest.get("arch") not in MACHINES:
        raise ValueError("invalid manifest architecture")
    payload = bundle / "payload"
    for relative, expected in manifest["files"].items():
        if Path(relative).is_absolute() or ".." in Path(relative).parts:
            raise ValueError("invalid manifest path")
        path = safe_target(payload, relative)
        if not path.is_file() or sha256(path) != expected["sha256"]:
            raise ValueError(f"missing/changed payload: {relative}")
    actual = {str(path.relative_to(payload)) for path in payload.rglob("*") if path.is_file()}
    if actual != set(manifest["files"]):
        raise ValueError("unexpected payload files")
    validate_elf(payload / "opt/meadow/bin/meadow-desktop", manifest["arch"])
    validate_elf(payload / "opt/meadow/libexec/meadow-agentd", manifest["arch"])
    if manifest.get("native_window_resources"):
        for relative in manifest["native_window_resources"]["files"]:
            if relative not in WINDOW_RESOURCE_SOURCES:
                raise ValueError("unexpected native window resource")
            validate_elf(safe_target(payload / "opt/meadow/share/meadow-os", relative), manifest["arch"])
        if FOLDER_RESOURCE in manifest["native_window_resources"]["files"]:
            extension = safe_target(payload, folder_extension_path(manifest["arch"]))
            validate_elf(extension, manifest["arch"])
            if file_record(extension) != manifest["native_window_resources"]["files"][FOLDER_RESOURCE]:
                raise ValueError("Thunar extension differs from native folder resource")
    return manifest


def install(args):
    root = normalize_root(args.root)
    target_os(root)
    manifest = verify_bundle(args.bundle)
    if manifest["arch"] != args.arch:
        raise ValueError("bundle and target architectures differ")
    if root == Path("/"):
        if not args.dedicated_dev_image or os.geteuid() != 0 or sys.platform != "linux":
            raise ValueError("live installation requires Linux root and --dedicated-dev-image")
        arch = subprocess.check_output(["dpkg", "--print-architecture"], text=True).strip()
        if arch != args.arch:
            raise ValueError("actual target CPU architecture differs")
    validate_destination(root, manifest)
    prefix = safe_target(root, "opt/meadow")
    for relative, record in manifest["files"].items():
        copy_regular(args.bundle / "payload" / relative, root / relative,
                     bool(int(record["mode"], 8) & 0o111))
    receipt = prefix / "share/meadow-os/install-receipt.json"
    receipt.write_text(json.dumps({"schema_version": 1, "manifest_sha256": sha256(args.bundle / "manifest.json"),
                                   "build_version": manifest["build_version"], "arch": args.arch,
                                   "installed_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                                   "product_ready": False}, indent=2) + "\n")
    print(json.dumps({"installed_prefix": str(prefix), "product_ready": False}))


def validate_destination(root, manifest):
    prefix = safe_target(root, "opt/meadow")
    if prefix.exists():
        raise ValueError("Meadow payload already exists; live upgrades require separate idle/rollback integration")
    # Reject planted directories before copying anything. This is first-install only.
    for relative in manifest["files"]:
        target = safe_target(root, relative)
        if target.exists():
            raise ValueError(f"existing target file would be overwritten: {target}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    renderer = sub.add_parser("render")
    renderer.add_argument("--arch", choices=MACHINES, required=True)
    renderer.add_argument("--build-version", required=True)
    renderer.add_argument("--desktop-binary", type=Path, required=True)
    renderer.add_argument("--agent-binary", type=Path, required=True)
    renderer.add_argument("--output", type=Path, required=True)
    renderer.add_argument("--window-resources", type=Path,
                          help="target-Linux build_window_resources.py output with source/hash receipt")
    installer = sub.add_parser("install")
    installer.add_argument("--bundle", type=Path, required=True)
    installer.add_argument("--root", type=Path, required=True)
    installer.add_argument("--arch", choices=MACHINES, required=True)
    installer.add_argument("--dedicated-dev-image", action="store_true")
    args = parser.parse_args()
    try:
        (render if args.command == "render" else install)(args)
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"MeadowOS image preparation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
