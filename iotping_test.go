package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStripComments(t *testing.T) {
	input := []byte(`{
  // line comment
  "devices": {
    "A": "192.168.1.10", /* block comment */
    "B": "192.168.1.11"  // trailing
  },
  "token": "abc//def",
  "path": "/etc/foo/*/bar"
}`)
	got := string(stripComments(input))
	for _, want := range []string{
		`"A": "192.168.1.10",`,
		`"B": "192.168.1.11"`,
		`"abc//def"`,       // comment markers inside strings are data
		`"/etc/foo/*/bar"`, // block-comment marker inside a string survives
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stripComments output missing %q:\n%s", want, got)
		}
	}
	// Output must still parse as JSON once trailing commas are fixed.
	data, _ := fixTrailingCommas([]byte(got))
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Errorf("stripped output is not valid JSON: %v\n%s", err, data)
	}
}

func TestAddMissingCommas(t *testing.T) {
	input := []byte(`{
  "devices": {
    "A": "192.168.1.10"
    "B": "192.168.1.11"
  },
  "interval": "60s"
}`)
	got, changed := addMissingCommas(input)
	if len(changed) == 0 {
		t.Fatal("expected missing commas to be detected")
	}
	var v map[string]interface{}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Errorf("output is not valid JSON: %v\n%s", err, got)
	}
}

func TestAddMissingCommasValidJSONUnchanged(t *testing.T) {
	input := []byte(`{"a": "1", "b": "2"}`)
	got, changed := addMissingCommas(input)
	if len(changed) != 0 {
		t.Errorf("unexpected changes for valid JSON: %v", changed)
	}
	if string(got) != string(input) {
		t.Errorf("valid JSON was modified:\n%s", got)
	}
}

func TestFixTrailingCommas(t *testing.T) {
	input := []byte(`{
  "a": "1",
  "b": "2",
}`)
	got, changed := fixTrailingCommas(input)
	if len(changed) == 0 {
		t.Fatal("expected trailing comma to be detected")
	}
	var v map[string]interface{}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Errorf("output is not valid JSON: %v", err)
	}
}

