//go:build windows

package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSpecialCommandConfigAndPublicLabels(t *testing.T) {
	isolateDiscovery(t)
	const config = `{
		"special_commands": [
			{"name":" Switch off ","short_name":" OFF ","executable":["C:\\Private Tools\\remote.exe","secret-argument"]},
			{"name":"Open tool","executable":"C:\\Private Tools\\tool.exe"},
			{"name":"Switch off","executable":["duplicate.exe"]},
			{"name":"Empty","executable":[]},
			{"name":"","executable":["empty-name.exe"]}
		],
		"command_timeout_seconds": 0,
		"on_behalf_of":[{"id":"1","name":"Stage","special_commands":[
			{"name":"switch_off","short_name":"OFF","executable":["agent-secret.exe","secret-argument"]}
		]}]
	}`
	if err := json.Unmarshal([]byte(config), &appConfig); err != nil {
		t.Fatal(err)
	}
	normalizeAppConfig()
	commands := appConfig.SpecialCommands
	if len(commands) != 2 || commands[0].Name != "Switch off" || commands[0].ShortName != "OFF" ||
		commands[1].ShortName != "Open tool" || len(commands[1].Executable) != 1 ||
		commands[1].Executable[0] != `C:\Private Tools\tool.exe` || appConfig.CommandTimeoutSeconds != 10 {
		t.Fatalf("configuration was not normalized: %+v", appConfig)
	}
	timer := virtualTimers.items["1"]
	timer.Config = appConfig.OnBehalfOf[0]
	timer.Config.SpecialCommands = normalizeSpecialCommands(timer.Config.SpecialCommands, "timer 1")
	identity := localNodeIdentity()
	if !reflect.DeepEqual(identity.Timers[0].SpecialCommands, specialCommandLabels(commands)) ||
		identity.Timers[1].SpecialCommands[0].Name != "switch_off" {
		t.Fatalf("identity mixed command owners: %+v", identity)
	}
	if len(localTimerStatus().SpecialCommands) != 2 || len(onBehalfStatuses()[0].SpecialCommands) != 1 {
		t.Fatal("aggregate status omitted special command buttons")
	}
	for _, response := range []any{identity, localTimerStatus(), onBehalfStatuses()} {
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"executable", "Private Tools", "secret-argument", "agent-secret.exe"} {
			if strings.Contains(string(data), private) {
				t.Fatalf("public response leaked %q: %s", private, data)
			}
		}
	}
}

func TestSpecialCommandRequestValidationAndOwnerIsolation(t *testing.T) {
	called := make(chan string, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("/special", specialCommandHandler([]SpecialCommandConfig{
		{Name: "Switch off", Executable: CommandExecutable{"root.exe"}},
	}, func(c SpecialCommandConfig) { called <- c.Executable[0] }))
	mux.HandleFunc("/1/special", specialCommandHandler([]SpecialCommandConfig{
		{Name: "Switch off", Executable: CommandExecutable{"agent.exe"}},
		{Name: "Agent only", Executable: CommandExecutable{"agent-only.exe"}},
	}, func(c SpecialCommandConfig) { called <- c.Executable[0] }))
	handler := cors(mux)
	for _, tc := range []struct {
		method, path, body string
		code               int
		program            string
	}{
		{"POST", "/special", `{"name":"Switch off"}`, 202, "root.exe"},
		{"POST", "/1/special", `{"name":"Switch off"}`, 202, "agent.exe"},
		{"POST", "/special", `{"name":"Agent only"}`, 404, ""},
		{"POST", "/2/special", `{"name":"Switch off"}`, 404, ""},
		{"POST", "/1/special", `{"name":"missing"}`, 404, ""},
		{"POST", "/special", `{"name":"Switch off","executable":["injected.exe"]}`, 400, ""},
		{"POST", "/special", `{"name":"Switch off"} {}`, 400, ""},
		{"POST", "/special", `{"name":null}`, 400, ""},
		{"POST", "/special", `{}`, 400, ""},
		{"GET", "/special", `{"name":"Switch off"}`, 405, ""},
		{"OPTIONS", "/special", "", 204, ""},
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if w.Code != tc.code {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.path, tc.body, w.Code, w.Body.String())
		}
		if tc.program != "" {
			select {
			case program := <-called:
				if program != tc.program {
					t.Fatalf("executed %q, want %q", program, tc.program)
				}
			case <-time.After(time.Second):
				t.Fatal("configured command never started")
			}
		}
	}
	select {
	case unexpected := <-called:
		t.Fatalf("invalid request executed %q", unexpected)
	default:
	}
}

