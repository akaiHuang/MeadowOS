//go:build !meadow_protected

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// memSnapshot feeds the checkoutRow memory pill: how much of the machine
// the model server is holding versus everything else. Numbers in bytes.
type memSnapshot struct {
	TotalBytes int64 `json:"totalBytes"`
	ModelBytes int64 `json:"modelBytes"` // resident engine footprint (weights + runtime)
	OtherBytes int64 `json:"otherBytes"` // remaining used memory (active+wired+compressed)
}

func epPort(baseURL string) string {
	u := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	hostPort := strings.SplitN(strings.TrimSuffix(u, "/v1"), "/", 2)[0]
	if _, port, ok := strings.Cut(hostPort, ":"); ok {
		return port
	}
	return "8080"
}

var vmStatLineRe = regexp.MustCompile(`^(Pages (?:active|wired down|occupied by compressor)):\s+(\d+)`)

// memStats shells out to the standard macOS tools — sysctl/vm_stat/lsof/ps
// are always present, and this runs at pill-refresh cadence (seconds), not
// in any hot path.
func memStats(baseURL string) memSnapshot {
	snap := memSnapshot{}
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		snap.TotalBytes, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}

	// Meadow Core owns the public listener, while the resident MLX engine is a
	// child process. Counting the listener therefore reports the tiny Go Core
	// footprint (~0.1 GB) instead of the model. Prefer the authoritative engine
	// PID from /v1/status and retain the listener scan only for legacy runtimes.
	pid, meadowCore := meadowCoreResidentProcess(baseURL)
	if pid != "" {
		snap.ModelBytes = processGroupFootprintBytes(pid)
	}

	// Legacy model = phys footprint of every process listening on the endpoint
	// port. MLX keeps the weights in Metal unified memory (IOAccelerator),
	// which ps RSS does not count — a 14 GB server reports 0.2 GB RSS.
	// `footprint` includes it; fall back to RSS if the tool is missing.
	// -sTCP:LISTEN — without it lsof also lists ESTABLISHED clients
	// (this very app), which both inflates the number and, in the unload
	// path, would kill ourselves.
	if snap.ModelBytes == 0 && !meadowCore {
		out, err := exec.Command("lsof", "-ti", ":"+epPort(baseURL), "-sTCP:LISTEN").Output()
		if err == nil {
			for _, pid := range strings.Fields(string(out)) {
				snap.ModelBytes += processFootprintBytes(pid)
			}
		}
	}

	// used = (active + wired + compressor) pages; subtract the model share
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		var pages int64
		pageSize := int64(16384)
		for _, line := range strings.Split(string(out), "\n") {
			if m := vmStatLineRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				n, _ := strconv.ParseInt(strings.TrimSuffix(m[2], "."), 10, 64)
				pages += n
			}
			if strings.Contains(line, "page size of") {
				if m := regexp.MustCompile(`page size of (\d+)`).FindStringSubmatch(line); m != nil {
					pageSize, _ = strconv.ParseInt(m[1], 10, 64)
				}
			}
		}
		used := pages * pageSize
		snap.OtherBytes = used - snap.ModelBytes
		if snap.OtherBytes < 0 {
			snap.OtherBytes = 0
		}
	}
	return snap
}

func processFootprintBytes(pid string) int64 {
	if fp := footprintBytes(pid); fp > 0 {
		return fp
	}
	rss, err := exec.Command("ps", "-o", "rss=", "-p", pid).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseInt(strings.TrimSpace(string(rss)), 10, 64)
	return kb * 1024
}

// processGroupFootprintBytes includes the process guard, SGLang launcher,
// scheduler and detokenizer. Core reports the process-group leader; the MLX
// scheduler child is what owns most of the unified-memory model allocation.
func processGroupFootprintBytes(pid string) int64 {
	pgidOut, err := exec.Command("ps", "-o", "pgid=", "-p", pid).Output()
	if err != nil {
		return processFootprintBytes(pid)
	}
	pgid := strings.TrimSpace(string(pgidOut))
	processOut, err := exec.Command("ps", "-axo", "pid=,pgid=").Output()
	if err != nil || pgid == "" {
		return processFootprintBytes(pid)
	}
	pids := make([]string, 0, 4)
	for _, line := range strings.Split(string(processOut), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == pgid {
			pids = append(pids, fields[0])
		}
	}
	if len(pids) == 0 {
		return processFootprintBytes(pid)
	}
	args := []string{"--noCategories"}
	for _, childPID := range pids {
		args = append(args, "-p", childPID)
	}
	if out, err := exec.Command("footprint", args...).Output(); err == nil {
		if total := parseFootprintBytes(out); total > 0 {
			return total
		}
	}
	var total int64
	for _, childPID := range pids {
		rss, err := exec.Command("ps", "-o", "rss=", "-p", childPID).Output()
		if err != nil {
			continue
		}
		kb, _ := strconv.ParseInt(strings.TrimSpace(string(rss)), 10, 64)
		total += kb * 1024
	}
	return total
}

// meadowCoreResidentPID returns the engine PID, not the public Core listener
// PID. Non-Core/old status payloads simply fall back to the listener scan.
func meadowCoreResidentPID(baseURL string) string {
	pid, _ := meadowCoreResidentProcess(baseURL)
	return pid
}

// meadowCoreResidentProcess also reports whether the endpoint is a Meadow
// Core. An unloaded Core must return modelBytes=0 instead of falling back to
// the tiny public supervisor listener and displaying the misleading 0.1 GB.
func meadowCoreResidentProcess(baseURL string) (string, bool) {
	client := &http.Client{Timeout: 600 * time.Millisecond}
	req, err := newMeadowCoreStatusRequest(context.Background(), baseURL)
	if err != nil {
		return "", false
	}
	res, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", false
	}
	var body struct {
		RuntimeBackend string `json:"runtime_backend"`
		Model          struct {
			State string `json:"state"`
			PID   any    `json:"pid"`
		} `json:"model"`
	}
	if json.NewDecoder(res.Body).Decode(&body) != nil ||
		strings.TrimSpace(body.RuntimeBackend) == "" {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(body.Model.State), "ready") {
		return "", true
	}
	switch pid := body.Model.PID.(type) {
	case float64:
		if pid > 0 {
			return strconv.FormatInt(int64(pid), 10), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(pid)); err == nil && n > 0 {
			return strconv.Itoa(n), true
		}
	}
	return "", true
}

var footprintRe = regexp.MustCompile(`Footprint:\s+([\d.]+)\s+(KB|MB|GB)`)

func parseFootprintBytes(out []byte) int64 {
	var largest int64
	for _, m := range footprintRe.FindAllStringSubmatch(string(out), -1) {
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			continue
		}
		var bytes int64
		switch m[2] {
		case "KB":
			bytes = int64(v * 1024)
		case "MB":
			bytes = int64(v * 1024 * 1024)
		case "GB":
			bytes = int64(v * 1024 * 1024 * 1024)
		}
		// MLX model buffers are visible from multiple cooperating processes.
		// Adding every process footprint double-counts the same shared Metal
		// allocation; the largest member is the scheduler and best represents
		// the resident model footprint.
		if bytes > largest {
			largest = bytes
		}
	}
	return largest
}

// footprintBytes parses the header line of `footprint -p <pid>`:
//
//	Python [123]: 64-bit    Footprint: 14 GB (16384 bytes per page)
func footprintBytes(pid string) int64 {
	out, err := exec.Command("footprint", "-p", pid).Output()
	if err != nil {
		return 0
	}
	return parseFootprintBytes(out)
}
