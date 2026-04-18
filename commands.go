package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

const (
	repoOwner       = "ender049"
	repoName        = "ocgo"
	opencodeOwner   = "anomalyco"
	opencodeRepo    = "opencode"
	pidFile         = ".ocgo.pid"
	stateFile       = ".ocgo.state.json"
	frontendDir     = "dist"
	shutdownTTL     = 5 * time.Second
	opencodeBinName = "opencode"
)

const (
	defaultListenIP           = "127.0.0.1"
	defaultListenPort         = 3000
	defaultOpencodeListenIP   = "127.0.0.1"
	defaultOpencodeListenPort = 4096
)

var toolVersion = "dev"
var showRemote bool
var updateOCVersion string

//go:embed dist/**
var embeddedFrontend embed.FS

type serverState struct {
	PID                int    `json:"pid"`
	ListenIP           string `json:"listen_ip"`
	Port               int    `json:"port"`
	Backend            string `json:"backend"`
	ManageOpencode     bool   `json:"manage_opencode,omitempty"`
	OpencodeBinary     string `json:"opencode_binary,omitempty"`
	OpencodeListenIP   string `json:"opencode_listen_ip,omitempty"`
	OpencodePort       int    `json:"opencode_port,omitempty"`
	OpencodeProjectDir string `json:"opencode_project_dir,omitempty"`
	OpencodePID        int    `json:"opencode_pid,omitempty"`
}

type commandRunOptions struct {
	BackendURL         string
	ListenIP           string
	ListenPort         int
	Foreground         bool
	ExternalBackend    bool
	ManageOpencode     bool
	ManageOpencodeSet  bool
	OpencodeBinary     string
	OpencodeListenIP   string
	OpencodeListenPort int
	OpencodeProjectDir string
}

type preparedToolUpdate struct {
	TargetPath string
	TempPath   string
	Version    string
}

type preparedOpencodeUpdate struct {
	TargetPath  string
	ArchivePath string
	Version     string
	Existing    bool
	Skip        bool
}

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
	Short: "Update tool and local opencode",
	RunE:  runUpdate,
}

func runUpdate(_ *cobra.Command, _ []string) error {
	var runningState *serverState
	runningPID := 0
	runningBinaryPath := ""
	if pid, err := readPID(); err == nil && pid > 0 && isProcessRunning(pid) {
		runningPID = pid
		if state, err := readState(); err == nil {
			runningState = &state
		}
		if exePath, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
			runningBinaryPath = exePath
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
	stopCheck()
	toolNeedsUpdate := remote != toolVersion
	if toolNeedsUpdate {
		fmt.Printf("ocgo update available: %s -> %s\n", toolVersion, remote)
	} else {
		fmt.Printf("ocgo already at latest version: %s\n", toolVersion)
	}
	opencodeTargetVersion := strings.TrimSpace(updateOCVersion)
	if opencodeTargetVersion == "" {
		latestOpencode, err := getLatestReleaseTag(opencodeOwner, opencodeRepo)
		if err != nil {
			return fmt.Errorf("failed to check opencode remote: %w", err)
		}
		if latestOpencode == "" {
			return fmt.Errorf("no published opencode release found")
		}
		opencodeTargetVersion = latestOpencode
	}
	updateTool := toolNeedsUpdate
	if updateTool {
		ok, err := confirmYesNo(fmt.Sprintf("Update ocgo to %s? [Y/n]: ", remote))
		if err != nil {
			return err
		}
		updateTool = ok
	} else {
		fmt.Println("ocgo update skipped: already latest")
	}

	updateOpencode := true
	if opencodeTargetVersion != "" {
		ok, err := confirmYesNo(fmt.Sprintf("Update opencode to %s? [Y/n]: ", opencodeTargetVersion))
		if err != nil {
			return err
		}
		updateOpencode = ok
	}

	if !updateTool && !updateOpencode {
		fmt.Println("No updates selected")
		return nil
	}

	if runningPID > 0 {
		ok, err := confirmYesNo("Running server will be stopped and restarted. Continue? [Y/n]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("Update cancelled")
			return nil
		}
	}

	if !updateTool {
		fmt.Println("ocgo update skipped")
	}
	if !updateOpencode {
		fmt.Println("opencode update skipped")
	}

	toolBinaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve current executable: %w", err)
	}
	if strings.TrimSpace(runningBinaryPath) != "" {
		toolBinaryPath = runningBinaryPath
	}

	var preparedTool *preparedToolUpdate
	var preparedOpencode *preparedOpencodeUpdate
	defer func() {
		if preparedTool != nil && strings.TrimSpace(preparedTool.TempPath) != "" {
			_ = os.Remove(preparedTool.TempPath)
		}
		if preparedOpencode != nil && strings.TrimSpace(preparedOpencode.ArchivePath) != "" {
			_ = os.Remove(preparedOpencode.ArchivePath)
		}
	}()

	opencodeBinaryForUpdate := ""
	if runningState != nil {
		opencodeBinaryForUpdate = strings.TrimSpace(runningState.OpencodeBinary)
	}
	if updateOpencode {
		preparedOpencode, err = prepareOpencodeUpdate(opencodeBinaryForUpdate, opencodeTargetVersion)
		if err != nil {
			return err
		}
	}
	if updateTool {
		preparedTool, err = prepareToolUpdate(toolBinaryPath, remote)
		if err != nil {
			return err
		}
	}
	if preparedOpencode != nil && preparedOpencode.Skip {
		updateOpencode = false
		fmt.Println("opencode update skipped: already latest")
	}
	if !updateTool && !updateOpencode {
		fmt.Println("No updates to apply")
		return nil
	}

	serverWasRunning := runningPID > 0
	serverStopped := false
	restartState := serverState{}
	if runningState != nil {
		restartState = *runningState
	}
	if serverWasRunning {
		state := serverState{}
		if runningState != nil {
			state = *runningState
		}
		if err := stopTrackedServer(state, runningPID); err != nil {
			return fmt.Errorf("failed to stop running server: %w", err)
		}
		serverStopped = true
		fmt.Println("Server stopped")
	}

	restartOnFailure := func(cause error) error {
		if !serverStopped || runningState == nil {
			return cause
		}
		pid, restartErr := startDetached(toolBinaryPath, &restartState)
		if restartErr != nil {
			return fmt.Errorf("%w; also failed to restart previous server: %v", cause, restartErr)
		}
		return fmt.Errorf("%w; previous server restored on pid %d", cause, pid)
	}

	if updateOpencode && preparedOpencode != nil {
		updatedOpencodeBinary, err := applyPreparedOpencodeUpdate(preparedOpencode)
		if err != nil {
			return restartOnFailure(err)
		}
		fmt.Printf("Updated opencode to %s ✓\n", preparedOpencode.Version)
		if runningState != nil && runningState.ManageOpencode {
			runningState.OpencodeBinary = updatedOpencodeBinary
			restartState.OpencodeBinary = updatedOpencodeBinary
		}
	}

	if updateTool && preparedTool != nil {
		if err := applyPreparedToolUpdate(preparedTool); err != nil {
			return restartOnFailure(err)
		}
		fmt.Printf("Updated ocgo to %s ✓\n", preparedTool.Version)
		toolBinaryPath = preparedTool.TargetPath
	}

	if runningState != nil {
		fmt.Println("Restarting server")
		pid, err := startDetached(toolBinaryPath, runningState)
		if err != nil {
			return fmt.Errorf("updated, but failed to restart server: %w", err)
		}
		runningState.PID = pid
		fmt.Printf("Server restarted on %s:%d\n", runningState.ListenIP, runningState.Port)
	} else if runningPID > 0 {
		fmt.Println("Server state unavailable, restart manually if needed")
	}

	return nil
}

