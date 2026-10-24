package hosts

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A's real CLI output shape: NDJSON progress lines, one final done object.
const sampleTaskOutput = `{"type":"start","task":"wb-checkin","at":1760000000}
{"type":"account","status":"ok","account":"acc_1","message":"+100","index":0}
{"type":"account","status":"already","account":"acc_2","index":1}
{"type":"done","ok":1,"already":1,"failed":0}`

func TestParseTaskOutputFinalLine(t *testing.T) {
	res, err := ParseTaskOutput(sampleTaskOutput)
	if err != nil {
		t.Fatalf("ParseTaskOutput: %v", err)
	}
	if !res.OK {
		t.Fatalf("OK = false, want true (summary %q)", res.Summary)
	}
	if got := res.Summary; got != "ok=1 already=1 failed=0" {
		t.Fatalf("Summary = %q", got)
	}
	if res.Raw != sampleTaskOutput {
		t.Fatalf("Raw was not preserved")
	}
	if res.Task != "" {
		t.Fatalf("Task = %q, want empty (caller fills it)", res.Task)
	}
}

func TestParseTaskOutputCountsFailure(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"account","status":"fail","account":"acc_1","message":"未配置 jwt"}`,
		`{"type":"done","ok":0,"already":0,"failed":1}`,
	}, "\r\n")
	res, err := ParseTaskOutput(out)
	if err != nil {
		t.Fatalf("ParseTaskOutput: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true, want false when failed=1")
	}
	if res.Summary != "ok=0 already=0 failed=1" {
		t.Fatalf("Summary = %q", res.Summary)
	}
}

func TestParseTaskOutputErrorEvent(t *testing.T) {
	// A prints this for an unknown task key or a fatal task error.
	res, err := ParseTaskOutput(`{"ok":false,"error":"未知任务: trae-checkin"}` + "\n")
	if err != nil {
		t.Fatalf("ParseTaskOutput: %v", err)
	}
	if res.OK {
		t.Fatal("OK = true, want false")
	}
	if res.Summary != "未知任务: trae-checkin" {
		t.Fatalf("Summary = %q", res.Summary)
	}
}

func TestParseTaskOutputSkipsTrailingNoise(t *testing.T) {
	// Trailing log noise / truncated JSON must not hide the earlier done event.
	out := `{"type":"done","ok":2,"already":0,"failed":0}
warning: sqlite busy, retrying
{"broken`
	res, err := ParseTaskOutput(out)
	if err != nil {
		t.Fatalf("ParseTaskOutput: %v", err)
	}
	if !res.OK || res.Summary != "ok=2 already=0 failed=0" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestParseTaskOutputNoJSON(t *testing.T) {
	res, err := ParseTaskOutput("starting...\ndone\n\n")
	if !errors.Is(err, ErrNoTaskJSON) {
		t.Fatalf("err = %v, want ErrNoTaskJSON", err)
	}
	if res.Raw == "" {
		t.Fatal("Raw should keep the captured output on failure")
	}
	if res.OK {
		t.Fatal("OK must be false without JSON")
	}
}

func TestRunTaskRejectsBadArgs(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "A.exe")
	if _, err := RunTask(context.Background(), "", TaskWBCheckin); err == nil {
		t.Fatal("expected error for empty exe path")
	}
	if _, err := RunTask(context.Background(), exe, ""); err == nil {
		t.Fatal("expected error for empty task key")
	}
	// Existing path is not required: a missing exe must surface as an error,
	// not as a silent zero result.
	if _, err := RunTask(context.Background(), filepath.Join(t.TempDir(), "missing.exe"), TaskQoderCheckin); err == nil {
		t.Fatal("expected start error for missing exe")
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	b := &tailBuffer{max: 8}
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := b.String(); got != "23456789" {
		t.Fatalf("tail = %q, want 23456789", got)
	}
	_, _ = b.Write([]byte("ABC"))
	if got := b.String(); got != "56789ABC" {
		t.Fatalf("tail = %q, want 56789ABC", got)
	}
}
