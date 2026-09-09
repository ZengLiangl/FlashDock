package machine

import (
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"FlashDock/define"
	"FlashDock/utils"

	"github.com/pkg/sftp"
)

// ShellAuxManager 辅助 SSH（监控 + SFTP），与 PTY 分离
type ShellAuxManager struct {
	mu           sync.Mutex
	client       *SSHClient
	ownsClient   bool // false 时复用 PTY 的 SSH，Close 仅释放 SFTP
	machineName  string
	host         string
	uidNames     map[uint32]string
	gidNames     map[uint32]string
	idMapsReady  bool
	lastNetAt    time.Time
	lastNetRx    uint64
	lastNetTx    uint64
	lastNetIface string
	fileBackend  string // sftp | scp | ""
	transferPool *sftpTransferPool
	sftpSudo     bool
}

// NewShellAuxManager 创建辅助连接管理器
func NewShellAuxManager() *ShellAuxManager {
	return &ShellAuxManager{}
}

// Connect 建立辅助 SSH；SFTP 尽力初始化，失败不阻断监控/Exec。
func (a *ShellAuxManager) Connect(machine *define.Machine, workVars map[string]string) error {
	a.mu.Lock()
	if a.client != nil && a.client.IsConnected() {
		_ = a.client.Close()
		a.client = nil
	}
	a.mu.Unlock()

	client := NewSSHClient(machine, workVars)
	if err := client.Connect(machine, false); err != nil {
		return err
	}
	sensitive, err := machine.GetSensitiveData()
	if err != nil {
		_ = client.Close()
		return err
	}
	// SFTP 初始化可能较慢，不持有 a.mu；sudo 模式留给 EnsureFileBackend 提权打开
	if rm := client.remoteMachine; rm != nil && !machine.SftpSudo {
		_ = rm.EnsureSFTP()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		_ = a.releaseLocked()
	}
	a.client = client
	a.ownsClient = true
	a.machineName = machine.Name
	a.host = sensitive.Host
	a.uidNames = nil
	a.gidNames = nil
	a.idMapsReady = false
	return nil
}

// Attach 复用 PTY 已有 SSH 连接初始化 Exec/SFTP（避免第二路 TCP 触发 MaxSessions / packet too long）。
// SFTP 尽力初始化：失败不阻断监控（Exec 不依赖 SFTP）。
func (a *ShellAuxManager) Attach(client *SSHClient, machineName, host string) error {
	if client == nil || !client.IsConnected() {
		return fmt.Errorf("共享 SSH 未连接")
	}
	cfg := resolveMachine(machineName)
	if rm := client.remoteMachine; rm != nil && (cfg == nil || !cfg.SftpSudo) {
		_ = rm.EnsureSFTP()
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil {
		_ = a.releaseLocked()
	}
	a.client = client
	a.ownsClient = false
	a.machineName = machineName
	a.host = host
	a.uidNames = nil
	a.gidNames = nil
	a.idMapsReady = false
	return nil
}

// EnsureSFTP 按需初始化文件后端（SFTP 或 SCP 回退）。
func (a *ShellAuxManager) EnsureSFTP() error {
	return a.EnsureFileBackend()
}

// Close 关闭辅助连接（复用 PTY 时仅关闭 SFTP，不关 SSH）
func (a *ShellAuxManager) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.releaseLocked()
}

func (a *ShellAuxManager) releaseLocked() error {
	if a.transferPool != nil {
		a.transferPool.Close()
		a.transferPool = nil
	}
	a.sftpSudo = false
	if a.client == nil {
		return nil
	}
	var err error
	if a.ownsClient {
		err = a.client.Close()
	} else if a.client.remoteMachine != nil && a.client.remoteMachine.SFTPClient != nil {
		if closeErr := a.client.remoteMachine.SFTPClient.Close(); closeErr != nil {
			err = closeErr
		}
		a.client.remoteMachine.SFTPClient = nil
	}
	a.client = nil
	a.ownsClient = false
	a.fileBackend = ""
	a.uidNames = nil
	a.gidNames = nil
	a.idMapsReady = false
	return err
}

// IsConnected 是否已连接
func (a *ShellAuxManager) IsConnected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.client != nil && a.client.IsConnected()
}

// Exec 在辅助连接上执行命令并返回 stdout+stderr 文本
func (a *ShellAuxManager) Exec(command string) (string, error) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || !client.IsConnected() {
		return "", fmt.Errorf("辅助连接未建立")
	}
	session, err := client.remoteMachine.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	out, err := session.CombinedOutput(command)
	return string(out), err
}

// ExecBash 通过 stdin 执行 bash 脚本（避免 bash -c + 转义把换行弄丢）
func (a *ShellAuxManager) ExecBash(script string) (string, error) {
	return a.execBashPath("bash", script)
}

