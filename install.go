package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// printService writes a service definition to stdout and install instructions to
// stderr, so `autolock -install systemd > file` yields a clean file.
func printService(kind, configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.Abs(exe)
	cfg, _ := filepath.Abs(configPath)
	dir := filepath.Dir(cfg)

	switch kind {
	case "systemd":
		fmt.Fprintln(os.Stderr, "# install:")
		fmt.Fprintln(os.Stderr, "#   autolock -install systemd | sudo tee /etc/systemd/system/autolock.service")
		fmt.Fprintln(os.Stderr, "#   sudo systemctl daemon-reload && sudo systemctl enable --now autolock")
		fmt.Fprintf(os.Stdout, `[Unit]
Description=airlock - Airbnb smart-lock automation
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s -daemon -config %s
Restart=on-failure
RestartSec=30

[Install]
WantedBy=multi-user.target
`, dir, exe, cfg)

	case "initd":
		fmt.Fprintln(os.Stderr, "# install:")
		fmt.Fprintln(os.Stderr, "#   autolock -install initd | sudo tee /etc/init.d/autolock")
		fmt.Fprintln(os.Stderr, "#   sudo chmod +x /etc/init.d/autolock && sudo update-rc.d autolock defaults")
		fmt.Fprintln(os.Stderr, "#   sudo service autolock start")
		fmt.Fprintf(os.Stdout, `#!/bin/sh
### BEGIN INIT INFO
# Provides:          autolock
# Required-Start:    $network $remote_fs
# Required-Stop:     $network $remote_fs
# Default-Start:     2 3 4 5
# Default-Stop:      0 1 6
# Short-Description: airlock - Airbnb smart-lock automation
### END INIT INFO

NAME=autolock
DIR=%s
EXE=%s
CFG=%s
PIDFILE=/var/run/$NAME.pid

case "$1" in
  start)
    echo "Starting $NAME"
    start-stop-daemon --start --background --make-pidfile --pidfile "$PIDFILE" \
      --chdir "$DIR" --exec "$EXE" -- -daemon -config "$CFG"
    ;;
  stop)
    echo "Stopping $NAME"
    start-stop-daemon --stop --pidfile "$PIDFILE" --retry 10
    rm -f "$PIDFILE"
    ;;
  restart)
    "$0" stop
    "$0" start
    ;;
  status)
    start-stop-daemon --status --pidfile "$PIDFILE" && echo "running" || echo "stopped"
    ;;
  *)
    echo "Usage: $0 {start|stop|restart|status}"
    exit 1
    ;;
esac
`, dir, exe, cfg)

	case "cron":
		fmt.Fprintln(os.Stderr, "# install (one-shot hourly; the binary self-locks, so overlaps exit cleanly):")
		fmt.Fprintln(os.Stderr, "#   (crontab -l 2>/dev/null; autolock -install cron) | crontab -")
		fmt.Fprintf(os.Stdout, "0 * * * * cd %s && %s -config %s >> %s/autolock.log 2>&1\n",
			dir, exe, cfg, dir)

	default:
		return fmt.Errorf("unknown -install %q (want: systemd | initd | cron)", kind)
	}
	return nil
}
