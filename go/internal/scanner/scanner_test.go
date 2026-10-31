package scanner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEncodeForCursor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"fix__fe-1098-distribution_amounts_is_None_but_is_distributed-is-true",
			"fix-fe-1098-distribution-amounts-is-None-but-is-distributed-is-true"},
		{"feat__fe-1243-personalize-poker_av_enabled-in-features_status_config",
			"feat-fe-1243-personalize-poker-av-enabled-in-features-status-config"},
		{"chore__cover-poker-v2-tests-100%",
			"chore-cover-poker-v2-tests-100"},
		{"feat__fe-1228-av-enabled-param-on-room-creation-with=eligibility_validation",
			"feat-fe-1228-av-enabled-param-on-room-creation-with-eligibility-validation"},
		{"a/b/c", "a-b-c"},
		{"already-fine", "already-fine"},
		{"trailing--", "trailing"},
		{"", ""},
	}
	for _, c := range cases {
		if got := EncodeForCursor(c.in); got != c.want {
			t.Errorf("EncodeForCursor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDecodeExistingPath(t *testing.T) {
	dir, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "amnexisting")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	enc := strings.TrimPrefix(root, "/")
	enc = strings.ReplaceAll(enc, "/", "-")
	if strings.Contains(strings.TrimPrefix(root, "/"), "-") {
		t.Skip("temp path contains a hyphen; decode is ambiguous")
	}
	if got := DecodeExistingPath(enc); got != root {
		t.Fatalf("DecodeExistingPath(%q) = %q, want %q", enc, got, root)
	}
	if got := DecodeProjectPath(enc); got != "" {
		t.Fatalf("non-repo decoded as git repo %q", got)
	}
}

func TestLatestWorkDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "chat.jsonl")
	body := `{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"ls","working_directory":"/first"}}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"cd /second && ls"}}]}}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LatestWorkDir(path); got != "/second" {
		t.Fatalf("LatestWorkDir = %q, want /second", got)
	}
}