func updateToolBinary(remote string) error {
	stopDownload := startSpinner("Downloading update")
	selfPath, err := os.Executable()
	if err != nil {
		stopDownload()
		return err
	}

	tmpPath := selfPath + ".new"
	if err := downloadReleaseAssetToFile(remote, toolAssetName(), tmpPath); err != nil {
		stopDownload()
		return err
	}
	stopDownload()

	backupPath := selfPath + ".bak"
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
	return nil
}

func prepareToolUpdate(targetPath, version string) (*preparedToolUpdate, error) {
	targetPath = strings.TrimSpace(targetPath)
	version = strings.TrimSpace(version)
	if targetPath == "" {
		return nil, fmt.Errorf("missing tool target path")
	}
	if version == "" {
		return nil, fmt.Errorf("missing tool target version")
	}
	tmpPath := targetPath + ".new"
	if err := downloadReleaseAssetToFile(version, toolAssetName(), tmpPath); err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmpPath, 0755)
	}
	return &preparedToolUpdate{TargetPath: targetPath, TempPath: tmpPath, Version: version}, nil
}

func applyPreparedToolUpdate(update *preparedToolUpdate) error {
	if update == nil {
		return fmt.Errorf("missing prepared tool update")
	}
	backupPath := update.TargetPath + ".bak"
	if err := os.Rename(update.TargetPath, backupPath); err != nil {
		_ = os.Remove(update.TempPath)
		return err
	}
	if err := os.Rename(update.TempPath, update.TargetPath); err != nil {
		_ = os.Rename(backupPath, update.TargetPath)
		return err
	}
	_ = os.Remove(backupPath)
	update.TempPath = ""
	return nil
}

func prepareOpencodeUpdate(explicit, targetVersion string) (*preparedOpencodeUpdate, error) {
	targetVersion = strings.TrimSpace(targetVersion)
	resolved, _, err := findOpencodeBinary(explicit)
	existing := err == nil
	targetPath := ""
	if existing {
		targetPath = resolved
	} else {
		targetPath = explicitInstallTarget(explicit)
		if targetPath == "" {
			defaultPath, pathErr := defaultOpencodeInstallPath()
			if pathErr != nil {
				return nil, pathErr
			}
			targetPath = defaultPath
		}
	}
	if targetVersion == "" {
		latest, err := getLatestReleaseTag(opencodeOwner, opencodeRepo)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve latest opencode version: %w", err)
		}
		targetVersion = latest
	}
	if targetVersion == "" {
		return nil, fmt.Errorf("no published opencode release found")
	}
	if existing {
		currentVersion, err := opencodeBinaryVersion(resolved)
		if err == nil && sameVersion(currentVersion, targetVersion) {
			fmt.Printf("opencode already at latest version: %s\n", strings.TrimSpace(currentVersion))
			return &preparedOpencodeUpdate{TargetPath: targetPath, Version: targetVersion, Existing: true, Skip: true}, nil
		}
	}
	assetName, err := opencodeAssetName()
	if err != nil {
		return nil, err
	}
	archiveFile, err := os.CreateTemp("", "opencode-*.tar.gz")
	if err != nil {
		return nil, err
	}
	archivePath := archiveFile.Name()
	_ = archiveFile.Close()
	if err := downloadReleaseAssetToFileFromRepo(opencodeOwner, opencodeRepo, targetVersion, assetName, archivePath); err != nil {
		_ = os.Remove(archivePath)
		return nil, fmt.Errorf("failed to download opencode %s: %w", targetVersion, err)
	}
	return &preparedOpencodeUpdate{TargetPath: targetPath, ArchivePath: archivePath, Version: targetVersion, Existing: existing}, nil
}

