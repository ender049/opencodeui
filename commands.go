package main

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	repoOwner   = "ender049"
	repoName    = "opencodeui"
	pidFile     = ".opencodeui.pid"
	stateFile   = ".opencodeui.state.json"
	logFile     = ".opencodeui.log"
	frontendDir = "dist"
)

var toolVersion = "dev"

//go:embed dist/**
var embeddedFrontend embed.FS

type serverState struct {
	PID      int    `json:"pid"`
	ListenIP string `json:"listen_ip"`
	Port     int    `json:"port"`
	Backend  string `json:"backend"`
}

var (
	backendURL string
	listenIP   string
	listenPort int
	showRemote bool
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show tool version",
	RunE:  runVersion,
}

func runVersion(_ *cobra.Command, _ []string) error {
	fmt.Printf("Tool version: %s\n", toolVersion)
	if !showRemote {
		return nil
	}

	stop := startSpinner("Checking remote version")
	defer stop()

	remote, err := getLatestReleaseTag(repoOwner, repoName)
	if err != nil {
		stop()
		fmt.Printf("Remote version: error - %v\n", err)
		return nil
	}
	if remote == "" {
		stop()
		fmt.Println("Remote version: not published")
		return nil
	}
	stop()

	fmt.Printf("Remote version: %s\n", remote)
	if remote == toolVersion {
		fmt.Println("Status: up to date ✓")
	} else {
		fmt.Println("Status: update available ↓")
	}

	return nil
}

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update tool binary",
	RunE:  runUpdate,
}

func runUpdate(_ *cobra.Command, _ []string) error {
	var runningState *serverState
	if pid, _ := readPID(); pid > 0 && isProcessRunning(pid) {
		state, err := readState()
		if err == nil {
			runningState = &state
		}
	}

	stopCheck := startSpinner("Checking latest version")

	remote, err := getLatestReleaseTag(repoOwner, repoName)
	if err != nil {
		stopCheck()
		return fmt.Errorf("failed to check remote: %w", err)
	}
	if remote == "" {
		stopCheck()
		return fmt.Errorf("no published release found")
	}
	if remote == toolVersion {
		stopCheck()
		fmt.Printf("Already at latest version: %s\n", toolVersion)
		return nil
	}
	stopCheck()

	if runningState != nil {
		ok, err := confirmStopForUpdate(runningState.PID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("update cancelled")
		}
		if err := stopProcess(runningState.PID); err != nil {
			return fmt.Errorf("failed to stop running server: %w", err)
		}
		_ = os.Remove(pidFile)
		_ = os.Remove(stateFile)
		fmt.Println("Server stopped")
	}

	stopDownload := startSpinner("Downloading update")
	resp, sourceURL, err := downloadReleaseAsset(remote, toolAssetName())
	if err != nil {
		stopDownload()
		return err
	}
	defer resp.Body.Close()
	stopDownload()
	fmt.Printf("Downloading from %s\n", sourceURL)

	selfPath, err := os.Executable()
	if err != nil {
		return err
	}

	tmpPath := selfPath + ".new"
	backupPath := selfPath + ".bak"

	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0755)
	}

	if err := os.Rename(selfPath, backupPath); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, selfPath); err != nil {
		_ = os.Rename(backupPath, selfPath)
		return err
	}
	_ = os.Remove(backupPath)

	fmt.Printf("Updated to %s ✓\n", remote)

	if runningState != nil {
		fmt.Println("Restarting server")
		pid, err := startDetached(selfPath, runningState)
		if err != nil {
			return fmt.Errorf("updated, but failed to restart server: %w", err)
		}
		runningState.PID = pid
		fmt.Printf("Server restarted on %s:%d\n", runningState.ListenIP, runningState.Port)
	}

	return nil
}

func confirmStopForUpdate(pid int) (bool, error) {
	fmt.Printf("Server is running (PID %d). Stop it and continue update? [y/N]: ", pid)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(input))
	return answer == "y" || answer == "yes", nil
}

