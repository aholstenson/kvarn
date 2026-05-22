//go:build linux

package local

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aholstenson/kvarn/internal/egress/link"
	egressproxy "github.com/aholstenson/kvarn/internal/egress/proxy"
	"github.com/aholstenson/kvarn/internal/project"
	"github.com/aholstenson/kvarn/internal/runnerbin"
	"github.com/aholstenson/kvarn/internal/vm"
	"github.com/aholstenson/kvarn/internal/vm/disk"
	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
)

// ovmfSearchDirs lists directories where OVMF firmware may be installed
// across common Linux distributions.
var ovmfSearchDirs = []string{
	"/usr/share/OVMF",          // Debian/Ubuntu
	"/usr/share/edk2/ovmf",     // Fedora
	"/usr/share/edk2-ovmf/x64", // Arch (edk2-ovmf)
	"/usr/share/edk2-ovmf",     // Arch (older)
	"/usr/share/qemu",          // openSUSE
}

// ovmfFirmware holds a matched pair of OVMF CODE and VARS files.
type ovmfFirmware struct {
	codePath string
	varsPath string
}

type Provider struct {
	mu       sync.Mutex
	vms      map[string]*vmInstance
	nextCID  atomic.Uint32
	nextPort atomic.Uint32
}

// NewProvider creates a Provider and seeds the vsock CID counter above any
// CIDs already in use by running QEMU processes, preventing collisions when
// the orchestrator restarts while VMs are still alive.
func NewProvider() *Provider {
	p := &Provider{}
	if highest := scanHighestQEMUCID(); highest > 0 {
		// allocateCID does nextCID.Add(1)+2, so storing (highest-2) makes the
		// next call yield (highest+1), safely above all running guests.
		p.nextCID.Store(highest - 2)
	}
	return p
}

type vmInstance struct {
	cmd       *exec.Cmd
	qmpSock   string
	tmpDisk   string
	tmpSeed   string
	tmpVars   string
	netCancel context.CancelFunc
	netFiles  []*os.File
	network   *link.Network
	// waitDone is closed by the watcher goroutine after cmd.Wait() returns,
	// ensuring QEMU is always reaped exactly once.
	waitDone chan struct{}
	waitErr  error
}

// cleanup releases per-VM resources. Safe to call from either Destroy or
// the watcher goroutine after an unexpected QEMU exit, but only one of
// them should call it for a given instance.
func (inst *vmInstance) cleanup() {
	if inst.netCancel != nil {
		inst.netCancel()
	}
	if inst.network != nil {
		inst.network.Close()
	}
	for _, f := range inst.netFiles {
		f.Close()
	}
	os.Remove(inst.tmpDisk)
	os.Remove(inst.tmpSeed)
	os.Remove(inst.tmpVars)
	os.Remove(inst.qmpSock)
}

func (p *Provider) Name() string { return "local" }

func (p *Provider) PrepareImage(_ context.Context, base vm.BaseImage) (*vm.ProviderImage, error) {
	return &vm.ProviderImage{Base: &base}, nil
}

