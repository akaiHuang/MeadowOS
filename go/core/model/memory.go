package model

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

const bytesPerGiB = int64(1024 * 1024 * 1024)

var vmAvailableLineRe = regexp.MustCompile(`^(Pages (?:free|inactive|speculative|purgeable)):\s+(\d+)`)

func (c Config) memoryPreflight() error {
	if !c.MemoryPreflight || memoryPreflightDisabled() {
		return nil
	}
	available, ok := availableMemoryBytes()
	if !ok {
		return nil
	}
	profile := CurrentMachineProfile()
	reserveGiB := memoryReserveGiB(profile)
	workingSetGiB := estimatedModelWorkingSetGiB(c.ServedName)
	requiredGiB := reserveGiB + workingSetGiB
	if override := envInt64("MEADOW_MEMORY_REQUIRE_FREE_GIB"); override > 0 {
		requiredGiB = override
	}
	if available >= requiredGiB*bytesPerGiB {
		return nil
	}
	return fmt.Errorf(
		"available memory %.1fGB is below Meadow's product guard %.1fGB for %s (estimated model %.1fGB + system reserve %.1fGB). Close other model servers/apps, unload the current model, or choose a smaller Meadow model",
		float64(available)/float64(bytesPerGiB),
		float64(requiredGiB),
		c.ServedName,
		float64(workingSetGiB),
		float64(reserveGiB),
	)
}

func memoryPreflightDisabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_MEMORY_PREFLIGHT")))
	return v == "0" || v == "false" || v == "no" || v == "off"
}

func memoryReserveGiB(profile MachineProfile) int64 {
	if override := envInt64("MEADOW_MEMORY_RESERVE_GIB"); override > 0 {
		return override
	}
	switch {
	case profile.TotalRAMGiB <= 16:
		return 4
	case profile.TotalRAMGiB <= 32:
		return 6
	case profile.TotalRAMGiB <= 64:
		return 8
	default:
		return 12
	}
}

func estimatedModelWorkingSetGiB(servedName string) int64 {
	if override := envInt64("MEADOW_MODEL_WORKING_SET_GIB"); override > 0 {
		return override
	}
	tier, ok := tierByName(servedName)
	if !ok {
		return 12
	}
	switch tier.ServedName {
	case tier9B.ServedName:
		return 8
	case tier12B.ServedName:
		return 10
	case tier26B.ServedName:
		return 20
	case tier27B.ServedName:
		return 24
	case tier35B.ServedName:
		return 24
	default:
		return int64(tier.ApproxGB + 4)
	}
}

func availableMemoryBytes() (int64, bool) {
	if override := envInt64("MEADOW_MEMORY_AVAILABLE_GIB"); override > 0 {
		return override * bytesPerGiB, true
	}
	out, err := exec.Command("vm_stat").Output()
	if err != nil {
		return 0, false
	}
	return parseAvailableMemoryBytes(string(out))
}

func parseAvailableMemoryBytes(out string) (int64, bool) {
	pageSize := int64(16384)
	var pages int64
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "page size of") {
			if m := regexp.MustCompile(`page size of (\d+)`).FindStringSubmatch(line); m != nil {
				pageSize, _ = strconv.ParseInt(m[1], 10, 64)
			}
			continue
		}
		if m := vmAvailableLineRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseInt(strings.TrimSuffix(m[2], "."), 10, 64)
			pages += n
		}
	}
	if pages <= 0 {
		return 0, false
	}
	return pages * pageSize, true
}

func envInt64(key string) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
