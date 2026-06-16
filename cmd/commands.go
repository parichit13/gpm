package cmd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"syscall"

	"github.com/spf13/cobra"

	"github.com/parichit13/gpm/internal/ipc"
)

var (
	flagArgs       []string
	flagEnv        []string
	flagWorkDir    string
	flagLines      int
	flagFollow     bool
	flagInstances  int
	flagPort       int
	flagHost       string
	flagMode       string
	flagHealthPath string
	flagShutdown   int
	flagPortEnv    string
	flagWatch      bool
	flagWatchEvery int
)

var rootCmd = &cobra.Command{
	Use:   "gpm",
	Short: "gpm - Go Process Manager",
	Long:  "A PM2-inspired process manager for Go (and any) binaries.",
	// Runtime failures (e.g. an aborted reload) shouldn't dump command usage;
	// main.go prints the error itself.
	SilenceUsage:  true,
	SilenceErrors: true,
}

func Execute() error {
	rootCmd.Version = Version // enables `gpm --version`
	return rootCmd.Execute()
}

func init() {
	rootCmd.AddCommand(startCmd, stopCmd, restartCmd, reloadCmd, scaleCmd, deleteCmd, listCmd, logsCmd, saveCmd, resurrectCmd, daemonCmd, statusCmd, versionCmd, updateCmd)
	updateCmd.Flags().BoolVar(&flagUpdateCheck, "check", false, "Only check whether an update is available; don't install")
	updateCmd.Flags().BoolVar(&flagUpdateForce, "force", false, "Reinstall the latest release even if already up to date")
}

// ─── start ────────────────────────────────────────────────────────────────────

var startCmd = &cobra.Command{
	Use:   "start <binary> <name>",
	Short: "Start and register a process",
	Long: "Start and register a process.\n\n" +
		"For zero-downtime reloads, give the service a --port and run N instances with -i.\n" +
		"Services that import the gpm SDK (mode=reuseport, default) share the port via\n" +
		"SO_REUSEPORT; opaque binaries can use --mode proxy.",
	Args: cobra.RangeArgs(1, 2),
	RunE: func(cmd *cobra.Command, args []string) error {
		binary := args[0]
		// Resolve to an absolute path so the daemon (different cwd) can exec it.
		if abs, err := filepath.Abs(binary); err == nil {
			binary = abs
		}
		name := filepath.Base(args[0])
		if len(args) == 2 {
			name = args[1]
		}

		// Resolve env map
		envMap := make(map[string]string)
		for _, e := range flagEnv {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		resp, err := ipc.Send(ipc.Request{
			Action:          ipc.ActionStart,
			Name:            name,
			Binary:          binary,
			Args:            flagArgs,
			Env:             envMap,
			WorkDir:         flagWorkDir,
			Instances:       flagInstances,
			Port:            flagPort,
			Host:            flagHost,
			Mode:            flagMode,
			HealthPath:      flagHealthPath,
			ShutdownTimeout: flagShutdown,
			PortEnv:         flagPortEnv,
			Watch:           flagWatch,
			WatchInterval:   flagWatchEvery,
		})
		return handleResp(resp, err)
	},
}

// ─── stop ─────────────────────────────────────────────────────────────────────

var stopCmd = &cobra.Command{
	Use:   "stop <id|name>",
	Short: "Stop a running process",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionStop, Name: args[0]})
		return handleResp(resp, err)
	},
}

// ─── restart ──────────────────────────────────────────────────────────────────

var restartCmd = &cobra.Command{
	Use:   "restart <id|name>",
	Short: "Restart a process",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionRestart, Name: args[0]})
		return handleResp(resp, err)
	},
}

// ─── reload (zero-downtime) ─────────────────────────────────────────────────

var reloadCmd = &cobra.Command{
	Use:   "reload <id|name>",
	Short: "Zero-downtime reload (rolling, one instance at a time)",
	Long: "Reload a service with no dropped connections: for each instance, start a\n" +
		"replacement, wait for it to pass its health check, then drain the old one.\n" +
		"Requires a --port; services should import the gpm SDK (or use --mode proxy).\n" +
		"If a new instance fails its health check the reload aborts and the old\n" +
		"instances keep serving.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// The daemon runs the rolling reload in the background and returns
		// immediately — draining old instances can outlast any IPC deadline.
		// Track progress with `gpm list` (old instances show as "reloading").
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionReload, Name: args[0]})
		return handleResp(resp, err)
	},
}

