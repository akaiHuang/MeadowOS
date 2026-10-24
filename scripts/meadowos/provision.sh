#!/bin/bash
# Run only INSIDE the dedicated Ubuntu development image, never on a host Mac/PC.
set -euo pipefail

meadow_validate_apt_plan() {
  python3 - "$1" <<'PY'
import pathlib, re, sys
text = pathlib.Path(sys.argv[1]).read_text()
allowed = {'coreutils-from-uutils', 'build-essential', 'sudo-rs'}
removed = []
for line in text.splitlines():
    if line.startswith('Remv '):
        package = line.split()[1].split(':')[0]
        if package not in allowed:
            raise SystemExit(f'Refusing unexpected apt removal: {package}')
        removed.append(package)
# apt-get simulation's action records are checked before granting protected
# provider removal. GNU/coreutils and the explicit compiler packages are never
# in this allowlist. Fail if apt did not produce its expected simulation records.
if not any(line.startswith(('Inst ', 'Conf ')) for line in text.splitlines()) and \
        not re.search(r'0 upgraded, 0 newly installed, 0 to remove', text):
    raise SystemExit('Refusing unrecognized or empty apt simulation output')
print('Validated apt removals: ' + (', '.join(removed) if removed else '(none)'))
PY
}

# Sourcing only exposes the read-only plan validator for regression checks.
if [[ "${BASH_SOURCE[0]}" != "$0" ]]; then return 0; fi

task_arch=""
task_bundle=""
task_confirmed=0
task_simulate=0
while (($#)); do
  case "$1" in
    --arch) task_arch="$2"; shift 2 ;;
    --bundle) task_bundle="$2"; shift 2 ;;
    --dedicated-dev-image) task_confirmed=1; shift ;;
    --simulate-packages) task_simulate=1; shift ;;
    *) echo "Unknown provisioning argument: $1" >&2; exit 2 ;;
  esac
done
if [[ "$task_confirmed" != 1 || "$EUID" != 0 || "$(uname -s)" != Linux ]]; then
  echo 'Requires root INSIDE a dedicated Ubuntu26.04 dev image and --dedicated-dev-image.' >&2
  exit 1
fi
if [[ "$task_arch" != arm64 && "$task_arch" != amd64 ]]; then
  echo 'Require --arch arm64|amd64.' >&2; exit 1