func (p *Provider) Create(ctx context.Context, opts vm.CreateOpts) (*vm.VM, *vm.RunnerConn, error) {
	if opts.Image == nil || opts.Image.Base == nil {
		return nil, nil, errors.New("prepared image with base paths is required")
	}

	base := opts.Image.Base
	if base.DiskImagePath == "" {
		return nil, nil, errors.New("disk image path is required")
	}

	qemuBin, err := findQEMU()
	if err != nil {
		return nil, nil, err
	}

	ovmf, err := findOVMF()
	if err != nil {
		return nil, nil, err
	}

	log := slog.With("disk", base.DiskImagePath)

	// Track temp files for cleanup on failure.
	var tmpDisk, tmpSeed, tmpVars, qmpSock string
	var netFiles []*os.File
	var network *link.Network
	var netCancel context.CancelFunc
	success := false
	defer func() {
		if success {
			return
		}
		for _, f := range netFiles {
			f.Close()
		}
		if netCancel != nil {
			netCancel()
		}
		if network != nil {
			network.Close()
		}
		for _, p := range []string{tmpDisk, tmpSeed, tmpVars, qmpSock} {
			if p != "" {
				os.Remove(p)
			}
		}
	}()

	// Copy qcow2 disk to a temp file for this VM instance.
	tmpDiskFile, err := os.CreateTemp("", "kvarn-disk-*.qcow2")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp disk file: %w", err)
	}
	tmpDisk = tmpDiskFile.Name()
	tmpDiskFile.Close()

	if err := copyFile(base.DiskImagePath, tmpDisk); err != nil {
		return nil, nil, fmt.Errorf("copy disk image: %w", err)
	}

	// Resize qcow2 disk to requested size.
	diskSize := opts.DiskSizeBytes
	if diskSize == 0 {
		diskSize = project.DefaultDiskSize
	}
	if err := disk.ResizeQcow2(tmpDisk, diskSize); err != nil {
		return nil, nil, fmt.Errorf("resize disk: %w", err)
	}

	// Allocate unique CID and vsock port for this VM.
	cid := p.allocateCID()
	vsockPort := p.allocatePort()

	// Generate per-VM CA so the proxy can MITM TLS, and bake the public
	// certificate into the cloud-init seed for the in-VM trust store.
	ca, err := egressproxy.GenerateCA()
	if err != nil {
		return nil, nil, fmt.Errorf("generate proxy CA: %w", err)
	}

	// Local providers always boot host-arch VMs, so the embedded runner for
	// runtime.GOARCH is exactly what the guest needs.
	runnerBin, err := runnerbin.Bytes(runtime.GOARCH)
	if err != nil {
		return nil, nil, fmt.Errorf("load embedded runner: %w", err)
	}

	// Create cloud-init seed disk.
	tmpSeed = tmpDisk + ".cidata.iso"
	if err := disk.CreateCloudInitDisk(tmpSeed, disk.CloudInitOpts{
		Token:     opts.Token,
		VsockPort: vsockPort,
		Runner:    runnerBin,
		ProxyCA:   ca.CertPEM(),
	}); err != nil {
		return nil, nil, fmt.Errorf("create cloud-init seed disk: %w", err)
	}

	// Copy OVMF_VARS to a writable per-VM temp file.
	tmpVarsFile, err := os.CreateTemp("", "kvarn-ovmf-vars-*.fd")
	if err != nil {
		return nil, nil, fmt.Errorf("create temp OVMF vars file: %w", err)
	}
	tmpVars = tmpVarsFile.Name()
	tmpVarsFile.Close()

	if err := copyFile(ovmf.varsPath, tmpVars); err != nil {
		return nil, nil, fmt.Errorf("copy OVMF vars: %w", err)
	}

	// Create QMP socket path.
	qmpSock = tmpDisk + ".qmp"

	cpus := opts.CPUs
	if cpus == 0 {
		cpus = project.DefaultCPUs
	}
	memoryBytes := opts.MemoryBytes
	if memoryBytes == 0 {
		memoryBytes = project.DefaultMemory
	}
	memoryMB := memoryBytes / (1024 * 1024)

	// Userspace network: a SOCK_STREAM socketpair carries length-prefixed
	// ethernet frames between qemu and our gvisor netstack. The
	// orchestrator owns the entire L3 fabric the VM sees; the egress
	// proxy is the only reachable network endpoint.
	hostFd, vmFd, err := unixSocketpairStream()
	if err != nil {
		return nil, nil, fmt.Errorf("create network socket pair: %w", err)
	}
	hostFile := os.NewFile(uintptr(hostFd), "kvarn-vm-net-host")
	vmFile := os.NewFile(uintptr(vmFd), "kvarn-vm-net-vm")
	netFiles = []*os.File{hostFile, vmFile}

	args := []string{
		"-enable-kvm",
		"-machine", "q35",
		"-cpu", "host",
		"-smp", fmt.Sprintf("%d", cpus),
		"-m", fmt.Sprintf("%d", memoryMB),
		"-nographic",
		"-no-reboot",
		"-drive", fmt.Sprintf("if=pflash,format=raw,unit=0,readonly=on,file=%s", ovmf.codePath),
		"-drive", fmt.Sprintf("if=pflash,format=raw,unit=1,file=%s", tmpVars),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio,cache=writeback", tmpDisk),
		"-drive", fmt.Sprintf("file=%s,format=raw,if=virtio,media=cdrom,readonly=on", tmpSeed),
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", cid),
		// fd 3 = first ExtraFiles entry passed to qemu.
		"-netdev", "stream,id=net0,addr.type=fd,addr.str=3",
		"-device", "virtio-net-pci,netdev=net0",
		"-qmp", fmt.Sprintf("unix:%s,server,nowait", qmpSock),
	}

	cmd := exec.CommandContext(ctx, qemuBin, args...)
	cmd.ExtraFiles = []*os.File{vmFile}

	// Capture serial output from stdout.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("create stdout pipe: %w", err)
	}

	cmd.Stderr = cmd.Stdout // merge stderr into stdout

	log.Info("starting QEMU", "cid", cid, "vsockPort", vsockPort, "cpus", cpus, "memoryMB", memoryMB)

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start QEMU: %w", err)
	}

	// Bring up the netstack on the host side of the socket pair. qemu
	// owns the vmFile end now; close our reference so the kernel can
	// reclaim the fd cleanly when qemu exits.
	vmFile.Close()

	rw := link.NewStreamFrameRW(hostFile)
	gatewayMAC := tcpip.LinkAddress("\x02\x00\x00\x00\x00\x01")
	ethEndpoint := link.NewEthernetEndpoint(rw, gatewayMAC, "", link.DefaultMTU)

	network, err = link.New(link.Config{Endpoint: ethEndpoint})
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, nil, fmt.Errorf("create userspace network: %w", err)
	}

	netCtx, cancel := context.WithCancel(context.Background())
	netCancel = cancel
	go ethEndpoint.Run(netCtx)
	go func() { _ = network.Run(netCtx) }()

	if err := startProxy(netCtx, network, ca, opts.Network); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, nil, fmt.Errorf("start egress proxy: %w", err)
	}

	// Forward serial console output.
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if opts.OnConsoleOutput != nil {
				opts.OnConsoleOutput(line + "\n")
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			slog.Debug("serial console reader stopped", "error", err)
		}
	}()

	// Connect to QMP and run handshake.
	if err := qmpHandshake(qmpSock); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, nil, fmt.Errorf("QMP handshake: %w", err)
	}

	// Listen on vsock for the runner to connect.
	listener, err := vsock.Listen(vsockPort, nil)
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, nil, fmt.Errorf("vsock listen: %w", err)
	}

	id := fmt.Sprintf("local-%d", time.Now().UnixNano())

	inst := &vmInstance{
		cmd:       cmd,
		qmpSock:   qmpSock,
		tmpDisk:   tmpDisk,
		tmpSeed:   tmpSeed,
		tmpVars:   tmpVars,
		netCancel: netCancel,
		netFiles:  []*os.File{hostFile},
		network:   network,
		waitDone:  make(chan struct{}),
	}

	p.mu.Lock()
	if p.vms == nil {
		p.vms = make(map[string]*vmInstance)
	}
	p.vms[id] = inst
	p.mu.Unlock()

	// Reap QEMU exactly once. If it exits before Destroy is called we
	// claim the instance from the map and run cleanup ourselves so we
	// don't leak a zombie or its temp files.
	go p.watchQEMU(id, inst)

	slog.Info("local VM started", "vm", id, "cid", cid, "vsockPort", vsockPort)

	success = true
	return &vm.VM{
			ID:    id,
			Token: opts.Token,
		}, &vm.RunnerConn{
			Listener: listener,
		}, nil
}

