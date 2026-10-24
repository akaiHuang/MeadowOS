"""Compile the native helper on the target Linux architecture, never at paint time."""
import argparse
from pathlib import Path
import subprocess


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--output", type=Path, default=Path(__file__).with_name("meadow-glass-capture"))
    args = parser.parse_args()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(["cc", "-std=c99", "-O2", "-Wall", "-Wextra", "-Werror", str(Path(__file__).with_name("capture.c")),
                    "-o", str(args.output), "-lX11", "-lXcomposite", "-lXrender"], check=True)
    print(args.output)


if __name__ == "__main__":
    main()
