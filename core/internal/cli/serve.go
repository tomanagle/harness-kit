package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/toolhost"
	"github.com/tomanagle/harness-kit/core/tools"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

func (a app) serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	rf := runtimeFlags{version: a.version}
	rf.register(fs)
	allowUnsafe := fs.Bool("allow-unsafe", false, "accept session options that disable runtime sandboxes or approvals")
	approvalTimeout := fs.Duration("approval-timeout", 0, "deny approvals not answered within this duration (0 = wait indefinitely)")
	logSize := fs.Int("event-log-size", 2048, "events retained per session for replay")
	eventBuffer := fs.Int("event-buffer", 4096, "events queued for a slow client before they are dropped (reported as stream.gap)")
	logLevel := fs.String("log-level", "info", "stderr log level: debug, info, warn, error")
	storeDir := fs.String("store", "", `persist session records in this directory; "default" shares the CLI's history (default: in memory)`)
	kitMCP := fs.Bool("kit-mcp", false, "merge MCP servers from the user config and trusted project harness-kit.json into every session")
	settingsFile := fs.String("settings", "", "initial settings JSON file (explicit flags override the file; live updates use settings.configure)")
	settingsJSON := fs.String("settings-json", "", "initial settings JSON document (used by the owning SDK transport)")
	fs.Bool("stdio", true, "serve on stdin/stdout (the only transport)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	initial := settings.Defaults()
	initial.Process = settings.Process{Store: *storeDir, Runtimes: strings.Split(rf.runtimes, ","), CodexPath: rf.codexPath, ClaudePath: rf.claudePath, GitPath: rf.gitPath, FakeDelayMs: rf.fakeDelay.Milliseconds(), KitMCP: *kitMCP, AllowUnsafe: *allowUnsafe, ShutdownTimeoutMs: 10000}
	if rf.fake {
		initial.Process.Runtimes = append(initial.Process.Runtimes, "fake")
	}
	initial.Logging.Level = *logLevel
	initial.Approvals.TimeoutMs = approvalTimeout.Milliseconds()
	initial.Memory.EventLogSize = *logSize
	initial.Streams.MaxQueueEvents = *eventBuffer
	if *settingsFile != "" && *settingsJSON != "" {
		return errors.New("use only one of --settings and --settings-json")
	}
	var err error
	if *settingsJSON != "" {
		flagValues := initial
		initial, err = settings.Decode(strings.NewReader(*settingsJSON), initial)
		if err == nil {
			initial, err = overrideServeFlags(fs, initial, flagValues)
		}
	} else {
		initial, err = loadServeSettings(*settingsFile, fs, initial)
	}
	if err != nil {
		return err
	}
	var settingsStore settings.Store
	if initial.Process.SettingsPath != "" {
		path := initial.Process.SettingsPath
		settingsStore = settings.FileStore{Path: path}
		saved, loadErr := settings.Load(context.Background(), settingsStore, initial)
		if loadErr != nil {
			return fmt.Errorf("load saved settings: %w", loadErr)
		}
		flagValues := initial
		if *settingsJSON != "" {
			saved, err = settings.Decode(strings.NewReader(*settingsJSON), saved)
		} else if *settingsFile != "" {
			file, openErr := os.Open(*settingsFile)
			if openErr != nil {
				return openErr
			}
			saved, err = settings.Decode(file, saved)
			_ = file.Close()
		}
		if err != nil {
			return err
		}
		initial, err = overrideServeFlags(fs, saved, flagValues)
		if err != nil {
			return err
		}
		initial.Process.SettingsPath = path
	}
	rf.runtimes, rf.fake = strings.Join(initial.Process.Runtimes, ","), false
	rf.acp = initial.Process.ACP
	rf.codexPath, rf.claudePath = initial.Process.CodexPath, initial.Process.ClaudePath
	rf.gitPath = initial.Process.GitPath
	rf.fakeDelay = time.Duration(initial.Process.FakeDelayMs) * time.Millisecond
	*storeDir, *kitMCP, *allowUnsafe = initial.Process.Store, initial.Process.KitMCP, initial.Process.AllowUnsafe
	*logSize, *eventBuffer = initial.Memory.EventLogSize, initial.Streams.MaxQueueEvents
	*approvalTimeout = time.Duration(initial.Approvals.TimeoutMs) * time.Millisecond
	var level slog.Level
	if err := level.UnmarshalText([]byte(initial.Logging.Level)); err != nil {
		return err
	}
	logLevelVar := new(slog.LevelVar)
	logLevelVar.Set(level)
	logs := logging.New(logLevelVar, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelVar}), logging.Config{
		MaxQueueRecords: initial.Logging.MaxQueueRecords, MaxQueueBytes: initial.Logging.MaxQueueBytes, MaxRecordBytes: initial.Logging.MaxRecordBytes,
	})
	defer logs.Close()
	log := slog.New(logs)

	reg, err := rf.registry()
	if err != nil {
		return err
	}
	store, err := openStore(*storeDir)
	if err != nil {
		return err
	}
	// Host tools: apps register them over RPC (tools.register); runtimes
	// reach them, and Claude Code's permission prompts reach the client,
	// through the MCP bridge.
	registry := tools.NewRegistry()
	host := toolhost.New(toolhost.Options{Registry: registry, Version: a.version, Logger: log})
	defer host.Close()
	var prepare func(context.Context, *session.CreateParams) ([]string, error)
	var resolver server.MCPResolver
	if *kitMCP {
		r, err := kitResolver()
		if err != nil {
			return err
		}
		prepare, resolver = kitPrepare(r), &r
	}
	mgr := session.NewManager(reg, session.Options{
		SettingsStore:   settingsStore,
		Workspace:       gitworkspace.New(rf.gitPath),
		ToolServer:      host,
		Prepare:         prepare,
		Store:           store,
		AllowUnsafe:     *allowUnsafe,
		ApprovalTimeout: *approvalTimeout,
		EventLogSize:    *logSize,
		Logger:          log,
		LogLevel:        logLevelVar,
		Logs:            logs,
	})
	if err := mgr.InitializeSettings(context.Background(), initial); err != nil {
		return err
	}
	srv := server.New(mgr, server.Options{
		MCP:          resolver,
		Tools:        registry,
		Version:      a.version,
		EventBuffer:  *eventBuffer,
		AllowUnsafe:  *allowUnsafe,
		EventLogSize: *logSize,
		Logger:       log,
	})

	ignoreSIGPIPE()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Info("signal received; shutting down", "signal", sig.String())
		os.Stdin.Close() // ends Serve
	}()

	log.Info("harness-kit serving on stdio", "version", a.version, "runtimes", rf.runtimes, "fake", rf.fake)
	serveErr := srv.Serve(context.Background(), os.Stdin, os.Stdout)
	if errors.Is(serveErr, os.ErrClosed) || errors.Is(serveErr, io.ErrClosedPipe) {
		serveErr = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	log.Info("harness-kit exited")
	return serveErr
}
