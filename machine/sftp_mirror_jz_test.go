package machine

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"testing"
	"time"
)

// 验证目录覆盖上传会清理旧资产并做完整性校验。
// 运行：go test ./machine -run TestJZDirectoryUploadReplaceMirror -count=1 -v
func TestJZDirectoryUploadReplaceMirror(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz mirror test in -short")
	}

	machine := loadJZMachine(t)
	aux := NewShellAuxManager()
	if err := aux.Connect(machine, nil); err != nil {
		t.Fatalf("Connect jz aux: %v", err)
	}
	defer aux.Close()
	if err := aux.EnsureFileBackend(); err != nil {
		t.Fatalf("EnsureFileBackend: %v", err)
	}

	remoteDir := path.Join("/tmp", fmt.Sprintf("flashdock-jz-mirror-%d", time.Now().UnixNano()))
	defer func() { _ = aux.RemovePathReliable(remoteDir) }()

	buildV1 := func(root string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(`<script src="/assets/index-old.js"></script>`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "assets", "index-old.js"), []byte("console.log('old')"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	buildV2 := func(root string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, "assets"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "index.html"), []byte(`<script src="/assets/index-new.js"></script>`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "assets", "index-new.js"), []byte("console.log('new')"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	localV1 := t.TempDir()
	buildV1(localV1)
	ctx := context.Background()

	if err := aux.RemovePathReliable(remoteDir); err != nil {
		t.Fatalf("prepare remote dir: %v", err)
	}
	if err := aux.UploadDirectoryRecursive(ctx, localV1, remoteDir, nil); err != nil {
		t.Fatalf("upload v1: %v", err)
	}
	if err := aux.VerifyRemoteDirMirror(localV1, remoteDir); err != nil {
		t.Fatalf("verify v1: %v", err)
	}

	localV2 := t.TempDir()
	buildV2(localV2)
	// 模拟 SFTP「覆盖」：不先删远端目录，直接上传 v2 再 prune（避免运行中 jar 被清空）。
	if err := aux.UploadDirectoryRecursive(ctx, localV2, remoteDir, nil); err != nil {
		t.Fatalf("upload v2: %v", err)
	}
	if err := aux.PruneRemoteDirToMirror(localV2, remoteDir); err != nil {
		t.Fatalf("prune after v2: %v", err)
	}
	if err := aux.VerifyRemoteDirMirror(localV2, remoteDir); err != nil {
		t.Fatalf("verify v2: %v", err)
	}

	remoteFiles, err := aux.collectRemoteDirFiles(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := remoteFiles["assets/index-old.js"]; ok {
		t.Fatal("旧版 assets/index-old.js 未被清理，会导致前端白屏")
	}
	if _, ok := remoteFiles["assets/index-new.js"]; !ok {
		t.Fatal("新版 assets/index-new.js 未上传")
	}
	t.Log("directory replace mirror verify ok")
}

// 验证强制覆盖上传会重写同尺寸但内容不同的文件。
// 运行：go test ./machine -run TestJZUploadOverwriteSameSize -count=1 -v
func TestJZUploadOverwriteSameSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz overwrite test in -short")
	}

	machine := loadJZMachine(t)
	aux := NewShellAuxManager()
	if err := aux.Connect(machine, nil); err != nil {
		t.Fatalf("Connect jz aux: %v", err)
	}
	defer aux.Close()
	if err := aux.EnsureFileBackend(); err != nil {
		t.Fatalf("EnsureFileBackend: %v", err)
	}

	remotePath := path.Join("/tmp", fmt.Sprintf("flashdock-jz-overwrite-%d.txt", time.Now().UnixNano()))
	defer func() { _ = aux.RemovePathReliable(remotePath) }()

	localA, err := os.CreateTemp("", "flashdock-jz-overwrite-a-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	localAPath := localA.Name()
	defer os.Remove(localAPath)
	payloadA := []byte("version-a-same-len!!")
	if _, err := localA.Write(payloadA); err != nil {
		t.Fatal(err)
	}
	_ = localA.Close()

	ctx := context.Background()
	if err := aux.UploadFile(ctx, localAPath, remotePath, nil); err != nil {
		t.Fatalf("upload a: %v", err)
	}

	localB, err := os.CreateTemp("", "flashdock-jz-overwrite-b-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	localBPath := localB.Name()
	defer os.Remove(localBPath)
	payloadB := []byte("version-b-same-len!!")
	if _, err := localB.Write(payloadB); err != nil {
		t.Fatal(err)
	}
	_ = localB.Close()

	if err := aux.UploadFileOverwrite(ctx, localBPath, remotePath, nil); err != nil {
		t.Fatalf("overwrite upload b: %v", err)
	}

	localDst := filepath.Join(t.TempDir(), "verify.txt")
	if err := aux.DownloadFile(ctx, remotePath, localDst, nil); err != nil {
		t.Fatalf("download verify: %v", err)
	}
	got, err := os.ReadFile(localDst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payloadB) {
		t.Fatalf("overwrite failed: got %q want %q", got, payloadB)
	}
	t.Log("same-size overwrite ok")
}

// 模拟 SFTP「系统默认打开」：下载到本地、同尺寸改内容后回传。
// UploadFile 会因远端尺寸≥本地而跳过；外置编辑必须走 UploadFileOverwrite。
// 运行：go test ./machine -run TestJZExternalEditSameSizeSync -count=1 -v
func TestJZExternalEditSameSizeSync(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz external-edit test in -short")
	}

	machine := loadJZMachine(t)
	aux := NewShellAuxManager()
	if err := aux.Connect(machine, nil); err != nil {
		t.Fatalf("Connect jz aux: %v", err)
	}
	defer aux.Close()
	if err := aux.EnsureFileBackend(); err != nil {
		t.Fatalf("EnsureFileBackend: %v", err)
	}

	remotePath := path.Join("/tmp", fmt.Sprintf("flashdock-jz-extedit-%d.html", time.Now().UnixNano()))
	defer func() { _ = aux.RemovePathReliable(remotePath) }()

	ctx := context.Background()
	seed := []byte("<html><title>jz-seed-aaaa</title></html>\n")
	edited := []byte("<html><title>jz-edit-bbbb</title></html>\n")
	if len(seed) != len(edited) {
		t.Fatalf("fixture size mismatch: %d vs %d", len(seed), len(edited))
	}

	seedLocal := filepath.Join(t.TempDir(), "index.html")
	if err := os.WriteFile(seedLocal, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := aux.UploadFile(ctx, seedLocal, remotePath, nil); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	localTemp := filepath.Join(t.TempDir(), "index.html")
	if err := aux.DownloadFile(ctx, remotePath, localTemp, nil); err != nil {
		t.Fatalf("download like system-default open: %v", err)
	}
	if err := os.WriteFile(localTemp, edited, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := aux.UploadFile(ctx, localTemp, remotePath, nil); err != nil {
		t.Fatalf("legacy sync upload: %v", err)
	}
	stale := filepath.Join(t.TempDir(), "after-legacy.txt")
	if err := aux.DownloadFile(ctx, remotePath, stale, nil); err != nil {
		t.Fatalf("download after legacy sync: %v", err)
	}
	got, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == string(edited) {
		t.Fatal("UploadFile unexpectedly replaced same-size content; resume skip may have been removed")
	}
	if string(got) != string(seed) {
		t.Fatalf("legacy sync corrupted remote: got %q want seed %q", got, seed)
	}

	if err := aux.UploadFileOverwrite(ctx, localTemp, remotePath, nil); err != nil {
		t.Fatalf("external-edit overwrite: %v", err)
	}
	verify := filepath.Join(t.TempDir(), "after-overwrite.txt")
	if err := aux.DownloadFile(ctx, remotePath, verify, nil); err != nil {
		t.Fatalf("download after overwrite: %v", err)
	}
	got, err = os.ReadFile(verify)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(edited) {
		t.Fatalf("external-edit sync failed: got %q want %q", got, edited)
	}
	t.Log("external-edit same-size sync ok")
}
