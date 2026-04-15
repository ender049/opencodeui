package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	frontendDir = "./dist"
	versionFile = ".version"
	toolVersion = "v0.1.0"
	repoOwner   = "ender049"
	repoName    = "opencodeui"
	pidFile     = ".opencodeui.pid"
)

var (
	backendURL   string
	listenIP     string
	listenPort   int
	frontendPath string
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show tool and frontend versions",
	RunE:  runVersion,
}

func runVersion(_ *cobra.Command, _ []string) error {
	fmt.Printf("Tool version:  %s\n", toolVersion)

	remote, err := getToolRemoteVersion()
	if err != nil {
		fmt.Printf("Tool remote:    error - %v\n", err)
	} else {
		fmt.Printf("Tool remote:    %s\n", remote)
		if remote == toolVersion {
			fmt.Println("Tool status:    up to date ✓")
		} else {
			fmt.Println("Tool status:    update available ↓")
		}
	}

	fmt.Println()

	frontendLocal := readLocalVersion()
	if frontendLocal != "" {
		fmt.Printf("Frontend local: %s\n", frontendLocal)
	}

	frontendRemote, err := getFrontendRemoteVersion()
	if err != nil {
		fmt.Printf("Frontend remote: error - %v\n", err)
	} else {
		fmt.Printf("Frontend remote: %s\n", frontendRemote)
		if frontendLocal == "" {
			fmt.Println("Frontend:       not installed")
		} else if frontendLocal == frontendRemote {
			fmt.Println("Frontend:       up to date ✓")
		} else {
			fmt.Println("Frontend:       update available ↓")
		}
	}

	return nil
}

func readLocalVersion() string {
	data, _ := os.ReadFile(versionFile)
	return strings.TrimSpace(string(data))
}

func getToolRemoteVersion() (string, error) {
	return getLatestReleaseTag(repoOwner, repoName)
}

func getFrontendRemoteVersion() (string, error) {
	return getLatestReleaseTag(repoOwner, repoName)
}

func getLatestReleaseTag(owner, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	return result.TagName, nil
}

var updateTool, updateFrontend bool

var updateCmd = &cobra.Command{
	Use:   "update",
	Short: "Update tool and frontend",
	RunE:  runUpdate,
}

func init() {
	updateCmd.Flags().BoolVar(&updateTool, "tool", false, "Only update tool")
	updateCmd.Flags().BoolVar(&updateFrontend, "frontend", false, "Only update frontend")
}

func runUpdate(_ *cobra.Command, _ []string) error {
	all := !updateTool && !updateFrontend

	if all || updateTool {
		if err := updateToolBinary(); err != nil {
			fmt.Printf("Tool update failed: %v\n", err)
		}
	}

	if all || updateFrontend {
		if err := updateFrontendFiles(); err != nil {
			fmt.Printf("Frontend update failed: %v\n", err)
		}
	}

	return nil
}

func updateToolBinary() error {
	remote, err := getToolRemoteVersion()
	if err != nil {
		return fmt.Errorf("failed to check remote: %w", err)
	}

	if remote == toolVersion {
		fmt.Printf("Tool already at latest: %s\n", toolVersion)
		return nil
	}

	fmt.Printf("Updating tool %s → %s\n", toolVersion, remote)

	osName := runtime.GOOS
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	}

	url := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/opencodeui-%s-%s",
		repoOwner, repoName, remote, osName, arch)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	selfPath, err := os.Executable()
	if err != nil {
		return err
	}

	tmpFile := filepath.Join(os.TempDir(), "opencodeui-new")
	out, err := os.Create(tmpFile)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmpFile)
		return err
	}
	out.Close()

	backupPath := selfPath + ".bak"
	os.Rename(selfPath, backupPath)
	if err := os.Rename(tmpFile, selfPath); err != nil {
		os.Rename(backupPath, selfPath)
		return err
	}
	os.Chmod(selfPath, 0755)
	os.Remove(backupPath)

	fmt.Printf("Tool updated to %s ✓\n", remote)
	return nil
}

