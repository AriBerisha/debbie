//go:build windows

package tools

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"
)

var (
	procCreateJobObjectW         = kernel32.NewProc("CreateJobObjectW")
	procSetInformationJobObject  = kernel32.NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = kernel32.NewProc("AssignProcessToJobObject")
	procTerminateJobObject       = kernel32.NewProc("TerminateJobObject")
	procThread32First            = kernel32.NewProc("Thread32First")
	procThread32Next             = kernel32.NewProc("Thread32Next")
	procOpenThread               = kernel32.NewProc("OpenThread")
	procResumeThread             = kernel32.NewProc("ResumeThread")
	procGetOEMCP                 = kernel32.NewProc("GetOEMCP")
	procMultiByteToWideChar      = kernel32.NewProc("MultiByteToWideChar")
)

const (
	createSuspended       = 0x00000004
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000

	th32csSnapThread    = 0x00000004
	threadSuspendResume = 0x0002
	processTerminate    = 0x0001
	processSetQuota     = 0x0100

	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x00002000
)

// FindShell picks the program that runs commands, from the config's
// shell.program: "" or "auto" for Windows PowerShell; "bash", "powershell",
// "pwsh" or "cmd"; or the full path of one of those programs.
func FindShell(program string) (*Shell, error) {
	switch strings.ToLower(program) {
	case "", "auto":
		return windowsPowerShell(), nil
	case "bash":
		if p := gitBash(); p != "" {
			return &Shell{Path: p, Kind: "bash", Name: "Git Bash"}, nil
		}
		return nil, errors.New(`shell.program is "bash", but Git for Windows isn't installed`)
	case "powershell":
		return windowsPowerShell(), nil
	case "pwsh":
		p, err := exec.LookPath("pwsh.exe")
		if err != nil {
			return nil, errors.New(`shell.program is "pwsh", but pwsh.exe (PowerShell 7) isn't on PATH`)
		}
		return &Shell{Path: p, Kind: "pwsh", Name: "PowerShell 7"}, nil
	case "cmd":
		return &Shell{Path: filepath.Join(systemRoot(), "System32", "cmd.exe"), Kind: "cmd", Name: "cmd.exe"}, nil
	}
	if !filepath.IsAbs(program) {
		return nil, fmt.Errorf("shell.program %q: use auto, bash, powershell, pwsh, cmd or a full path", program)
	}
	if fi, err := os.Stat(program); err != nil || fi.IsDir() {
		return nil, fmt.Errorf("shell.program %s: no such program", program)
	}
	switch strings.ToLower(filepath.Base(program)) {
	case "bash.exe", "sh.exe":
		return &Shell{Path: program, Kind: "bash", Name: "bash"}, nil
	case "powershell.exe":
		return &Shell{Path: program, Kind: "powershell", Name: "Windows PowerShell"}, nil
	case "pwsh.exe":
		return &Shell{Path: program, Kind: "pwsh", Name: "PowerShell 7"}, nil
	case "cmd.exe":
		return &Shell{Path: program, Kind: "cmd", Name: "cmd.exe"}, nil
	}
	return nil, fmt.Errorf("shell.program %s: not bash.exe, sh.exe, powershell.exe, pwsh.exe or cmd.exe", program)
}

func systemRoot() string {
	if r := os.Getenv("SystemRoot"); filepath.IsAbs(r) {
		return r
	}
	return `C:\Windows`
}

