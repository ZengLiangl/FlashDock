package cmds

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"FlashDock/crypto"
	"FlashDock/data"
	"FlashDock/define"
	"FlashDock/utils"
)

func loadJZMachine(t *testing.T) *define.Machine {
	t.Helper()
	if err := crypto.InitVault(); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	if crypto.IsLocked() {
		if err := crypto.Unlock(""); err != nil {
			t.Fatalf("解锁凭据失败: %v", err)
		}
	}
	gcm := data.NewGlobalConfigManager("")
	cfg, err := gcm.LoadGlobalConfig()
	if err != nil {
		t.Fatalf("LoadGlobalConfig: %v", err)
	}
	for i := range cfg.Machines {
		m := &cfg.Machines[i]
		if m.Name == "jz" {
			return m
		}
	}
	t.Fatal("global_config 中未找到名为 jz 的机器")
	return nil
}

func connectJZ(t *testing.T) *define.RemoteMachine {
	t.Helper()
	m := loadJZMachine(t)
	rm := define.NewRemoteMachine()
	if err := rm.Connect(m, true); err != nil {
		t.Fatalf("Connect jz: %v", err)
	}
	return rm
}

// 用独立 SSH session 查尺寸，避免与上传共用 SFTP 客户端并发不安全。
func remoteFileSize(t *testing.T, rm *define.RemoteMachine, remotePath string) (int64, bool) {
	t.Helper()
	session, err := rm.SSHClient.NewSession()
	if err != nil {
		return 0, false
	}
	defer session.Close()
	out, err := session.CombinedOutput("stat -c%s -- " + shellSingleQuote(remotePath) + " 2>/dev/null")
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// 任务模式 upload：覆盖已有文件后内容正确，且不留下 .part。
// 运行：go test ./cmds -run TestJZTaskUploadOverwrite -count=1 -v
func TestJZTaskUploadOverwrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz task upload test in -short")
	}
	rm := connectJZ(t)
	defer rm.Close()

	remotePath := path.Join("/tmp", fmt.Sprintf("flashdock-jz-task-up-%d.bin", time.Now().UnixNano()))
	partRemote := utils.RemoteUploadPartPath(remotePath)
	defer func() {
		_ = rm.SFTPClient.Remove(remotePath)
		_ = rm.SFTPClient.Remove(partRemote)
	}()

	seedLocal := filepath.Join(t.TempDir(), "seed.bin")
	seed := []byte("task-upload-seed-v1!!!!!!")
	if err := os.WriteFile(seedLocal, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 64)
	if err := doUpload(rm, []string{"upload", seedLocal, remotePath}, out); err != nil {
		t.Fatalf("seed upload: %v", err)
	}

	nextLocal := filepath.Join(t.TempDir(), "next.bin")
	next := []byte("task-upload-seed-v2!!!!!!")
	if err := os.WriteFile(nextLocal, next, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := doUpload(rm, []string{"upload", nextLocal, remotePath}, out); err != nil {
		t.Fatalf("overwrite upload: %v", err)
	}

	f, err := rm.SFTPClient.Open(remotePath)
	if err != nil {
		t.Fatalf("open remote: %v", err)
	}
	got := make([]byte, len(next)+8)
	n, _ := f.Read(got)
	_ = f.Close()
	if !bytes.Equal(got[:n], next) {
		t.Fatalf("overwrite content mismatch: got %q want %q", got[:n], next)
	}
	if _, err := rm.SFTPClient.Stat(partRemote); err == nil {
		t.Fatalf(".part should be gone after success: %s", partRemote)
	}
	t.Log("task upload overwrite ok")
}

