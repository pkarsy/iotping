package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

type Config struct {
	Devices                map[string]string
	TelegramToken          string
	TelegramChatID         string
	Interval               time.Duration
	FailureThreshold       int
	RecoveryNotify         bool
	PingTimeout            time.Duration
	Debug                  bool
	LogFile                string
	RepeatInterval         time.Duration
	MaxRepeatNotifications int
}

type DeviceState struct {
	Failures     int       `json:"failures"`
	IsOnline     bool      `json:"is_online"`
	LastChange   time.Time `json:"last_change"`
	LastNotified time.Time `json:"last_notified"`
	NotifyCount  int       `json:"notify_count"`
}

type Monitor struct {
	config    Config
	states    map[string]*DeviceState
	mu        sync.RWMutex
	client    *http.Client
	icmpAvail bool // Cached: can we do unprivileged ICMP?
	reloadCh  chan Config
	msgQueue  []string
	queueMu   sync.Mutex
}

// PidFile manages a PID file to prevent multiple instances
type PidFile struct {
	path string
}

func NewPidFile(path string) *PidFile {
	return &PidFile{path: path}
}

func (p *PidFile) Acquire() error {
	// Ensure directory exists
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create PID file directory: %v", err)
	}

	pid := os.Getpid()
	// Create the PID file atomically (O_EXCL) so two instances started at the
	// same moment cannot both pass the check (TOCTOU race).
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", pid)
			f.Close()
			return nil
		}
		if !os.IsExist(err) {
			return fmt.Errorf("failed to create PID file %s: %v", p.path, err)
		}

		// PID file exists: check whether the owning process is still alive.
		if data, rerr := os.ReadFile(p.path); rerr == nil {
			if old, aerr := strconv.Atoi(strings.TrimSpace(string(data))); aerr == nil && old > 0 && isProcessRunning(old) {
				return fmt.Errorf("another instance is already running (PID: %d)", old)
			}
		}
		// Stale PID file, remove it and retry
		if rerr := os.Remove(p.path); rerr != nil && !os.IsNotExist(rerr) {
			return fmt.Errorf("failed to remove stale PID file %s: %v", p.path, rerr)
		}
	}
	return fmt.Errorf("failed to acquire PID file %s", p.path)
}

func (p *PidFile) Release() {
	os.Remove(p.path)
}

func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds, need to send signal 0 to check
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

// debugf logs a message only if debug mode is enabled
func debugf(enabled bool, format string, v ...interface{}) {
	if enabled {
		log.Printf("[DEBUG] "+format, v...)
	}
}

// stripComments removes // and /* */ style comments from JSON.
// String literals are preserved verbatim: comment markers inside a string
// value (e.g. a token or path containing "//") are data, not comments.
func stripComments(data []byte) []byte {
	result := make([]byte, 0, len(data))
	i := 0
	for i < len(data) {
		// Copy whole string literals (handling escaped quotes) unchanged
		if data[i] == '"' {
			start := i
			i++
			for i < len(data) {
				if data[i] == '\\' {
					i += 2
					continue
				}
				if data[i] == '"' {
					i++
					break
				}
				i++
			}
			result = append(result, data[start:i]...)
			continue
		}
		if data[i] == '/' && i+1 < len(data) {
			if data[i+1] == '/' {
				// Skip // comment to end of line
				for i < len(data) && data[i] != '\n' {
					i++
				}
				continue
			} else if data[i+1] == '*' {
				// Skip /* */ comment
				i += 2
				for i+1 < len(data) {
					if data[i] == '*' && data[i+1] == '/' {
						i += 2
						break
					}
					i++
				}
				continue
			}
		}
		result = append(result, data[i])
		i++
	}
	return result
}

func countLines(data []byte) int {
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	return count
}

func hasCommaAtEnd(result []byte, endIdx int) bool {
	for i := endIdx + 1; i < len(result); i++ {
		if result[i] == ',' {
			return true
		}
		if result[i] != ' ' && result[i] != '\t' && result[i] != '\n' && result[i] != '\r' {
			break
		}
	}
	return false
}

