package machine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// SFTP 目录覆盖（zip）：上传过程中已有 jar 不得消失或被截断为 0。
// 运行：go test ./machine -run TestJZSFTPDirectoryReplaceNoVanish -count=1 -v
func TestJZSFTPDirectoryReplaceNoVanish(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz sftp dir replace test in -short")
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

	remoteDir := path.Join("/tmp", fmt.Sprintf("flashdock-jz-dir-replace-%d", time.Now().UnixNano()))
	runningJar := path.Join(remoteDir, "app.jar")
	defer func() { _ = aux.RemovePathReliable(remoteDir) }()

	const seedSize = 256 << 10
	seedLocal := filepath.Join(t.TempDir(), "app.jar")
	if err := os.WriteFile(seedLocal, bytes.Repeat([]byte{0x5A}, seedSize), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := aux.UploadFile(ctx, seedLocal, runningJar, nil); err != nil {
		t.Fatalf("seed jar: %v", err)
	}

	localV2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(localV2, "app.jar"), bytes.Repeat([]byte{0xB7}, seedSize+1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localV2, "lib-new.jar"), []byte("new-lib"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu          sync.Mutex
		sawMissing  bool
		sawBadTrunc bool
		minSize     int64 = seedSize
		uploadDone  = make(chan error, 1)
	)
	go func() {
		uploadDone <- aux.UploadDirectoryZip(ctx, localV2, remoteDir, nil, nil)
	}()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-uploadDone:
			if err != nil {
				t.Fatalf("directory replace upload: %v", err)
			}
			goto after
		default:
		}
		if sz, ok := remoteStatSize(t, aux, runningJar); ok {
			mu.Lock()
			if sz < minSize {
				minSize = sz
			}
			if sz == 0 {
				sawBadTrunc = true
			}
			mu.Unlock()
		} else {
			mu.Lock()
			sawMissing = true
			mu.Unlock()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("upload timed out")

after:
	mu.Lock()
	defer mu.Unlock()
	if sawMissing {
		t.Fatal("目录覆盖过程中 app.jar 路径消失（可能先删了远端目录）")
	}
	if sawBadTrunc || minSize == 0 {
		t.Fatalf("目录覆盖过程中 app.jar 被截断（minSize=%d）", minSize)
	}
	if minSize < seedSize {
		t.Fatalf("目录覆盖过程中 app.jar 变小: min=%d seed=%d", minSize, seedSize)
	}
	if err := aux.PruneRemoteDirToMirror(localV2, remoteDir); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if err := aux.VerifyRemoteDirMirror(localV2, remoteDir); err != nil {
		t.Fatalf("verify: %v", err)
	}
	t.Log("sftp directory replace no-vanish ok")
}