func (p *Provider) Destroy(_ context.Context, id string) error {
	p.mu.Lock()
	inst, ok := p.vms[id]
	if ok {
		delete(p.vms, id)
	}
	p.mu.Unlock()

	if !ok {
		return fmt.Errorf("VM %s not found", id)
	}

	// Attempt graceful shutdown via QMP.
	if err := qmpShutdown(inst.qmpSock); err != nil {
		slog.Warn("QMP shutdown failed, will force kill", "vm", id, "error", err)
	}

	// Wait up to 10s for graceful exit. The watcher goroutine owns
	// cmd.Wait(); we only observe completion via waitDone.
	select {
	case <-inst.waitDone:
		// Process exited.
	case <-time.After(10 * time.Second):
		slog.Warn("QEMU did not exit gracefully, killing", "vm", id)
		inst.cmd.Process.Kill()
		<-inst.waitDone
	}

	inst.cleanup()
	slog.Info("local VM destroyed", "vm", id)
	return nil
}

// watchQEMU reaps a running QEMU process and cleans up after an unexpected
// exit. It must be started exactly once per registered vmInstance, after
// the instance has been inserted into p.vms.
func (p *Provider) watchQEMU(id string, inst *vmInstance) {
	inst.waitErr = inst.cmd.Wait()
	close(inst.waitDone)

	// If the instance is still in the map, Destroy has not been called
	// and ownership of cleanup falls to us.
	p.mu.Lock()
	_, stillRegistered := p.vms[id]
	if stillRegistered {
		delete(p.vms, id)
	}
	p.mu.Unlock()

	if stillRegistered {
		slog.Warn("QEMU exited unexpectedly", "vm", id, "error", inst.waitErr)
		inst.cleanup()
	}
}