// addMissingCommas adds commas between elements where missing
// NOTE: This assumes string values don't contain escaped quotes (")
// This is safe for iotping configs where values are device names, IPs,
// tokens, and file paths - none of which contain quotes
func addMissingCommas(data []byte) ([]byte, []int) {
	result := make([]byte, 0, len(data))
	changedLines := []int{}
	i := 0

	for i < len(data) {
		// Copy char first
		result = append(result, data[i])

		// After copying a closing quote, check if comma is needed
		if data[i] == '"' && i > 0 {
			// Check this is the closing quote of a string value (not a key)
			// Find the matching opening quote in result
			if findPrevMatchingQuote(result) {
				// Good - this ends a string. Check what comes next in original data
				nextIdx := i + 1
				for nextIdx < len(data) && (data[nextIdx] == ' ' || data[nextIdx] == '\t' || data[nextIdx] == '\n' || data[nextIdx] == '\r') {
					nextIdx++
				}
				// If next is " or { or [ or } or ], may need comma
				if nextIdx < len(data) && (data[nextIdx] == '"' || data[nextIdx] == '{' || data[nextIdx] == '[' || data[nextIdx] == '}' || data[nextIdx] == ']') {
					// Don't add if } or ] immediately follows (unless there's stuff in between)
					if data[nextIdx] != '}' && data[nextIdx] != ']' {
						// Only add if we don't already have a comma
						if len(result) >= 2 && result[len(result)-2] != ',' && result[len(result)-1] != ',' {
							// Verify this is actually preceded by value content (not just whitespace)
							prev := len(result) - 2
							for prev >= 0 && (result[prev] == ' ' || result[prev] == '\t' || result[prev] == '\n' || result[prev] == '\r') {
								prev--
							}
							if prev >= 0 && result[prev] != ',' {
								result = append(result, ',')
								changedLines = append(changedLines, countLines(result)+1)
							}
						}
					}
				}
			}
		}
		i++
	}
	return result, changedLines
}

func findPrevMatchingQuote(result []byte) bool {
	// Find the matching opening quote for the closing quote we just added
	// Look backwards to find the opening " that starts this string
	if len(result) < 3 {
		return false
	}
	quoteCount := 0
	for idx := len(result) - 1; idx >= 0; idx-- {
		if result[idx] == '"' {
			quoteCount++
			if quoteCount == 2 {
				// Found opening quote - check if this is a value (preceded by :)
				prev := idx - 1
				for prev >= 0 && (result[prev] == ' ' || result[prev] == '\t') {
					prev--
				}
				if prev >= 0 && result[prev] == ':' {
					return true
				}
				return false
			}
		}
	}
	return false
}

// fixTrailingCommas removes trailing commas before } or ]
func fixTrailingCommas(data []byte) ([]byte, []int) {
	result := make([]byte, 0, len(data))
	changedLines := []int{}
	i := 0
	lineNum := 1

	for i < len(data) {
		if data[i] == ',' {
			// Look ahead to find next non-whitespace/non-newline char
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				if data[j] == '\n' {
					lineNum++
				}
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				// This is a trailing comma - skip it
				changedLines = append(changedLines, lineNum)
				i++
				continue
			}
		}
		if data[i] == '\n' {
			lineNum++
		}
		result = append(result, data[i])
		i++
	}
	return result, changedLines
}

