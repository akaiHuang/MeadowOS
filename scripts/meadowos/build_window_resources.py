#!/usr/bin/env python3
"""Build architecture-specific window resources without rebuilding/stopping Agent."""
import argparse
import json
from pathlib import Path
import subprocess
import sys

from image import MACHINES, PACKAGING, WINDOW_RESOURCE_SOURCES, file_record, validate_elf

OUTPUTS = WINDOW_RESOURCE_SOURCES


def build(output, arch):
    if sys.platform != "linux":
        raise ValueError("build window resources inside the target Linux guest")
    if subprocess.check_output(["dpkg", "--print-architecture"], text=True).strip() != arch:
        raise ValueError("window resource architecture differs from the native compiler")
    if output.exists():
        raise ValueError("output already exists; use a fresh resource directory")
    source_names = {name for names in OUTPUTS.values() for name in names}
    sources = {name: file_record(PACKAGING / name) for name in sorted(source_names)}
    output.mkdir(parents=True)
    subprocess.run([sys.executable, str(PACKAGING / "desktop/glass/build.py"),
                    "--output", str(output / "desktop/glass/meadow-glass-capture")], check=True)
    subprocess.run(["sh", str(PACKAGING / "window_manager/build.sh"),
                    "--output", str(output / "window_manager")], check=True)
    subprocess.run(["sh", str(PACKAGING / "window_manager/build-folders.sh"),
                    "--output", str(output / "folder_materials")], check=True)
    if sources != {name: file_record(PACKAGING / name) for name in source_names}:
        raise ValueError("window sources changed during compilation")
    for relative in OUTPUTS:
        validate_elf(output / relative, arch)
    receipt = {"schema_version": 1, "arch": arch, "source_files": sources,
               "files": {name: file_record(output / name) for name in OUTPUTS},
               "acceptance": "compiled only; native visual acceptance is separate"}
    (output / "window-resources.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--arch", choices=MACHINES, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        build(args.output.absolute(), args.arch)
        print(json.dumps({"window_resources": str(args.output.absolute()), "arch": args.arch}))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print(f"Window resource build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
