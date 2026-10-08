//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// An executable is an argv array like show_command/hide_command. A single
// string is also accepted as a program path, never parsed as a shell command.
type CommandExecutable []string

func (command *CommandExecutable) UnmarshalJSON(data []byte) error {
	var path string
	if err := json.Unmarshal(data, &path); err == nil {
		*command = CommandExecutable{path}
		return nil
	}
	var argv []string
	if err := json.Unmarshal(data, &argv); err != nil {
		return errors.New("executable must be a program path or an array of program and arguments")
	}
	*command = argv
	return nil
}

type SpecialCommandConfig struct {
	Name       string            `json:"name"`
	ShortName  string            `json:"short_name"`
	Executable CommandExecutable `json:"executable"`
}

// Only labels are public. Executable paths and arguments stay on their owner.
type SpecialCommand struct {
	Name      string `json:"name"`
	ShortName string `json:"short_name"`
}

func normalizeSpecialCommands(commands []SpecialCommandConfig, owner string) []SpecialCommandConfig {
	result := make([]SpecialCommandConfig, 0, len(commands))
	seen := map[string]bool{}
	for _, command := range commands {
		command.Name = strings.TrimSpace(command.Name)
		command.ShortName = strings.TrimSpace(command.ShortName)
		valid := command.Name != "" && strings.IndexFunc(command.Name, unicode.IsControl) < 0 &&
			strings.IndexFunc(command.ShortName, unicode.IsControl) < 0 &&
			len(command.Executable) > 0 && strings.TrimSpace(command.Executable[0]) != ""
		for _, arg := range command.Executable {
			valid = valid && !strings.ContainsRune(arg, 0)
		}
		if !valid || seen[command.Name] {
			logf("config", "ignoring invalid or duplicate special command %q for %s", command.Name, owner)
			continue
		}
		seen[command.Name] = true
		if command.ShortName == "" {
			command.ShortName = command.Name
		}
		result = append(result, command)
	}
	return result
}

func specialCommandLabels(commands []SpecialCommandConfig) []SpecialCommand {
	labels := make([]SpecialCommand, 0, len(commands))
	for _, command := range commands {
		labels = append(labels, SpecialCommand{Name: command.Name, ShortName: command.ShortName})
	}
	return labels
}

func registerSpecialCommands(mux *http.ServeMux, prefix string, commands []SpecialCommandConfig, timeoutSeconds int, owner PC, activity *ActivityLog) {
	if timeoutSeconds < 1 {
		timeoutSeconds = 10
	}
	mux.HandleFunc(prefix+"/special", specialCommandHandler(commands, func(command SpecialCommandConfig) {
		result, detail := executeSpecialCommand(command, time.Duration(timeoutSeconds)*time.Second, appDataPath(""))
		activity.append("special_command", owner, command.Name, "", result, http.StatusAccepted, detail)
	}))
}

func specialCommandHandler(commands []SpecialCommandConfig, run func(SpecialCommandConfig)) http.HandlerFunc {
	configured := make(map[string]SpecialCommandConfig, len(commands))
	for _, command := range commands {
		configured[command.Name] = command
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&body) != nil || body.Name == "" || decoder.Decode(new(interface{})) != io.EOF {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON body must contain only a command name"})
			return
		}
		command, ok := configured[body.Name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "special command is not configured for this timer"})
			return
		}
		// Commands are independent of countdown/alert state, like explicit hooks.
		go run(command)
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "name": command.Name})
	}
}

func executeSpecialCommand(command SpecialCommandConfig, timeout time.Duration, directory string) (result, detail string) {
	if len(command.Executable) == 0 {
		return "failed", "no executable configured"
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	program := command.Executable[0]
	if !filepath.IsAbs(program) && strings.ContainsAny(program, `/\`) {
		program = filepath.Join(directory, program)
	}
	cmd := exec.CommandContext(ctx, program, command.Executable[1:]...)
	cmd.Dir = directory
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	// Bound waiting for inherited output pipes after the command exits/times out.
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	detail = strings.TrimSpace(string(output))
	if ctx.Err() == context.DeadlineExceeded {
		return "failed", "command timed out"
	}
	if err != nil {
		return "failed", strings.TrimSpace(detail + " " + err.Error())
	}
	return "success", detail
}