func downloadReleaseAsset(version, asset string) (*http.Response, string, error) {
	urls := releaseAssetURLs(version, asset)
	client := &http.Client{Timeout: 10 * time.Minute}
	var errs []string

	for _, url := range urls {
		resp, err := client.Get(url)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", url, err))
			continue
		}
		if resp.StatusCode == http.StatusOK {
			return resp, url, nil
		}
		_ = resp.Body.Close()
		errs = append(errs, fmt.Sprintf("%s: HTTP %d", url, resp.StatusCode))
	}

	return nil, "", fmt.Errorf("download failed, tried all mirrors: %s", strings.Join(errs, " | "))
}

func releaseAssetURLs(version, asset string) []string {
	base := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", repoOwner, repoName, version, asset)
	return []string{
		base,
		"https://gh-proxy.com/" + base,
		"https://mirror.ghproxy.com/" + base,
		"https://ghproxy.net/" + base,
	}
}

func latestReleaseInfoURLs(owner, repo string) []string {
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	return []string{
		base,
		"https://gh-proxy.com/" + base,
		"https://mirror.ghproxy.com/" + base,
		"https://ghproxy.net/" + base,
	}
}

func toolAssetName() string {
	name := fmt.Sprintf("opencodeui-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func startSpinner(label string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticks := []rune{'|', '/', '-', '\\'}
		i := 0
		for {
			select {
			case <-stop:
				fmt.Printf("\r%s... done\n", label)
				return
			case <-time.After(150 * time.Millisecond):
				fmt.Printf("\r%s... %c", label, ticks[i%len(ticks)])
				i++
			}
		}
	}()

	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		close(stop)
		<-done
	}
}