// ─── scale ────────────────────────────────────────────────────────────────────

var scaleCmd = &cobra.Command{
	Use:   "scale <id|name> <n>",
	Short: "Change the number of running instances",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return fmt.Errorf("instance count must be a positive integer")
		}
		resp, err := ipc.SendTimeout(ipc.Request{Action: ipc.ActionScale, Name: args[0], Replicas: n}, time.Minute)
		return handleResp(resp, err)
	},
}

// ─── delete ───────────────────────────────────────────────────────────────────

var deleteCmd = &cobra.Command{
	Use:     "delete <id|name>",
	Aliases: []string{"del", "rm"},
	Short:   "Stop and remove a process",
	Args:    cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionDelete, Name: args[0]})
		return handleResp(resp, err)
	},
}

// ─── list ─────────────────────────────────────────────────────────────────────

type ProcessInfo struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	PID       int    `json:"pid"`
	Status    string `json:"status"`
	Restarts  int    `json:"restarts"`
	Uptime    string `json:"uptime"`
	Binary    string `json:"binary"`
	Mode      string `json:"mode"`
	Port      int    `json:"port"`
	Instances int    `json:"instances"`
	Healthy   int    `json:"healthy"`
	Draining  int    `json:"draining"`
	Watch     bool   `json:"watch"`
}

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls", "status"},
	Short:   "List all processes",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionList})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}

		raw, _ := json.Marshal(resp.Data)
		var infos []ProcessInfo
		if err := json.Unmarshal(raw, &infos); err != nil {
			return err
		}

		if len(infos) == 0 {
			fmt.Println("No processes registered.")
			return nil
		}

		// The WATCH column only appears when at least one service uses it, so it
		// doesn't clutter the table for everyone else.
		showWatch := false
		for _, p := range infos {
			if p.Watch {
				showWatch = true
				break
			}
		}

		headers := []string{"ID", "NAME", "INSTANCES", "STATUS", "MODE", "PORT", "RESTARTS", "UPTIME"}
		if showWatch {
			headers = append(headers, "WATCH")
		}
		headers = append(headers, "BINARY")

		rows := make([][]string, 0, len(infos))
		for _, p := range infos {
			mode := p.Mode
			if mode == "" {
				mode = "-"
			}
			port := "-"
			if p.Port > 0 {
				port = strconv.Itoa(p.Port)
			}
			status := p.Status
			if p.Draining > 0 {
				status = fmt.Sprintf("reloading (%d draining)", p.Draining)
			}
			// Numerator counts every LIVE process — running plus any still
			// draining — so it reconciles with `ps`/Activity Monitor. During a
			// reload this exceeds the configured count (e.g. 2/1) and stays
			// elevated if a drain is stuck, which the draining note explains.
			live := p.Healthy + p.Draining
			row := []string{
				strconv.Itoa(p.ID),
				p.Name,
				fmt.Sprintf("%d/%d", live, p.Instances),
				status, // colored at render time; kept plain here for width
				mode,
				port,
				strconv.Itoa(p.Restarts),
				p.Uptime,
			}
			if showWatch {
				w := "-"
				if p.Watch {
					w = "on"
				}
				row = append(row, w)
			}
			row = append(row, p.Binary)
			rows = append(rows, row)
		}
		printTable(os.Stdout, headers, rows, 3 /* STATUS column */)
		return nil
	},
}

// printTable renders an aligned table. Column widths are measured from the
// plain text, so ANSI color codes (applied only to statusCol, and only on a
// TTY) never throw off the layout — which is what tabwriter got wrong.
func printTable(out *os.File, headers []string, rows [][]string, statusCol int) {
	const gap = 2
	n := len(headers)
	widths := make([]int, n)
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i := 0; i < n && i < len(r); i++ {
			if w := utf8.RuneCountInString(r[i]); w > widths[i] {
				widths[i] = w
			}
		}
	}

	color := isTTY(out)
	var b strings.Builder

	writeCells := func(render func(i int) string) {
		b.Reset()
		for i := 0; i < n; i++ {
			b.WriteString(render(i))
			if i < n-1 {
				b.WriteString(strings.Repeat(" ", gap))
			}
		}
		fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	}

	writeCells(func(i int) string { return padRight(headers[i], widths[i]) })
	writeCells(func(i int) string { return strings.Repeat("─", widths[i]) })
	for _, r := range rows {
		writeCells(func(i int) string {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}
			padded := padRight(cell, widths[i])
			if i == statusCol && color {
				if code := statusColor(cell); code != "" {
					return code + padded + "\033[0m"
				}
			}
			return padded
		})
	}
}

