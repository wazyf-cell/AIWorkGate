package redact

import (
	"fmt"
	"strings"
	"testing"
)

func TestE2EShapeRestore(t *testing.T) {
	// 与 gateway e2e 同形状：messages.content 里明文邮箱+密钥
	body := []byte(`{"messages":[{"role":"user","content":"write to zoe@quill.dev using a1b2c3d4e5f60718293a4b5c6d7e8f9012345678901 now"}],"model":"m1"}`)
	masked, n := MaskJSON(body, Options{Secrets: true, Personal: true, Route: "p1"})
	if n < 1 {
		t.Fatalf("未脱敏 n=%d", n)
	}
	fmt.Printf("masked: %s\n", masked)
	// 上游 echo 第一个占位符
	i := strings.Index(string(masked), "{{")
	end := strings.Index(string(masked)[i:], "}}") + i + 2
	ph := masked[i:end]
	resp := []byte(`{"choices":[{"message":{"role":"assistant","content":"got ` + string(ph) + `"}}]}`)
	got := RestoreJSON(resp)
	want := []byte(`{"choices":[{"message":{"role":"assistant","content":"got zoe@quill.dev"}}]}`)
	fmt.Printf("got: %s\n", got)
	if string(got) != string(want) {
		t.Fatalf("上游 echo 占位符未还原为明文：got=%s want=%s", got, want)
	}
}