func applyPreparedOpencodeUpdate(update *preparedOpencodeUpdate) (string, error) {
	if update == nil {
		return "", fmt.Errorf("missing prepared opencode update")
	}
	if update.Skip {
		return update.TargetPath, nil
	}
	installDir := filepath.Dir(update.TargetPath)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}
	if err := extractTarGzFile(update.ArchivePath, opencodeBinName, update.TargetPath); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(update.TargetPath, 0755); err != nil {
			return "", err
		}
	}
	_ = os.Remove(update.ArchivePath)
	update.ArchivePath = ""
	return update.TargetPath, nil
}

func downloadReleaseAssetToFile(version, asset, targetPath string) error {
	return downloadReleaseAssetToFileFromRepo(repoOwner, repoName, version, asset, targetPath)
}

func downloadReleaseAssetToFileFromRepo(owner, repo, version, asset, targetPath string) error {
	resp, sourceURL, err := downloadReleaseAssetFromRepo(owner, repo, version, asset)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	fmt.Printf("Downloading from %s\n", sourceURL)

	out, err := os.Create(targetPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		_ = os.Remove(targetPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(targetPath)
		return err
	}
	return nil
}

func downloadReleaseAsset(version, asset string) (*http.Response, string, error) {
	return downloadReleaseAssetFromRepo(repoOwner, repoName, version, asset)
}

func downloadReleaseAssetFromRepo(owner, repo, version, asset string) (*http.Response, string, error) {
	urls := repoReleaseAssetURLs(owner, repo, version, asset)
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

func confirmYesNo(prompt string) (bool, error) {
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(input))
	if answer == "" {
		return true, nil
	}
	return answer != "n" && answer != "no", nil
}

func releaseAssetURLs(version, asset string) []string {
	return repoReleaseAssetURLs(repoOwner, repoName, version, asset)
}

func repoReleaseAssetURLs(owner, repo, version, asset string) []string {
	base := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s", owner, repo, version, asset)
	return []string{
		base,
		"https://gh-proxy.com/" + base,
		"https://mirror.ghproxy.com/" + base,
		"https://ghproxy.net/" + base,
	}
}

func opencodeAssetName() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("automatic opencode install is only implemented for linux")
	}

	archToken := ""
	switch runtime.GOARCH {
	case "amd64":
		archToken = "x64"
	case "arm64":
		archToken = "arm64"
	default:
		return "", fmt.Errorf("unsupported linux architecture for opencode: %s", runtime.GOARCH)
	}

	name := fmt.Sprintf("opencode-linux-%s", archToken)
	if archToken == "x64" && !linuxSupportsAVX2() {
		name += "-baseline"
	}
	if isLinuxMusl() {
		name += "-musl"
	}
	return name + ".tar.gz", nil
}

func linuxSupportsAVX2() bool {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return true
	}
	return strings.Contains(strings.ToLower(string(data)), "avx2")
}

func isLinuxMusl() bool {
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return true
	}
	cmd := exec.Command("ldd", "--version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(output)), "musl")
}

func opencodeInstallDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, ".opencode", "bin"), nil
}

func defaultOpencodeInstallPath() (string, error) {
	dir, err := opencodeInstallDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, opencodeBinName), nil
}

func extractTarGzFile(archivePath, binaryName, targetPath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()

	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if path.Base(header.Name) != binaryName {
			continue
		}
		tmpPath := targetPath + ".new"
		out, err := os.Create(tmpPath)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tarReader); err != nil {
			out.Close()
			_ = os.Remove(tmpPath)
			return err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		if err := os.Rename(tmpPath, targetPath); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		return nil
	}
	return fmt.Errorf("binary %s not found in archive %s", binaryName, archivePath)
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

func taggedReleaseInfoURLs(owner, repo, version string) []string {
	base := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, strings.TrimSpace(version))
	return []string{
		base,
		"https://gh-proxy.com/" + base,
		"https://mirror.ghproxy.com/" + base,
		"https://ghproxy.net/" + base,
	}
}

func toolAssetName() string {
	name := fmt.Sprintf("ocgo-%s-%s", runtime.GOOS, runtime.GOARCH)
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

var restartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the server",
	RunE:  runRestart,
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check server status",
	RunE:  runStatus,
}

var serveCmd = &cobra.Command{
	Use:    "serve",
	Short:  "Internal foreground entrypoint",
	Hidden: true,
	RunE:   runServe,
}

