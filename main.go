package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lyingloong/gpuglint/internal/collect"
	"github.com/lyingloong/gpuglint/internal/hosts"
	"github.com/lyingloong/gpuglint/internal/state"
)

const usage = `GPU Glint — GPU monitoring over SSH

Usage: gpuglint <command> [options]

  hosts   List selected SSH aliases without connecting
  scan    Poll selected machines once
  watch   Poll continuously and maintain a live state file
  status  Read the running monitor's snapshot without connecting

Use gpuglint <command> -h for options.`

type patterns []string

func (p *patterns) String() string { return strings.Join(*p, ", ") }
func (p *patterns) Set(s string) error {
	if s == "" {
		return errors.New("empty host selector")
	}
	*p = append(*p, s)
	return nil
}

type options struct {
	command, config, path, order string
	include, exclude             patterns
	interval, timeout            time.Duration
	concurrency                  int
	json, headless, follow       bool
}

func expandHome(s string) (string, error) {
	if s == "~" || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimLeft(s[1:], `/\`)), nil
	}
	return s, nil
}

func parse(args []string, stderr io.Writer) (options, error) {
	o := options{command: args[0]}
	fs := flag.NewFlagSet(o.command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	switch o.command {
	case "hosts", "scan", "watch", "status":
	default:
		return o, fmt.Errorf("unknown command %q", o.command)
	}
	if o.command != "status" {
		fs.StringVar(&o.config, "ssh-config", "", "OpenSSH configuration file (default ~/.ssh/config)")
		fs.Var(&o.include, "host", "alias or pattern to include (repeatable)")
		fs.Var(&o.exclude, "exclude", "alias or pattern to exclude (repeatable)")
	}
	if o.command == "scan" || o.command == "watch" {
		fs.IntVar(&o.concurrency, "concurrency", 4, "maximum concurrent SSH polls")
		fs.DurationVar(&o.timeout, "timeout", 20*time.Second, "timeout for an entire host poll")
	}
	if o.command == "watch" {
		fs.DurationVar(&o.interval, "interval", 30*time.Second, "time between polls of the same host")
		fs.BoolVar(&o.headless, "headless", false, "collect without drawing a dashboard")
	}
	if o.command == "watch" || o.command == "status" {
		fs.StringVar(&o.path, "state-file", "~/.ssh/.gpuglint/state", "live JSON state file")
	}
	if o.command != "hosts" {
		fs.StringVar(&o.order, "sort", "host", "row order: host or free (most free memory first)")
	}
	if o.command != "watch" {
		fs.BoolVar(&o.json, "json", false, "write machine-readable JSON to stdout")
	}
	if o.command == "status" {
		fs.BoolVar(&o.follow, "follow", false, "view the running monitor's dashboard until it stops")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if o.follow && o.json {
		return o, errors.New("use --follow for a dashboard or --json for a snapshot")
	}
	if o.order != "" && o.order != "host" && o.order != "free" {
		return o, errors.New("--sort must be host or free")
	}
	if (o.command == "watch" || o.command == "scan") && (o.timeout <= 0 || o.concurrency < 1) {
		return o, errors.New("--timeout and --concurrency must be positive")
	}
	if o.command == "watch" && o.interval <= 0 {
		return o, errors.New("--interval must be positive")
	}
	var err error
	if o.config, err = expandHome(o.config); err != nil {
		return o, err
	}
	o.path, err = expandHome(o.path)
	return o, err
}

func run(ctx context.Context, args []string, out, stderr io.Writer, poll pollFunc) (int, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprintln(out, usage)
		return 0, err
	}
	o, err := parse(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0, nil
	}
	if err != nil {
		return 1, err
	}
	if o.command == "status" {
		data, err := state.Read(o.path)
		if errors.Is(err, state.ErrInactive) {
			return 3, err
		}
		if err != nil {
			return 1, err
		}
		if o.follow {
			return 0, follow(ctx, out, data, o)
		}
		return 0, displayState(out, data, o)
	}
	names, err := hosts.Select(o.config, o.include, o.exclude)
	if err != nil {
		return 1, err
	}
	if o.command == "hosts" {
		if o.json {
			return 0, writeJSON(out, append([]string{}, names...))
		}
		for _, n := range names {
			if _, err := fmt.Fprintln(out, n); err != nil {
				return 1, err
			}
		}
		return 0, nil
	}
	if len(names) == 0 {
		return 1, errors.New("no SSH hosts selected (see gpuglint hosts or pass --host ALIAS)")
	}
	if o.command == "watch" {
		return 0, watch(ctx, out, o, names, poll)
	}
	return scan(ctx, out, o, names, poll)
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code, err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, collect.Poll)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gpuglint:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}