// 任务模式 upload：覆盖大文件过程中，目标路径不得被 O_TRUNC 截成 0（防 jar 假死）。
// 运行：go test ./cmds -run TestJZTaskUploadNoTruncateDuringOverwrite -count=1 -v
func TestJZTaskUploadNoTruncateDuringOverwrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz task upload truncate test in -short")
	}
	rm := connectJZ(t)
	defer rm.Close()

	remotePath := path.Join("/tmp", fmt.Sprintf("flashdock-jz-task-atom-%d.bin", time.Now().UnixNano()))
	partRemote := utils.RemoteUploadPartPath(remotePath)
	defer func() {
		_ = rm.SFTPClient.Remove(remotePath)
		_ = rm.SFTPClient.Remove(partRemote)
	}()

	const seedSize = 256 << 10 // 256 KiB
	seedLocal := filepath.Join(t.TempDir(), "seed.bin")
	seedPayload := bytes.Repeat([]byte{0xA5}, seedSize)
	if err := os.WriteFile(seedLocal, seedPayload, 0o644); err != nil {
		t.Fatal(err)
	}
	out := make(chan string, 64)
	if err := doUpload(rm, []string{"upload", seedLocal, remotePath}, out); err != nil {
		t.Fatalf("seed upload: %v", err)
	}
	st0, err := rm.SFTPClient.Stat(remotePath)
	if err != nil {
		t.Fatalf("stat seed: %v", err)
	}
	if st0.Size() != seedSize {
		t.Fatalf("seed size %d want %d", st0.Size(), seedSize)
	}

	const nextSize = 4 << 20 // 4 MiB，拉长上传窗口便于观察
	nextLocal := filepath.Join(t.TempDir(), "next.bin")
	nextPayload := make([]byte, nextSize)
	if _, err := rand.Read(nextPayload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nextLocal, nextPayload, 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu           sync.Mutex
		sawPart      bool
		minTargetSz  = st0.Size()
		sawBadTrunc  bool
		uploadDone   = make(chan error, 1)
	)
	go func() {
		uploadDone <- doUpload(rm, []string{"upload", nextLocal, remotePath}, out)
	}()

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-uploadDone:
			if err != nil {
				t.Fatalf("overwrite upload: %v", err)
			}
			goto after
		default:
		}
		if sz, ok := remoteFileSize(t, rm, partRemote); ok && sz > 0 {
			mu.Lock()
			sawPart = true
			mu.Unlock()
		}
		if sz, ok := remoteFileSize(t, rm, remotePath); ok {
			mu.Lock()
			if sz < minTargetSz {
				minTargetSz = sz
			}
			if sz == 0 {
				sawBadTrunc = true
			}
			mu.Unlock()
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("upload timed out")

after:
	mu.Lock()
	defer mu.Unlock()
	if !sawPart {
		t.Fatal("overwrite 过程中未观察到 .part 暂存文件，原子上传路径可能未生效")
	}
	if sawBadTrunc || minTargetSz == 0 {
		t.Fatalf("覆盖上传过程中目标被截断（minSize=%d），仍在原地 O_TRUNC", minTargetSz)
	}
	if minTargetSz < seedSize {
		t.Fatalf("覆盖上传过程中目标变小: min=%d seed=%d", minTargetSz, seedSize)
	}
	st1, err := rm.SFTPClient.Stat(remotePath)
	if err != nil {
		t.Fatalf("stat final: %v", err)
	}
	if st1.Size() != nextSize {
		t.Fatalf("final size %d want %d", st1.Size(), nextSize)
	}
	if _, err := rm.SFTPClient.Stat(partRemote); err == nil {
		t.Fatalf(".part should be gone: %s", partRemote)
	}
	t.Log("task upload atomic overwrite (no truncate) ok")
}

// 任务模式目录 upload：zip 解压到 staging 再 mv，覆盖后内容正确。
// 运行：go test ./cmds -run TestJZTaskUploadDirectory -count=1 -v
func TestJZTaskUploadDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("skip remote jz task dir upload test in -short")
	}
	rm := connectJZ(t)
	defer rm.Close()

	remoteDir := path.Join("/tmp", fmt.Sprintf("flashdock-jz-task-dir-%d", time.Now().UnixNano()))
	defer func() {
		session, err := rm.SSHClient.NewSession()
		if err == nil {
			_ = session.Run("rm -rf " + remoteDir)
			_ = session.Close()
		}
	}()

	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("dir-a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "sub", "b.txt"), []byte("dir-b"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := make(chan string, 64)
	if err := doUpload(rm, []string{"upload", localDir, remoteDir}, out); err != nil {
		t.Fatalf("dir upload: %v", err)
	}

	// 增量合并：本地去掉 sub/ 后重传，远端旧 sub/b.txt 应保留（任务模式不 prune）
	if err := os.RemoveAll(filepath.Join(localDir, "sub")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("dir-a2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := doUpload(rm, []string{"upload", localDir, remoteDir}, out); err != nil {
		t.Fatalf("dir re-upload: %v", err)
	}

	if _, err := rm.SFTPClient.Stat(path.Join(remoteDir, "sub", "b.txt")); err != nil {
		t.Fatal("任务模式合并上传不应 prune 掉未出现在本地的 sub/b.txt")
	}

	f, err := rm.SFTPClient.Open(path.Join(remoteDir, "a.txt"))
	if err != nil {
		t.Fatalf("open a.txt: %v", err)
	}
	buf := make([]byte, 16)
	n, _ := f.Read(buf)
	_ = f.Close()
	if string(buf[:n]) != "dir-a2" {
		t.Fatalf("dir overwrite got %q want dir-a2", buf[:n])
	}
	t.Log("task directory upload ok")
}