fi
task_script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
task_bundle="${task_bundle:-$task_script_dir}"
export DEBIAN_FRONTEND=noninteractive
# Validate both target and artifact before apt or any other mutation.
python3 - "$task_bundle" "$task_arch" <<'PY'
import importlib.util, pathlib, subprocess, sys
bundle = pathlib.Path(sys.argv[1]).absolute()
spec = importlib.util.spec_from_file_location("meadow_image", bundle / "image.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.target_os(pathlib.Path("/"))
manifest = module.verify_bundle(bundle)
arch = subprocess.check_output(["dpkg", "--print-architecture"], text=True).strip()
if arch != sys.argv[2] or manifest["arch"] != arch:
    raise SystemExit("Target/bundle architecture mismatch")
if pathlib.Path("/opt/meadow").exists():
    raise SystemExit("Existing Meadow payload: use a separately validated upgrade flow")
module.validate_destination(pathlib.Path("/"), manifest)
PY
if id meadow >/dev/null 2>&1; then
  if [[ "$(getent passwd meadow | cut -d: -f6)" != /home/meadow ]]; then
    echo 'Refusing unexpected existing meadow user home before modifying target.' >&2; exit 1
  fi
  if [[ "$(id -u meadow)" == 0 || "$(getent passwd meadow | cut -d: -f7)" != /bin/bash ]]; then
    echo 'Refusing unexpected existing meadow UID/shell before modifying target.' >&2; exit 1
  fi
fi
mapfile -t task_packages < <(python3 - "$task_bundle" <<'PY'
import json, pathlib, sys
value = json.loads((pathlib.Path(sys.argv[1]) / "payload/opt/meadow/share/meadow-os/packages.json").read_text())
print("\n".join(value["packages"]))
PY
)
if [[ "$task_simulate" != 1 ]]; then apt-get update; fi
# Ubuntu26.04's build-essential metapackage hard-depends on the uutils provider.
# Its compiler/header dependencies are explicitly listed in packages.json.
# Select the replacement and remove only the conflicting selectors in ONE apt
# transaction; never remove a provider in a separate preliminary apt command.
task_packages+=(coreutils-from-uutils- build-essential- sudo-rs-)
task_plan="$(mktemp)"
trap 'rm -f -- "$task_plan"' EXIT
LC_ALL=C apt-get -s -o APT::Color=0 install -y --allow-remove-essential --no-install-recommends \
  "${task_packages[@]}" > "$task_plan"
meadow_validate_apt_plan "$task_plan"
if [[ "$task_simulate" == 1 ]]; then
  cat "$task_plan"
  echo 'Simulation only; no packages or Meadow payload were installed.'
  exit 0
fi
# --allow-remove-essential is required because the old selector is Protected.
# The dedicated-image guard plus exact removal allowlist above bounds its use.
LC_ALL=C apt-get install -y --allow-remove-essential --no-install-recommends "${task_packages[@]}"
python3 - <<'PY'
import subprocess, sys
for package, expected in [('coreutils-from-gnu', True), ('coreutils-from-uutils', False), ('sudo-rs', False)]:
    result = subprocess.run(['dpkg-query', '-W', '-f=${Status}', package], capture_output=True, text=True)
    installed = result.returncode == 0 and result.stdout == 'install ok installed'
    if installed != expected:
        raise SystemExit(f'Unexpected final package provider state: {package}')
if 'GNU coreutils' not in subprocess.check_output(['ls', '--version'], text=True):
    raise SystemExit('GNU coreutils must own the default commands')
if not subprocess.check_output(['sudo', '--version'], text=True).startswith('Sudo version '):
    raise SystemExit('Classic sudo must own the default command')
for command, minimum in [("node", 22), ("go", 1)]:
    text = subprocess.check_output([command, "version" if command == "go" else "--version"], text=True)
    if command == "node" and int(text.strip().lstrip("v").split(".")[0]) < minimum:
        raise SystemExit("Node >=22 is required")
    if command == "go" and tuple(map(int, text.split()[2][2:].split(".")[:2])) < (1, 25):
        raise SystemExit("Go >=1.25 is required")
if sys.version_info < (3, 11):
    raise SystemExit("Python >=3.11 is required")
PY
python3 "$task_bundle/image.py" install --bundle "$task_bundle" --root / --arch "$task_arch" --dedicated-dev-image
if ! id meadow >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash meadow
fi
# This development VM has a local graphical autologin, no shared remote password or SSH credential.
install -d -m 0755 /etc/lightdm/lightdm.conf.d
cat > /etc/lightdm/lightdm.conf.d/60-meadowos-dev.conf <<'EOF'
[Seat:*]
autologin-user=meadow
autologin-session=meadow
user-session=meadow
EOF
dpkg-query -W -f='${binary:Package}\t${Version}\t${Architecture}\n' > /opt/meadow/share/meadow-os/installed-packages.tsv
python3 - <<'PY'
import hashlib, json, pathlib, subprocess
root = pathlib.Path('/opt/meadow/share/meadow-os')
versions = {name: subprocess.check_output(command, text=True).strip() for name, command in {
    'python': ['python3', '--version'], 'node': ['node', '--version'], 'npm': ['npm', '--version'],
    'go': ['go', 'version'], 'git': ['git', '--version'], 'bash': ['bash', '--version'],
    'coreutils': ['ls', '--version'], 'sudo': ['sudo', '--version']}.items()}
package_bytes = (root / 'installed-packages.tsv').read_bytes()
(root / 'provision-receipt.json').write_text(json.dumps({
    'schema_version': 1, 'development_only': True, 'product_ready': False,
    'os_release': pathlib.Path('/etc/os-release').read_text(),
    'package_inventory_sha256': hashlib.sha256(package_bytes).hexdigest(),
    'tool_versions': versions, 'optional_services': json.loads((root / 'packages.json').read_text())['optional_services'],
}, indent=2) + '\n')
PY
echo 'MeadowOS foundation installed. VM boot, model, SDK, URL and service acceptance remain pending.'
