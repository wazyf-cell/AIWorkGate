package hosts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Task keys accepted by A's CLI (A.exe --task-run <key>, F69).
const (
	// TaskTraeCheckin is the key A's in-process scheduler uses for Trae
	// (tasks\scheduler.rs, "trae-checkin").
	TaskTraeCheckin = "trae-checkin"
	// TaskWBCheckin is WorkBuddy's daily check-in.
	TaskWBCheckin = "wb-checkin"
	// TaskQoderCheckin is Qoder's daily check-in (both campaigns).
	TaskQoderCheckin = "qoder-checkin"
	// TaskTraeCheckinCLI is the key A's run_cli_task currently dispatches for
	// Trae (tasks\mod.rs matches "checkin"); the scheduler key above is only
	// known inside a running GUI. Try this one if TaskTraeCheckin reports
	// "未知任务".
	TaskTraeCheckinCLI = "checkin"
)

// TaskTimeout bounds one CLI task run (all accounts of one platform).
const TaskTimeout = 10 * time.Minute

// maxTaskOutput caps the captured stdout/stderr kept in TaskResult.Raw.
// The tail is kept so the final JSON line always survives.
const maxTaskOutput = 512 << 10

// ErrNoTaskJSON means the task output contained no line that parses as a JSON
// object (A prints NDJSON progress plus one final JSON line).
var ErrNoTaskJSON = errors.New("hosts: task output has no json line")

// TaskResult is the outcome of one A CLI task.
type TaskResult struct {
	Task    string
	OK      bool
	Summary string
	Raw     string
}

// A's done events look like {"type":"done","ok":2,"already":1,"failed":0}
// where ok/already are counts, not a boolean. That is why OK is derived from
// "failed" first, then from a boolean/numeric "ok", then from status/type.
//
// RunTask executes `exePath --task-run taskKey` with a hidden console window
// (10 minute cap), captures stdout+stderr into TaskResult.Raw, and parses the
// last valid JSON line of stdout. Errors are returned only for infrastructure
// failures (bad args, cannot start, timeout); a non-zero exit code is reported
// through TaskResult.OK=false so callers can still show the parsed result.
func RunTask(ctx context.Context, exePath, taskKey string) (TaskResult, error) {
	res := TaskResult{Task: taskKey}
	if strings.TrimSpace(exePath) == "" {
		return res, errors.New("hosts: empty exe path")
	}
	if strings.TrimSpace(taskKey) == "" {
		return res, errors.New("hosts: empty task key")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, TaskTimeout)
	defer cancel()

	stdout := &tailBuffer{max: maxTaskOutput}
	stderr := &tailBuffer{max: maxTaskOutput}

	cmd := exec.CommandContext(runCtx, exePath, "--task-run", taskKey)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = hideWindow()
	// Do not wait long for orphaned pipes after the process is killed; the
	// tail buffer keeps whatever was already flushed.
	cmd.WaitDelay = 2 * time.Second

	runErr := cmd.Run()
	res.Raw = combineTaskOutput(stdout.String(), stderr.String())

	if err := runCtx.Err(); err != nil {
		return res, fmt.Errorf("hosts: task %s: %w", taskKey, err)
	}

	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return res, fmt.Errorf("hosts: run %q --task-run %s: %w", exePath, taskKey, runErr)
		}
		exitCode = exitErr.ExitCode()
	}

	parsed, parseErr := ParseTaskOutput(stdout.String())
	if parseErr == nil {
		res.OK = parsed.OK
		res.Summary = parsed.Summary
	} else {
		res.Summary = "no json"
	}

	if exitCode != 0 {
		// A returns 1 when any account failed but still prints a done event;
		// the exit code is authoritative for "this run did not fully succeed".
		res.OK = false
		if res.Summary == "" || res.Summary == "no json" {
			res.Summary = fmt.Sprintf("no json (exit code %d)", exitCode)
		}
	}
	if res.Summary == "" {
		res.Summary = "ok"
	}
	return res, nil
}

// ParseTaskOutput scans the output bottom-up for the last line that is a JSON
// object and derives TaskResult.OK/Summary from it. It is a pure function so
// the parsing rules can be tested without spawning A.
//
// On success TaskResult.Task is left empty (RunTask fills it) and Raw holds the
// whole input. When no JSON line exists, ErrNoTaskJSON is returned together with
// a TaskResult whose Raw is still set.
func ParseTaskOutput(output string) (TaskResult, error) {
	res := TaskResult{Raw: output}
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(strings.TrimSuffix(lines[i], "\r"))
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		res.OK = eventOK(event)
		res.Summary = eventSummary(event)
		return res, nil
	}
	return res, ErrNoTaskJSON
}

// eventOK derives success from one JSON event.
func eventOK(event map[string]any) bool {
	if msg, ok := event["error"].(string); ok && strings.TrimSpace(msg) != "" {
		return false
	}
	if n, ok := eventNumber(event, "failed"); ok {
		return n == 0
	}
	if raw, present := event["ok"]; present {
		if b, isBool := raw.(bool); isBool {
			return b
		}
		if n, isNum := toNumber(raw); isNum {
			return n > 0
		}
	}
	if status, ok := event["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "ok", "already", "success", "done":
			return true
		case "fail", "failed", "error":
			return false
		}
	}
	if typ, ok := event["type"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "done":
			return true
		case "error":
			return false
		}
	}
	// No verdict inside the JSON: let the exit code decide in RunTask.
	return true
}

// eventSummary renders a short human readable line for the result.
func eventSummary(event map[string]any) string {
	if msg, ok := event["error"].(string); ok && strings.TrimSpace(msg) != "" {
		return strings.TrimSpace(msg)
	}
	okN, hasOK := eventNumber(event, "ok")
	alreadyN, hasAlready := eventNumber(event, "already")
	failedN, hasFailed := eventNumber(event, "failed")
	if hasOK || hasAlready || hasFailed {
		return fmt.Sprintf("ok=%s already=%s failed=%s",
			numText(okN, hasOK), numText(alreadyN, hasAlready), numText(failedN, hasFailed))
	}
	for _, key := range []string{"message", "skipped", "summary", "status", "type"} {
		if s, ok := event[key].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func eventNumber(event map[string]any, key string) (int64, bool) {
	raw, present := event[key]
	if !present {
		return 0, false
	}
	n, ok := toNumber(raw)
	return n, ok
}

func toNumber(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	case int:
		return int64(x), true
	case int64:
		return x, true
	}
	return 0, false
}

func numText(n int64, present bool) string {
	if !present {
		return "-"
	}
	return strconv.FormatInt(n, 10)
}

// combineTaskOutput keeps stdout first so the final JSON line stays readable,
// and appends stderr (A writes diagnostics there).
func combineTaskOutput(stdout, stderr string) string {
	stdout = strings.TrimRight(stdout, "\r\n")
	stderr = strings.TrimRight(stderr, "\r\n")
	switch {
	case stderr == "":
		return stdout
	case stdout == "":
		return "stderr:\n" + stderr
	default:
		return stdout + "\nstderr:\n" + stderr
	}
}

// tailBuffer keeps at most max bytes and discards the oldest data, so a
// chatty or runaway task cannot exhaust memory while the final JSON line
// (always near the end) is preserved.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.max <= 0 {
		return n, nil
	}
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		copy(b.buf, b.buf[over:])
		b.buf = b.buf[:len(b.buf)-over]
	}
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