// expandPath expands ~ and $HOME at the start of a path
func expandPath(path, home string) string {
	if path == "" {
		return path
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	if strings.HasPrefix(path, "$HOME/") || strings.HasPrefix(path, "${HOME}/") {
		if strings.HasPrefix(path, "${HOME}/") {
			return filepath.Join(home, path[7:])
		}
		return filepath.Join(home, path[6:])
	}
	return path
}

func printHelp() {
	fmt.Println(`iotping - ICMP-based device monitor with Telegram notifications

Usage: iotping [options]

Options:
  -h, -help, --help         Show this help message and exit
  -c, -config, --config     Path to config file (default: ~/.config/iotping/config.json)

Configuration:
  Config file: ~/.config/iotping/config.json

  Example configuration:
    {
      "devices": {
        "DEVICE1": "192.168.1.10",
        "DEVICE2": "192.168.1.11"
      },
      "telegram-token": "YOUR_BOT_TOKEN",
      "telegram-chat-id": "YOUR_CHAT_ID",
      "interval": "60s",
      "failure-threshold": 3,
      "recovery-notify": true,
      "ping-timeout": "5s",
      "debug": false,
      "log-file": ""
    }

  Configuration options:
    devices            - Map of device names to IP addresses
    telegram-token     - Telegram bot token (from @BotFather)
    telegram-chat-id   - Your Telegram chat ID
    interval           - Check interval (default: 60s)
    failure-threshold  - Failed pings before marking offline (default: 3)
    recovery-notify    - Notify when device recovers (default: true)
    ping-timeout       - Timeout for each ping (default: 5s)
    debug              - Enable verbose logging (default: false)
    log-file           - Path to log file (optional)
    repeat-interval    - Interval between repeat notifications (default: 1h)
    max-repeat-notifications - Max repeat notifications while offline (default: 3)

Hot Reload:
  The config file is watched for changes. When modified, the program waits
  10 seconds (debouncing) before reloading. If the file changes again during
  this period, the timer resets. Invalid configs are ignored.

Single Instance:
  Only one instance of iotping can run at a time. A PID file is created at
  ~/.config/iotping/iotping.pid to prevent multiple instances.

Examples:
  iotping              # Run with default config
  iotping --help       # Show this help`)
}

func parseArgs() string {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get home directory: %v", err)
	}
	configDir := filepath.Join(home, ".config", "iotping")
	configPath := filepath.Join(configDir, "config.json")

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "-help", "--help":
			printHelp()
			os.Exit(0)
		case "-c", "-config", "--config":
			if i+1 < len(args) {
				configPath = args[i+1]
				i++
			} else {
				log.Fatal("Error: -config requires a path argument")
			}
		}
	}
	return configPath
}

func main() {
	configPath := parseArgs()

	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Failed to get home directory: %v", err)
	}

	// Acquire PID lock to prevent multiple instances
	pidFile := NewPidFile(filepath.Join(home, ".config", "iotping", "iotping.pid"))
	if err := pidFile.Acquire(); err != nil {
		log.Fatalf("Error: %v", err)
	}
	defer pidFile.Release()

	cfg := loadConfig(configPath)

	// Setup log file if specified
	var logFile *os.File
	if cfg.LogFile != "" {
		logFilePath := expandPath(cfg.LogFile, home)
		var err error
		logFile, err = os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Fatalf("Failed to open log file %s: %v", logFilePath, err)
		}
		defer logFile.Close()
		log.SetOutput(logFile)
	}

	if cfg.TelegramToken == "" || cfg.TelegramChatID == "" {
		log.Println("Warning: Telegram not configured, running in dry-run mode")
	}

	mon := NewMonitor(cfg)
	if !mon.icmpAvail {
		// ICMP is required; exit with a non-zero status. os.Exit skips defers,
		// so clean up the PID file and log file explicitly first.
		pidFile.Release()
		if logFile != nil {
			logFile.Close()
		}
		log.Println("Exiting: ICMP unavailable")
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigCh
		log.Println("Shutting down...")
		cancel()
	}()

	log.Printf("Starting monitor for %d devices...", len(cfg.Devices))
	log.Printf("ICMP available: %v", mon.icmpAvail)

	// Start config file watcher
	watcher := NewConfigWatcher(configPath, mon.reloadCh)
	go watcher.Start(ctx)

	mon.Run(ctx)
}

// ConfigWatcher handles file watching with debouncing
type ConfigWatcher struct {
	path     string
	watcher  *fsnotify.Watcher
	debounce *time.Timer
	mu       sync.Mutex
	reloadCh chan<- Config
}

func NewConfigWatcher(path string, reloadCh chan<- Config) *ConfigWatcher {
	return &ConfigWatcher{
		path:     path,
		reloadCh: reloadCh,
	}
}

func (cw *ConfigWatcher) Start(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Failed to create file watcher: %v", err)
		return
	}
	cw.watcher = watcher
	defer watcher.Close()

	// Watch the config file's directory (to handle renames/atomic writes)
	configDir := filepath.Dir(cw.path)
	if err := watcher.Add(configDir); err != nil {
		log.Printf("Failed to watch config directory %s: %v", configDir, err)
		return
	}

	log.Printf("Watching config file: %s", cw.path)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			// Check if the event is for our config file
			if event.Name == cw.path {
				// Handle Rename too: editors often save atomically by renaming a
				// temp file over the config (vim-style), which only fires Rename.
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
					cw.handleChange()
				}
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("File watcher error: %v", err)
		case <-ctx.Done():
			return
		}
	}
}