func getLatestReleaseTag(owner, repo string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var errs []string

	for _, url := range latestReleaseInfoURLs(owner, repo) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}

		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", url, err))
			continue
		}

		if resp.StatusCode == http.StatusNotFound {
			_ = resp.Body.Close()
			return "", nil
		}
		if resp.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Sprintf("%s: HTTP %d", url, resp.StatusCode))
			_ = resp.Body.Close()
			continue
		}

		var payload struct {
			TagName string `json:"tag_name"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			_ = resp.Body.Close()
			errs = append(errs, fmt.Sprintf("%s: %v", url, err))
			continue
		}
		_ = resp.Body.Close()
		return payload.TagName, nil
	}

	return "", fmt.Errorf("failed to query latest release, tried all mirrors: %s", strings.Join(errs, " | "))
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the server",
	RunE:  runStart,
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the server",
	RunE:  runStop,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check server status",
	RunE:  runStatus,
}

var serveCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Internal: run server",
	Hidden: true,
	Run:    runServe,
}

func init() {
	versionCmd.Flags().BoolVar(&showRemote, "remote", false, "Also check remote version")
	startCmd.Flags().StringVarP(&backendURL, "backend", "b", "", "OpenCode backend URL")
	startCmd.Flags().StringVar(&listenIP, "ip", "127.0.0.1", "Listen IP")
	startCmd.Flags().IntVarP(&listenPort, "port", "p", 3000, "Listen port")

	serveCmd.Flags().StringVarP(&backendURL, "backend", "b", "", "OpenCode backend URL")
	serveCmd.Flags().StringVar(&listenIP, "ip", "127.0.0.1", "Listen IP")
	serveCmd.Flags().IntVarP(&listenPort, "port", "p", 3000, "Listen port")
}

func runStart(_ *cobra.Command, _ []string) error {
	if pid, _ := readPID(); pid > 0 && isProcessRunning(pid) {
		fmt.Printf("Server already running (PID %d)\n", pid)
		return nil
	}

	if backendURL == "" {
		backendURL = os.Getenv("OPENCODE_BACKEND")
		if backendURL == "" {
			backendURL = "127.0.0.1:4096"
		}
	}

	state := serverState{
		ListenIP: listenIP,
		Port:     listenPort,
		Backend:  backendURL,
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable: %w", err)
	}
	pid, err := startDetached(selfPath, &state)
	if err != nil {
		return err
	}

	fmt.Println("Server started")
	fmt.Printf("  PID:     %d\n", pid)
	fmt.Printf("  Listen:  %s:%d\n", state.ListenIP, state.Port)
	fmt.Printf("  Backend: %s\n", state.Backend)
	fmt.Printf("  Log:     %s\n", logFile)
	return nil
}

func startDetached(selfPath string, state *serverState) (int, error) {
	if state == nil {
		return 0, fmt.Errorf("missing server state")
	}

	logHandle, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("failed to open log file: %w", err)
	}
	defer logHandle.Close()

	nullHandle, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open devnull: %w", err)
	}
	defer nullHandle.Close()

	cmd := exec.Command(selfPath, "serve",
		"--backend", state.Backend,
		"--ip", state.ListenIP,
		"--port", fmt.Sprintf("%d", state.Port),
	)
	cmd.Stdin = nullHandle
	cmd.Stdout = logHandle
	cmd.Stderr = logHandle
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("failed to start: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()

	writePID(pid)
	state.PID = pid
	if err := writeState(*state); err != nil {
		return 0, fmt.Errorf("failed to write state: %w", err)
	}

	time.Sleep(500 * time.Millisecond)
	if !isProcessRunning(pid) {
		_ = os.Remove(pidFile)
		_ = os.Remove(stateFile)
		return 0, fmt.Errorf("server exited immediately, check %s", logFile)
	}

	return pid, nil
}

func runStop(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Server not running")
		return nil
	}
	if !isProcessRunning(pid) {
		_ = os.Remove(pidFile)
		_ = os.Remove(stateFile)
		fmt.Println("Server not running (stale pid file)")
		return nil
	}
	if err := stopProcess(pid); err != nil {
		return fmt.Errorf("failed to stop: %w", err)
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(stateFile)
	fmt.Println("Server stopped")
	return nil
}

func runStatus(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Server:  Stopped")
		return nil
	}
	if !isProcessRunning(pid) {
		_ = os.Remove(pidFile)
		_ = os.Remove(stateFile)
		fmt.Println("Server:  Stopped (stale pid file)")
		return nil
	}

	fmt.Printf("Server:  Running (PID %d)\n", pid)
	state, err := readState()
	if err == nil {
		fmt.Printf("Listen:  %s:%d\n", state.ListenIP, state.Port)
		fmt.Printf("Backend: %s\n", state.Backend)
		fmt.Printf("Log:     %s\n", logFile)
	}
	return nil
}

func writePID(pid int) {
	_ = os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644)
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	var pid int
	_, err = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid, err
}

func writeState(state serverState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(stateFile, data, 0644)
}

func readState() (serverState, error) {
	var state serverState
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal(data, &state)
	return state, err
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func stopProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !isProcessRunning(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("process %d did not exit in time", pid)
}

func runServe(_ *cobra.Command, _ []string) {
	if backendURL == "" {
		backendURL = "127.0.0.1:4096"
	}
	addr := fmt.Sprintf("%s:%d", listenIP, listenPort)

	frontendFS, err := fs.Sub(embeddedFrontend, frontendDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "embedded frontend not available: %v\n", err)
		os.Exit(1)
	}

	fileServer := http.FileServer(http.FS(frontendFS))
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = backendURL
			req.URL.Path = strings.TrimPrefix(req.URL.Path, "/api")
		},
		Transport: http.DefaultTransport,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cleanPath := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if cleanPath == "." {
			cleanPath = "index.html"
		}
		if f, err := frontendFS.Open(cleanPath); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		index, err := frontendFS.Open("index.html")
		if err != nil {
			http.Error(w, "embedded frontend unavailable", http.StatusInternalServerError)
			return
		}
		defer index.Close()

		stat, err := index.Stat()
		if err != nil {
			http.Error(w, "embedded frontend unavailable", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", stat.ModTime(), index.(io.ReadSeeker))
	})

	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "serve failed on %s: %v\n", addr, err)
		os.Exit(1)
	}
}