// unixSocketpairStream returns a SOCK_STREAM AF_UNIX socket pair. One end
// is plumbed into the gvisor netstack; the other is handed to qemu via
// cmd.ExtraFiles so it can attach via "-netdev stream,addr.type=fd".
func unixSocketpairStream() (host, vm int, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return 0, 0, err
	}
	return fds[0], fds[1], nil
}

func (p *Provider) List(_ context.Context) ([]*vm.VM, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var vms []*vm.VM
	for id := range p.vms {
		vms = append(vms, &vm.VM{
			ID: id,
		})
	}
	return vms, nil
}

func (p *Provider) allocateCID() uint32 {
	// CIDs 0-2 are reserved (hypervisor, local, host). nextCID starts at 0
	// (yielding CID 3 on first call) or is pre-seeded by NewProvider() to
	// avoid collisions with already-running VMs after an orchestrator restart.
	return p.nextCID.Add(1) + 2
}

func (p *Provider) allocatePort() uint32 {
	// Start vsock ports at 1024 to avoid privileged range.
	return p.nextPort.Add(1) + 1023
}

// scanHighestQEMUCID returns the highest guest vsock CID held by any
// currently running QEMU process, or 0 if none are found.
func scanHighestQEMUCID() uint32 {
	return scanHighestQEMUCIDFromProc("/proc")
}

// scanHighestQEMUCIDFromProc walks procRoot looking for numeric PID
// directories and returns the highest vhost-vsock-pci guest-cid value seen.
// It accepts the proc root as a parameter to make it testable without real
// QEMU processes.
func scanHighestQEMUCIDFromProc(procRoot string) uint32 {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	var highest uint32
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Only numeric entries are PID directories.
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cid := readCIDFromCmdline(filepath.Join(procRoot, e.Name(), "cmdline"))
		if cid > highest {
			highest = cid
		}
	}
	return highest
}

