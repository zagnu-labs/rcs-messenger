// androidprobe drives the Android sender from a terminal, without the server
// or the database. Use it to check a phone and to calibrate the selectors.
//
//	androidprobe health
//	androidprobe dump  [+15551234567]     # what the screen says, node by node
//	androidprobe check +15551234567       # opens the chat, reports encrypted / plain RCS / SMS
//	androidprobe send  +15551234567 "text"
//
// Settings come from the same RCS_ANDROID_* variables as the server.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsandroid"
	"github.com/zagnu-labs/rcs-messenger/internal/providers/rcsdirect"
)

func env(k, d string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return d
}

func secs(k string, d int) time.Duration {
	n, err := strconv.Atoi(env(k, ""))
	if err != nil {
		n = d
	}
	return time.Duration(n) * time.Second
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: androidprobe health | dump [number] | check <number> | send <number> <text>")
		os.Exit(2)
	}
	sel, err := rcsandroid.LoadSelectors(env("RCS_ANDROID_SELECTORS", ""))
	fatal(err)
	runner := rcsandroid.ExecRunner{Bin: env("RCS_ANDROID_ADB", "adb"), Serial: env("RCS_ANDROID_SERIAL", "")}
	s, err := rcsandroid.New(rcsandroid.Config{
		Runner: runner, Selectors: sel, PIN: env("RCS_ANDROID_PIN", ""), Addr: env("RCS_ANDROID_ADDR", ""),
		MinGap: secs("RCS_ANDROID_MIN_GAP_SECONDS", 0), SettleBudget: secs("RCS_ANDROID_SETTLE_SECONDS", 60),
	})
	fatal(err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch os.Args[1] {
	case "health":
		h, err := s.Health(ctx)
		fatal(err)
		out(h)
		fmt.Println("ready:", h.Ready())
	case "dump":
		dev, err := rcsandroid.NewDevice(runner, sel, env("RCS_ANDROID_PIN", ""))
		fatal(err)
		if len(os.Args) > 2 {
			fatal(dev.Awake(ctx))
			fatal(dev.OpenChat(ctx, os.Args[2]))
			time.Sleep(3 * time.Second)
		}
		scr, err := dev.Dump(ctx)
		fatal(err)
		for _, n := range scr.Nodes {
			if n.ID() == "" && n.Text == "" && n.Desc == "" {
				continue
			}
			fmt.Printf("%-34s id=%-30q text=%-30q desc=%q\n", short(n.Class), n.ID(), n.Text, n.Desc)
		}
	case "check":
		need(3)
		c, err := s.Check(ctx, os.Args[2])
		fatal(err)
		out(c)
		fmt.Printf("\n=> %s  (eligible to send: %v %s)\n", verdict(c), c.Eligible, c.Reason)
	case "send":
		need(4)
		r, err := s.Send(ctx, os.Args[2], os.Args[3], rcsdirect.SendOptions{Wait: 30 * time.Second})
		if err != nil {
			fmt.Println("REFUSED / ERROR:", err)
			os.Exit(1)
		}
		out(r)
		fmt.Printf("\n=> sent=%v status=%q type=%s after=%s\n", r.Sent, r.FinalStatus, r.Transport(), verdict(r.After))
	default:
		fatal(fmt.Errorf("unknown command %q", os.Args[1]))
	}
}

func verdict(c *rcsdirect.Check) string {
	switch {
	case c == nil:
		return "unknown"
	case c.E2EE == "on":
		return "ENCRYPTED RCS"
	case c.Transport == "RCS":
		return "PLAIN RCS (not encrypted)"
	case c.Transport == "SMS":
		return "SMS"
	}
	return "UNREADABLE (check the selectors)"
}

func short(s string) string { return s[strings.LastIndex(s, ".")+1:] }

func need(n int) {
	if len(os.Args) < n {
		fatal(fmt.Errorf("missing arguments"))
	}
}

func out(v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(b))
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