func padRight(s string, w int) string {
	if pad := w - utf8.RuneCountInString(s); pad > 0 {
		return s + strings.Repeat(" ", pad)
	}
	return s
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

var statusCmd = &cobra.Command{
	Use:   "ps",
	Short: "Alias for list",
	RunE:  listCmd.RunE,
}

// ─── logs ─────────────────────────────────────────────────────────────────────

var logsCmd = &cobra.Command{
	Use:   "logs <id|name>",
	Short: "Show process logs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionLogs, Name: args[0], LogLines: flagLines})
		if err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}

		raw, _ := json.Marshal(resp.Data)
		var paths struct {
			Out []string `json:"out"`
			Err []string `json:"err"`
		}
		json.Unmarshal(raw, &paths)

		if flagFollow {
			return tailFollow(append(paths.Out, paths.Err...))
		}
		return tailN(paths.Out, paths.Err, flagLines)
	},
}

func tailN(outPaths, errPaths []string, n int) error {
	printLastN := func(path, label string) {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		lines := readLastN(f, n)
		for _, l := range lines {
			fmt.Printf("[%s] %s\n", label, l)
		}
	}
	for _, p := range outPaths {
		printLastN(p, "stdout "+instanceLabel(p))
	}
	for _, p := range errPaths {
		printLastN(p, "stderr "+instanceLabel(p))
	}
	return nil
}

// instanceLabel extracts the instance index from a log filename like
// "api-2-out.log" -> "i2".
func instanceLabel(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, filepath.Ext(base)) // drop .log
	base = strings.TrimSuffix(base, "-out")
	base = strings.TrimSuffix(base, "-err")
	if i := strings.LastIndex(base, "-"); i >= 0 {
		return "i" + base[i+1:]
	}
	return ""
}

func tailFollow(paths []string) error {
	fmt.Printf("Tailing logs (Ctrl-C to stop)...\n\n")
	existing := []string{}
	for _, p := range paths {
		if fileExists(p) {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		fmt.Println("No log files yet.")
		return nil
	}

	// tail -f prefixes each line with the filename when given multiple files.
	args := append([]string{"-f"}, existing...)
	c := exec.Command("tail", args...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func readLastN(r io.ReadSeeker, n int) []string {
	// Read all lines, return last n
	r.Seek(0, io.SeekStart)
	var lines []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// ─── save / resurrect ─────────────────────────────────────────────────────────

var saveCmd = &cobra.Command{
	Use:   "save",
	Short: "Save current process list for resurrect",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionSave})
		return handleResp(resp, err)
	},
}

var resurrectCmd = &cobra.Command{
	Use:   "resurrect",
	Short: "Restore previously saved processes",
	RunE: func(cmd *cobra.Command, args []string) error {
		resp, err := ipc.Send(ipc.Request{Action: ipc.ActionResurrect})
		return handleResp(resp, err)
	},
}

// ─── daemon management ────────────────────────────────────────────────────────

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the gpm daemon",
}

func init() {
	// Any command that talks to the daemon will auto-start it if it's not
	// running (covers post-install and post-reboot).
	ipc.EnsureDaemon = ensureDaemonRunning

	daemonCmd.AddCommand(daemonStartCmd, daemonStopCmd, daemonStatusCmd)

	startCmd.Flags().StringArrayVarP(&flagArgs, "args", "a", nil, "Arguments to pass to the process")
	startCmd.Flags().StringArrayVarP(&flagEnv, "env", "e", nil, "Environment variables (KEY=VALUE)")
	startCmd.Flags().StringVarP(&flagWorkDir, "cwd", "d", "", "Working directory")
	startCmd.Flags().IntVarP(&flagInstances, "instances", "i", 1, "Number of instances to run (cluster mode)")
	startCmd.Flags().IntVar(&flagPort, "port", 0, "Shared listen port (enables zero-downtime reload)")
	startCmd.Flags().StringVar(&flagHost, "host", "", "Bind host (default: all interfaces)")
	startCmd.Flags().StringVar(&flagMode, "mode", "reuseport", "Reload mode: reuseport (SDK) or proxy (opaque binary)")
	startCmd.Flags().StringVar(&flagHealthPath, "health-path", "", "HTTP health path for readiness (default /healthz in reuseport mode)")
	startCmd.Flags().IntVar(&flagShutdown, "shutdown-timeout", 0, "Graceful drain budget in seconds (default 30)")
	startCmd.Flags().StringVar(&flagPortEnv, "port-env", "", "proxy mode: env var to inject the per-instance internal port")
	startCmd.Flags().BoolVarP(&flagWatch, "watch", "w", false, "Auto-reload when the binary at the run path changes")
	startCmd.Flags().IntVar(&flagWatchEvery, "watch-interval", 1, "Seconds between binary checks in watch mode")

	logsCmd.Flags().IntVarP(&flagLines, "lines", "n", 50, "Number of log lines to show")
	logsCmd.Flags().BoolVarP(&flagFollow, "follow", "f", false, "Follow log output")
}

var daemonStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the gpm daemon in the background",
	RunE: func(cmd *cobra.Command, args []string) error {
		if ipc.PingRaw() {
			fmt.Println("gpm daemon is already running.")
			return nil
		}
		pid, err := startDaemonProcess()
		if err != nil {
			return err
		}
		fmt.Printf("gpm daemon started (pid=%d)\n", pid)
		return nil
	},
}

// startDaemonProcess spawns the daemon in the background (self-re-exec into
// __daemon_run, detached, logs to ~/.gpm/daemon.log) and waits until it answers
// pings. Shared by `gpm daemon start` and the auto-start path.
func startDaemonProcess() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(gpmDir(), 0755); err != nil {
		return 0, err
	}
	logPath := filepath.Join(gpmDir(), "daemon.log")
	logF, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, err
	}

	c := exec.Command(exe, "__daemon_run")
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.Stdout = logF
	c.Stderr = logF
	if err := c.Start(); err != nil {
		return 0, fmt.Errorf("failed to start daemon: %w", err)
	}

	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 25; i++ {
		if ipc.PingRaw() {
			return c.Process.Pid, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return c.Process.Pid, fmt.Errorf("daemon started (pid=%d) but did not become ready — check %s", c.Process.Pid, logPath)
}

// ensureDaemonRunning is the auto-start hook the ipc client calls when it can't
// reach the daemon. It's a no-op if the daemon is already up.
func ensureDaemonRunning() error {
	if ipc.PingRaw() {
		return nil
	}
	_, err := startDaemonProcess()
	return err
}

// stopDaemon signals the running daemon to stop (graceful: it stops all
// services, then exits). Shared by `gpm daemon stop` and self-update.
func stopDaemon() error {
	pidFile := filepath.Join(gpmDir(), "daemon.pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("daemon not running (no pid file)")
	}
	var pid int
	fmt.Sscan(string(data), &pid)
	p, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("process not found")
	}
	if err := p.Signal(os.Interrupt); err != nil {
		return err
	}
	os.Remove(pidFile)
	return nil
}

var daemonStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the gpm daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := stopDaemon(); err != nil {
			return err
		}
		fmt.Println("gpm daemon stopped.")
		return nil
	},
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check if the daemon is running",
	RunE: func(cmd *cobra.Command, args []string) error {
		// PingRaw doesn't auto-start, so status reports the truth.
		if ipc.PingRaw() {
			fmt.Println("gpm daemon: running")
		} else {
			fmt.Println("gpm daemon: NOT running")
		}
		return nil
	},
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func handleResp(resp *ipc.Response, err error) error {
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("%s", resp.Error)
	}
	if resp.Data != nil {
		fmt.Println(resp.Data)
	} else {
		fmt.Println("OK")
	}
	return nil
}

// statusColor returns the ANSI color prefix for a status, or "" for unknown.
// The aggregate status from the daemon may carry a suffix (e.g. "running (2/3)"),
// so match on the leading word.
func statusColor(s string) string {
	switch {
	case strings.HasPrefix(s, "running"):
		return "\033[32m" // green
	case strings.HasPrefix(s, "reloading"), strings.HasPrefix(s, "draining"):
		return "\033[35m" // magenta
	case strings.HasPrefix(s, "stopped"):
		return "\033[33m" // yellow
	case strings.HasPrefix(s, "errored"):
		return "\033[31m" // red
	case strings.HasPrefix(s, "starting"):
		return "\033[36m" // cyan
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func gpmDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".gpm")
}
