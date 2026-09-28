// Package android wraps the adb operations needed to enumerate attached
// devices and pull their SMS store. It is factored so the TUI and the
// flag-only entrypoint share the same plumbing.
package android

import (
	"bufio"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// Device is a single entry from `adb devices`.
type Device struct {
	Serial       string // e.g. "emulator-5554" or "RF8N30XXXXX"
	State        string // "device", "unauthorized", "offline", ...
	Manufacturer string // e.g. "Google", "samsung" (best-effort)
	Model        string // e.g. "Pixel 7", "SM-G990B" (best-effort)
	Marketing    string // e.g. "Galaxy S21 5G" (best-effort, may be empty)
}

// Name returns a human-friendly device name, or "" if none could be resolved.
func (d Device) Name() string {
	if d.Marketing != "" {
		return d.Marketing
	}
	parts := make([]string, 0, 2)
	if m := titleFirst(d.Manufacturer); m != "" {
		parts = append(parts, m)
	}
	if d.Model != "" {
		parts = append(parts, d.Model)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Label renders the device for a picker list.
func (d Device) Label() string {
	label := d.Serial
	if name := d.Name(); name != "" {
		label = fmt.Sprintf("%s — %s", name, d.Serial)
	}
	if d.State != "device" {
		label = fmt.Sprintf("%s (%s)", label, d.State)
	}
	return label
}

// Ready reports whether the device is usable (authorized and online).
func (d Device) Ready() bool { return d.State == "device" }

// Toolchain locates the external binaries we depend on.
type Toolchain struct {
	ADB string // path to adb
}

// Discover resolves adb from PATH.
func Discover() (*Toolchain, error) {
	adb, err := exec.LookPath("adb")
	if err != nil {
		return nil, fmt.Errorf("adb not found on PATH (install platform-tools)")
	}
	return &Toolchain{ADB: adb}, nil
}

// Devices lists attached devices via `adb devices`.
func (t *Toolchain) Devices() ([]Device, error) {
	out, err := t.run(t.ADB, "devices")
	if err != nil {
		return nil, err
	}
	var devices []Device
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "List of devices") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		devices = append(devices, Device{Serial: fields[0], State: fields[1]})
	}
	// Enrich ready devices with a friendly name (best-effort; ignore errors).
	for i := range devices {
		if devices[i].Ready() {
			t.fillDeviceInfo(&devices[i])
		}
	}
	return devices, nil
}

// fillDeviceInfo reads make/model props off the device. Failures are ignored
// so an odd device still shows up by serial.
func (t *Toolchain) fillDeviceInfo(d *Device) {
	// One shell round-trip; getprop prints an empty line for missing props.
	out, err := t.run(t.ADB, t.deviceArgs(d.Serial, "shell",
		"getprop ro.config.marketing_name; getprop ro.product.manufacturer; getprop ro.product.model")...)
	if err != nil {
		return
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	d.Marketing = get(0)
	d.Manufacturer = get(1)
	d.Model = get(2)
}

// deviceArgs prepends `-s <serial>` when a serial is set.
func (t *Toolchain) deviceArgs(serial string, args ...string) []string {
	if serial == "" {
		return args
	}
	return append([]string{"-s", serial}, args...)
}

func (t *Toolchain) run(bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); len(ee.Stderr) > 0 && ok {
			return "", fmt.Errorf("%s: %s", filepath.Base(bin), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", filepath.Base(bin), err)
	}
	return strings.ReplaceAll(string(out), "\r", ""), nil
}

// titleFirst upper-cases the first rune (e.g. "samsung" -> "Samsung").
func titleFirst(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