func (cw *ConfigWatcher) handleChange() {
	cw.mu.Lock()
	defer cw.mu.Unlock()

	// Reset existing timer if any
	if cw.debounce != nil {
		cw.debounce.Stop()
	}

	// Start new 10-second timer
	cw.debounce = time.AfterFunc(10*time.Second, func() {
		cw.triggerReload()
	})

	log.Println("Config file changed, waiting 10s for debounce...")
}

func (cw *ConfigWatcher) triggerReload() {
	// Validate config before reloading
	cfg, err := tryLoadConfig(cw.path)
	if err != nil {
		log.Printf("Config reload skipped: invalid config file: %v", err)
		return
	}

	// Send valid config to monitor (non-blocking)
	select {
	case cw.reloadCh <- cfg:
		// Success
	default:
		log.Println("Config reload channel full, dropping reload request")
	}
}

// tryLoadConfig loads and validates the config file. Returns an error if the
// file is missing or invalid. On success the returned Config always has
// positive durations and safe defaults for any unset field.
func tryLoadConfig(path string) (Config, error) {
	cfg := Config{
		Devices:                map[string]string{},
		Interval:               60 * time.Second,
		FailureThreshold:       3,
		RecoveryNotify:         true,
		PingTimeout:            5 * time.Second,
		RepeatInterval:         60 * time.Minute,
		MaxRepeatNotifications: 3,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	// Process JSON: strip comments, add missing commas, fix trailing commas
	data = stripComments(data)

	data, missingLines := addMissingCommas(data)
	if len(missingLines) > 0 {
		log.Printf("[iotping] Warning: Missing commas added (lines: %v)", missingLines)
	}

	data, trailingLines := fixTrailingCommas(data)
	if len(trailingLines) > 0 {
		log.Printf("[iotping] Warning: Trailing commas removed (lines: %v)", trailingLines)
	}

	// Use a temporary struct to parse JSON with string durations.
	// Supports both hyphen and underscore variants for backward compatibility.
	// Pointer fields distinguish "not set" from an explicit zero value.
	type configJSON struct {
		Devices                 map[string]string `json:"devices"`
		TelegramToken           string            `json:"telegram-token"`
		TelegramToken_          string            `json:"telegram_token"`
		TelegramChatID          string            `json:"telegram-chat-id"`
		TelegramChatID_         string            `json:"telegram_chat_id"`
		Interval                string            `json:"interval"`
		FailureThreshold        int               `json:"failure-threshold"`
		FailureThreshold_       int               `json:"failure_threshold"`
		RecoveryNotify          bool              `json:"recovery-notify"`
		RecoveryNotify_         bool              `json:"recovery_notify"`
		PingTimeout             string            `json:"ping-timeout"`
		PingTimeout_            string            `json:"ping_timeout"`
		Debug                   bool              `json:"debug"`
		LogFile                 string            `json:"log-file"`
		LogFile_                string            `json:"log_file"`
		RepeatInterval          string            `json:"repeat-interval"`
		RepeatInterval_         string            `json:"repeat_interval"`
		MaxRepeatNotifications  *int              `json:"max-repeat-notifications"`
		MaxRepeatNotifications_ *int              `json:"max_repeat_notifications"`
	}

	var cj configJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return cfg, err
	}

	// Copy values (underscore variants take precedence if set)
	cfg.Devices = cj.Devices
	cfg.TelegramToken = firstNonEmpty(cj.TelegramToken, cj.TelegramToken_)
	cfg.TelegramChatID = firstNonEmpty(cj.TelegramChatID, cj.TelegramChatID_)
	cfg.FailureThreshold = cj.FailureThreshold
	if cj.FailureThreshold_ != 0 {
		cfg.FailureThreshold = cj.FailureThreshold_
	}
	cfg.RecoveryNotify = cj.RecoveryNotify
	if cj.RecoveryNotify_ {
		cfg.RecoveryNotify = cj.RecoveryNotify_
	}
	cfg.Debug = cj.Debug
	cfg.LogFile = firstNonEmpty(cj.LogFile, cj.LogFile_)

	cfg.Interval = parseDurationField(cj.Interval, cfg.Interval, "interval")
	cfg.PingTimeout = parseDurationField(firstNonEmpty(cj.PingTimeout, cj.PingTimeout_), cfg.PingTimeout, "ping-timeout")
	cfg.RepeatInterval = parseDurationField(firstNonEmpty(cj.RepeatInterval, cj.RepeatInterval_), cfg.RepeatInterval, "repeat-interval")

	// Pointer fields: an explicit 0 ("0 = disabled") must be honored; the old
	// != 0 guard silently clobbered it back to the default.
	if cj.MaxRepeatNotifications_ != nil {
		cfg.MaxRepeatNotifications = *cj.MaxRepeatNotifications_
	} else if cj.MaxRepeatNotifications != nil {
		cfg.MaxRepeatNotifications = *cj.MaxRepeatNotifications
	}

	return cfg, nil
}

