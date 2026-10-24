package model

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type ChipClass string

const (
	ChipUnknown ChipClass = "unknown"
	ChipBase    ChipClass = "base"
	ChipPro     ChipClass = "pro"
	ChipMax     ChipClass = "max"
	ChipUltra   ChipClass = "ultra"
)

type MachineProfile struct {
	TotalRAMBytes      int64     `json:"totalRamBytes"`
	TotalRAMGiB        int64     `json:"totalRamGiB"`
	ChipName           string    `json:"chipName"`
	ChipClass          ChipClass `json:"chipClass"`
	ChipGeneration     int       `json:"chipGeneration,omitempty"`
	MemoryBandwidthGBs int       `json:"memoryBandwidthGBs,omitempty"`
}

func CurrentMachineProfile() MachineProfile {
	return MachineProfileFor(TotalRAMBytes(), appleChipName())
}

func MachineProfileFor(totalRAMBytes int64, chipName string) MachineProfile {
	ramGiB := totalRAMBytes >> 30
	if totalRAMBytes <= 0 {
		ramGiB = 0
	}
	chipName = strings.TrimSpace(chipName)
	class := classifyChip(chipName)
	generation := chipGeneration(chipName)
	return MachineProfile{
		TotalRAMBytes:      totalRAMBytes,
		TotalRAMGiB:        ramGiB,
		ChipName:           chipName,
		ChipClass:          class,
		ChipGeneration:     generation,
		MemoryBandwidthGBs: estimateMemoryBandwidthGBs(class, generation, ramGiB),
	}
}

func appleChipName() string {
	if v := strings.TrimSpace(os.Getenv("MEADOW_MACHINE_CHIP_NAME")); v != "" {
		return v
	}
	if out, err := exec.Command("/usr/sbin/sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	if out, err := exec.Command("/usr/sbin/sysctl", "-n", "hw.model").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

func classifyChip(name string) ChipClass {
	n := strings.ToLower(strings.TrimSpace(name))
	switch {
	case strings.Contains(n, "ultra"):
		return ChipUltra
	case strings.Contains(n, "max"):
		return ChipMax
	case strings.Contains(n, "pro"):
		return ChipPro
	case strings.Contains(n, "apple m"):
		return ChipBase
	default:
		return ChipUnknown
	}
}

func chipGeneration(name string) int {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, gen := range []int{5, 4, 3, 2, 1} {
		if strings.Contains(n, fmt.Sprintf("m%d", gen)) {
			return gen
		}
	}
	return 0
}

func estimateMemoryBandwidthGBs(class ChipClass, generation int, ramGiB int64) int {
	switch class {
	case ChipUltra:
		switch generation {
		case 3:
			return 819
		case 1, 2:
			return 800
		default:
			return 800
		}
	case ChipMax:
		switch generation {
		case 5:
			if ramGiB >= 48 {
				return 614
			}
			return 460
		case 4:
			if ramGiB >= 48 {
				return 546
			}
			return 410
		case 3:
			if ramGiB == 36 || ramGiB == 96 {
				return 300
			}
			return 400
		case 1, 2:
			return 400
		default:
			return 400
		}
	case ChipPro:
		switch generation {
		case 5:
			return 307
		case 4:
			return 273
		case 3:
			return 150
		case 1, 2:
			return 200
		default:
			return 200
		}
	case ChipBase:
		switch generation {
		case 5:
			return 153
		case 4:
			return 120
		case 2, 3:
			return 100
		case 1:
			return 68
		default:
			return 100
		}
	default:
		return 0
	}
}

func allowUnsupportedModelDownloads() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ALLOW_UNSUPPORTED_MODEL_DOWNLOADS")))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(os.Getenv("MEADOW_ALLOW_UNSUPPORTED_MODEL")))
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func DownloadableModelsForCurrentMachine() []ModelInfo {
	if allowUnsupportedModelDownloads() {
		return KnownModels()
	}
	return DownloadableModelsForMachine(CurrentMachineProfile())
}

func DownloadableModelsForMachine(profile MachineProfile) []ModelInfo {
	allowed := allowedTierSet(profile)
	out := make([]ModelInfo, 0, len(releaseTiers))
	for _, t := range releaseTiers {
		if allowed[t.ServedName] {
			out = append(out, t.info())
		}
	}
	return out
}

func ModelDownloadAllowed(servedName string) (bool, string) {
	if allowUnsupportedModelDownloads() {
		return true, ""
	}
	return ModelDownloadAllowedForMachine(servedName, CurrentMachineProfile())
}

func ModelDownloadAllowedForMachine(servedName string, profile MachineProfile) (bool, string) {
	tier, ok := tierByName(servedName)
	if !ok {
		return false, fmt.Sprintf("未知模型 %q", servedName)
	}
	if allowedTierSet(profile)[tier.ServedName] {
		return true, ""
	}
	return false, fmt.Sprintf("%s 不適合這台機器下載（%s）。可下載：%s",
		tier.ServedName, profile.Summary(), strings.Join(allowedModelNames(profile), ", "))
}

func RecommendedModelForCurrentMachine() string {
	return RecommendedModelForMachine(CurrentMachineProfile())
}

func RecommendedModelForMachine(profile MachineProfile) string {
	switch {
	case allowedTierSet(profile)[tierMuse30B.ServedName]:
		return tierMuse30B.ServedName
	case allowedTierSet(profile)[tier26B.ServedName]:
		return tier26B.ServedName
	case allowedTierSet(profile)[tierLightning.ServedName]:
		return tierLightning.ServedName
	default:
		return tierLightning.ServedName
	}
}

func (p MachineProfile) Summary() string {
	chip := strings.TrimSpace(p.ChipName)
	if chip == "" {
		chip = string(p.ChipClass)
	}
	if p.MemoryBandwidthGBs > 0 {
		return fmt.Sprintf("%s, %dGB RAM, %dGB/s", chip, p.TotalRAMGiB, p.MemoryBandwidthGBs)
	}
	return fmt.Sprintf("%s, %dGB RAM", chip, p.TotalRAMGiB)
}

func allowedModelNames(profile MachineProfile) []string {
	models := DownloadableModelsForMachine(profile)
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.ServedName)
	}
	return names
}

func allowedTierSet(profile MachineProfile) map[string]bool {
	ram := profile.TotalRAMGiB
	allow := map[string]bool{}
	if ram <= 0 {
		allow[tierLightning.ServedName] = true
		return allow
	}

	if ram >= tierLightning.MinRAMGiB {
		allow[tierLightning.ServedName] = true
	}
	if ram >= tier26B.MinRAMGiB {
		allow[tier26B.ServedName] = true
	}
	if ram >= tier27B.MinRAMGiB {
		allow[tier27B.ServedName] = true
	}
	if ram >= tierMuse30B.MinRAMGiB {
		allow[tierMuse30B.ServedName] = true
	}
	if ram >= tier35B.MinRAMGiB {
		allow[tier35B.ServedName] = true
	}
	return allow
}

func envRAMBytes() (int64, bool) {
	v := strings.TrimSpace(os.Getenv("MEADOW_MACHINE_RAM_GIB"))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n << 30, true
}