func init() {
	versionCmd.Flags().BoolVar(&showRemote, "remote", false, "Also check remote version")
	updateCmd.Flags().StringVar(&updateOCVersion, "oc-version", "", "Update managed/local opencode to a specific version")
	configureRuntimeFlags(startCmd, true)
	configureRuntimeFlags(serveCmd, true)
	configureRuntimeFlags(restartCmd, false)

	startCmd.Flags().BoolP("foreground", "f", false, "Run in foreground instead of daemon mode")
	startCmd.Flags().Bool("external", false, "Use an external opencode backend")

	restartCmd.Flags().BoolP("foreground", "f", false, "Ignored on restart; kept for CLI symmetry")
	restartCmd.Flags().MarkHidden("foreground")
	restartCmd.Flags().Bool("external", false, "Use an external opencode backend after restart")
}

func configureRuntimeFlags(cmd *cobra.Command, withDefaults bool) {
	defaultIP := ""
	defaultPort := 0
	defaultOpencodeIP := ""
	defaultOpencodePort := 0
	if withDefaults {
		defaultIP = defaultListenIP
		defaultPort = defaultListenPort
		defaultOpencodeIP = defaultOpencodeListenIP
		defaultOpencodePort = defaultOpencodeListenPort
	}
	cmd.Flags().String("backend", "", "OpenCode backend URL")
	cmd.Flags().String("host", defaultIP, "Listen host")
	cmd.Flags().IntP("port", "p", defaultPort, "Listen port")
	cmd.Flags().String("oc-bin", "", "Path to managed opencode binary")
	cmd.Flags().String("oc-host", defaultOpencodeIP, "Listen host for managed opencode")
	cmd.Flags().Int("oc-port", defaultOpencodePort, "Listen port for managed opencode")
	cmd.Flags().String("path", "", "Project directory for managed opencode serve")
	_ = cmd.Flags().MarkHidden("oc-bin")
}

func runStart(cmd *cobra.Command, _ []string) error {
	if pid, _ := readPID(); pid > 0 && isProcessRunning(pid) {
		fmt.Printf("Server already running (PID %d)\n", pid)
		return nil
	}

	options, err := readCommandRunOptions(cmd, true)
	if err != nil {
		return err
	}
	return runStartFromOptions(options)
}

func readCommandRunOptions(cmd *cobra.Command, withDefaults bool) (commandRunOptions, error) {
	options := commandRunOptions{}
	var err error
	options.BackendURL, err = cmd.Flags().GetString("backend")
	if err != nil {
		return options, err
	}
	options.ListenIP, err = cmd.Flags().GetString("host")
	if err != nil {
		return options, err
	}
	options.ListenPort, err = cmd.Flags().GetInt("port")
	if err != nil {
		return options, err
	}
	options.Foreground, _ = cmd.Flags().GetBool("foreground")
	options.OpencodeBinary, err = cmd.Flags().GetString("oc-bin")
	if err != nil {
		return options, err
	}
	options.ExternalBackend, _ = cmd.Flags().GetBool("external")
	options.OpencodeListenIP, err = cmd.Flags().GetString("oc-host")
	if err != nil {
		return options, err
	}
	options.OpencodeListenPort, err = cmd.Flags().GetInt("oc-port")
	if err != nil {
		return options, err
	}
	options.OpencodeProjectDir, err = cmd.Flags().GetString("path")
	if err != nil {
		return options, err
	}
	if strings.TrimSpace(options.BackendURL) != "" || options.ExternalBackend {
		options.ManageOpencode = false
		options.ManageOpencodeSet = true
	} else {
		options.ManageOpencode = true
		options.ManageOpencodeSet = withDefaults
	}
	return options, nil
}

func buildServerStateFromOptions(options commandRunOptions) (serverState, error) {
	state := serverState{
		ListenIP:           options.ListenIP,
		Port:               options.ListenPort,
		ManageOpencode:     options.ManageOpencode,
		OpencodeBinary:     strings.TrimSpace(options.OpencodeBinary),
		OpencodeListenIP:   strings.TrimSpace(options.OpencodeListenIP),
		OpencodePort:       options.OpencodeListenPort,
		OpencodeProjectDir: strings.TrimSpace(options.OpencodeProjectDir),
	}
	if strings.TrimSpace(state.ListenIP) == "" {
		state.ListenIP = defaultListenIP
	}
	if state.Port <= 0 {
		state.Port = defaultListenPort
	}

	if state.ManageOpencode {
		if state.OpencodeListenIP == "" {
			state.OpencodeListenIP = defaultOpencodeListenIP
		}
		if state.OpencodePort <= 0 {
			state.OpencodePort = defaultOpencodeListenPort
		}
		resolvedBinary, err := resolveOpencodeBinary(state.OpencodeBinary)
		if err != nil {
			return state, err
		}
		state.OpencodeBinary = resolvedBinary

		managedBackend := managedOpencodeBackend(state.OpencodeListenIP, state.OpencodePort)
		configuredBackend := strings.TrimSpace(options.BackendURL)
		if configuredBackend == "" {
			configuredBackend = strings.TrimSpace(os.Getenv("OPENCODE_BACKEND"))
		}
		if configuredBackend != "" && configuredBackend != managedBackend {
			return state, fmt.Errorf("managed opencode requires backend %s, got %s", managedBackend, configuredBackend)
		}
		if strings.TrimSpace(state.OpencodeProjectDir) != "" {
			info, err := os.Stat(state.OpencodeProjectDir)
			if err != nil {
				return state, fmt.Errorf("invalid opencode project dir %s: %w", state.OpencodeProjectDir, err)
			}
			if !info.IsDir() {
				return state, fmt.Errorf("invalid opencode project dir %s: not a directory", state.OpencodeProjectDir)
			}
		}
		state.Backend = managedBackend
		return state, nil
	}

	state.Backend = strings.TrimSpace(options.BackendURL)
	if state.Backend == "" {
		state.Backend = strings.TrimSpace(os.Getenv("OPENCODE_BACKEND"))
		if state.Backend == "" {
			state.Backend = fmt.Sprintf("%s:%d", defaultOpencodeListenIP, defaultOpencodeListenPort)
		}
	}
	return state, nil
}

