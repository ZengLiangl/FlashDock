package cmds

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"FlashDock/define"
	"FlashDock/utils"
)

// 与 config.yaml「发布测试【taj-admin】」remote 步骤一致（日常增量：target/jar + 主 jar）。
const (
	tajAdminWorkDir          = `D:\IdeaProjects\taj_plus`
	tajAdminLocalIncremental = tajAdminWorkDir + `\taj-admin\target\jar` // com.taj 模块，Maven copy-jar
	tajAdminLocalFullLib     = tajAdminWorkDir + `\taj-admin\target\lib` // 首次或依赖变更时全量
	tajAdminLocalMainJar     = tajAdminWorkDir + `\taj-admin\target\taj-admin.jar`
	tajAdminRemoteLib        = "/root/app/taj_plus/lib"
	tajAdminRemoteMainJar    = "/root/app/taj_plus/taj-admin.jar"
	tajAdminContainer        = "taj-admin"
	tajAdminMinRemoteLibFiles = 50 // 远端 lib 基线：须曾全量上传过 target/lib
)

type deployFileWatch struct {
	mu         sync.Mutex
	baseline   map[string]int64
	minSizes   map[string]int64
	sawMissing map[string]bool
	sawZero    map[string]bool
}

func newDeployFileWatch(baseline map[string]int64) *deployFileWatch {
	min := make(map[string]int64, len(baseline))
	miss := make(map[string]bool, len(baseline))
	zero := make(map[string]bool, len(baseline))
	for p, sz := range baseline {
		min[p] = sz
		miss[p] = false
		zero[p] = false
	}
	return &deployFileWatch{
		baseline:   baseline,
		minSizes:   min,
		sawMissing: miss,
		sawZero:    zero,
	}
}

func (w *deployFileWatch) poll(rm *define.RemoteMachine) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, base := range w.baseline {
		sz, ok := remoteFileSizeQuiet(rm, p)
		if !ok {
			if base > 0 {
				w.sawMissing[p] = true
			}
			continue
		}
		if sz < w.minSizes[p] {
			w.minSizes[p] = sz
		}
		if sz == 0 && base > 0 {
			w.sawZero[p] = true
		}
	}
}

func (w *deployFileWatch) assertOK(t *testing.T) {
	t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	for p, base := range w.baseline {
		if base <= 0 {
			continue
		}
		if w.sawMissing[p] {
			t.Fatalf("发布过程中目标消失: %s（基线 %d 字节）", p, base)
		}
		if w.sawZero[p] {
			t.Fatalf("发布过程中目标被截断为 0: %s", p)
		}
		if w.minSizes[p] < base {
			t.Fatalf("发布过程中目标变小: %s min=%d base=%d", p, w.minSizes[p], base)
		}
	}
}

func remoteFileSizeQuiet(rm *define.RemoteMachine, remotePath string) (int64, bool) {
	session, err := rm.SSHClient.NewSession()
	if err != nil {
		return 0, false
	}
	defer session.Close()
	out, err := session.CombinedOutput("stat -c%s -- " + shellSingleQuote(remotePath) + " 2>/dev/null")
	if err != nil {
		return 0, false
	}
	n, err := parseInt64(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseInt64(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("not int: %q", s)
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

func localFileSize(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if st.IsDir() {
		return 0, fmt.Errorf("is dir")
	}
	return st.Size(), nil
}

func sumLocalDirBytes(root string) (int64, int, error) {
	root = filepath.Clean(root)
	var total int64
	var count int
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		count++
		return nil
	})
	return total, count, err
}

// verifyRemoteHasLocalFiles 校验远端包含本地目录中的每个文件（允许远端有额外框架 jar）。
func verifyRemoteHasLocalFiles(rm *define.RemoteMachine, localDir, remoteDir string) error {
	localFiles, err := collectLocalDirFilesForMirror(localDir)
	if err != nil {
		return err
	}
	remoteFiles, err := collectRemoteDirFilesForMirror(rm.SFTPClient, remoteDir)
	if err != nil {
		return err
	}
	for rel, lm := range localFiles {
		rm, ok := remoteFiles[rel]
		if !ok {
			return fmt.Errorf("远端缺少: %s", rel)
		}
		if rm.size != lm.size {
			return fmt.Errorf("大小不一致 %s: local=%d remote=%d", rel, lm.size, rm.size)
		}
	}
	return nil
}

func verifyRemoteLibMirror(rm *define.RemoteMachine, localDir, remoteDir string) error {
	localFiles, err := collectLocalDirFilesForMirror(localDir)
	if err != nil {
		return err
	}
	remoteFiles, err := collectRemoteDirFilesForMirror(rm.SFTPClient, remoteDir)
	if err != nil {
		return err
	}
	for rel, lm := range localFiles {
		rm, ok := remoteFiles[rel]
		if !ok {
			return fmt.Errorf("远端缺少: %s", rel)
		}
		if rm.size != lm.size {
			return fmt.Errorf("大小不一致 %s: local=%d remote=%d", rel, lm.size, rm.size)
		}
	}
	for rel := range remoteFiles {
		if _, ok := localFiles[rel]; !ok {
			return fmt.Errorf("远端多余: %s", rel)
		}
	}
	return nil
}

func runDeployWatch(rm *define.RemoteMachine, w *deployFileWatch, stop <-chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			w.poll(rm)
		}
	}
}