func updateFrontendFiles() error {
	remote, err := getFrontendRemoteVersion()
	if err != nil {
		return fmt.Errorf("failed to check remote: %w", err)
	}

	local := readLocalVersion()
	if local == remote {
		fmt.Printf("Frontend already at latest: %s\n", local)
		return nil
	}

	fmt.Printf("Updating frontend %s → %s\n", local, remote)

	url := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/opencodeui-dist.tar.gz",
		repoOwner, repoName, remote)

	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}

	tmpFile := filepath.Join(os.TempDir(), "opencodeui-dist.tar.gz")
	out, err := os.Create(tmpFile)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)

	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	out.Close()

	os.RemoveAll(frontendDir)
	os.MkdirAll(frontendDir, 0755)

	if err := extractTarGz(tmpFile, frontendDir); err != nil {
		return fmt.Errorf("extract failed: %w", err)
	}

	os.WriteFile(versionFile, []byte(remote), 0644)

	fmt.Printf("Frontend updated to %s ✓\n", remote)
	return nil
}

func extractTarGz(tgzPath, destDir string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	os.RemoveAll(destDir)
	os.MkdirAll(destDir, 0755)

	var buf []byte
	for {
		chunk := make([]byte, 4096)
		n, err := gz.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err != nil {
			break
		}
	}

	r := strings.NewReader(string(buf))

	for {
		hdr := make([]byte, 512)
		n, err := r.Read(hdr)
		if n < 512 || err != nil {
			break
		}

		nameLen := 0
		for i := 0; i < 100; i++ {
			if hdr[i] == 0 {
				nameLen = i
				break
			}
		}
		if nameLen == 0 {
			continue
		}

		name := string(hdr[:nameLen])
		if name == "./././LongLink" || strings.HasPrefix(name, "PaxHeader") {
			size := parseOctal(string(hdr[124:136]))
			r.Seek(size, 1)
			if size%512 != 0 {
				r.Seek(512-size%512, 1)
			}
			continue
		}

		size := parseOctal(string(hdr[124:136]))
		r.Seek(512, 1)

		if !strings.HasPrefix(name, "/") && !strings.Contains(name, "..") {
			filePath := filepath.Join(destDir, name)
			if strings.HasSuffix(name, "/") {
				os.MkdirAll(filePath, 0755)
			} else {
				os.MkdirAll(filepath.Dir(filePath), 0755)
				out, _ := os.Create(filePath)
				remaining := int(size)
				for remaining > 0 {
					data := make([]byte, remaining)
					n, _ := r.Read(data)
					if n > 0 {
						out.Write(data[:n])
					}
					remaining -= n
				}
				out.Close()
			}
		}

		if size%512 != 0 {
			r.Seek(512-size%512, 1)
		}
	}

	return nil
}

func parseOctal(s string) int64 {
	var n int64
	for _, c := range strings.TrimSpace(s) {
		if c >= '0' && c <= '7' {
			n = n*8 + int64(c-'0')
		}
	}
	return n
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the server",
	RunE:  runStart,
}

func init() {
	startCmd.Flags().StringVarP(&backendURL, "backend", "b", "", "OpenCode backend URL")
	startCmd.Flags().StringVar(&listenIP, "ip", "127.0.0.1", "Listen IP")
	startCmd.Flags().IntVarP(&listenPort, "port", "p", 3000, "Listen port")
	startCmd.Flags().StringVarP(&frontendPath, "dir", "d", "", "Frontend directory")
}

func runStart(_ *cobra.Command, _ []string) error {
	if pid, _ := readPID(); pid > 0 && isProcessRunning(pid) {
		fmt.Printf("Server already running (PID %d)\n", pid)
		return nil
	}

	dir := frontendPath
	if dir == "" {
		dir = frontendDir
	}

	if backendURL == "" {
		backendURL = os.Getenv("OPENCODE_BACKEND")
		if backendURL == "" {
			backendURL = "127.0.0.1:4096"
		}
	}

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Printf("Frontend not found: %s\n", dir)
		fmt.Println("Run 'opencodeui update --frontend' first")
		return nil
	}

	selfPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable: %w", err)
	}

	cmd := exec.Command(selfPath, "serve",
		"--backend", backendURL,
		"--ip", listenIP,
		"--port", fmt.Sprintf("%d", listenPort),
		"--dir", dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start: %w", err)
	}

	writePID(cmd.Process.Pid)

	fmt.Printf("Server started\n")
	fmt.Printf("  PID:    %d\n", cmd.Process.Pid)
	fmt.Printf("  Listen: %s:%d\n", listenIP, listenPort)
	fmt.Printf("  Backend: %s\n", backendURL)

	return nil
}

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the server",
	RunE:  runStop,
}