func resolveOpencodeBinary(binary string) (string, error) {
	resolved, source, err := findOpencodeBinary(binary)
	if err != nil {
		return installAndResolveOpencode(binary)
	}
	if source != "" {
		fmt.Printf("Using opencode binary: %s (%s)\n", resolved, source)
	}
	return resolved, nil
}

func installAndResolveOpencode(explicit string) (string, error) {
	targetPath := explicitInstallTarget(explicit)
	installed, err := installOpencode(targetPath, "")
	if err != nil {
		return "", err
	}
	return installed, nil
}

func explicitInstallTarget(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit == "" {
		return ""
	}
	if strings.HasPrefix(explicit, "~/") {
		homeDir, _ := os.UserHomeDir()
		if homeDir != "" {
			return filepath.Join(homeDir, strings.TrimPrefix(explicit, "~/"))
		}
	}
	if strings.Contains(explicit, string(os.PathSeparator)) {
		return explicit
	}
	return ""
}

func findOpencodeBinary(explicit string) (string, string, error) {
	type candidate struct {
		Path   string
		Source string
	}

	homeDir, _ := os.UserHomeDir()
	candidates := []candidate{}
	seen := map[string]struct{}{}
	addCandidate := func(pathValue, source string) {
		pathValue = strings.TrimSpace(pathValue)
		if pathValue == "" {
			return
		}
		if strings.HasPrefix(pathValue, "~/") && homeDir != "" {
			pathValue = filepath.Join(homeDir, strings.TrimPrefix(pathValue, "~/"))
		}
		if _, ok := seen[pathValue]; ok {
			return
		}
		seen[pathValue] = struct{}{}
		candidates = append(candidates, candidate{Path: pathValue, Source: source})
	}

	addCandidate(explicit, "flag")
	addCandidate(os.Getenv("OPENCODE_BINARY"), "env:OPENCODE_BINARY")
	addCandidate(os.Getenv("OPENCODE_PATH"), "env:OPENCODE_PATH")
	addCandidate(os.Getenv("OPENCHAMBER_OPENCODE_PATH"), "env:OPENCHAMBER_OPENCODE_PATH")
	addCandidate(os.Getenv("OPENCHAMBER_OPENCODE_BIN"), "env:OPENCHAMBER_OPENCODE_BIN")
	addCandidate(filepath.Join(homeDir, ".opencode", "bin", "opencode"), "default:~/.opencode/bin")
	addCandidate(filepath.Join(homeDir, ".local", "bin", "opencode"), "default:~/.local/bin")
	addCandidate("/usr/local/bin/opencode", "default:/usr/local/bin")
	addCandidate("/opt/homebrew/bin/opencode", "default:/opt/homebrew/bin")
	addCandidate("/usr/bin/opencode", "default:/usr/bin")
	addCandidate("opencode", "PATH")

	var tried []string
	for _, candidate := range candidates {
		resolved, err := resolveExecutablePath(candidate.Path)
		if err == nil {
			return resolved, candidate.Source, nil
		}
		tried = append(tried, fmt.Sprintf("%s (%s)", candidate.Path, candidate.Source))
	}

	return "", "", fmt.Errorf("failed to locate opencode binary; tried %s", strings.Join(tried, ", "))
}

func resolveExecutablePath(pathValue string) (string, error) {
	pathValue = strings.TrimSpace(pathValue)
	if pathValue == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.Contains(pathValue, string(os.PathSeparator)) {
		info, err := os.Stat(pathValue)
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			return "", fmt.Errorf("is a directory")
		}
		if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
			return "", fmt.Errorf("not executable")
		}
		return pathValue, nil
	}
	return exec.LookPath(pathValue)
}

func updateOrInstallOpencode(explicit string, targetVersion string) (string, error) {
	resolved, source, err := findOpencodeBinary(explicit)
	if err != nil {
		fmt.Println("opencode not found, installing")
		installed, installErr := installOpencode(explicitInstallTarget(explicit), targetVersion)
		if installErr != nil {
			return "", installErr
		}
		fmt.Printf("Installed opencode: %s\n", installed)
		return installed, nil
	}

	if source != "" {
		fmt.Printf("Using opencode binary: %s (%s)\n", resolved, source)
	}
	updatedBinary, err := upgradeOpencode(resolved, targetVersion)
	if err != nil {
		return "", err
	}
	return updatedBinary, nil
}