func NewMonitor(cfg Config) *Monitor {
	m := &Monitor{
		config:   cfg,
		states:   make(map[string]*DeviceState),
		client:   &http.Client{Timeout: 10 * time.Second},
		reloadCh: make(chan Config, 1),
	}
	// Note: State file removed - fresh start every time

	// Test ICMP availability once at startup
	m.icmpAvail = m.testICMP()

	return m
}

// Reload updates the config, preserving state for devices that are still
// configured. Resetting states would make a still-down device re-send its
// OFFLINE notification and lose its downtime history.
func (m *Monitor) Reload(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg

	newStates := make(map[string]*DeviceState, len(cfg.Devices))
	for name := range cfg.Devices {
		if st, ok := m.states[name]; ok {
			newStates[name] = st
		}
	}
	m.states = newStates
}

// testICMP checks if unprivileged ICMP works on this system
func (m *Monitor) testICMP() bool {
	// Try to create an unprivileged ICMP socket
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		log.Printf("ERROR: Unprivileged ICMP not available: %v", err)
		log.Println("")
		log.Println("To fix this, choose one of the following options:")
		log.Println("")
		log.Println("  1. Allow unprivileged ICMP (recommended - most secure):")
		log.Println("     Temporary: sudo sysctl -w net.ipv4.ping_group_range='0 2147483647'")
		log.Println("     Permanent: echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-ping.conf")
		log.Println("                sudo sysctl --system")
		log.Println("")
		log.Println("  2. Grant CAP_NET_RAW capability to the binary:")
		log.Println("     sudo setcap cap_net_raw+ep ./iotping")
		log.Println("     (Note: must be re-run after each recompile)")
		log.Println("")
		log.Println("  3. Run as root (not recommended for security)")
		return false
	}
	conn.Close()
	return true
}

