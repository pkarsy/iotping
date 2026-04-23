package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	// Check if PID file exists
	if data, err := os.ReadFile(p.path); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 && isProcessRunning(pid) {
			return fmt.Errorf("another instance is already running (PID: %d)", pid)
		}
		// Stale PID file, remove it
		os.Remove(p.path)
	}

	// Ensure directory exists
	dir := filepath.Dir(p.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create PID file directory: %v", err)
	}

	// Write our PID
	pid := os.Getpid()
	return os.WriteFile(p.path, []byte(strconv.Itoa(pid)+"\n"), 0644)
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

// debug logs a message only if debug mode is enabled
func (m *Monitor) debug(format string, v ...interface{}) {
	if m.config.Debug {
		log.Printf("[DEBUG] "+format, v...)
	}
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
  iotping --help       # Show this help
`)
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
	if cfg.LogFile != "" {
		logFilePath := expandPath(cfg.LogFile, home)
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
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
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
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

// tryLoadConfig attempts to load config, returns error if invalid
func tryLoadConfig(path string) (Config, error) {
	cfg := Config{
		Devices:          map[string]string{},
		Interval:         60 * time.Second,
		FailureThreshold: 3,
		RecoveryNotify:   true,
		PingTimeout:      5 * time.Second,
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	type configJSON struct {
		Devices          map[string]string `json:"devices"`
		TelegramToken    string            `json:"telegram-token"`
		TelegramChatID   string            `json:"telegram-chat-id"`
		Interval         string            `json:"interval"`
		FailureThreshold int               `json:"failure-threshold"`
		RecoveryNotify   bool              `json:"recovery-notify"`
		PingTimeout      string            `json:"ping-timeout"`
		Debug            bool              `json:"debug"`
		LogFile          string            `json:"log-file"`
	}

	var cj configJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return cfg, err
	}

	cfg.Devices = cj.Devices
	cfg.TelegramToken = cj.TelegramToken
	cfg.TelegramChatID = cj.TelegramChatID
	cfg.FailureThreshold = cj.FailureThreshold
	cfg.RecoveryNotify = cj.RecoveryNotify
	cfg.Debug = cj.Debug
	cfg.LogFile = cj.LogFile

	if cj.Interval != "" {
		if d, err := time.ParseDuration(cj.Interval); err == nil {
			cfg.Interval = d
		}
	}
	if cj.PingTimeout != "" {
		if d, err := time.ParseDuration(cj.PingTimeout); err == nil {
			cfg.PingTimeout = d
		}
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

// Reload updates the config and resets all device states
func (m *Monitor) Reload(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config = cfg
	m.states = make(map[string]*DeviceState) // Reset all counters
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
		log.Println("     sudo setcap cap_net_raw+ep ./iotpinger")
		log.Println("     (Note: must be re-run after each recompile)")
		log.Println("")
		log.Println("  3. Run as root (not recommended for security)")
		os.Exit(1)
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
			m.flushQueue()
		case newCfg := <-m.reloadCh:
			// Config already validated by watcher before sending
			ticker.Stop()
			queueTicker.Stop()
			m.Reload(newCfg)
			ticker = time.NewTicker(m.config.Interval)
			queueTicker = time.NewTicker(5 * time.Minute)
			log.Println("Config reloaded, all counters reset")
		case <-ctx.Done():
			return
		}
	}
}

func (m *Monitor) checkAll(ctx context.Context) {
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 10)

	for name, ip := range m.config.Devices {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(name, ip string) {
			defer wg.Done()
			defer func() { <-semaphore }()
			m.checkDevice(ctx, name, ip)
		}(name, ip)
	}

	wg.Wait()
}

func (m *Monitor) checkDevice(ctx context.Context, name, ip string) {
	m.mu.Lock()
	state, exists := m.states[name]
	if !exists {
		// First time seeing this device - start as online
		state = &DeviceState{IsOnline: true, LastChange: time.Now()}
		m.states[name] = state
	}
	m.mu.Unlock()

	// Use ICMP only
	isOnline := m.checkICMP(ip)

	m.mu.Lock()
	defer m.mu.Unlock()

	if !isOnline {
		state.Failures++
		if state.Failures >= m.config.FailureThreshold && state.IsOnline {
			state.IsOnline = false
			state.LastChange = time.Now()
			state.LastNotified = time.Now()
			state.NotifyCount = 1
			eventTime := state.LastChange.Format("15:04")
			m.mu.Unlock()
			msg := fmt.Sprintf("🚨 %s OFFLINE (%s)", name, eventTime)
			log.Printf("[iotping] %s is OFFLINE", name)
			m.notify(msg)
			m.mu.Lock()
		} else if !state.IsOnline && state.NotifyCount > 0 && state.NotifyCount < m.config.MaxRepeatNotifications {
			timeSinceLastNotify := time.Since(state.LastNotified)
			if timeSinceLastNotify >= m.config.RepeatInterval {
				state.NotifyCount++
				state.LastNotified = time.Now()
				downtime := time.Since(state.LastChange)
				eventTime := state.LastChange.Format("15:04")
				m.mu.Unlock()
				var msg string
				if state.NotifyCount >= m.config.MaxRepeatNotifications {
					msg = fmt.Sprintf("🚨 %s STILL OFFLINE (%s) (%s) - no more notifications", name, downtime.Round(time.Minute), eventTime)
				} else {
					msg = fmt.Sprintf("🚨 %s STILL OFFLINE (%s) (%s)", name, downtime.Round(time.Minute), eventTime)
				}
				log.Printf("[iotping] %s still offline (%s) - repeat notification %d/%d", name, downtime.Round(time.Minute), state.NotifyCount, m.config.MaxRepeatNotifications)
				m.notify(msg)
				m.mu.Lock()
			}
		}
	} else {
		if !state.IsOnline && m.config.RecoveryNotify {
			downtime := time.Since(state.LastChange)
			eventTime := state.LastChange.Format("15:04")
			m.mu.Unlock()
			msg := fmt.Sprintf("✅ %s ONLINE (%s) (%s)", name, downtime.Round(time.Second), eventTime)
			log.Printf("[iotping] %s is BACK ONLINE (downtime: %s)", name, downtime.Round(time.Second))
			m.notify(msg)
			m.mu.Lock()
		}
		if state.Failures > 0 {
			m.debug("[%s] Back online (was down %d checks)", name, state.Failures)
		}
		state.Failures = 0
		state.IsOnline = true
		state.NotifyCount = 0
	}
}

func (m *Monitor) checkICMP(ip string) bool {
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

	msg := &icmp.Message{
		Type: ipv4.ICMPTypeEcho,
		Code: 0,
		Body: &icmp.Echo{
			ID:   os.Getpid() & 0xffff,
			Seq:  1,
			Data: []byte("monitor"),
		},
	}
	data, err := msg.Marshal(nil)
	if err != nil {
		return false
	}

	m.debug("[ICMP] PING -> %s", ip)
	udpDst := &net.UDPAddr{IP: dst.IP, Zone: dst.Zone}
	if _, err := conn.WriteTo(data, udpDst); err != nil {
		m.debug("[ICMP] PING send failed to %s: %v", ip, err)
		return false
	}

	reply := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(m.config.PingTimeout))
	n, peer, err := conn.ReadFrom(reply)
	if err != nil {
		m.debug("[ICMP] PING timeout/no reply from %s: %v", ip, err)
		return false
	}

	rm, err := icmp.ParseMessage(ipv4.ICMPTypeEchoReply.Protocol(), reply[:n])
	if err != nil {
		m.debug("[ICMP] PING invalid reply from %s: %v", ip, err)
		return false
	}

	if rm.Type == ipv4.ICMPTypeEchoReply {
		m.debug("[ICMP] PONG <- %s (from %s)", ip, peer)
		return true
	}
	m.debug("[ICMP] PING unexpected reply type from %s: %v", ip, rm.Type)
	return false
}

const maxQueueSize = 25

func (m *Monitor) notify(message string) {
	if m.config.TelegramToken == "" {
		log.Printf("[iotping DRY RUN] %s", message)
		return
	}

	// Try to flush any queued messages first
	m.flushQueue()

	// Try to send the current message
	if m.sendTelegram(message) {
		// Success - try to flush queue again in case more were added
		m.flushQueue()
	} else {
		// Failed - add to queue
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

func (m *Monitor) sendTelegram(message string) bool {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", m.config.TelegramToken)
	payload := map[string]string{
		"chat_id":    m.config.TelegramChatID,
		"text":       message,
		"parse_mode": "Markdown",
	}

	jsonData, _ := json.Marshal(payload)
	resp, err := m.client.Post(url, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}
	return true
}

func (m *Monitor) flushQueue() {
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
		if m.sendTelegram(msg) {
			sent++
		} else {
			// Put remaining messages back in queue
			m.queueMu.Lock()
			m.msgQueue = append([]string{msg}, m.msgQueue...)
			for _, remaining := range queue[sent+1:] {
				m.msgQueue = append([]string{remaining}, m.msgQueue...)
			}
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
	// Default config
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
		log.Fatalf("Failed to read config file %s: %v", path, err)
	}

	// Use a temporary struct to parse JSON with string durations
	// Supports both hyphen and underscore variants for backward compatibility
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
		MaxRepeatNotifications  int               `json:"max-repeat-notifications"`
		MaxRepeatNotifications_ int               `json:"max_repeat_notifications"`
	}

	var cj configJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		log.Fatalf("Failed to parse config file: %v", err)
	}

	// Copy values (underscore variants take precedence if set)
	cfg.Devices = cj.Devices
	cfg.TelegramToken = cj.TelegramToken
	if cj.TelegramToken_ != "" {
		cfg.TelegramToken = cj.TelegramToken_
	}
	cfg.TelegramChatID = cj.TelegramChatID
	if cj.TelegramChatID_ != "" {
		cfg.TelegramChatID = cj.TelegramChatID_
	}
	cfg.FailureThreshold = cj.FailureThreshold
	if cj.FailureThreshold_ != 0 {
		cfg.FailureThreshold = cj.FailureThreshold_
	}
	cfg.RecoveryNotify = cj.RecoveryNotify
	if cj.RecoveryNotify_ {
		cfg.RecoveryNotify = cj.RecoveryNotify_
	}
	cfg.Debug = cj.Debug
	cfg.LogFile = cj.LogFile
	if cj.LogFile_ != "" {
		cfg.LogFile = cj.LogFile_
	}

	if cj.Interval != "" {
		if d, err := time.ParseDuration(cj.Interval); err == nil {
			cfg.Interval = d
		}
	}
	if cj.PingTimeout != "" {
		if d, err := time.ParseDuration(cj.PingTimeout); err == nil {
			cfg.PingTimeout = d
		}
	}
	cfg.MaxRepeatNotifications = cj.MaxRepeatNotifications
	if cj.MaxRepeatNotifications_ != 0 {
		cfg.MaxRepeatNotifications = cj.MaxRepeatNotifications_
	}

	repeatIntervalStr := cj.RepeatInterval
	if cj.RepeatInterval_ != "" {
		repeatIntervalStr = cj.RepeatInterval_
	}
	if repeatIntervalStr != "" {
		if d, err := time.ParseDuration(repeatIntervalStr); err == nil {
			cfg.RepeatInterval = d
		}
	}

	return cfg
}
