package tools

import (
	"context"
	"fmt"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
)

func collectSystemInfoLinux(ctx context.Context, p systemInfoProbe, r *systemInfoReport) {
	readFile := p.readFile
	p.readFile = func(name string) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return readFile(name)
	}
	read := func(name string) string {
		if ctx.Err() != nil {
			return ""
		}
		data, err := p.readFile(name)
		if err != nil {
			return ""
		}
		return string(data)
	}
	if kernel := systemInfoText(read("/proc/sys/kernel/osrelease")); kernel != "" {
		r.OS["kernel_release"] = kernel
	}
	distro := read("/etc/os-release")
	if distro == "" {
		distro = read("/usr/lib/os-release")
	}
	for _, line := range strings.Split(distro, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if key == "ID" || key == "VERSION_ID" || key == "PRETTY_NAME" {
			// os-release is data, never source/shell evaluation.
			r.OS[strings.ToLower(key)] = systemInfoText(strings.Trim(value, "\"'"))
		}
	}
	for _, line := range strings.Split(read("/proc/cpuinfo"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "Hardware") {
			r.CPU["model"] = systemInfoText(value)
			break
		}
	}
	for _, line := range strings.Split(read("/proc/self/status"), "\n") {
		if value, ok := strings.CutPrefix(line, "Cpus_allowed_list:"); ok {
			if count, err := systemInfoCPUListCount(strings.TrimSpace(value)); err == nil {
				r.CPU["affinity_logical_cpus"] = count
			}
		}
	}
	for _, line := range strings.Split(read("/proc/meminfo"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[2] == "kB" && (fields[0] == "MemTotal:" || fields[0] == "MemAvailable:") {
			if n, err := strconv.ParseUint(fields[1], 10, 64); err == nil && n <= math.MaxUint64/1024 {
				key := "kernel_visible_total_bytes"
				if fields[0] == "MemAvailable:" {
					key = "kernel_visible_available_bytes"
				}
				r.Memory[key] = n * 1024
			}
		}
	}
	r.Memory["scope"] = "proc_meminfo_may_describe_resources_outside_the_container"
	evidence := []string{}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := p.stat(marker); err == nil {
			evidence = append(evidence, marker)
		}
	}
	if value := systemInfoText(read("/run/systemd/container")); value != "" {
		evidence = append(evidence, "systemd container marker: "+value)
	}
	membership, mounts := read("/proc/self/cgroup"), read("/proc/self/mountinfo")
	// A cgroup name is evidence only, not proof of a runtime or physical host.
	for _, marker := range []string{"docker", "kubepods", "libpod", "containerd", "lxc"} {
		if strings.Contains(membership, marker) {
			evidence = append(evidence, "cgroup path contains "+marker)
		}
	}
	r.Isolation["container_evidence"] = evidence
	r.Isolation["container_detection"] = "unknown"
	if len(evidence) != 0 {
		r.Isolation["container_detection"] = "evidence_present"
	}
	limits := systemInfoCgroups(p, membership, mounts)
	r.Isolation["cgroup"] = limits
	if memory, ok := limits["visible_memory_limit_bytes"].(uint64); ok {
		ceiling := memory
		if total, ok := r.Memory["kernel_visible_total_bytes"].(uint64); ok && total < ceiling {
			ceiling = total
		}
		r.Memory["observed_memory_upper_bound_bytes"] = ceiling
	}
	r.Limitations = append(r.Limitations,
		"Cgroup limits aggregate only readable ancestors in this mount namespace; hidden ancestors may impose tighter limits. Unlimited means no finite limit was observed, not unlimited physical resources.",
		"Container markers are evidence, not exhaustive detection. Missing markers do not prove bare metal. A container and its surrounding VM can coexist.",
		"Memory available in /proc/meminfo is not container-available memory; cgroup usage and shared ancestor limits cannot safely be subtracted to promise free memory.")
}

func systemInfoCPUListCount(value string) (int, error) {
	count, previous := 0, -1
	if value == "" {
		return 0, fmt.Errorf("empty CPU list")
	}
	for _, span := range strings.Split(value, ",") {
		start, end, hasRange := strings.Cut(span, "-")
		lo, err := strconv.Atoi(start)
		if err != nil || lo < 0 || lo <= previous {
			return 0, fmt.Errorf("invalid CPU range")
		}
		hi := lo
		if hasRange {
			hi, err = strconv.Atoi(end)
		}
		if err != nil || hi < lo || hi > 1<<20 {
			return 0, fmt.Errorf("invalid CPU range")
		}
		count += hi - lo + 1
		previous = hi
	}
	return count, nil
}

type systemInfoCgroupMount struct {
	root, mount, version string
	controllers          string
}