func (m *Monitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()

	queueTicker := time.NewTicker(5 * time.Minute)
	defer queueTicker.Stop()

	m.checkAll(ctx)

	for {
		select {
		case <-ticker.C:
			m.checkAll(ctx)
		case <-queueTicker.C:
			m.flushQueue(m.config)
		case newCfg := <-m.reloadCh:
			// Config already validated by watcher before sending
			ticker.Stop()
			queueTicker.Stop()
			m.Reload(newCfg)
			ticker = time.NewTicker(m.config.Interval)
			queueTicker = time.NewTicker(5 * time.Minute)
			log.Println("Config reloaded")
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	// Snapshot the config under the read lock: a concurrent reload must not
	// race with the lock-free reads done by the worker goroutines.
	m.mu.RLock()
	cfg := m.config
	m.mu.RUnlock()

	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)

	for name, ip := range cfg.Devices {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(name, ip string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			m.checkDevice(ctx, cfg, name, ip)
		}(name, ip)
	}

	wg.Wait()
}

func (m *Monitor) checkDevice(ctx context.Context, cfg Config, name, ip string) {
	m.mu.Lock()
	state, exists := m.states[name]
	if !exists {
		// First time seeing this device - start as online
		state = &DeviceState{IsOnline: true, LastChange: time.Now()}
		m.states[name] = state
	}
	m.mu.Unlock()

	// Use ICMP only
	isOnline := m.checkICMP(ctx, ip, cfg)

	// Update state under the lock, but build the notification outside of it so
	// sendTelegram (network I/O) never blocks other device checks.
	m.mu.Lock()
	var notifyMsg string
	notifyNow, queueOnFailure := false, false
	if !isOnline {
		state.Failures++
		if state.Failures >= cfg.FailureThreshold && state.IsOnline {
			state.IsOnline = false
			state.LastChange = time.Now()
			state.LastNotified = time.Now()
			state.NotifyCount = 1
			eventTime := state.LastChange.Format("15:04")
			log.Printf("[iotping] %s is OFFLINE", name)
			notifyNow, queueOnFailure = true, true
			notifyMsg = fmt.Sprintf("🚨 %s OFFLINE (%s)", name, eventTime)
		} else if !state.IsOnline && state.NotifyCount > 0 && state.NotifyCount < cfg.MaxRepeatNotifications {
			timeSinceLastNotify := time.Since(state.LastNotified)
			if timeSinceLastNotify >= cfg.RepeatInterval {
				state.NotifyCount++
				state.LastNotified = time.Now()
				downtime := time.Since(state.LastChange)
				eventTime := state.LastChange.Format("15:04")
				log.Printf("[iotping] %s still offline (%s) - repeat notification %d/%d", name, downtime.Round(time.Minute), state.NotifyCount, cfg.MaxRepeatNotifications)
				notifyNow = true
				if state.NotifyCount >= cfg.MaxRepeatNotifications {
					notifyMsg = fmt.Sprintf("🚨 %s STILL OFFLINE (%s) (%s) - no more notifications", name, downtime.Round(time.Minute), eventTime)
				} else {
					notifyMsg = fmt.Sprintf("🚨 %s STILL OFFLINE (%s) (%s)", name, downtime.Round(time.Minute), eventTime)
				}
			}
		}
	} else {
		if !state.IsOnline && cfg.RecoveryNotify {
			downtime := time.Since(state.LastChange)
			eventTime := state.LastChange.Format("15:04")
			log.Printf("[iotping] %s is BACK ONLINE (downtime: %s)", name, downtime.Round(time.Second))
			notifyNow, queueOnFailure = true, true
			notifyMsg = fmt.Sprintf("✅ %s ONLINE (%s) (%s)", name, downtime.Round(time.Second), eventTime)
		}
		if state.Failures > 0 {
			debugf(cfg.Debug, "[%s] Back online (was down %d checks)", name, state.Failures)
		}
		state.Failures = 0
		state.IsOnline = true
		state.NotifyCount = 0
	}
	m.mu.Unlock()

	if notifyNow {
		m.notify(notifyMsg, queueOnFailure, cfg)
	}
}

// randomToken returns a random hex string used to tag ICMP echo requests so
// replies can be verified as ours.
func randomToken() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (m *Monitor) checkICMP(ctx context.Context, ip string, cfg Config) bool {
	dst, err := net.ResolveIPAddr("ip4", ip)
	if err != nil {
		return false
	}

	// Use unprivileged ICMP (UDP socket) - requires kernel support
	conn, err := icmp.ListenPacket("udp4", "0.0.0.0")
	if err != nil {
		log.Printf("[ICMP] Failed to create socket: %v", err)
		return false
	}
	defer conn.Close()

	// Unique token + random seq so a stale or unrelated echo reply is never
	// mistaken for a response from the device we pinged. Note: with unprivileged
	// UDP ping the kernel fills the ICMP ID, so replies are matched by payload.
	token := randomToken()
	echo := &icmp.Echo{
		ID:   os.Getpid() & 0xffff,
		Seq:  mrand.IntN(0x10000),
		Data: []byte(token),
	}
	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: echo,
	}
	data, err := msg.Marshal(nil)
	if err != nil {
		return false
	}

	debugf(cfg.Debug, "[ICMP] PING -> %s", ip)
	udpDst := &net.UDPAddr{IP: dst.IP, Zone: dst.Zone}
	if _, err := conn.WriteTo(data, udpDst); err != nil {
		debugf(cfg.Debug, "[ICMP] PING send failed to %s: %v", ip, err)
		return false
	}

	// Close the socket when the context is cancelled so the blocked read
	// returns immediately during shutdown.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()

	reply := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(cfg.PingTimeout))
	n, peer, err := conn.ReadFrom(reply)
	if err != nil {
		debugf(cfg.Debug, "[ICMP] PING timeout/no reply from %s: %v", ip, err)
		return false
	}

	rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
	if err != nil {
		debugf(cfg.Debug, "[ICMP] PING invalid reply from %s: %v", ip, err)
		return false
	}

	// Verify the reply is an echo reply from the target carrying our token.
	if rm.Type != ipv4.ICMPTypeEchoReply {
		debugf(cfg.Debug, "[ICMP] PING unexpected reply type from %s: %v", ip, rm.Type)
		return false
	}
	tokenBytes := []byte(token)
	body, ok := rm.Body.(*icmp.Echo)
	if !ok || !bytes.Equal(body.Data, tokenBytes) {
		debugf(cfg.Debug, "[ICMP] PING reply from %s did not match our request", ip)
		return false
	}
	if peerAddr, ok := peer.(*net.UDPAddr); !ok || !peerAddr.IP.Equal(dst.IP) {
		debugf(cfg.Debug, "[ICMP] PING reply from unexpected source: %v", peer)
		return false
	}

	debugf(cfg.Debug, "[ICMP] PONG <- %s (from %s)", ip, peer)
	return true
}

