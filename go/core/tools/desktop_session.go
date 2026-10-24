package tools

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// DesktopSession is an immutable host-supplied capability. Model arguments
// cannot select a display, session bus, helper executable or user profile.
// One OS Agent may own many chats, but they operate its one visible desktop.
type DesktopSession struct {
	Display    string
	SessionID  string
	HelperPath string
	PythonPath string
	env        []string
}

var localDesktopDisplay = regexp.MustCompile(`^:[0-9]+(?:\.[0-9]+)?$`)
var desktopLocks sync.Map

// DesktopSessionFromEnvironment captures only named desktop/runtime fields.
// It must be called by the explicit OS host, never inferred from tool requests
// or an inventory of unrelated X11 sockets.
func DesktopSessionFromEnvironment() (*DesktopSession, error) {
	display := os.Getenv("DISPLAY")
	if !localDesktopDisplay.MatchString(display) {
		return nil, fmt.Errorf("a local bound DISPLAY is required for MeadowOS desktop control")
	}
	prefix := strings.TrimSpace(os.Getenv("MEADOW_PREFIX"))
	if prefix == "" {
		executable, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("desktop helper prefix: %w", err)
		}
		prefix = filepath.Dir(filepath.Dir(executable))
	}
	if !filepath.IsAbs(prefix) {
		return nil, fmt.Errorf("desktop helper prefix must be absolute")
	}
	d := &DesktopSession{Display: display,
		HelperPath: filepath.Join(prefix, "share", "meadow-os", "desktop", "desktop_control.py"),
		PythonPath: "/usr/bin/python3"}
	// The identifier exposes no session-bus address or authentication material.
	identity := fmt.Sprintf("%d\x00%s\x00%s\x00%s", os.Geteuid(), display,
		os.Getenv("XDG_RUNTIME_DIR"), os.Getenv("DBUS_SESSION_BUS_ADDRESS"))
	digest := sha256.Sum256([]byte(identity))
	d.SessionID = fmt.Sprintf("desktop-%x", digest[:12])
	for _, key := range []string{
		"PATH", "LANG", "LC_ALL", "LC_CTYPE", "USER", "LOGNAME", "HOME", "TMPDIR",
		"DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "AT_SPI_BUS_ADDRESS", "XDG_RUNTIME_DIR",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_DATA_DIRS", "XDG_CONFIG_DIRS",
		"XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "GTK_THEME", "GTK_PATH", "GTK_MODULES",
		"XCURSOR_THEME", "XCURSOR_SIZE", "XCURSOR_PATH", "MEADOW_PREFIX",
		"MEADOW_OS_SESSION", "MEADOW_GLASS_BRIDGE_PATH", "MEADOW_QUICKLOOK_HELPER",
		"THUNARX_DIRS", "LD_LIBRARY_PATH", "LD_PRELOAD", "PYTHONPATH",
	} {
		if value := os.Getenv(key); value != "" && !strings.ContainsRune(value, '\x00') {
			d.env = append(d.env, key+"="+value)
		}
	}
	// Xlib normally derives this from HOME. Shell commands retain their private
	// scratch HOME, so preserve the selected user's default X authority by path
	// when the graphical launcher did not export an explicit location.
	if os.Getenv("XAUTHORITY") == "" && filepath.IsAbs(os.Getenv("HOME")) {
		authority := filepath.Join(os.Getenv("HOME"), ".Xauthority")
		if info, err := os.Stat(authority); err == nil && info.Mode().IsRegular() {
			d.env = append(d.env, "XAUTHORITY="+authority)
		}
	}
	d.env = append(d.env, "MEADOW_DESKTOP_SESSION_ID="+d.SessionID)
	return d, nil
}

func (d *DesktopSession) Environment() []string {
	if d == nil {
		return nil
	}
	return append([]string(nil), d.env...)
}

// Lock serializes native input and its resulting observation across all chats
// and workers in this Agent that share a display. It is not a semantic task
// planner, nor a reservation across multiple calls or other OS processes.
func (d *DesktopSession) Lock() {
	lock, _ := desktopLocks.LoadOrStore(strings.SplitN(d.Display, ".", 2)[0], &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
}

func (d *DesktopSession) Unlock() {
	lock, _ := desktopLocks.LoadOrStore(strings.SplitN(d.Display, ".", 2)[0], &sync.Mutex{})
	lock.(*sync.Mutex).Unlock()
}

// shellEnvironment preserves the existing shell's scratch HOME/cache boundary;
// application launches through desktop use the actual desktop profile instead.
func (d *DesktopSession) shellEnvironment() []string {
	var result []string
	for _, entry := range d.Environment() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "AT_SPI_BUS_ADDRESS", "XDG_RUNTIME_DIR",
			"XDG_SESSION_TYPE", "XDG_CURRENT_DESKTOP", "MEADOW_DESKTOP_SESSION_ID":
			result = append(result, entry)
		}
	}
	return result
}

func (w *Workspace) SetDesktopSession(desktop *DesktopSession) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.desktop = desktop
}

func (w *Workspace) DesktopSession() *DesktopSession {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.desktop
}