func installOpencode(targetPath string, targetVersion string) (string, error) {
	version := strings.TrimSpace(targetVersion)
	if version == "" {
		var err error
		version, err = getLatestReleaseTag(opencodeOwner, opencodeRepo)
		if err != nil {
			return "", fmt.Errorf("failed to resolve latest opencode version: %w", err)
		}
	}
	if version == "" {
		return "", fmt.Errorf("no published opencode release found")
	}

	assetName, err := opencodeAssetName()
	if err != nil {
		return "", err
	}
	tmpArchive, err := os.CreateTemp("", "opencode-*.tar.gz")
	if err != nil {
		return "", err
	}
	tmpArchivePath := tmpArchive.Name()
	_ = tmpArchive.Close()
	defer func() { _ = os.Remove(tmpArchivePath) }()

	if err := downloadReleaseAssetToFileFromRepo(opencodeOwner, opencodeRepo, version, assetName, tmpArchivePath); err != nil {
		return "", fmt.Errorf("failed to download opencode %s: %w", version, err)
	}

	targetPath = strings.TrimSpace(targetPath)
	if targetPath == "" {
		var err error
		targetPath, err = defaultOpencodeInstallPath()
		if err != nil {
			return "", err
		}
	}
	installDir := filepath.Dir(targetPath)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return "", err
	}
	if err := extractTarGzFile(tmpArchivePath, opencodeBinName, targetPath); err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(targetPath, 0755); err != nil {
			return "", err
		}
	}
	return targetPath, nil
}

func upgradeOpencode(binary string, targetVersion string) (string, error) {
	binary = strings.TrimSpace(binary)
	if binary == "" {
		return "", fmt.Errorf("missing opencode binary")
	}
	version := strings.TrimSpace(targetVersion)
	if version == "" {
		latest, err := getLatestReleaseTag(opencodeOwner, opencodeRepo)
		if err != nil {
			return "", fmt.Errorf("failed to resolve latest opencode version: %w", err)
		}
		version = latest
	}
	if version == "" {
		return "", fmt.Errorf("no published opencode release found")
	}
	currentVersion, err := opencodeBinaryVersion(binary)
	if err == nil && sameVersion(currentVersion, version) {
		fmt.Printf("opencode already at latest version: %s\n", strings.TrimSpace(currentVersion))
		return binary, nil
	}
	installed, err := installOpencode(binary, version)
	if err != nil {
		return "", fmt.Errorf("failed to update opencode: %w", err)
	}
	fmt.Printf("Updated opencode to %s ✓\n", version)
	return installed, nil
}

func opencodeBinaryVersion(binary string) (string, error) {
	cmd := exec.Command(binary, "--version")
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func sameVersion(left, right string) bool {
	left = strings.TrimSpace(strings.TrimPrefix(left, "v"))
	right = strings.TrimSpace(strings.TrimPrefix(right, "v"))
	return left != "" && left == right
}

func stopTrackedServer(state serverState, pid int) error {
	if pid > 0 && isProcessRunning(pid) {
		if err := stopProcess(pid); err != nil {
			return err
		}
	}
	if state.OpencodePID > 0 && isProcessRunning(state.OpencodePID) {
		if err := stopProcess(state.OpencodePID); err != nil {
			return err
		}
	}
	removeStateFiles()
	return nil
}

func managedOpencodeBackend(host string, port int) string {
	return fmt.Sprintf("%s:%d", resolveDialHost(host), port)
}

func resolveDialHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return "127.0.0.1"
	}
	switch host {
	case "0.0.0.0":
		return "127.0.0.1"
	case "::", "[::]":
		return "::1"
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return host
}

func startDetached(selfPath string, state *serverState) (int, error) {
	if state == nil {
		return 0, fmt.Errorf("missing server state")
	}
	if err := ensureListenAddressAvailable(state.ListenIP, state.Port); err != nil {
		return 0, err
	}
	if state.ManageOpencode {
		if err := ensureListenAddressAvailable(state.OpencodeListenIP, state.OpencodePort); err != nil {
			return 0, fmt.Errorf("managed opencode listen %s:%d unavailable: %w", state.OpencodeListenIP, state.OpencodePort, err)
		}
	}

	nullHandle, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open devnull: %w", err)
	}
	defer nullHandle.Close()
	nullWriteHandle, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return 0, fmt.Errorf("failed to open devnull: %w", err)
	}
	defer nullWriteHandle.Close()

	cmd := exec.Command(selfPath, "serve",
		"--host", state.ListenIP,
		"--port", fmt.Sprintf("%d", state.Port),
	)
	if state.ManageOpencode {
		cmd.Args = append(cmd.Args,
			"--oc-bin", state.OpencodeBinary,
			"--oc-host", state.OpencodeListenIP,
			"--oc-port", fmt.Sprintf("%d", state.OpencodePort),
		)
		if strings.TrimSpace(state.OpencodeProjectDir) != "" {
			cmd.Args = append(cmd.Args, "--path", state.OpencodeProjectDir)
		}
	} else {
		cmd.Args = append(cmd.Args, "--backend", state.Backend)
	}
	cmd.Stdin = nullHandle
	cmd.Stdout = nullWriteHandle
	cmd.Stderr = nullWriteHandle
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
		removeStateFiles()
		return 0, fmt.Errorf("server exited immediately")
	}

	return pid, nil
}

func runStop(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Server not running")
		return nil
	}
	state, _ := readState()
	if !isProcessRunning(pid) {
		removeStateFiles()
		fmt.Println("Server not running (stale pid file)")
		return nil
	}
	if err := stopProcess(pid); err != nil {
		return fmt.Errorf("failed to stop: %w", err)
	}
	if state.OpencodePID > 0 && isProcessRunning(state.OpencodePID) {
		if err := stopProcess(state.OpencodePID); err != nil {
			return fmt.Errorf("server stopped but managed opencode is still running: %w", err)
		}
	}
	removeStateFiles()
	fmt.Println("Server stopped")
	return nil
}

