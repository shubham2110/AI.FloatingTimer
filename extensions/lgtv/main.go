package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/snabb/webostv"
)

// getExeDir returns the absolute directory containing the executable.
func getExeDir() string {
	exePath, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exePath)
}

func parseCLIArgs() (ip string, port int, keyFile string, help bool, args []string) {
	port = 3001
	keyFile = filepath.Join(getExeDir(), "lgtv_key.txt")

	rawArgs := os.Args[1:]
	for i := 0; i < len(rawArgs); i++ {
		arg := rawArgs[i]
		switch {
		case arg == "-h" || arg == "--h" || arg == "-help" || arg == "--help":
			help = true
		case arg == "-ip" || arg == "--ip" || arg == "-host" || arg == "--host":
			if i+1 < len(rawArgs) {
				ip = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-ip=") || strings.HasPrefix(arg, "--ip="):
			ip = strings.SplitN(arg, "=", 2)[1]
		case strings.HasPrefix(arg, "-host=") || strings.HasPrefix(arg, "--host="):
			ip = strings.SplitN(arg, "=", 2)[1]
		case arg == "-port" || arg == "--port":
			if i+1 < len(rawArgs) {
				if p, err := strconv.Atoi(rawArgs[i+1]); err == nil {
					port = p
				}
				i++
			}
		case strings.HasPrefix(arg, "-port=") || strings.HasPrefix(arg, "--port="):
			if p, err := strconv.Atoi(strings.SplitN(arg, "=", 2)[1]); err == nil {
				port = p
			}
		case arg == "-keyfile" || arg == "--keyfile":
			if i+1 < len(rawArgs) {
				keyFile = rawArgs[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-keyfile=") || strings.HasPrefix(arg, "--keyfile="):
			keyFile = strings.SplitN(arg, "=", 2)[1]
		default:
			if ip == "" && isIPOrHost(arg) {
				ip = arg
			} else {
				args = append(args, arg)
			}
		}
	}
	return
}

func main() {
	ip, port, keyFile, help, args := parseCLIArgs()

	if help {
		printHelp()
		os.Exit(0)
	}

	if ip == "" {
		fmt.Println("Error: LG TV IP address is required. Use -ip <IP> or pass IP as first argument.")
		fmt.Println()
		printHelp()
		os.Exit(1)
	}

	if len(args) == 0 {
		fmt.Println("Error: No command specified. Available commands: toast, mute, apps, inputs, off, volume")
		fmt.Println()
		printHelp()
		os.Exit(1)
	}

	// Clean host and port
	host := ip
	if idx := strings.Index(host, "://"); idx != -1 {
		host = host[idx+3:]
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		host = h
		if parsedPort, err := strconv.Atoi(p); err == nil {
			port = parsedPort
		}
	}

	dialer := webostv.DefaultDialer
	if port == 3000 {
		dialer.DisableTLS = true
	} else {
		dialer.DisableTLS = false
	}

	addr := fmt.Sprintf("%s:%d", host, port)
	fmt.Printf("[LG TV Remote] Connecting to TV at %s (TLS disabled: %v)...\n", addr, dialer.DisableTLS)

	tv, err := dialer.Dial(host)
	if err != nil {
		log.Fatalf("Error connecting to LG TV at %s: %v", addr, err)
	}
	defer tv.Close()
	go tv.MessageHandler()

	// Handle pairing key file (auto-pair on first run, stored in same directory as exe)
	var existingKey string
	if data, err := os.ReadFile(keyFile); err == nil {
		existingKey = strings.TrimSpace(string(data))
	}

	if existingKey == "" {
		fmt.Println("[Pairing] No existing pairing key found.")
		fmt.Println("[Pairing] Please accept the pairing prompt on your LG TV screen if prompted...")
	} else {
		fmt.Println("[Pairing] Using existing pairing key from:", keyFile)
	}

	newKey, err := tv.Register(existingKey)
	if err != nil {
		log.Fatalf("Pairing/Registration failed: %v", err)
	}

	if newKey != "" && newKey != existingKey {
		if err := os.WriteFile(keyFile, []byte(newKey), 0600); err != nil {
			fmt.Printf("Warning: Failed to save pairing key to %s: %v\n", keyFile, err)
		} else {
			fmt.Printf("[Pairing] Successfully paired with TV! Saved key to: %s\n", keyFile)
		}
	}

	// Execute commands sequentially
	idx := 0
	for idx < len(args) {
		cmd := strings.ToLower(args[idx])
		idx++

		switch cmd {
		case "toast":
			msg := "Hello from LG TV Remote!"
			if idx < len(args) && !isCommandOrFlag(args[idx]) {
				msg = args[idx]
				idx++
			}
			fmt.Printf("-> Sending Toast message: %q...", msg)
			if _, err := tv.SystemNotificationsCreateToast(msg); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "mute":
			muteState := "toggle"
			if idx < len(args) && !isCommandOrFlag(args[idx]) {
				muteState = strings.ToLower(args[idx])
				idx++
			}

			var setTo bool
			switch muteState {
			case "true", "on", "1", "yes", "mute":
				setTo = true
			case "false", "off", "0", "no", "unmute":
				setTo = false
			default:
				// Toggle mode
				currMute, err := tv.AudioGetMute()
				if err != nil {
					setTo = true
				} else {
					setTo = !currMute
				}
			}

			fmt.Printf("-> Setting Mute to: %v...", setTo)
			if err := tv.AudioSetMute(setTo); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "apps", "list-apps", "listapps":
			fmt.Println("-> Requesting Installed Applications list...")
			apps, err := tv.ApplicationManagerListApps()
			if err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Printf("\n=== Installed Applications (%d) ===\n", len(apps))
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
				fmt.Fprintln(w, "ID\tTITLE\tVENDOR\tVERSION")
				fmt.Fprintln(w, "--\t-----\t------\t-------")
				for _, app := range apps {
					title := app.Title
					if title == "" {
						title = "-"
					}
					vendor := app.Vendor
					if vendor == "" {
						vendor = "-"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", app.Id, title, vendor, app.Version)
				}
				w.Flush()
				fmt.Println()
			}

		case "inputs", "list-inputs", "listinputs":
			fmt.Println("-> Requesting External Inputs list...")
			inputs, err := tv.TvGetExternalInputList()
			if err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Printf("\n=== External Inputs (%d) ===\n", len(inputs))
				w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
				fmt.Fprintln(w, "ID\tLABEL\tCONNECTED\tAPP ID")
				fmt.Fprintln(w, "--\t-----\t---------\t------")
				for _, in := range inputs {
					fmt.Fprintf(w, "%s\t%s\t%v\t%s\n", in.Id, in.Label, in.Connected, in.AppId)
				}
				w.Flush()
				fmt.Println()
			}

		case "off", "power-off", "poweroff", "shutdown":
			fmt.Print("-> Turning off LG TV...")
			if err := tv.SystemTurnOff(); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "volume", "vol":
			if idx >= len(args) || isCommandOrFlag(args[idx]) {
				fmt.Println("Error: 'volume' command requires a number (0-100)")
				continue
			}
			volNum, err := strconv.Atoi(args[idx])
			idx++
			if err != nil || volNum < 0 || volNum > 100 {
				fmt.Printf("Error: Invalid volume level %q (must be integer 0-100)\n", args[idx-1])
				continue
			}
			fmt.Printf("-> Setting Volume to %d...", volNum)
			if err := tv.AudioSetVolume(volNum); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "volup", "volume-up", "vol-up":
			fmt.Print("-> Volume Up...")
			if err := tv.AudioVolumeUp(); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "voldown", "volume-down", "vol-down":
			fmt.Print("-> Volume Down...")
			if err := tv.AudioVolumeDown(); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "launch", "app":
			if idx >= len(args) || isCommandOrFlag(args[idx]) {
				fmt.Println("Error: 'launch' command requires an App ID (e.g. youtube.leanback.v4)")
				continue
			}
			appId := args[idx]
			idx++
			fmt.Printf("-> Launching App %q...", appId)
			if _, err := tv.SystemLauncherLaunch(appId, nil); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "switch-input", "input":
			if idx >= len(args) || isCommandOrFlag(args[idx]) {
				fmt.Println("Error: 'input' command requires an Input ID (e.g. HDMI_1)")
				continue
			}
			inputId := args[idx]
			idx++
			fmt.Printf("-> Switching Input to %q...", inputId)
			if err := tv.TvSwitchInput(inputId); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		case "active-app", "current-app", "foreground", "get-active":
			fmt.Println("-> Requesting Active Foreground App/Input...")
			info, err := tv.ApplicationManagerGetForegroundAppInfo()
			if err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Printf("\n=== Active Foreground App ===\n")
				fmt.Printf("App ID     : %s\n", info.AppId)
				if info.ProcessId != "" {
					fmt.Printf("Process ID : %s\n", info.ProcessId)
				}
				if info.WindowId != "" {
					fmt.Printf("Window ID  : %s\n", info.WindowId)
				}
				fmt.Println()
			}

		case "open-url", "open", "browser", "url":
			if idx >= len(args) || isCommandOrFlag(args[idx]) {
				fmt.Println("Error: 'open-url' command requires a URL (e.g. http://192.168.1.10:18082/timeout.html)")
				continue
			}
			targetURL := args[idx]
			idx++
			fmt.Printf("-> Opening URL %q on TV browser...", targetURL)
			appId, sessionId, err := tv.SystemLauncherOpen(targetURL)
			if err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Printf(" Done! (AppId: %s, SessionId: %s)\n", appId, sessionId)
			}

		case "close-app", "close":
			sessionId := ""
			if idx < len(args) && !isCommandOrFlag(args[idx]) {
				sessionId = args[idx]
				idx++
			}
			fmt.Printf("-> Closing App Session %q...", sessionId)
			if err := tv.SystemLauncherClose(sessionId); err != nil {
				fmt.Printf(" Failed: %v\n", err)
			} else {
				fmt.Println(" Done!")
			}

		default:
			fmt.Printf("Unknown command: %q. Run with -help for usage.\n", cmd)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

func isIPOrHost(s string) bool {
	if strings.HasPrefix(s, "-") {
		return false
	}
	return strings.Contains(s, ".") || strings.EqualFold(s, "localhost") || strings.HasSuffix(s, ".lan")
}

func isCommandOrFlag(s string) bool {
	if strings.HasPrefix(s, "-") {
		return true
	}
	switch strings.ToLower(s) {
	case "toast", "mute", "apps", "list-apps", "listapps", "inputs", "list-inputs", "listinputs",
		"off", "power-off", "poweroff", "shutdown", "volume", "vol", "volup", "voldown",
		"launch", "app", "switch-input", "input", "active-app", "current-app", "foreground", "get-active",
		"open-url", "open", "browser", "url", "close-app", "close":
		return true
	default:
		return false
	}
}

func printHelp() {
	helpText := `===============================================================
  LG WebOS TV Remote Control Tool (lgtv_remote)
===============================================================

Usage:
  lgtv_remote.exe -ip <TV_IP> [options] <command1> [args] [<command2>...]
  lgtv_remote.exe <TV_IP> <command1> [args]

Options:
  -ip string      IP address of the LG WebOS TV (e.g., 192.168.1.50)
  -port int       WebSocket port of TV (default: 3001 for WSS, 3000 for WS)
  -keyfile string Custom path to save/load pairing key (defaults to lgtv_key.txt in exe directory)
  -help, -h       Display this help documentation

Supported Commands:
  active-app            Show the currently active foreground app or input ID
                        Example: lgtv_remote.exe -ip 192.168.1.50 active-app

  open-url <URL>        Open any HTML URL/webpage on the TV in full screen browser
                        Example: lgtv_remote.exe -ip 192.168.1.50 open-url http://192.168.1.10:18082/timeout.html

  close-app             Close current application / browser session
                        Example: lgtv_remote.exe -ip 192.168.1.50 close-app

  toast <message>       Show toast pop-up message on the TV screen
                        Example: lgtv_remote.exe -ip 192.168.1.50 toast "Meeting Starting!"

  mute [true|false]     Mute or unmute TV audio (toggles if true/false omitted)
                        Example: lgtv_remote.exe -ip 192.168.1.50 mute true

  apps / list-apps      List all installed applications on the TV
                        Example: lgtv_remote.exe -ip 192.168.1.50 list-apps

  inputs / list-inputs  List all external TV input sources (HDMI, AV, etc.)
                        Example: lgtv_remote.exe -ip 192.168.1.50 list-inputs

  launch <appId>        Launch an application by its App ID (e.g., com.webos.app.hdmi1)
                        Example: lgtv_remote.exe -ip 192.168.1.50 launch youtube.leanback.v4

  input <inputId>       Switch active external input
                        Example: lgtv_remote.exe -ip 192.168.1.50 input HDMI_1

  volume <0-100>        Set volume level (0-100)
                        Example: lgtv_remote.exe -ip 192.168.1.50 volume 25

  volup / voldown       Increase or decrease TV volume

  off / power-off       Turn off the TV

Multiple Commands Example:
  lgtv_remote.exe -ip 192.168.1.50 active-app open-url http://192.168.1.10:18082/timeout.html
`
	fmt.Println(helpText)
}