func runStop(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Server not running")
		return nil
	}

	if !isProcessRunning(pid) {
		os.Remove(pidFile)
		fmt.Println("Server not running (stale pid file)")
		return nil
	}

	if err := exec.Command("kill", fmt.Sprintf("%d", pid)).Run(); err != nil {
		fmt.Printf("Failed to stop: %v\n", err)
	} else {
		os.Remove(pidFile)
		fmt.Println("Server stopped")
	}

	return nil
}

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Check server status",
	RunE:  runStatus,
}

func runStatus(_ *cobra.Command, _ []string) error {
	pid, err := readPID()
	if err != nil || pid == 0 {
		fmt.Println("Server:  Stopped")
		return nil
	}

	if !isProcessRunning(pid) {
		os.Remove(pidFile)
		fmt.Println("Server:  Stopped (stale pid file)")
		return nil
	}

	fmt.Printf("Server:  Running (PID %d)\n", pid)

	if backendURL != "" {
		fmt.Printf("Backend: %s\n", backendURL)
	}
	if listenIP != "" && listenPort > 0 {
		fmt.Printf("Listen:  %s:%d\n", listenIP, listenPort)
	}

	return nil
}

func writePID(pid int) {
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", pid)), 0644)
}

func readPID() (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	var pid int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid)
	return pid, nil
}

func isProcessRunning(pid int) bool {
	process, _ := os.FindProcess(pid)
	return process.Signal(os.Signal(nil)) == nil
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Internal: run server",
	Run:   runServe,
}

func init() {
	serveCmd.Flags().StringVarP(&backendURL, "backend", "b", "", "OpenCode backend URL")
	serveCmd.Flags().StringVar(&listenIP, "ip", "127.0.0.1", "Listen IP")
	serveCmd.Flags().IntVarP(&listenPort, "port", "p", 3000, "Listen port")
	serveCmd.Flags().StringVarP(&frontendPath, "dir", "d", "", "Frontend directory")
}

func runServe(_ *cobra.Command, _ []string) {
	dir := frontendPath
	if dir == "" {
		dir = frontendDir
	}

	if backendURL == "" {
		backendURL = "127.0.0.1:4096"
	}

	addr := fmt.Sprintf("%s:%d", listenIP, listenPort)

	proxy := newProxy(backendURL)
	mux := http.NewServeMux()

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
		proxy.ServeHTTP(w, r)
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") {
			return
		}
		filePath := filepath.Join(dir, r.URL.Path)
		if info, err := os.Stat(filePath); err == nil && !info.IsDir() {
			serveFile(w, r, filePath)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html"))
	})

	http.ListenAndServe(addr, mux)
}

func newProxy(backend string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = backend
			req.Header.Del("Origin")
			req.Header.Del("Referer")
		},
		Transport: &noBufferTransport{Timeout: 5 * time.Minute},
	}
}

type noBufferTransport struct{ Timeout time.Duration }

func (t *noBufferTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(r)
}

func serveFile(w http.ResponseWriter, r *http.Request, filePath string) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ext := strings.ToLower(filepath.Ext(filePath))
	contentType := map[string]string{
		".html":  "text/html; charset=utf-8",
		".js":    "application/javascript",
		".mjs":   "application/javascript",
		".css":   "text/css",
		".json":  "application/json",
		".png":   "image/png",
		".jpg":   "image/jpeg",
		".jpeg":  "image/jpeg",
		".svg":   "image/svg+xml",
		".ico":   "image/x-icon",
		".woff":  "font/woff",
		".woff2": "font/woff2",
		".ttf":   "font/ttf",
		".eot":   "application/vnd.ms-fontobject",
		".map":   "application/json",
		".txt":   "text/plain",
		".wasm":  "application/wasm",
	}[ext]
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")

	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && isCompressible(ext) {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write(data)
		gz.Close()
	} else {
		w.Write(data)
	}
}

func isCompressible(ext string) bool {
	switch ext {
	case ".js", ".css", ".html", ".svg", ".json", ".mjs":
		return true
	}
	return false
}
