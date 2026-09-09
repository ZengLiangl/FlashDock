package utils

import (
	"strings"
	"testing"
)

func TestRemoteUploadPartPath(t *testing.T) {
	got := RemoteUploadPartPath("/home/u/a.txt")
	want := "/home/u/.a.txt.flashdock.part"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	got = RemoteUploadPartPath("a.txt")
	want = ".a.txt.flashdock.part"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestShellCommitRemoteUpload(t *testing.T) {
	cmd := ShellCommitRemoteUpload("/tmp/.a.jar.flashdock.part", "/tmp/a.jar")
	if !strings.Contains(cmd, "mv -f") {
		t.Fatalf("want mv -f: %s", cmd)
	}
	if !strings.Contains(cmd, "/tmp/.a.jar.flashdock.part") || !strings.Contains(cmd, "/tmp/a.jar") {
		t.Fatalf("paths missing: %s", cmd)
	}
}

func TestRemoteAtomicUnzipCandidates(t *testing.T) {
	cmds := RemoteAtomicUnzipCandidates("/tmp/a.zip", "/root/app/lib")
	if len(cmds) < 4 {
		t.Fatalf("want >=4 candidates, got %d", len(cmds))
	}
	for i, cmd := range cmds {
		for _, need := range []string{"/tmp/a.zip", "/root/app/lib", "flashdock.extract", "mv -f"} {
			if !strings.Contains(cmd, need) {
				t.Fatalf("candidate %d missing %q: %s", i, need, cmd)
			}
		}
		if strings.Contains(cmd, "unzip -o '/tmp/a.zip' -d '/root/app/lib'") {
			t.Fatalf("candidate %d still extracts directly into target: %s", i, cmd)
		}
	}
}