// readCIDFromCmdline reads a /proc/<pid>/cmdline file and returns the
// vhost-vsock-pci guest-cid value if present, or 0 otherwise.
func readCIDFromCmdline(cmdlinePath string) uint32 {
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return 0
	}
	// /proc/<pid>/cmdline separates argv elements with NUL bytes.
	args := strings.Split(string(data), "\x00")
	for _, arg := range args {
		if !strings.HasPrefix(arg, "vhost-vsock-pci,") {
			continue
		}
		for _, part := range strings.Split(arg, ",") {
			if strings.HasPrefix(part, "guest-cid=") {
				val, err := strconv.ParseUint(strings.TrimPrefix(part, "guest-cid="), 10, 32)
				if err == nil {
					return uint32(val)
				}
			}
		}
	}
	return 0
}

func findQEMU() (string, error) {
	path, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return "", errors.New("qemu-system-x86_64 not found in PATH")
	}
	return path, nil
}

// findOVMF searches for a matched pair of OVMF_CODE and OVMF_VARS files,
// preferring the 4M variants over legacy 2M ones.
func findOVMF() (*ovmfFirmware, error) {
	// Candidate file name pairs, ordered by preference.
	pairs := []struct{ code, vars string }{
		{"OVMF_CODE.4m.fd", "OVMF_VARS.4m.fd"},
		{"OVMF_CODE_4M.fd", "OVMF_VARS_4M.fd"},
		{"OVMF_CODE.fd", "OVMF_VARS.fd"},
	}

	for _, dir := range ovmfSearchDirs {
		for _, p := range pairs {
			code := filepath.Join(dir, p.code)
			vars := filepath.Join(dir, p.vars)
			if _, err := os.Stat(code); err != nil {
				continue
			}
			if _, err := os.Stat(vars); err != nil {
				continue
			}
			return &ovmfFirmware{codePath: code, varsPath: vars}, nil
		}
	}

	return nil, fmt.Errorf("OVMF firmware not found; searched directories: %v", ovmfSearchDirs)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(dst)
		}
	}()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// qmpHandshake connects to the QMP socket and performs the capabilities
// negotiation. QEMU may take a moment to create the socket, so we retry.
func qmpHandshake(sockPath string) error {
	var conn net.Conn
	var err error

	for i := 0; i < 50; i++ {
		conn, err = net.Dial("unix", sockPath)
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("connect to QMP socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Read the QMP greeting.
	dec := json.NewDecoder(conn)
	var greeting json.RawMessage
	if err := dec.Decode(&greeting); err != nil {
		return fmt.Errorf("read QMP greeting: %w", err)
	}

	// Send qmp_capabilities to exit capabilities negotiation mode.
	if _, err := conn.Write([]byte(`{"execute":"qmp_capabilities"}` + "\n")); err != nil {
		return fmt.Errorf("send qmp_capabilities: %w", err)
	}

	// Read the success response.
	var resp json.RawMessage
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("read qmp_capabilities response: %w", err)
	}

	return nil
}

// qmpShutdown sends a system_powerdown command via QMP for graceful shutdown.
func qmpShutdown(sockPath string) error {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect to QMP socket: %w", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(5 * time.Second))

	dec := json.NewDecoder(conn)

	// Read greeting.
	var greeting json.RawMessage
	if err := dec.Decode(&greeting); err != nil {
		return fmt.Errorf("read QMP greeting: %w", err)
	}

	// Negotiate capabilities.
	if _, err := conn.Write([]byte(`{"execute":"qmp_capabilities"}` + "\n")); err != nil {
		return fmt.Errorf("send qmp_capabilities: %w", err)
	}
	var capResp json.RawMessage
	if err := dec.Decode(&capResp); err != nil {
		return fmt.Errorf("read qmp_capabilities response: %w", err)
	}

	// Send system_powerdown.
	if _, err := conn.Write([]byte(`{"execute":"system_powerdown"}` + "\n")); err != nil {
		return fmt.Errorf("send system_powerdown: %w", err)
	}
	var pdResp json.RawMessage
	if err := dec.Decode(&pdResp); err != nil {
		return fmt.Errorf("read system_powerdown response: %w", err)
	}

	return nil
}