func requireTajAdminArtifacts(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(tajAdminLocalMainJar); err != nil {
		t.Fatalf("缺少主 jar，请先执行打包【taj-admin】: %v", err)
	}
	if _, err := os.Stat(tajAdminLocalIncremental); err != nil {
		t.Fatalf("缺少 target/jar，请先执行打包【taj-admin】: %v", err)
	}
}

func remoteBaseline(rm *define.RemoteMachine, paths ...string) map[string]int64 {
	out := make(map[string]int64, len(paths))
	for _, p := range paths {
		if sz, ok := remoteFileSizeQuiet(rm, p); ok {
			out[p] = sz
		}
	}
	return out
}

func remoteLibFileCount(rm *define.RemoteMachine) (int, error) {
	session, err := rm.SSHClient.NewSession()
	if err != nil {
		return 0, err
	}
	defer session.Close()
	out, err := session.CombinedOutput("find " + shellSingleQuote(tajAdminRemoteLib) + " -type f 2>/dev/null | wc -l")
	if err != nil {
		return 0, err
	}
	n, err := parseInt64(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// 端到端回归：发布测试【taj-admin】日常增量（upload target/jar 合并 → upload 主 jar → restart）。
// 前提：远端 lib 曾全量上传过 target/lib（框架 jar 基线保留，本次不 prune）。
// 运行：go test ./cmds -run TestTajAdminIncrementalReleaseJZ -count=1 -v -timeout 15m
func TestTajAdminIncrementalReleaseJZ(t *testing.T) {
	if testing.Short() {
		t.Skip("skip taj-admin release e2e in -short")
	}
	requireTajAdminArtifacts(t)

	rm := connectJZ(t)
	defer rm.Close()

	beforeLibCount, err := remoteLibFileCount(rm)
	if err != nil {
		t.Fatalf("stat remote lib before: %v", err)
	}
	if beforeLibCount < tajAdminMinRemoteLibFiles {
		t.Fatalf("远端 lib 仅 %d 个文件，请先全量上传 target/lib 建立基线（或运行 TestTajAdminSeedFullLibJZ）", beforeLibCount)
	}

	localMainSz, err := localFileSize(tajAdminLocalMainJar)
	if err != nil {
		t.Fatalf("stat local main jar: %v", err)
	}
	incTotal, incCount, err := sumLocalDirBytes(tajAdminLocalIncremental)
	if err != nil {
		t.Fatalf("walk target/jar: %v", err)
	}
	if incCount == 0 {
		t.Fatal("target/jar 为空")
	}
	t.Logf("本地产物: main=%d bytes, 增量 jar=%d files / %d bytes, 远端 lib 基线=%d files",
		localMainSz, incCount, incTotal, beforeLibCount)

	baseline := remoteBaseline(rm, tajAdminRemoteMainJar)
	if len(baseline) == 0 {
		t.Log("远端尚无 taj-admin.jar，跳过截断监测基线")
	} else {
		t.Logf("远端基线 taj-admin.jar = %d bytes", baseline[tajAdminRemoteMainJar])
	}
	watcher := newDeployFileWatch(baseline)
	stopWatch := make(chan struct{})
	go runDeployWatch(rm, watcher, stopWatch)
	out := make(chan string, 128)

	t.Log("步骤1: upload target/jar（合并）→ " + tajAdminRemoteLib)
	if err := doUpload(rm, []string{"upload", tajAdminLocalIncremental, tajAdminRemoteLib}, out); err != nil {
		close(stopWatch)
		t.Fatalf("upload incremental jar: %v", err)
	}
	watcher.assertOK(t)

	afterLibCount, err := remoteLibFileCount(rm)
	if err != nil {
		close(stopWatch)
		t.Fatalf("stat remote lib after: %v", err)
	}
	if afterLibCount < beforeLibCount {
		t.Fatalf("增量上传后 lib 文件变少: before=%d after=%d（不应 prune 框架 jar）", beforeLibCount, afterLibCount)
	}
	if err := verifyRemoteHasLocalFiles(rm, tajAdminLocalIncremental, tajAdminRemoteLib); err != nil {
		close(stopWatch)
		t.Fatalf("增量 jar 校验失败: %v", err)
	}
	t.Logf("增量 lib 合并通过: 远端 %d → %d files", beforeLibCount, afterLibCount)

	t.Log("步骤2: upload " + tajAdminRemoteMainJar)
	if err := doUpload(rm, []string{"upload", tajAdminLocalMainJar, tajAdminRemoteMainJar}, out); err != nil {
		close(stopWatch)
		t.Fatalf("upload main jar: %v", err)
	}
	close(stopWatch)
	watcher.assertOK(t)

	if sz, ok := remoteFileSizeQuiet(rm, tajAdminRemoteMainJar); !ok || sz != localMainSz {
		t.Fatalf("主 jar 大小不一致: remote=%v want=%d", sz, localMainSz)
	}
	partMain := utils.RemoteUploadPartPath(tajAdminRemoteMainJar)
	if _, err := rm.SFTPClient.Stat(partMain); err == nil {
		t.Fatalf("主 jar .part 未清理: %s", partMain)
	}
	t.Logf("主 jar 校验通过: %d bytes", localMainSz)

	t.Log("步骤3: docker restart " + tajAdminContainer)
	if err := runRemoteCommand(rm, "docker restart "+tajAdminContainer, out); err != nil {
		t.Fatalf("docker restart: %v", err)
	}

	if err := waitContainerHealthy(rm, tajAdminContainer, 120*time.Second); err != nil {
		out, _ := runRemoteCommandCapture(rm, "docker logs --tail 40 "+tajAdminContainer+" 2>&1")
		t.Fatalf("容器未稳定运行: %v\n%s", err, out)
	}
	t.Log("发布测试【taj-admin】增量回归完成：target/jar 合并 + 主 jar + 容器稳定 Running")
}

// 首次或依赖大版本变更：全量上传 target/lib 建立远端基线。
// 运行：go test ./cmds -run TestTajAdminSeedFullLibJZ -count=1 -v -timeout 20m
func TestTajAdminSeedFullLibJZ(t *testing.T) {
	if testing.Short() {
		t.Skip("skip full lib seed in -short")
	}
	if _, err := os.Stat(tajAdminLocalFullLib); err != nil {
		t.Fatalf("缺少 target/lib: %v", err)
	}
	rm := connectJZ(t)
	defer rm.Close()
	out := make(chan string, 128)
	t.Log("全量上传 target/lib → " + tajAdminRemoteLib)
	if err := doUpload(rm, []string{"upload", tajAdminLocalFullLib, tajAdminRemoteLib}, out); err != nil {
		t.Fatalf("upload full lib: %v", err)
	}
	if err := verifyRemoteLibMirror(rm, tajAdminLocalFullLib, tajAdminRemoteLib); err != nil {
		t.Fatalf("全量 lib 校验失败: %v", err)
	}
	n, err := remoteLibFileCount(rm)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("全量 lib 基线就绪: %d files", n)
}

func waitContainerHealthy(rm *define.RemoteMachine, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	stableSince := time.Time{}
	for time.Now().Before(deadline) {
		session, err := rm.SSHClient.NewSession()
		if err != nil {
			return err
		}
		statusOut, err := session.CombinedOutput(
			"docker inspect -f '{{.State.Status}}' " + shellSingleQuote(name) + " 2>/dev/null",
		)
		_ = session.Close()
		status := strings.TrimSpace(string(statusOut))
		if err != nil || status != "running" {
			stableSince = time.Time{}
			time.Sleep(2 * time.Second)
			continue
		}
		if stableSince.IsZero() {
			stableSince = time.Now()
		}
		if time.Since(stableSince) >= 15*time.Second {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("超时：%s 未连续 15s 保持 running（可能 Restarting 循环）", name)
}
