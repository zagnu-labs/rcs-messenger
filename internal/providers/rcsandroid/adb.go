package rcsandroid

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode"
)

// Runner runs one adb command and returns what it printed. It is the only
// thing that touches the machine, so tests replace it.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// ExecRunner runs the real adb binary, optionally against one device.
type ExecRunner struct {
	Bin    string // default "adb"
	Serial string // "-s <serial>"; for Wi-Fi this is "host:port"
}

func (r ExecRunner) Run(ctx context.Context, args ...string) ([]byte, error) {
	bin := r.Bin
	if bin == "" {
		bin = "adb"
	}
	if r.Serial != "" {
		args = append([]string{"-s", r.Serial}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.Bytes(), fmt.Errorf("adb %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return out.Bytes(), nil
}

// Device drives one Android phone through adb.
type Device struct {
	r   Runner
	m   *matcher
	pin string // optional screen lock PIN, digits only
}

func NewDevice(r Runner, sel Selectors, pin string) (*Device, error) {
	m, err := newMatcher(sel)
	if err != nil {
		return nil, err
	}
	return &Device{r: r, m: m, pin: pin}, nil
}

func (d *Device) sh(ctx context.Context, cmd string) (string, error) {
	out, err := d.r.Run(ctx, "shell", cmd)
	return strings.TrimSpace(string(out)), err
}

// shQuote single-quotes s for the phone's shell (adb joins shell arguments
// with spaces and the phone's shell re-parses them).
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ErrOffline means adb cannot see the phone.
var ErrOffline = errors.New("the phone is not connected over adb")

// State is `adb get-state`: "device" when the phone is reachable.
func (d *Device) State(ctx context.Context) (string, error) {
	out, err := d.r.Run(ctx, "get-state")
	state := strings.TrimSpace(string(out))
	if err != nil || state != "device" {
		if state == "" {
			state = "offline"
		}
		return state, fmt.Errorf("%w (%s)", ErrOffline, state)
	}
	return state, nil
}

// Reconnect nudges adb to find the phone again: for a Wi-Fi phone it
// reconnects to addr, for USB it restarts the host's view of the device.
func (d *Device) Reconnect(ctx context.Context, addr string) error {
	if addr != "" {
		if _, err := d.r.Run(ctx, "connect", addr); err != nil {
			return err
		}
	} else if _, err := d.r.Run(ctx, "reconnect"); err != nil {
		return err
	}
	// adb needs a moment to see the phone again after a restart.
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, _ = d.r.Run(wctx, "wait-for-device")
	_, err := d.State(ctx)
	return err
}

// Awake wakes the screen and gets past the lock screen. A phone that needs
// a PIN is only unlocked when one was configured; otherwise it is reported
// locked, never guessed at.
func (d *Device) Awake(ctx context.Context) error {
	if d.unlocked(ctx) {
		return nil
	}
	_, _ = d.sh(ctx, "input keyevent KEYCODE_WAKEUP")
	pause(ctx, time.Second)
	_, _ = d.sh(ctx, "wm dismiss-keyguard")
	pause(ctx, time.Second)
	if d.unlocked(ctx) {
		return nil
	}
	_, _ = d.sh(ctx, "input swipe 540 1800 540 600 200") // lift the lock screen to the PIN pad
	pause(ctx, time.Second)
	if d.pin != "" {
		_, _ = d.sh(ctx, "input text "+shQuote(d.pin))
		_, _ = d.sh(ctx, "input keyevent KEYCODE_ENTER")
		pause(ctx, 2*time.Second)
	}
	if !d.unlocked(ctx) {
		return errors.New(ReasonPhoneLocked)
	}
	return nil
}

func (d *Device) unlocked(ctx context.Context) bool {
	out, err := d.sh(ctx, "dumpsys window | grep -E 'isKeyguardShowing|mShowingLockscreen'; dumpsys power | grep -m1 mWakefulness=")
	if err != nil {
		return false
	}
	return strings.Contains(out, "mWakefulness=Awake") &&
		!strings.Contains(out, "isKeyguardShowing=true") && !strings.Contains(out, "mShowingLockscreen=true")
}

// pause waits d or until ctx ends.
func pause(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// KeepAwake makes the phone stay on while plugged in, so the screen is there
// to read.
func (d *Device) KeepAwake(ctx context.Context) error {
	_, err := d.sh(ctx, "settings put global stay_on_while_plugged_in 7")
	return err
}

// OpenChat opens the Messages conversation for number, starting from a clean
// task so a thread left open earlier cannot be mistaken for this one.
func (d *Device) OpenChat(ctx context.Context, number string) error {
	out, err := d.sh(ctx, "am start -W -f 0x10008000 -a android.intent.action.SENDTO -d "+
		shQuote("sms:"+number)+" -p "+shQuote(d.m.Package))
	if err != nil {
		return err
	}
	if strings.Contains(out, "Error") || strings.Contains(out, "does not exist") {
		return fmt.Errorf("could not open Google Messages: %s", out)
	}
	return nil
}

// Dump reads the screen.
func (d *Device) Dump(ctx context.Context) (*Screen, error) {
	out, err := d.r.Run(ctx, "exec-out", "uiautomator", "dump", "/dev/tty")
	if err == nil {
		if s, perr := ParseScreen(out); perr == nil {
			return s, nil
		}
	}
	// Some builds refuse /dev/tty: dump to a file and read it back.
	if _, err2 := d.sh(ctx, "uiautomator dump /sdcard/rcs-window.xml"); err2 != nil {
		if err != nil {
			return nil, err
		}
		return nil, err2
	}
	raw, err := d.r.Run(ctx, "exec-out", "cat", "/sdcard/rcs-window.xml")
	if err != nil {
		return nil, err
	}
	return ParseScreen(raw)
}

func (d *Device) Tap(ctx context.Context, x, y int) error {
	_, err := d.sh(ctx, fmt.Sprintf("input tap %d %d", x, y))
	return err
}

func (d *Device) Key(ctx context.Context, code string) error {
	_, err := d.sh(ctx, "input keyevent "+code)
	return err
}

// ClearCompose empties the message box (focus it first).
func (d *Device) ClearCompose(ctx context.Context) error {
	_, _ = d.sh(ctx, "input keycombination 113 29") // ctrl+A
	return d.Key(ctx, "KEYCODE_DEL")
}

// typeable reports whether text can be typed with `input text`: printable
// ASCII and newlines. `%s` is how `input text` spells a space, so it cannot
// be typed literally.
func typeable(text string) bool {
	if strings.Contains(text, "%s") {
		return false
	}
	for _, r := range text {
		if r == '\n' {
			continue
		}
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// TypeText types text into the focused box. Lines are separated by the
// Enter key, which Google Messages treats as a new line.
func (d *Device) TypeText(ctx context.Context, text string) error {
	if !typeable(text) {
		return errors.New(ReasonUnsupported)
	}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if i > 0 {
			if err := d.Key(ctx, "KEYCODE_ENTER"); err != nil {
				return err
			}
		}
		// Short chunks keep each adb call quick and let a failure show early.
		for len(line) > 0 {
			n := min(len(line), 48)
			chunk := strings.ReplaceAll(line[:n], " ", "%s")
			line = line[n:]
			if _, err := d.sh(ctx, "input text "+shQuote(chunk)); err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(30 * time.Millisecond):
			}
		}
	}
	return nil
}

// Info reads facts about the phone for the health check.
type Info struct {
	Model         string
	Android       string
	Carrier       string
	SimReady      bool
	MessagesFound bool
	DefaultSMS    bool
	Battery       string
}

func (d *Device) Info(ctx context.Context) (Info, error) {
	var in Info
	get := func(cmd string) string { out, _ := d.sh(ctx, cmd); return out }
	in.Model = get("getprop ro.product.model")
	in.Android = get("getprop ro.build.version.release")
	in.Carrier = get("getprop gsm.operator.alpha")
	// "READY" on phones, "LOADED" on the emulator; two SIMs read "READY,ABSENT" etc.
	sim := get("getprop gsm.sim.state")
	in.SimReady = strings.Contains(sim, "READY") || strings.Contains(sim, "LOADED")
	in.MessagesFound = strings.Contains(get("pm list packages "+d.m.Package), d.m.Package)
	// The SMS role is the source of truth; the old setting can read "null"
	// even when Messages holds the role.
	in.DefaultSMS = strings.Contains(get("cmd role get-role-holders --user 0 android.app.role.SMS"), d.m.Package) ||
		strings.TrimSpace(get("settings get secure sms_default_application")) == d.m.Package
	in.Battery = strings.TrimSpace(get("dumpsys battery | grep -m1 level"))
	if in.Model == "" {
		return in, ErrOffline
	}
	return in, nil
}