func (a *ShellAuxManager) execBashPath(bash, script string) (string, error) {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || !client.IsConnected() {
		return "", fmt.Errorf("辅助连接未建立")
	}
	if bash == "" {
		bash = "bash"
	}
	session, err := client.remoteMachine.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	session.Stdin = strings.NewReader(script)
	out, err := session.CombinedOutput(bash + " -s")
	return string(out), err
}

// shellMonitorScript 远端监控：uptime/mem 直读；CPU 与 TOP 用 top -bn2 -c 第二帧（瞬时占用 + 完整命令行）。
const shellMonitorScript = `set +e
echo __UP__
awk '{print $1; exit}' /proc/uptime 2>/dev/null
echo __MEM__
LANG=C free -b 2>/dev/null | awk '/^Mem:/{print $2,$3; exit}'
echo __SWAP__
LANG=C free -b 2>/dev/null | awk '/^Swap:/{print $2,$3; exit}'
echo __NETLIST__
ls /sys/class/net 2>/dev/null | grep -Ev '^(lo|veth)'
echo __NET__
iface="${MONITOR_IFACE:-}"
if [ -z "$iface" ]; then
  iface=$(ip -o -4 route show to default 2>/dev/null | awk '{print $5; exit}')
fi
if [ -z "$iface" ]; then
  iface=$(ls /sys/class/net 2>/dev/null | grep -Ev '^(lo|veth)' | grep -E '^(eth|ens|enp|eno)' | head -1)
fi
if [ -z "$iface" ]; then
  iface=$(ls /sys/class/net 2>/dev/null | grep -Ev '^(lo|veth)' | head -1)
fi
echo "IFACE=$iface"
if [ -n "$iface" ]; then
  awk -v d="$iface" '$1 ~ d":" {print $2, $10; exit}' /proc/net/dev 2>/dev/null
fi
echo __TOPRAW__
# -c 输出完整命令行（默认只显示进程名如 java）；-w 512 尽量拉宽，避免命令被终端宽度截断
COLUMNS=512 LC_ALL=C top -bn2 -c -d 0.5 -w 512 2>/dev/null || COLUMNS=512 LC_ALL=C top -bn2 -d 0.5 -w 512 2>/dev/null
`

const shellSystemInfoScript = `set +e
echo __HOST__
hostname 2>/dev/null
echo __UNAME__
uname -sr 2>/dev/null
echo __ARCH__
uname -m 2>/dev/null
echo __OS__
if [ -f /etc/os-release ]; then
  . /etc/os-release 2>/dev/null
  printf '%s\n' "${PRETTY_NAME:-$NAME}"
else
  uname -o 2>/dev/null
fi
echo __CPU__
awk -F: '/model name/{print $2; exit}' /proc/cpuinfo 2>/dev/null
echo __DISK__
df -hT --total 2>/dev/null | awk 'END{print $3" used / "$2" total ("$6")"}'
`

// FetchMonitor 拉取监控快照；netIface 为空时使用默认路由网卡
func (a *ShellAuxManager) FetchMonitor(netIface string) *define.ShellMonitorSnapshot {
	snap := &define.ShellMonitorSnapshot{
		MachineName: a.machineName,
		Host:        a.host,
		UpdatedAt:   time.Now().Unix(),
		TopMem:      []define.ShellProcessStat{},
		NetIfaces:   []string{},
	}
	if !a.IsConnected() {
		return snap
	}

	script := shellMonitorScript
	if strings.TrimSpace(netIface) != "" {
		safe := strings.ReplaceAll(strings.TrimSpace(netIface), "'", "'\\''")
		script = fmt.Sprintf("MONITOR_IFACE='%s'\n%s", safe, shellMonitorScript)
	}

	out, err := a.execBashPath("/bin/bash", script)
	if err != nil && strings.TrimSpace(out) == "" {
		out, err = a.execBashPath("bash", script)
	}
	if err != nil && strings.TrimSpace(out) == "" {
		snap.Error = err.Error()
		return snap
	}

	sections := parseTaggedSections(out, []string{"__UP__", "__MEM__", "__SWAP__", "__NETLIST__", "__NET__", "__TOPRAW__"})
	if v := strings.TrimSpace(sections["__UP__"]); v != "" {
		if sec, err := strconv.ParseFloat(v, 64); err == nil {
			snap.UptimeSec = sec
			snap.UptimeText = formatUptime(sec)
		}
	}
	if memLine := strings.TrimSpace(sections["__MEM__"]); memLine != "" {
		parts := strings.Fields(memLine)
		if len(parts) >= 2 {
			total, _ := strconv.ParseFloat(parts[0], 64)
			used, _ := strconv.ParseFloat(parts[1], 64)
			if total > 0 {
				snap.MemPercent = used / total * 100
				snap.MemTotal = formatBytes(total)
				snap.MemUsed = formatBytes(used)
			}
		}
	}
	if swapLine := strings.TrimSpace(sections["__SWAP__"]); swapLine != "" {
		parts := strings.Fields(swapLine)
		if len(parts) >= 2 {
			total, _ := strconv.ParseFloat(parts[0], 64)
			used, _ := strconv.ParseFloat(parts[1], 64)
			if total > 0 {
				snap.SwapPercent = used / total * 100
				snap.SwapTotal = formatBytes(total)
				snap.SwapUsed = formatBytes(used)
			}
		}
	}
	sysCPU, procs := parseTopBatch(sections["__TOPRAW__"], 5)
	snap.CPUPercent = sysCPU
	snap.TopMem = procs
	snap.NetIfaces = parseNetIfaces(sections["__NETLIST__"])
	a.applyNetRates(snap, sections["__NET__"])
	return snap
}