func systemInfoCgroups(p systemInfoProbe, membership, mountinfo string) map[string]any {
	result := map[string]any{"status": "unavailable", "ancestor_scope": "readable_mount_namespace_only", "outside_mount_ancestors": "unknown"}
	mounts := []systemInfoCgroupMount{}
	for _, line := range strings.Split(mountinfo, "\n") {
		before, after, ok := strings.Cut(line, " - ")
		left, right := strings.Fields(before), strings.Fields(after)
		if !ok || len(left) < 6 || len(right) < 3 || (right[0] != "cgroup" && right[0] != "cgroup2") {
			continue
		}
		decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`)
		mounts = append(mounts, systemInfoCgroupMount{decode.Replace(left[3]), decode.Replace(left[4]), right[0], right[2]})
	}
	var memoryLimit uint64
	var cpuLimit float64
	var hasMemory, hasCPU, memorySample, cpuSample, unreadable bool
	versions := map[string]bool{}
	for _, line := range strings.Split(membership, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 {
			continue
		}
		for _, mount := range mounts {
			v2 := mount.version == "cgroup2" && fields[0] == "0" && fields[1] == ""
			memory := v2 || (mount.version == "cgroup" && systemInfoHasController(fields[1], "memory") && systemInfoHasController(mount.controllers, "memory"))
			cpu := v2 || (mount.version == "cgroup" && systemInfoHasController(fields[1], "cpu") && systemInfoHasController(mount.controllers, "cpu"))
			if !memory && !cpu {
				continue
			}
			leaf, ok := systemInfoCgroupPath(mount.root, mount.mount, fields[2])
			if !ok {
				unreadable = true
				continue
			}
			versions[mount.version] = true
			result["status"] = "observed"
			for current, depth := leaf, 0; ; current, depth = path.Dir(current), depth+1 {
				if depth >= 64 {
					unreadable = true
					break
				}
				read := func(name string) string {
					b, err := p.readFile(path.Join(current, name))
					if err != nil {
						unreadable = true
						return ""
					}
					return strings.TrimSpace(string(b))
				}
				if memory {
					name := "memory.limit_in_bytes"
					if v2 {
						name = "memory.max"
					}
					applicable := true
					if !v2 && current != leaf {
						hierarchy := read("memory.use_hierarchy")
						applicable = hierarchy == "1"
						if hierarchy != "0" && hierarchy != "1" {
							unreadable = true
						}
					}
					if applicable {
						value := read(name)
						if n, err := strconv.ParseUint(value, 10, 64); err == nil {
							memorySample = true
							unlimitedSentinel := uint64(^uint(0)>>1) &^ uint64(os.Getpagesize()-1)
							if v2 || n != unlimitedSentinel {
								if !hasMemory || n < memoryLimit {
									memoryLimit = n
								}
								hasMemory = true
							}
						} else if v2 && value == "max" {
							memorySample = true
						} else {
							unreadable = true
						}
					}
					if current == leaf {
						usageName := "memory.usage_in_bytes"
						if v2 {
							usageName = "memory.current"
						}
						if usage, err := strconv.ParseUint(read(usageName), 10, 64); err == nil {
							result["leaf_memory_usage_bytes"] = usage
						}
					}
				}
				if cpu {
					quota, period := "", ""
					if v2 {
						if values := strings.Fields(read("cpu.max")); len(values) == 2 {
							quota, period = values[0], values[1]
						}
					} else {
						quota, period = read("cpu.cfs_quota_us"), read("cpu.cfs_period_us")
					}
					q, qe := strconv.ParseInt(quota, 10, 64)
					per, pe := strconv.ParseUint(period, 10, 64)
					if qe == nil && pe == nil && q > 0 && per > 0 {
						cpuSample = true
						n := float64(q) / float64(per)
						if !hasCPU || n < cpuLimit {
							cpuLimit = n
						}
						hasCPU = true
					} else if pe == nil && per > 0 && ((v2 && quota == "max") || (!v2 && quota == "-1")) {
						cpuSample = true
					} else {
						unreadable = true
					}
				}
				if current == path.Clean(mount.mount) {
					break
				}
			}
		}
	}
	if hasMemory {
		result["visible_memory_limit_bytes"] = memoryLimit
	}
	if hasCPU {
		result["visible_cpu_quota_cores"] = cpuLimit
	}
	if len(versions) > 0 {
		versionNames := []string{}
		for _, name := range []string{"cgroup", "cgroup2"} {
			if versions[name] {
				versionNames = append(versionNames, name)
			}
		}
		result["versions"] = versionNames
		result["memory_limit_status"], result["cpu_quota_status"] = "not_observed", "not_observed"
		if memorySample {
			result["memory_limit_status"] = "no_finite_limit_observed"
		}
		if cpuSample {
			result["cpu_quota_status"] = "no_finite_limit_observed"
		}
		if unreadable {
			result["memory_limit_status"], result["cpu_quota_status"] = "unknown_incomplete_observation", "unknown_incomplete_observation"
		}
		if hasMemory {
			result["memory_limit_status"] = "finite_limit_observed"
		}
		if hasCPU {
			result["cpu_quota_status"] = "finite_limit_observed"
		}
	}
	if unreadable {
		result["readable_hierarchy_complete"] = false
		result["status"] = "partial"
	} else if len(versions) > 0 {
		result["readable_hierarchy_complete"] = true
	}
	return result
}

func systemInfoHasController(value, controller string) bool {
	for _, part := range strings.Split(value, ",") {
		if part == controller {
			return true
		}
	}
	return false
}

func systemInfoCgroupPath(root, mount, member string) (string, bool) {
	for _, value := range []string{root, mount, member} {
		if !strings.HasPrefix(value, "/") {
			return "", false
		}
		for _, part := range strings.Split(value, "/") {
			if part == ".." {
				return "", false
			}
		}
	}
	root, mount, member = path.Clean(root), path.Clean(mount), path.Clean(member)
	var relative string
	switch {
	case member == root:
	case root == "/":
		relative = strings.TrimPrefix(member, "/")
	case strings.HasPrefix(member, root+"/"):
		relative = strings.TrimPrefix(member, root+"/")
	default:
		return "", false // do not guess an invisible namespace mapping
	}
	return path.Join(mount, relative), true
}