func windowsPowerShell() *Shell {
	return &Shell{
		Path: filepath.Join(systemRoot(), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		Kind: "powershell",
		Name: "Windows PowerShell",
	}
}

// gitBash finds the bash.exe of Git for Windows, next to the git on PATH or
// in the usual install folders. It never picks System32\bash.exe, which
// runs commands in WSL instead.
func gitBash() string {
	var roots []string
	if git, err := exec.LookPath("git.exe"); err == nil {
		dir := filepath.Dir(git) // Git\cmd, Git\bin or Git\mingw64\bin
		roots = append(roots, filepath.Dir(dir), filepath.Dir(filepath.Dir(dir)))
	}
	for _, v := range []string{"ProgramW6432", "ProgramFiles", "ProgramFiles(x86)"} {
		if d := os.Getenv(v); filepath.IsAbs(d) {
			roots = append(roots, filepath.Join(d, "Git"))
		}
	}
	if d := os.Getenv("LOCALAPPDATA"); filepath.IsAbs(d) {
		roots = append(roots, filepath.Join(d, "Programs", "Git"))
	}
	for _, root := range roots {
		bash := filepath.Join(root, "bin", "bash.exe")
		fi, err := os.Stat(bash)
		usr, uerr := os.Stat(filepath.Join(root, "usr", "bin"))
		if err == nil && !fi.IsDir() && uerr == nil && usr.IsDir() {
			return bash
		}
	}
	return ""
}

// powerShellPrefix makes PowerShell write UTF-8 and skip progress bars.
// Like the chcp call for cmd.exe below, it follows Qwen Code's
// applyUtf8Prefix (packages/core/src/services/shellExecutionService.ts).
const powerShellPrefix = "$ProgressPreference='SilentlyContinue';" +
	"[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false);" +
	"$OutputEncoding=[Console]::OutputEncoding;"

// powerShellExit makes the exit code the program's. PowerShell's own is 1
// whenever $? is false, and Windows PowerShell sets that for a program that
// only wrote to a redirected stderr: "python -m unittest 2>&1" looked failed
// when the tests passed. A failed cmdlet keeps its exit code of 1.
const powerShellExit = "\n$__ok=$?;$__code=$LASTEXITCODE;if(-not $__ok){" +
	"if($__code){exit $__code};" +
	"if($null -ne $__code -and $Error[0].FullyQualifiedErrorId -like 'NativeCommandError*'){exit 0};" +
	"exit 1}"

// command builds the command line itself, since each shell parses it its
// own way. Commands get their own hidden console: Ctrl+C in the user's
// console doesn't reach them, a code-page change stays theirs, and they
// can't draw over the agent's output. They start suspended so they can
// join a job object before they run anything (see startTree).
func (s *Shell) command(script string) *exec.Cmd {
	var line string
	switch s.Kind {
	case "cmd":
		// With /s, cmd.exe strips the first and the last quote and runs the
		// rest exactly as written.
		chcp := filepath.Join(systemRoot(), "System32", "chcp.com")
		line = quoteArg(s.Path) + ` /d /s /c "` + quoteArg(chcp) + ` 65001 >nul 2>&1 & ` + script + `"`
	case "powershell", "pwsh":
		line = quoteArg(s.Path) + " -NoLogo -NoProfile -NonInteractive -Command " + quoteArg(powerShellPrefix+script+powerShellExit)
	default:
		line = quoteArg(s.Path) + " -c " + quoteArg(script)
	}
	cmd := exec.Command(s.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine:       line,
		CreationFlags: createSuspended | createNewProcessGroup | createNoWindow,
	}
	return cmd
}

// quoteArg quotes an argument the way the C runtime parses it, always with
// quotes, so runtimes that expand wildcards in unquoted arguments (MSYS2)
// leave it alone.
func quoteArg(s string) string {
	if e := syscall.EscapeArg(s); strings.HasPrefix(e, `"`) {
		return e
	}
	// No quotes, spaces or tabs: only a final run of backslashes, which
	// would escape the closing quote, needs doubling.
	trailing := len(s) - len(strings.TrimRight(s, `\`))
	return `"` + s + strings.Repeat(`\`, trailing) + `"`
}

// procTree is a started command and every process it starts.
type procTree struct {
	cmd *exec.Cmd
	job syscall.Handle // 0 when the job object couldn't be set up
}

// startTree starts cmd, created suspended, puts it in a job object that
// its children join automatically, and only then lets it run, so nothing
// it starts can slip out before the job exists.
func startTree(cmd *exec.Cmd) (*procTree, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &procTree{cmd: cmd}
	if job, err := newJob(); err == nil {
		if assignJob(job, cmd.Process.Pid) == nil {
			t.job = job
		} else {
			syscall.CloseHandle(job)
		}
	}
	if err := resumeProcess(cmd.Process.Pid); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.close()
		return nil, err
	}
	return t, nil
}

// kill ends the command and everything it started.
func (t *procTree) kill() {
	if t.job != 0 {
		procTerminateJobObject.Call(uintptr(t.job), 1)
		return
	}
	// Without a job, taskkill finds the children by their parent's ID.
	tk := exec.Command(filepath.Join(systemRoot(), "System32", "taskkill.exe"), "/F", "/T", "/PID", strconv.Itoa(t.cmd.Process.Pid))
	tk.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
	tk.Run()
	t.cmd.Process.Kill()
}

// killRemaining ends whatever the command left running after it exited.
// Without a job object there is no safe way to find those processes.
func (t *procTree) killRemaining() {
	if t.job != 0 {
		procTerminateJobObject.Call(uintptr(t.job), 1)
	}
}

func (t *procTree) close() {
	if t.job != 0 {
		syscall.CloseHandle(t.job) // kills anything left, too
		t.job = 0
	}
}

// The layout of JOBOBJECT_EXTENDED_LIMIT_INFORMATION on 64-bit Windows.
type jobBasicLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimits struct {
	Basic                 jobBasicLimits
	IOCounters            [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// newJob creates a job object whose processes die when its last handle
// closes, so commands don't outlive the agent even if it crashes.
func newJob() (syscall.Handle, error) {
	h, _, err := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		return 0, err
	}
	var info jobExtendedLimits
	info.Basic.LimitFlags = jobObjectLimitKillOnJobClose
	if r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0, err
	}
	return syscall.Handle(h), nil
}

func assignJob(job syscall.Handle, pid int) error {
	p, err := syscall.OpenProcess(processSetQuota|processTerminate, false, uint32(pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(p)
	if r, _, err := procAssignProcessToJobObject.Call(uintptr(job), uintptr(p)); r == 0 {
		return err
	}
	return nil
}

// threadEntry32 is THREADENTRY32.
type threadEntry32 struct {
	Size           uint32
	Usage          uint32
	ThreadID       uint32
	OwnerProcessID uint32
	BasePri        int32
	DeltaPri       int32
	Flags          uint32
}

// resumeProcess starts the main thread of a process created suspended.
// os/exec doesn't keep the thread's handle, so it is found by listing
// threads.
func resumeProcess(pid int) error {
	snap, err := syscall.CreateToolhelp32Snapshot(th32csSnapThread, 0)
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(snap)
	te := threadEntry32{}
	te.Size = uint32(unsafe.Sizeof(te))
	resumed := false
	r, _, err := procThread32First.Call(uintptr(snap), uintptr(unsafe.Pointer(&te)))
	for r != 0 {
		if te.OwnerProcessID == uint32(pid) {
			if h, _, _ := procOpenThread.Call(threadSuspendResume, 0, uintptr(te.ThreadID)); h != 0 {
				if n, _, _ := procResumeThread.Call(h); uint32(n) != 0xFFFFFFFF {
					resumed = true
				}
				syscall.CloseHandle(syscall.Handle(h))
			}
		}
		r, _, err = procThread32Next.Call(uintptr(snap), uintptr(unsafe.Pointer(&te)))
	}
	if !resumed {
		return fmt.Errorf("can't resume the new process: %v", err)
	}
	return nil
}

// decodeOutput turns command output into text. Output that isn't UTF-8
// comes from a program writing in the console's OEM code page.
func decodeOutput(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if cp, _, _ := procGetOEMCP.Call(); cp != 0 && cp != 65001 {
		if s, ok := fromCodePage(b, uint32(cp)); ok {
			return s
		}
	}
	return strings.ToValidUTF8(string(b), string(utf8.RuneError))
}

func fromCodePage(b []byte, cp uint32) (string, bool) {
	if len(b) == 0 {
		return "", true
	}
	n, _, _ := procMultiByteToWideChar.Call(uintptr(cp), 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), 0, 0)
	if n == 0 {
		return "", false
	}
	w := make([]uint16, n)
	n, _, _ = procMultiByteToWideChar.Call(uintptr(cp), 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)),
		uintptr(unsafe.Pointer(&w[0])), n)
	if n == 0 {
		return "", false
	}
	return string(utf16.Decode(w[:n])), true
}

// hideConsole keeps a helper command, such as git, off the user's console.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// childAttr starts a Child the way commands start: suspended until it has
// joined its job object, in a new process group and a hidden console.
func childAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended | createNewProcessGroup | createNoWindow}
}