const maxQueueSize = 25

func (m *Monitor) notify(message string, queueOnFailure bool, cfg Config) {
	if cfg.TelegramToken == "" {
		log.Printf("[iotping DRY RUN] %s", message)
		return
	}

	// Try to flush any queued messages first
	m.flushQueue(cfg)

	// Try to send the current message
	if m.sendTelegram(message, cfg) {
		// Success - try to flush queue again in case more were added
		m.flushQueue(cfg)
	} else if queueOnFailure {
		// Failed - add to queue (only for important messages)
		m.queueMu.Lock()
		if len(m.msgQueue) >= maxQueueSize {
			// Drop oldest message
			m.msgQueue = m.msgQueue[1:]
			log.Printf("[iotping] Queue full, dropping oldest message")
		}
		m.msgQueue = append(m.msgQueue, message)
		queueLen := len(m.msgQueue)
		m.queueMu.Unlock()
		log.Printf("[iotping] Message queued (network error) - queue size: %d", queueLen)
	}
}

func (m *Monitor) sendTelegram(message string, cfg Config) bool {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", cfg.TelegramToken)
	payload := map[string]string{
		"chat_id": cfg.TelegramChatID,
		"text":    message,
	}

	jsonData, err := json.Marshal(payload)
	if err != nil {
		log.Printf("[iotping] Failed to encode Telegram message: %v", err)
		return false
	}
	resp, err := m.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Drain the body so the connection can be reused; also surfaces API errors.
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[iotping] Telegram API error: %s", resp.Status)
		return false
	}
	return true
}

func (m *Monitor) flushQueue(cfg Config) {
	m.queueMu.Lock()
	if len(m.msgQueue) == 0 {
		m.queueMu.Unlock()
		return
	}

	// Copy queue and clear it
	queue := make([]string, len(m.msgQueue))
	copy(queue, m.msgQueue)
	m.msgQueue = m.msgQueue[:0]
	m.queueMu.Unlock()

	// Try to send all queued messages
	sent := 0
	for _, msg := range queue {
		if m.sendTelegram(msg, cfg) {
			sent++
		} else {
			// Put the unsent remainder back in the queue, preserving order.
			m.queueMu.Lock()
			m.msgQueue = append(queue[sent:], m.msgQueue...)
			m.queueMu.Unlock()
			if sent > 0 {
				log.Printf("[iotping] Sent %d queued messages, %d remain", sent, len(queue)-sent)
			}
			return
		}
	}

	if sent > 0 {
		log.Printf("[iotping] Sent %d queued messages", sent)
	}
}

func loadConfig(path string) Config {
	cfg, err := tryLoadConfig(path)
	if err != nil {
		log.Fatalf("Failed to load config file %s: %v", path, err)
	}
	return cfg
}

// firstNonEmpty returns the first non-empty string argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseDurationField parses a duration from config; invalid or non-positive
// values fall back to `fallback` with a warning instead of crashing later
// (e.g. time.NewTicker(0) panics).
func parseDurationField(s string, fallback time.Duration, name string) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		log.Printf("[iotping] Warning: invalid %s %q, using default %s", name, s, fallback)
		return fallback
	}
	if d <= 0 {
		log.Printf("[iotping] Warning: %s must be positive (%q), using default %s", name, s, fallback)
		return fallback
	}
	return d
}