func runRestart(cmd *cobra.Command, _ []string) error {
	currentState, currentPID, running := currentRunningState()
	options, err := readCommandRunOptions(cmd, false)
	if err != nil {
		return err
	}
	if !running {
		if !options.ManageOpencodeSet {
			options.ManageOpencode = true
			options.ManageOpencodeSet = true
		}
		return runStartFromOptions(options)
	}
	nextState, err := restartTargetState(currentState, options)
	if err != nil {
		return err
	}

	if err := stopProcess(currentPID); err != nil {
		return fmt.Errorf("failed to stop current server: %w", err)
	}
	if currentState.OpencodePID > 0 && isProcessRunning(currentState.OpencodePID) {
		if err := stopProcess(currentState.OpencodePID); err != nil {
			return fmt.Errorf("server stopped but managed opencode is still running: %w", err)
		}
	}
	removeStateFiles()

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable: %w", err)
	}
	pid, err := startDetached(selfPath, &nextState)
	if err != nil {
		return err
	}

	printServerSummary("Server restarted", pid, nextState)
	return nil
}

func runStartFromOptions(options commandRunOptions) error {
	if options.Foreground {
		return runServeWithOptions(options)
	}
	state, err := buildServerStateFromOptions(options)
	if err != nil {
		return err
	}
	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable: %w", err)
	}
	pid, err := startDetached(selfPath, &state)
	if err != nil {
		return err
	}
	printServerSummary("Server started", pid, state)
	return nil
}

func printServerSummary(title string, pid int, state serverState) {
	if title != "" {
		fmt.Println(title)
	}
	if pid > 0 {
		fmt.Printf("  PID:      %d\n", pid)
	}
	fmt.Printf("  Listen:   %s:%d\n", state.ListenIP, state.Port)
	if state.ManageOpencode {
		fmt.Printf("  OpenCode: managed on %s:%d\n", state.OpencodeListenIP, state.OpencodePort)
		if strings.TrimSpace(state.OpencodeProjectDir) != "" {
			fmt.Printf("  Project:  %s\n", state.OpencodeProjectDir)
		}
	} else {
		fmt.Printf("  Backend:  %s\n", state.Backend)
	}
}

func currentRunningState() (serverState, int, bool) {
	pid, err := readPID()
	if err != nil || pid == 0 || !isProcessRunning(pid) {
		return serverState{}, 0, false
	}
	state, err := readState()
	if err != nil {
		state = serverState{}
	}
	if state.PID == 0 {
		state.PID = pid
	}
	return state, pid, true
}

func restartTargetState(current serverState, options commandRunOptions) (serverState, error) {
	state := current
	backendExplicit := strings.TrimSpace(options.BackendURL) != ""
	previousManagedBackend := ""
	if current.ManageOpencode {
		previousManagedBackend = managedOpencodeBackend(current.OpencodeListenIP, current.OpencodePort)
	}

	if strings.TrimSpace(options.ListenIP) != "" {
		state.ListenIP = strings.TrimSpace(options.ListenIP)
	}
	if options.ListenPort > 0 {
		state.Port = options.ListenPort
	}
	if backendExplicit {
		state.Backend = strings.TrimSpace(options.BackendURL)
	}

	if options.ManageOpencodeSet {
		state.ManageOpencode = options.ManageOpencode
	}
	if strings.TrimSpace(options.OpencodeBinary) != "" {
		state.OpencodeBinary = strings.TrimSpace(options.OpencodeBinary)
	}
	if strings.TrimSpace(options.OpencodeListenIP) != "" {
		state.OpencodeListenIP = strings.TrimSpace(options.OpencodeListenIP)
	}
	if options.OpencodeListenPort > 0 {
		state.OpencodePort = options.OpencodeListenPort
	}
	if strings.TrimSpace(options.OpencodeProjectDir) != "" {
		state.OpencodeProjectDir = strings.TrimSpace(options.OpencodeProjectDir)
	}

	if state.ListenIP == "" {
		state.ListenIP = defaultListenIP
	}
	if state.Port == 0 {
		state.Port = defaultListenPort
	}

	if state.ManageOpencode {
		if state.OpencodeListenIP == "" {
			state.OpencodeListenIP = defaultOpencodeListenIP
		}
		if state.OpencodePort == 0 {
			state.OpencodePort = defaultOpencodeListenPort
		}
		resolvedBinary, err := resolveOpencodeBinary(state.OpencodeBinary)
		if err != nil {
			return state, err
		}
		state.OpencodeBinary = resolvedBinary
		managedBackend := managedOpencodeBackend(state.OpencodeListenIP, state.OpencodePort)
		if !backendExplicit && (state.Backend == "" || state.Backend == previousManagedBackend) {
			state.Backend = managedBackend
		}
		if state.Backend != "" && state.Backend != managedBackend {
			return state, fmt.Errorf("managed opencode requires backend %s, got %s", managedBackend, state.Backend)
		}
		state.Backend = managedBackend
	}

	state.PID = 0
	state.OpencodePID = 0
	return state, nil
}