func helperSpecialCommand(t *testing.T, mode string, args ...string) SpecialCommandConfig {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argv := CommandExecutable{executable, "-test.run=^TestSpecialCommandProcess$", "--", "--special-command-helper", mode}
	return SpecialCommandConfig{Name: "switch_off", ShortName: "OFF", Executable: append(argv, args...)}
}

// Only the explicitly launched test subprocess takes this branch. It never
// runs any machine shutdown command or configured application hook.
func TestSpecialCommandProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--special-command-helper" {
			continue
		}
		switch os.Args[i+1] {
		case "echo":
			directory, _ := os.Getwd()
			_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": os.Args[i+2:], "directory": directory})
		case "fail":
			fmt.Print("command failed deliberately")
			os.Exit(7)
		case "wait":
			time.Sleep(10 * time.Second)
		}
		os.Exit(0)
	}
}

func TestSpecialCommandExecutionArgumentsFailureAndTimeout(t *testing.T) {
	directory := t.TempDir()
	args := []string{"two words", `quote"inside`, "& literal", "$(literal)", ""}
	result, detail := executeSpecialCommand(helperSpecialCommand(t, "echo", args...), 5*time.Second, directory)
	var output struct {
		Args      []string
		Directory string
	}
	if result != "success" || json.Unmarshal([]byte(detail), &output) != nil || !reflect.DeepEqual(output.Args, args) || !strings.EqualFold(output.Directory, directory) {
		t.Fatalf("arguments or working directory changed: %s %s", result, detail)
	}
	result, detail = executeSpecialCommand(helperSpecialCommand(t, "fail"), 5*time.Second, directory)
	if result != "failed" || !strings.Contains(detail, "command failed deliberately") || !strings.Contains(detail, "exit status 7") {
		t.Fatalf("exit failure not reported: %s %s", result, detail)
	}
	result, detail = executeSpecialCommand(helperSpecialCommand(t, "wait"), 100*time.Millisecond, directory)
	if result != "failed" || detail != "command timed out" {
		t.Fatalf("timeout not enforced: %s %s", result, detail)
	}
	result, _ = executeSpecialCommand(SpecialCommandConfig{Executable: CommandExecutable{"./missing.exe"}}, time.Second, directory)
	if result != "failed" {
		t.Fatal("missing executable reported success")
	}
}

func TestSpecialCommandsRunOnRootAndAgentWithoutChangingTimers(t *testing.T) {
	isolateDiscovery(t)
	rootLog := &ActivityLog{path: filepath.Join(t.TempDir(), "root.csv")}
	child := virtualTimers.items["1"]
	child.activity.path = filepath.Join(t.TempDir(), "child.csv")
	appConfig.SpecialCommands = []SpecialCommandConfig{helperSpecialCommand(t, "echo", "root")}
	child.Config.SpecialCommands = []SpecialCommandConfig{helperSpecialCommand(t, "echo", "child")}
	child.Countdown = Countdown{Remaining: 42, IsRunning: true}
	child.AlertVisible = true
	beforeRoot, beforeChild := localTimerStatus(), child.status()
	mux := http.NewServeMux()
	registerSpecialCommands(mux, "", appConfig.SpecialCommands, 5, localRegistryPC(), rootLog)
	registerOnBehalfHandlers(mux)
	for _, prefix := range []string{"", "/1"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", prefix+"/special", strings.NewReader(`{"name":"switch_off"}`)))
		if w.Code != 202 {
			t.Fatalf("route %s failed: %d %s", prefix, w.Code, w.Body.String())
		}
	}
	for label, activity := range map[string]*ActivityLog{"root": rootLog, "child": &child.activity} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			activity.Lock()
			data, err := os.ReadFile(activity.path)
			activity.Unlock()
			if err == nil {
				rows, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
				if err != nil || len(rows) != 2 || rows[1][1] != "special_command" || rows[1][5] != "switch_off" || rows[1][7] != "success" || !strings.Contains(rows[1][9], label) {
					t.Fatalf("wrong command log for %s: %s (%v)", label, data, err)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no completion log for %s", label)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !reflect.DeepEqual(beforeRoot, localTimerStatus()) || !reflect.DeepEqual(beforeChild, child.status()) {
		t.Fatal("special command changed a countdown or alert")
	}
}
