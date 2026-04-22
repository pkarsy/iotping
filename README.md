# iotping

A simple ICMP-based device monitor with Telegram notifications. Pings devices at regular intervals and alerts you when they go offline or come back online.

## Features

- **ICMP-only monitoring** - Uses raw ping, no TCP port scanning. All commercial IOT devices and fo course all open source (Tasmota etc) work.
- **Telegram notifications** - Get alerts on your phone when devices go down
- **LAN-based monitoring** - Works when internet is down (as long as LAN is up). It cannot send telegram messages obviously, but it can check if devices are alive.
- **Superior in detecting problems over built-in IoT notifications** - Device "cloud" notifications often misfire due to internet connectivity issues; this method monitors locally and is more reliable. **Example**: For a WIFI plug(Tuya, Ewelink etc) connected to a freezer a network connection issue canot be distinguished from a power failure (using in App notifications), but iotping being a LAN device has no problem with this.
- **Repeated notifications** - Configurable and can be disabled 
- **Configurable via JSON** - Easy configuration file
- **Debug mode** - Optional verbose logging
- **Log file support** - Redirect output to file with `~` and `$HOME` expansion


## Prerequisites

**Static IP addresses required**: Your LAN router/DHCP must be configured to assign static IPs to your IoT devices. This tool monitors devices by IP address, not hostname.

**Use IP addresses in config**: Always use IP addresses (e.g., `192.168.1.10`) in the configuration file, not hostnames. Even if you have DNS/hostnames configured, use the IP addresses to avoid dependency on DNS resolution.

**The device running iotping (presumably you home server)** must be alive 24/7 and UPS powered.

## Installation

```bash
# Build
go build -ldflags="-s -w" -trimpath

# Or build statically (no CGO dependencies)
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath
```

## Configuration

Config file location: `~/.config/iotping/config.json`

Create the directory and config:

```bash
mkdir -p ~/.config/iotping
cat > ~/.config/iotping/config.json << 'EOF'
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
  "log-file": "~/logs/iotping.log"
}
EOF
```

### Config Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `devices` | object | `{}` | Device name → **IP address** mapping (use IPs, not hostnames) |
| `telegram-token` | string | `""` | Telegram bot token (get from @BotFather) |
| `telegram-chat-id` | string | `""` | Your Telegram chat ID |
| `interval` | string | `"60s"` | Check interval (e.g., `30s`, `5m`, `1h`) |
| `failure-threshold` | int | `3` | Failed pings before marking offline |
| `recovery-notify` | bool | `true` | Notify when device comes back online |
| `ping-timeout` | string | `"5s"` | Timeout for each ping |
| `debug` | bool | `false` | Enable verbose debug logging |
| `log-file` | string | `""` | Log file path (supports `~` and `$HOME`) |
| `repeat-interval` | string | `"1h"` | Interval between repeat notifications while offline |
| `max-repeat-notifications` | int | `3` | Maximum repeat notifications (0 = disabled) |

## Getting Telegram Credentials

1. **Create a bot**: Message [@BotFather](https://t.me/BotFather), send `/newbot`, follow instructions
2. **Get token**: BotFather will give you a token like `123456789:ABCdefGHIjklMNOpqrsTUVwxyz`
3. **Get chat ID**: Message [@userinfobot](https://t.me/userinfobot) or [@raw_data_bot](https://t.me/raw_data_bot) to get your chat ID

## Running

```bash
# Just run it
./iotping

# In background with setsid
setsid -f ./iotping &

# crontab -e
@reboot setsid -f path/to/iotping 
```

## System Requirements

The program requires unprivileged ICMP to be enabled (most modern Linux distributions):

```bash
# Check current setting
cat /proc/sys/net/ipv4/ping_group_range

# If it shows "1 0" or doesn't include your user group, fix it:
sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"

# Make permanent:
echo 'net.ipv4.ping_group_range = 0 2147483647' | sudo tee /etc/sysctl.d/99-ping.conf
sudo sysctl --system
```

If you can't enable unprivileged ICMP but have root access, you can use capabilities:

```bash
sudo setcap cap_net_raw+ep ./iotping
```

Or run as root (not recommended):

```bash
sudo ./iotping
```

## Systemd Service

Create `/etc/systemd/system/iotping.service`:

```ini
[Unit]
Description=IoT Device Monitor
After=network.target

[Service]
Type=simple
User=youruser
ExecStart=/path/to/iotping
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable iotping
sudo systemctl start iotping
sudo systemctl status iotping
```

## License

MIT