func runStatus(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Stopped")
		return nil
	}
	if !isProcessRunning(pid) {
		removeStateFiles()
		fmt.Println("Stopped (stale pid file)")
		return nil
	}

	fmt.Printf("Running (PID %d)\n", pid)
	state, err := readState()
	if err == nil {
		fmt.Printf("Listen:  %s:%d\n", state.ListenIP, state.Port)
		if state.ManageOpencode {
			status := "managed"
			if state.OpencodePID > 0 && isProcessRunning(state.OpencodePID) {
				status = fmt.Sprintf("managed (PID %d)", state.OpencodePID)
			} else if state.OpencodePID > 0 {
				status = fmt.Sprintf("managed (stopped, expected PID %d)", state.OpencodePID)
			}
			fmt.Printf("OpenCode: %s on %s:%d\n", status, state.OpencodeListenIP, state.OpencodePort)
			if strings.TrimSpace(state.OpencodeProjectDir) != "" {
				fmt.Printf("Project: %s\n", state.OpencodeProjectDir)
			}
		} else {
			fmt.Printf("Backend: %s\n", state.Backend)
			fmt.Println("OpenCode: external backend")
		}
	}
	return nil
}

func ensureListenAddressAvailable(ip string, port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return fmt.Errorf("listen %s:%d unavailable: %w", ip, port, err)
	}
	_ = ln.Close()
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

func removeStateFiles() {
	_ = os.Remove(pidFile)
	_ = os.Remove(stateFile)
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

	deadline := time.Now().Add(shutdownTTL)
	for time.Now().Before(deadline) {
		if !isProcessRunning(pid) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return fmt.Errorf("process %d did not exit in time", pid)
}

func startManagedOpencode(state *serverState) (*exec.Cmd, error) {
	if state == nil {
		return nil, fmt.Errorf("missing server state")
	}
	cmd := exec.Command(state.OpencodeBinary, "serve",
		"--hostname", state.OpencodeListenIP,
		"--port", fmt.Sprintf("%d", state.OpencodePort),
	)
	if strings.TrimSpace(state.OpencodeProjectDir) != "" {
		cmd.Dir = state.OpencodeProjectDir
	}
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start managed opencode: %w", err)
	}
	if err := waitForTCPReady(resolveDialHost(state.OpencodeListenIP), state.OpencodePort, cmd.Process.Pid, 15*time.Second); err != nil {
		_ = stopProcess(cmd.Process.Pid)
		_, _ = cmd.Process.Wait()
		return nil, err
	}
	state.OpencodePID = cmd.Process.Pid
	return cmd, nil
}

func waitForTCPReady(host string, port int, pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	address := fmt.Sprintf("%s:%d", host, port)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if pid > 0 && !isProcessRunning(pid) {
			return fmt.Errorf("managed opencode exited before listening on %s", address)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("managed opencode did not become ready on %s", address)
}

func writeRuntimeState(state serverState) error {
	state.PID = os.Getpid()
	writePID(state.PID)
	return writeState(state)
}

func runServe(cmd *cobra.Command, _ []string) error {
	options, err := readCommandRunOptions(cmd, true)
	if err != nil {
		return err
	}
	return runServeWithOptions(options)
}

func runServeWithOptions(options commandRunOptions) error {
	state, err := buildServerStateFromOptions(options)
	if err != nil {
		return err
	}
	if err := ensureListenAddressAvailable(state.ListenIP, state.Port); err != nil {
		return err
	}
	if state.ManageOpencode {
		if err := ensureListenAddressAvailable(state.OpencodeListenIP, state.OpencodePort); err != nil {
			return fmt.Errorf("managed opencode listen %s:%d unavailable: %w", state.OpencodeListenIP, state.OpencodePort, err)
		}
	}

	addr := fmt.Sprintf("%s:%d", state.ListenIP, state.Port)

	frontendFS, err := fs.Sub(embeddedFrontend, frontendDir)
	if err != nil {
		return fmt.Errorf("embedded frontend not available: %w", err)
	}

	fileServer := http.FileServer(http.FS(frontendFS))
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = state.Backend
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

	var managedCmd *exec.Cmd
	var managedWaitCh chan error
	if state.ManageOpencode {
		managedCmd, err = startManagedOpencode(&state)
		if err != nil {
			return err
		}
		managedWaitCh = make(chan error, 1)
		go func() {
			managedWaitCh <- managedCmd.Wait()
		}()
	}

	if err := writeRuntimeState(state); err != nil {
		if managedCmd != nil && isProcessRunning(managedCmd.Process.Pid) {
			_ = stopProcess(managedCmd.Process.Pid)
		}
		return fmt.Errorf("failed to write state: %w", err)
	}
	defer func() {
		removeStateFiles()
	}()

	server := &http.Server{Addr: addr, Handler: mux}
	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.ListenAndServe()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	var serveErr error
	select {
	case sig := <-sigCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTTL)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("shutdown after %s failed: %w", sig, err)
		}
		if err := <-serverErrCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("serve failed on %s: %w", addr, err)
		}
	case err := <-managedWaitCh:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTTL)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		if srvErr := <-serverErrCh; srvErr != nil && !errors.Is(srvErr, http.ErrServerClosed) {
			serveErr = srvErr
		}
		if err != nil {
			serveErr = fmt.Errorf("managed opencode exited: %w", err)
		} else {
			serveErr = fmt.Errorf("managed opencode exited")
		}
	}

	if managedCmd != nil && managedCmd.Process != nil && isProcessRunning(managedCmd.Process.Pid) {
		if err := stopProcess(managedCmd.Process.Pid); err != nil && serveErr == nil {
			serveErr = fmt.Errorf("failed to stop managed opencode: %w", err)
		}
	}

	return serveErr
}