func parseNetIfaces(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]string, 0)
	seen := make(map[string]bool)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		seen[line] = true
		out = append(out, line)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := netIfaceRank(out[i]), netIfaceRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

func netIfaceRank(name string) int {
	switch {
	case strings.HasPrefix(name, "eth"),
		strings.HasPrefix(name, "ens"),
		strings.HasPrefix(name, "enp"),
		strings.HasPrefix(name, "eno"):
		return 0
	case strings.HasPrefix(name, "docker"):
		return 10
	case strings.HasPrefix(name, "br-"):
		return 20
	default:
		return 50
	}
}

// FetchSystemInfo 拉取系统信息
func (a *ShellAuxManager) FetchSystemInfo() *define.ShellSystemInfo {
	info := &define.ShellSystemInfo{
		MachineName: a.machineName,
		Host:        a.host,
	}
	if !a.IsConnected() {
		return info
	}
	out, err := a.execBashPath("/bin/bash", shellSystemInfoScript)
	if err != nil && strings.TrimSpace(out) == "" {
		out, err = a.execBashPath("bash", shellSystemInfoScript)
	}
	if err != nil && strings.TrimSpace(out) == "" {
		info.Error = err.Error()
		return info
	}
	sections := parseTaggedSections(out, []string{"__HOST__", "__UNAME__", "__ARCH__", "__OS__", "__CPU__", "__DISK__"})
	info.Hostname = strings.TrimSpace(sections["__HOST__"])
	info.Kernel = strings.TrimSpace(sections["__UNAME__"])
	info.Arch = strings.TrimSpace(sections["__ARCH__"])
	info.OS = strings.TrimSpace(sections["__OS__"])
	info.CPUModel = strings.TrimSpace(sections["__CPU__"])
	info.DiskSummary = strings.TrimSpace(sections["__DISK__"])
	return info
}

const shellProcessListScript = `set +e
echo __PS__
LC_ALL=C ps aux --sort=-%cpu 2>/dev/null | head -n 51
`

const shellListenPortsScript = `set +e
echo __PORTS__
if command -v ss >/dev/null 2>&1; then
  ss -tlnp 2>/dev/null
elif command -v netstat >/dev/null 2>&1; then
  netstat -tlnp 2>/dev/null
fi
`

// FetchProcessList 拉取进程列表（按 CPU 排序）
func (a *ShellAuxManager) FetchProcessList(limit int) *define.ShellProcessList {
	if limit <= 0 {
		limit = 50
	}
	out := &define.ShellProcessList{
		MachineName: a.machineName,
		Host:        a.host,
		Processes:   []define.ShellProcessStat{},
		UpdatedAt:   time.Now().Unix(),
	}
	if !a.IsConnected() {
		out.Error = "辅助连接未建立"
		return out
	}
	raw, err := a.execBashPath("/bin/bash", shellProcessListScript)
	if err != nil && strings.TrimSpace(raw) == "" {
		raw, err = a.execBashPath("bash", shellProcessListScript)
	}
	if err != nil && strings.TrimSpace(raw) == "" {
		out.Error = err.Error()
		return out
	}
	sections := parseTaggedSections(raw, []string{"__PS__"})
	out.Processes = parsePsAux(sections["__PS__"], limit)
	if len(out.Processes) == 0 {
		snap := a.FetchMonitor("")
		if len(snap.TopMem) > 0 {
			out.Processes = snap.TopMem
		}
	}
	return out
}

// FetchListenPorts 拉取 TCP 监听端口
func (a *ShellAuxManager) FetchListenPorts() *define.ShellListenPortList {
	out := &define.ShellListenPortList{
		MachineName: a.machineName,
		Host:        a.host,
		Ports:       []define.ShellListenPort{},
		UpdatedAt:   time.Now().Unix(),
	}
	if !a.IsConnected() {
		out.Error = "辅助连接未建立"
		return out
	}
	raw, err := a.execBashPath("/bin/bash", shellListenPortsScript)
	if err != nil && strings.TrimSpace(raw) == "" {
		raw, err = a.execBashPath("bash", shellListenPortsScript)
	}
	if err != nil && strings.TrimSpace(raw) == "" {
		out.Error = err.Error()
		return out
	}
	sections := parseTaggedSections(raw, []string{"__PORTS__"})
	out.Ports = parseListenPorts(sections["__PORTS__"])
	return out
}