func TestJSONChainCommentsAndMissingCommas(t *testing.T) {
	// Combines all three munging steps, as loadConfig does.
	input := []byte(`{
  // comment
  "devices": {
    "D1": "192.168.1.10"
    "D2": "192.168.1.11"
  },
  "telegram-token": "tok123",
}`)
	data := stripComments(input)
	data, _ = addMissingCommas(data)
	data, _ = fixTrailingCommas(data)
	var v map[string]interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("chain output invalid: %v\n%s", err, data)
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTryLoadConfigUnderscoreVariants(t *testing.T) {
	path := writeTempConfig(t, `{
  "devices": { "D1": "192.168.1.10" },
  "telegram_token": "tok-underscore",
  "telegram_chat_id": "chat-underscore",
  "interval": "90s",
  "ping_timeout": "2s",
  "repeat_interval": "45m",
  "max_repeat_notifications": 5
}`)
	cfg, err := tryLoadConfig(path)
	if err != nil {
		t.Fatalf("tryLoadConfig: %v", err)
	}
	if cfg.TelegramToken != "tok-underscore" {
		t.Errorf("TelegramToken = %q", cfg.TelegramToken)
	}
	if cfg.TelegramChatID != "chat-underscore" {
		t.Errorf("TelegramChatID = %q", cfg.TelegramChatID)
	}
	if cfg.Interval != 90*time.Second {
		t.Errorf("Interval = %v", cfg.Interval)
	}
	if cfg.PingTimeout != 2*time.Second {
		t.Errorf("PingTimeout = %v", cfg.PingTimeout)
	}
	if cfg.RepeatInterval != 45*time.Minute {
		t.Errorf("RepeatInterval = %v", cfg.RepeatInterval)
	}
	if cfg.MaxRepeatNotifications != 5 {
		t.Errorf("MaxRepeatNotifications = %d", cfg.MaxRepeatNotifications)
	}
}

func TestTryLoadConfigHyphenVariants(t *testing.T) {
	path := writeTempConfig(t, `{
  "devices": { "D1": "192.168.1.10" },
  "telegram-token": "tok-hyphen",
  "telegram-chat-id": "chat-hyphen",
  "max-repeat-notifications": 7
}`)
	cfg, err := tryLoadConfig(path)
	if err != nil {
		t.Fatalf("tryLoadConfig: %v", err)
	}
	if cfg.TelegramToken != "tok-hyphen" {
		t.Errorf("TelegramToken = %q", cfg.TelegramToken)
	}
	if cfg.TelegramChatID != "chat-hyphen" {
		t.Errorf("TelegramChatID = %q", cfg.TelegramChatID)
	}
	if cfg.MaxRepeatNotifications != 7 {
		t.Errorf("MaxRepeatNotifications = %d", cfg.MaxRepeatNotifications)
	}
}

func TestTryLoadConfigMaxRepeatZero(t *testing.T) {
	// README: max-repeat-notifications 0 = disabled; the old != 0 guard
	// clobbered an explicit 0 back to the default.
	path := writeTempConfig(t, `{
  "devices": {},
  "max-repeat-notifications": 0
}`)
	cfg, err := tryLoadConfig(path)
	if err != nil {
		t.Fatalf("tryLoadConfig: %v", err)
	}
	if cfg.MaxRepeatNotifications != 0 {
		t.Errorf("MaxRepeatNotifications = %d, want 0", cfg.MaxRepeatNotifications)
	}
}

func TestTryLoadConfigCommentsAndMissingCommas(t *testing.T) {
	path := writeTempConfig(t, `{
  // comment
  "devices": {
    "A": "192.168.1.10"
    "B": "192.168.1.11"
  },
  "telegram-token": "tok123",
}`)
	cfg, err := tryLoadConfig(path)
	if err != nil {
		t.Fatalf("tryLoadConfig: %v", err)
	}
	if len(cfg.Devices) != 2 {
		t.Errorf("Devices = %v", cfg.Devices)
	}
	if cfg.TelegramToken != "tok123" {
		t.Errorf("TelegramToken = %q", cfg.TelegramToken)
	}
}

func TestTryLoadConfigInvalidDurationsFallBack(t *testing.T) {
	// Zero/invalid durations must not crash (time.NewTicker(0) panics);
	// they fall back to defaults with a warning.
	path := writeTempConfig(t, `{
  "devices": {},
  "interval": "0s",
  "ping-timeout": "bogus",
  "repeat-interval": "-5m"
}`)
	cfg, err := tryLoadConfig(path)
	if err != nil {
		t.Fatalf("tryLoadConfig: %v", err)
	}
	if cfg.Interval != 60*time.Second {
		t.Errorf("Interval = %v, want default 60s", cfg.Interval)
	}
	if cfg.PingTimeout != 5*time.Second {
		t.Errorf("PingTimeout = %v, want default 5s", cfg.PingTimeout)
	}
	if cfg.RepeatInterval != 60*time.Minute {
		t.Errorf("RepeatInterval = %v, want default 1h", cfg.RepeatInterval)
	}
}

func TestTryLoadConfigMissingFile(t *testing.T) {
	if _, err := tryLoadConfig(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestExpandPath(t *testing.T) {
	home := "/home/test"
	cases := map[string]string{
		"":             "",
		"/etc/foo":     "/etc/foo",
		"~/logs/x.log": "/home/test/logs/x.log",
		"$HOME/x":      "/home/test/x",
		"${HOME}/x":    "/home/test/x",
	}
	for in, want := range cases {
		if got := expandPath(in, home); got != want {
			t.Errorf("expandPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "b", "c"); got != "b" {
		t.Errorf("firstNonEmpty(\"\", \"b\", \"c\") = %q", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(\"\", \"\") = %q", got)
	}
}

func TestParseDurationField(t *testing.T) {
	if got := parseDurationField("30s", time.Minute, "interval"); got != 30*time.Second {
		t.Errorf("parseDurationField(\"30s\") = %v", got)
	}
	if got := parseDurationField("", time.Minute, "interval"); got != time.Minute {
		t.Errorf("parseDurationField(\"\") = %v", got)
	}
	if got := parseDurationField("0s", time.Minute, "interval"); got != time.Minute {
		t.Errorf("parseDurationField(\"0s\") = %v", got)
	}
	if got := parseDurationField("bogus", time.Minute, "interval"); got != time.Minute {
		t.Errorf("parseDurationField(\"bogus\") = %v", got)
	}
}

func TestPidFileAcquireRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "iotping.pid")
	pf := NewPidFile(path)
	if err := pf.Acquire(); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	// A second instance must be rejected while we hold the lock.
	pf2 := NewPidFile(path)
	if err := pf2.Acquire(); err == nil {
		t.Fatal("second Acquire succeeded, want error")
	}
	pf.Release()
	// After release the lock is free again.
	if err := pf2.Acquire(); err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	pf2.Release()
}

func TestReloadPreservesDeviceStates(t *testing.T) {
	down := &DeviceState{Failures: 5, IsOnline: false, LastChange: time.Now().Add(-2 * time.Hour)}
	m := &Monitor{
		config: Config{Devices: map[string]string{"A": "192.168.1.10", "B": "192.168.1.11"}},
		states: map[string]*DeviceState{
			"A": down,
			"B": {IsOnline: true},
		},
		mu: sync.RWMutex{},
	}

	m.Reload(Config{Devices: map[string]string{"A": "192.168.1.10", "C": "192.168.1.12"}})

	m.mu.RLock()
	defer m.mu.RUnlock()
	if got, ok := m.states["A"]; !ok {
		t.Error("state for surviving device A was dropped")
	} else if got != down {
		t.Errorf("state for A not preserved: %+v", got)
	}
	if _, ok := m.states["B"]; ok {
		t.Error("state for removed device B should be dropped")
	}
	if _, ok := m.states["C"]; ok {
		t.Error("state for new device C should not exist yet")
	}
}