const shellDiskListScript = `set +e
echo __DF__
df -hP 2>/dev/null | awk 'NR==1 || /^\/|^tmpfs|^devtmpfs|^overlay/ {print}'
`

// FetchDiskList 拉取磁盘挂载点（路径 / 可用 / 大小）
func (a *ShellAuxManager) FetchDiskList() *define.ShellDiskList {
	out := &define.ShellDiskList{
		MachineName: a.machineName,
		Disks:       []define.ShellDiskMount{},
		UpdatedAt:   time.Now().Unix(),
	}
	if !a.IsConnected() {
		out.Error = "辅助连接未建立"
		return out
	}
	raw, err := a.execBashPath("/bin/bash", shellDiskListScript)
	if err != nil && strings.TrimSpace(raw) == "" {
		raw, err = a.execBashPath("bash", shellDiskListScript)
	}
	if err != nil && strings.TrimSpace(raw) == "" {
		out.Error = err.Error()
		return out
	}
	sections := parseTaggedSections(raw, []string{"__DF__"})
	out.Disks = parseDfMounts(sections["__DF__"])
	return out
}

func parseDfMounts(raw string) []define.ShellDiskMount {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]define.ShellDiskMount, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 && (strings.Contains(line, "Filesystem") || strings.Contains(line, "文件系统")) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		// Filesystem Size Used Avail Use% Mounted
		fs := fields[0]
		size := fields[1]
		used := fields[2]
		avail := fields[3]
		usePct := fields[4]
		mount := fields[5]
		if len(fields) > 6 {
			mount = strings.Join(fields[5:], " ")
		}
		pct := 0.0
		if strings.HasSuffix(usePct, "%") {
			pct, _ = strconv.ParseFloat(strings.TrimSuffix(usePct, "%"), 64)
		}
		out = append(out, define.ShellDiskMount{
			Path:       mount,
			Filesystem: fs,
			Size:       size,
			Used:       used,
			Avail:      avail,
			UsePct:     usePct,
			UsePercent: pct,
		})
	}
	return out
}

func parsePsAux(raw string, limit int) []define.ShellProcessStat {
	if limit <= 0 {
		limit = 50
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]define.ShellProcessStat, 0, limit)
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if i == 0 && strings.Contains(strings.ToUpper(line), "PID") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 11 {
			continue
		}
		if _, err := strconv.Atoi(fields[1]); err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		mem, _ := strconv.ParseFloat(fields[3], 64)
		out = append(out, define.ShellProcessStat{
			PID:     fields[1],
			User:    fields[0],
			CPU:     cpu,
			Mem:     mem,
			MemRSS:  fields[5],
			Command: strings.Join(fields[10:], " "),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

func parseListenPorts(raw string) []define.ShellListenPort {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make([]define.ShellListenPort, 0)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "STATE") ||
			strings.HasPrefix(strings.ToUpper(line), "ACTIVE") ||
			strings.HasPrefix(strings.ToUpper(line), "PROTO") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		proto := strings.ToLower(fields[0])
		if proto != "tcp" && proto != "tcp6" && proto != "udp" && proto != "udp6" {
			if strings.Contains(line, ":") && !strings.Contains(strings.ToUpper(line), "PID") {
				if len(fields) >= 6 && strings.EqualFold(fields[len(fields)-2], "LISTEN") {
					addr := fields[3]
					pidProc := fields[len(fields)-1]
					port := parsePortFromAddr(addr)
					pid, proc := parsePIDProcess(pidProc)
					key := proto + "|" + addr + "|" + pid
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					out = append(out, define.ShellListenPort{
						Proto:   "tcp",
						Address: stripPort(addr),
						Port:    port,
						PID:     pid,
						Process: proc,
					})
				}
			}
			continue
		}
		addr := fields[3]
		pid, proc := "", ""
		for _, f := range fields[4:] {
			if strings.Contains(f, "pid=") {
				pid, proc = parseSsProcessToken(f)
				break
			}
		}
		if pid == "" && len(fields) >= 6 {
			pid, proc = parsePIDProcess(fields[len(fields)-1])
		}
		port := parsePortFromAddr(addr)
		key := proto + "|" + addr + "|" + pid
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, define.ShellListenPort{
			Proto:   proto,
			Address: stripPort(addr),
			Port:    port,
			PID:     pid,
			Process: proc,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		pi, _ := strconv.Atoi(out[i].Port)
		pj, _ := strconv.Atoi(out[j].Port)
		if pi != pj {
			return pi < pj
		}
		return out[i].Address < out[j].Address
	})
	return out
}

func parsePortFromAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if strings.HasPrefix(addr, "[") {
		if idx := strings.LastIndex(addr, "]:"); idx >= 0 {
			return strings.TrimPrefix(addr[idx+2:], "*")
		}
	}
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return strings.TrimPrefix(addr[idx+1:], "*")
	}
	return ""
}

func stripPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "[") {
		if idx := strings.LastIndex(addr, "]:"); idx >= 0 {
			return addr[:idx+1]
		}
	}
	if idx := strings.LastIndex(addr, ":"); idx >= 0 {
		return addr[:idx]
	}
	return addr
}

func parsePIDProcess(token string) (pid, proc string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", ""
	}
	if idx := strings.Index(token, "/"); idx >= 0 {
		return token[:idx], token[idx+1:]
	}
	return token, ""
}

func parseSsProcessToken(token string) (pid, proc string) {
	token = strings.TrimPrefix(token, "users:((\"")
	token = strings.TrimPrefix(token, "(")
	token = strings.Trim(token, "\"),")
	if idx := strings.Index(token, "\","); idx >= 0 {
		proc = token[:idx]
		token = token[idx+2:]
	}
	if strings.HasPrefix(token, "pid=") {
		pid = strings.TrimSuffix(strings.TrimPrefix(token, "pid="), ",fd=3")
		pid = strings.TrimSuffix(pid, ")")
	}
	return pid, proc
}

func (a *ShellAuxManager) applyNetRates(snap *define.ShellMonitorSnapshot, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	iface := ""
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "IFACE=") {
			iface = strings.TrimPrefix(line, "IFACE=")
		}
	}
	var rx, tx uint64
	for _, line := range strings.Split(raw, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		if _, err := strconv.ParseUint(parts[0], 10, 64); err != nil {
			continue
		}
		rx, _ = strconv.ParseUint(parts[0], 10, 64)
		tx, _ = strconv.ParseUint(parts[1], 10, 64)
		break
	}
	if iface != "" {
		snap.NetIface = iface
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastNetIface != iface {
		a.lastNetAt = time.Time{}
		a.lastNetRx = 0
		a.lastNetTx = 0
	}
	if !a.lastNetAt.IsZero() && a.lastNetIface == iface {
		secs := now.Sub(a.lastNetAt).Seconds()
		if secs > 0.05 {
			snap.NetRxRate = float64(rx-a.lastNetRx) / secs
			snap.NetTxRate = float64(tx-a.lastNetTx) / secs
			if snap.NetRxRate < 0 {
				snap.NetRxRate = 0
			}
			if snap.NetTxRate < 0 {
				snap.NetTxRate = 0
			}
		}
	}
	a.lastNetAt = now
	a.lastNetRx = rx
	a.lastNetTx = tx
	a.lastNetIface = iface
	snap.NetRxText = formatRate(snap.NetRxRate)
	snap.NetTxText = formatRate(snap.NetTxRate)
}

// Home 远端登录用户 HOME
func (a *ShellAuxManager) Home() (string, error) {
	out, err := a.Exec("printf '%s\\n' \"$HOME\"")
	if err == nil {
		home := strings.TrimSpace(out)
		if idx := strings.IndexByte(home, '\n'); idx >= 0 {
			home = home[:idx]
		}
		home = strings.TrimSpace(home)
		if home != "" {
			return home, nil
		}
	}
	// fallback: SFTP 起始目录通常即 home
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client != nil && client.remoteMachine != nil && client.remoteMachine.SFTPClient != nil {
		if wd, e := client.remoteMachine.SFTPClient.Getwd(); e == nil && strings.TrimSpace(wd) != "" {
			return strings.TrimSpace(wd), nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", fmt.Errorf("HOME 为空")
}

// DirExists 判断远端路径是否为可列出的目录（以 ReadDir 为准，兼容 symlink）
func (a *ShellAuxManager) DirExists(remotePath string) (bool, error) {
	if err := a.EnsureFileBackend(); err != nil {
		return false, err
	}
	if a.isSCPBackend() {
		entry, err := a.statSCP(remotePath)
		if err != nil {
			errText := strings.ToLower(err.Error())
			if strings.Contains(errText, "不存在") || strings.Contains(errText, "no such") {
				return false, nil
			}
			return false, err
		}
		return entry.IsDir, nil
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.remoteMachine == nil || client.remoteMachine.SFTPClient == nil {
		return false, fmt.Errorf("SFTP 未连接")
	}
	if remotePath == "" {
		remotePath = "."
	}
	sftpClient := client.remoteMachine.SFTPClient

	info, err := sftpClient.Stat(remotePath)
	if err == nil {
		if info.IsDir() {
			return true, nil
		}
		// 非目录
		return false, nil
	}
	errText := strings.ToLower(err.Error())
	if os.IsNotExist(err) || strings.Contains(errText, "no such file") || strings.Contains(errText, "not found") {
		return false, nil
	}

	// Stat 异常时再尝试 ReadDir（部分服务器对目录 Stat 不稳定）
	if _, rdErr := sftpClient.ReadDir(remotePath); rdErr == nil {
		return true, nil
	} else {
		rdText := strings.ToLower(rdErr.Error())
		if os.IsNotExist(rdErr) || strings.Contains(rdText, "no such file") || strings.Contains(rdText, "not found") {
			return false, nil
		}
		return false, err
	}
}

// Pwd 当前家目录或工作目录（辅助通道无法知 PTY cwd，返回 Getwd）
func (a *ShellAuxManager) Pwd() (string, error) {
	out, err := a.Exec("pwd")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// PtyCwdFile 读取 PTY hook 写入的 sidecar 文件（~/.flashdock_pwd）。
func (a *ShellAuxManager) PtyCwdFile() (string, error) {
	home, err := a.Home()
	if err != nil || home == "" {
		home = "/tmp"
	}
	pwdFile := path.Join(home, shellCwdPwdFilename)
	out, err := a.Exec("cat " + strconv.Quote(pwdFile) + " 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	if cwd, ok := SanitizePtyCwd(out); ok {
		return cwd, nil
	}
	return "", fmt.Errorf("PTY cwd 未知")
}

// ListDir 列出目录
func (a *ShellAuxManager) ListDir(dirPath string, showHidden bool) ([]define.SftpEntry, error) {
	if err := a.EnsureFileBackend(); err != nil {
		return nil, err
	}
	if a.isSCPBackend() {
		return a.listDirSCP(dirPath, showHidden)
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.remoteMachine == nil || client.remoteMachine.SFTPClient == nil {
		return nil, fmt.Errorf("SFTP 未连接")
	}
	if dirPath == "" {
		dirPath = "."
	}
	infos, err := client.remoteMachine.SFTPClient.ReadDir(dirPath)
	if err != nil {
		return nil, err
	}
	machineCfg := resolveMachine(a.machineName)
	entries := make([]define.SftpEntry, 0, len(infos))
	sftpClient := client.remoteMachine.SFTPClient
	for _, info := range infos {
		name := info.Name()
		if machineCfg != nil {
			name = decodeSftpName(machineCfg, name)
		}
		if !showHidden && strings.HasPrefix(name, ".") {
			continue
		}
		full := path.Join(dirPath, info.Name())
		mode := info.Mode()
		entry := define.SftpEntry{
			Name:    name,
			Path:    full,
			IsDir:   info.IsDir(),
			Size:    info.Size(),
			Mode:    mode.String(),
			ModTime: info.ModTime().Unix(),
			Type:    fileTypeLabel(mode),
		}
		if stat, ok := info.Sys().(*sftp.FileStat); ok && stat != nil {
			entry.Owner = a.resolveUID(stat.UID)
			entry.Group = a.resolveGID(stat.GID)
		}
		if mode&os.ModeSymlink != 0 {
			if target, err := sftpClient.ReadLink(full); err == nil {
				entry.LinkTarget = target
			}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (a *ShellAuxManager) ensureIDNameMaps() {
	a.mu.Lock()
	ready := a.idMapsReady
	a.mu.Unlock()
	if ready {
		return
	}

	uidNames := parsePasswdOrGroupMap(a.fetchNameMapText(true))
	gidNames := parsePasswdOrGroupMap(a.fetchNameMapText(false))

	a.mu.Lock()
	a.uidNames = uidNames
	a.gidNames = gidNames
	a.idMapsReady = true
	a.mu.Unlock()
}

func (a *ShellAuxManager) fetchNameMapText(passwd bool) string {
	var cmds []string
	if passwd {
		cmds = []string{"getent passwd 2>/dev/null", "cat /etc/passwd 2>/dev/null"}
	} else {
		cmds = []string{"getent group 2>/dev/null", "cat /etc/group 2>/dev/null"}
	}
	for _, cmd := range cmds {
		out, err := a.Exec(cmd)
		if err == nil && strings.TrimSpace(out) != "" {
			return out
		}
	}
	return ""
}

// parsePasswdOrGroupMap 解析 passwd/group 文本：name:x:id:...
func parsePasswdOrGroupMap(text string) map[uint32]string {
	result := make(map[uint32]string)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		name := parts[0]
		id64, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil || name == "" {
			continue
		}
		result[uint32(id64)] = name
	}
	return result
}

func (a *ShellAuxManager) resolveUID(uid uint32) string {
	a.ensureIDNameMaps()
	a.mu.Lock()
	defer a.mu.Unlock()
	if name, ok := a.uidNames[uid]; ok && name != "" {
		return name
	}
	return strconv.FormatUint(uint64(uid), 10)
}

func (a *ShellAuxManager) resolveGID(gid uint32) string {
	a.ensureIDNameMaps()
	a.mu.Lock()
	defer a.mu.Unlock()
	if name, ok := a.gidNames[gid]; ok && name != "" {
		return name
	}
	return strconv.FormatUint(uint64(gid), 10)
}

// RemovePath 删除远端文件或空目录；目录非空则递归删除
func (a *ShellAuxManager) RemovePath(remotePath string) error {
	if err := a.EnsureFileBackend(); err != nil {
		return err
	}
	if a.isSCPBackend() {
		return a.removeSCP(remotePath)
	}
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client == nil || client.remoteMachine == nil || client.remoteMachine.SFTPClient == nil {
		return fmt.Errorf("SFTP 未连接")
	}
	sftpClient := client.remoteMachine.SFTPClient
	info, err := sftpClient.Stat(remotePath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return removeDirRecursive(sftpClient, remotePath)
	}
	return sftpClient.Remove(remotePath)
}

func removeDirRecursive(c *sftp.Client, dir string) error {
	infos, err := c.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, info := range infos {
		p := path.Join(dir, info.Name())
		if info.IsDir() {
			if err := removeDirRecursive(c, p); err != nil {
				return err
			}
		} else if err := c.Remove(p); err != nil {
			return err
		}
	}
	return c.Remove(dir)
}

// StatPath 获取远端路径信息
func (a *ShellAuxManager) StatPath(remotePath string) (*define.SftpEntry, error) {
	if err := a.EnsureFileBackend(); err != nil {
		return nil, err
	}
	if a.isSCPBackend() {
		return a.statSCP(remotePath)
	}
	c, err := a.sftpClient()
	if err != nil {
		return nil, err
	}
	info, err := c.Stat(remotePath)
	if err != nil {
		return nil, err
	}
	entry := &define.SftpEntry{
		Name:    path.Base(remotePath),
		Path:    remotePath,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		ModTime: info.ModTime().Unix(),
		Mode:    info.Mode().String(),
	}
	if info.IsDir() {
		entry.Type = "目录"
	} else {
		entry.Type = "文件"
	}
	return entry, nil
}

// OpenFile 打开远端文件读取
func (a *ShellAuxManager) OpenFile(remotePath string) (io.ReadCloser, error) {
	c, err := a.sftpClient()
	if err != nil {
		return nil, err
	}
	return c.Open(remotePath)
}

// WriteFile 写入远端文件（先写 .part 再原子替换，避免截断运行中文件）
func (a *ShellAuxManager) WriteFile(remotePath string, data []byte) error {
	c, err := a.sftpClient()
	if err != nil {
		return err
	}
	partRemote := utils.RemoteUploadPartPath(remotePath)
	_ = c.Remove(partRemote)
	f, err := c.Create(partRemote)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		_ = c.Remove(partRemote)
		return err
	}
	if err := f.Close(); err != nil {
		_ = c.Remove(partRemote)
		return err
	}
	return utils.CommitRemoteUpload(c, partRemote, remotePath)
}

func fileTypeLabel(mode os.FileMode) string {
	switch mode.Type() {
	case os.ModeDir:
		return "目录"
	case os.ModeSymlink:
		return "链接"
	case os.ModeNamedPipe:
		return "管道"
	case os.ModeSocket:
		return "套接字"
	case os.ModeDevice:
		return "设备"
	default:
		return "文件"
	}
}

func parseTaggedSections(out string, tags []string) map[string]string {
	result := map[string]string{}
	lines := strings.Split(out, "\n")
	var current string
	var buf strings.Builder
	flush := func() {
		if current != "" {
			result[current] = strings.TrimSpace(buf.String())
			buf.Reset()
		}
	}
	tagSet := map[string]bool{}
	for _, t := range tags {
		tagSet[t] = true
	}
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if tagSet[trim] {
			flush()
			current = trim
			continue
		}
		if current != "" {
			if buf.Len() > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(line)
		}
	}
	flush()
	return result
}

func parseCPUPercent(line string) float64 {
	line = strings.TrimSpace(line)
	if line == "" {
		return 0
	}
	// 新格式：短窗口双采样得到的纯数字
	if first := strings.Fields(line); len(first) > 0 {
		if v, err := strconv.ParseFloat(first[0], 64); err == nil {
			if v < 0 {
				return 0
			}
			if v > 100 {
				return 100
			}
			return v
		}
	}
	// 兼容旧 top 行：%Cpu(s):  3.2 us, ... idle
	if idx := strings.Index(strings.ToLower(line), "id"); idx >= 0 {
		chunk := line
		lower := strings.ToLower(chunk)
		idIdx := strings.Index(lower, "id")
		before := chunk[:idIdx]
		fields := strings.FieldsFunc(before, func(r rune) bool {
			return r == ',' || r == ':' || r == ' '
		})
		if len(fields) > 0 {
			idle, err := strconv.ParseFloat(strings.TrimSuffix(fields[len(fields)-1], "%"), 64)
			if err == nil {
				v := 100 - idle
				if v < 0 {
					v = 0
				}
				return v
			}
		}
	}
	return 0
}

// parseTopBatch 解析 top -bn2 输出：取最后一帧的整机 CPU 与进程 TOP。
func parseTopBatch(raw string, limit int) (float64, []define.ShellProcessStat) {
	if limit <= 0 {
		limit = 5
	}
	lines := strings.Split(raw, "\n")
	lastCPULine := ""
	headerIdx := -1
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		lower := strings.ToLower(trim)
		if strings.Contains(lower, "cpu(s)") || strings.HasPrefix(lower, "%cpu") {
			lastCPULine = trim
		}
		fields := strings.Fields(trim)
		upperJoined := strings.ToUpper(strings.Join(fields, " "))
		if strings.Contains(upperJoined, "PID") &&
			(strings.Contains(upperJoined, "%CPU") || strings.Contains(upperJoined, " CPU")) &&
			(strings.Contains(upperJoined, "COMMAND") || strings.Contains(upperJoined, "CMD")) {
			headerIdx = i
		}
	}

	sysCPU := parseCPUPercent(lastCPULine)
	if headerIdx < 0 {
		return sysCPU, []define.ShellProcessStat{}
	}

	headerFields := strings.Fields(strings.TrimSpace(lines[headerIdx]))
	idxPID, idxUser, idxCPU, idxMem, idxRes, idxCmd := -1, -1, -1, -1, -1, -1
	for i, h := range headerFields {
		switch strings.ToUpper(strings.TrimPrefix(h, "%")) {
		case "PID":
			idxPID = i
		case "USER":
			idxUser = i
		case "CPU":
			idxCPU = i
		case "MEM":
			idxMem = i
		case "RES", "RSS":
			idxRes = i
		case "COMMAND", "CMD":
			idxCmd = i
		}
	}
	// 再按原始表头匹配一次（含 %CPU / %MEM）
	for i, h := range headerFields {
		u := strings.ToUpper(h)
		switch u {
		case "%CPU":
			idxCPU = i
		case "%MEM":
			idxMem = i
		}
	}
	if idxPID < 0 || idxCPU < 0 || idxCmd < 0 {
		return sysCPU, []define.ShellProcessStat{}
	}

	out := make([]define.ShellProcessStat, 0, limit)
	for i := headerIdx + 1; i < len(lines) && len(out) < limit; i++ {
		trim := strings.TrimSpace(lines[i])
		if trim == "" {
			continue
		}
		lower := strings.ToLower(trim)
		if strings.HasPrefix(lower, "top -") || strings.HasPrefix(lower, "tasks:") ||
			strings.Contains(lower, "cpu(s)") || strings.HasPrefix(lower, "%cpu") {
			break
		}
		fields := strings.Fields(trim)
		if len(fields) <= idxCmd || len(fields) <= idxCPU || len(fields) <= idxPID {
			continue
		}
		if _, err := strconv.Atoi(fields[idxPID]); err != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[idxCPU], 64)
		mem := 0.0
		if idxMem >= 0 && idxMem < len(fields) {
			mem, _ = strconv.ParseFloat(fields[idxMem], 64)
		}
		user := ""
		if idxUser >= 0 && idxUser < len(fields) {
			user = fields[idxUser]
		}
		memRSS := ""
		if idxRes >= 0 && idxRes < len(fields) {
			memRSS = fields[idxRes]
		}
		out = append(out, define.ShellProcessStat{
			PID:     fields[idxPID],
			User:    user,
			CPU:     cpu,
			Mem:     mem,
			MemRSS:  memRSS,
			Command: strings.Join(fields[idxCmd:], " "),
		})
	}
	return sysCPU, out
}

func parseTopProcesses(block string, limit int) []define.ShellProcessStat {
	if limit <= 0 {
		limit = 5
	}
	lines := strings.Split(block, "\n")
	out := []define.ShellProcessStat{}
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || i == 0 && strings.Contains(strings.ToLower(line), "pid") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		mem, _ := strconv.ParseFloat(fields[3], 64)
		cmd := strings.Join(fields[4:], " ")
		out = append(out, define.ShellProcessStat{
			PID:     fields[0],
			User:    fields[1],
			CPU:     cpu,
			Mem:     mem,
			Command: cmd,
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// 兼容旧名
func parseTopMem(block string) []define.ShellProcessStat {
	return parseTopProcesses(block, 5)
}

func formatUptime(sec float64) string {
	s := int64(sec)
	days := s / 86400
	s %= 86400
	hours := s / 3600
	s %= 3600
	mins := s / 60
	if days > 0 {
		return fmt.Sprintf("%d天 %02d:%02d", days, hours, mins)
	}
	return fmt.Sprintf("%02d:%02d", hours, mins)
}

func formatBytes(b float64) string {
	units := []string{"B", "K", "M", "G", "T"}
	v := b
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.1f%s", v, units[i])
}

func formatRate(bps float64) string {
	if bps < 0 {
		bps = 0
	}
	units := []string{"B/s", "K/s", "M/s", "G/s"}
	v := bps
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}
