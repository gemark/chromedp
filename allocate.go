package chromedp

import (
	"bufio"
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// An Allocator is responsible for creating and managing a number of browsers.
//
// This interface abstracts away how the browser process is actually run. For
// example, an Allocator implementation may reuse browser processes, or connect
// to already-running browsers on remote machines.
type Allocator interface {
	// Allocate creates a new browser. It can be cancelled via the provided
	// context, at which point all the resources used by the browser (such
	// as temporary directories) will be freed.
	Allocate(context.Context, ...BrowserOption) (*Browser, error)

	// Wait blocks until an allocator has freed all of its resources.
	// Cancelling the context obtained via NewAllocator will already perform
	// this operation, so normally there's no need to call Wait directly.
	Wait()
}

// setupExecAllocator is similar to NewExecAllocator, but it allows NewContext
// to create the allocator without the unnecessary context layer.
func setupExecAllocator(opts ...ExecAllocatorOption) *ExecAllocator {
	ep := &ExecAllocator{
		initFlags: make(map[string]interface{}),
	}
	for _, o := range opts {
		o(ep)
	}
	if ep.execPath == "" {
		ep.execPath = findExecPath()
	}
	return ep
}

// NewExecAllocator creates a new context set up with an ExecAllocator, suitable
// for use with NewContext or Run.
func NewExecAllocator(parent context.Context, opts ...ExecAllocatorOption) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	c := &Context{Allocator: setupExecAllocator(opts...)}

	ctx = context.WithValue(ctx, contextKey{}, c)
	cancelWait := func() {
		cancel()
		c.Allocator.Wait()
	}
	return ctx, cancelWait
}

// ExecAllocatorOption is a exec allocator option.
type ExecAllocatorOption func(*ExecAllocator)

// ExecAllocator is an Allocator which starts new browser processes on the host
// machine.
type ExecAllocator struct {
	execPath  string
	initFlags map[string]interface{}

	wg sync.WaitGroup
}

// Allocate satisfies the Allocator interface.
func (p *ExecAllocator) Allocate(ctx context.Context, opts ...BrowserOption) (*Browser, error) {
	c := FromContext(ctx)
	if c == nil {
		return nil, ErrInvalidContext
	}

	var args []string
	for name, value := range p.initFlags {
		switch value := value.(type) {
		case string:
			args = append(args, fmt.Sprintf("--%s=%s", name, value))
		case bool:
			if value {
				args = append(args, fmt.Sprintf("--%s", name))
			}
		default:
			return nil, fmt.Errorf("invalid exec pool flag")
		}
	}

	removeDir := false
	dataDir, ok := p.initFlags["user-data-dir"].(string)
	if !ok {
		tempDir, err := ioutil.TempDir("", "chromedp-runner")
		if err != nil {
			return nil, err
		}
		args = append(args, "--user-data-dir="+tempDir)
		dataDir = tempDir
		removeDir = true
	}
	args = append(args, "--remote-debugging-port=0")

	var cmd *exec.Cmd
	p.wg.Add(1) // for the entire allocator
	c.wg.Add(1) // for this browser's root context
	go func() {
		<-ctx.Done()
		// First wait for the process to be finished.
		if cmd != nil {
			// TODO: do we care about this error in any scenario? if
			// the user cancelled the context and killed chrome,
			// this will most likely just be "signal: killed", which
			// isn't interesting.
			cmd.Wait()
		}
		// Then delete the temporary user data directory, if needed.
		if removeDir {
			if err := os.RemoveAll(dataDir); c.cancelErr == nil {
				c.cancelErr = err
			}
		}
		p.wg.Done()
		c.wg.Done()
	}()

	// force the first page to be blank, instead of the welcome page
	// TODO: why isn't --no-first-run enough?
	args = append(args, "about:blank")

	cmd = exec.CommandContext(ctx, p.execPath, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	// Pick up the browser's websocket URL from stderr.
	wsURL := ""
	scanner := bufio.NewScanner(stderr)
	prefix := "DevTools listening on"
	for scanner.Scan() {
		line := scanner.Text()
		if s := strings.TrimPrefix(line, prefix); s != line {
			wsURL = strings.TrimSpace(s)
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	stderr.Close()

	browser, err := NewBrowser(ctx, wsURL, opts...)
	if err != nil {
		return nil, err
	}
	browser.process = cmd.Process
	browser.userDataDir = dataDir
	return browser, nil
}

// Wait satisfies the Allocator interface.
func (p *ExecAllocator) Wait() {
	p.wg.Wait()
}

// ExecPath returns an ExecAllocatorOption which uses the given path to execute
// browser processes. The given path can be an absolute path to a binary, or
// just the name of the program to find via exec.LookPath.
func ExecPath(path string) ExecAllocatorOption {
	return func(p *ExecAllocator) {
		if fullPath, _ := exec.LookPath(path); fullPath != "" {
			// Convert to an absolute path if possible, to avoid
			// repeated LookPath calls in each Allocate.
			path = fullPath
		}
		p.execPath = path
	}
}

// findExecPath tries to find the Chrome browser somewhere in the current
// system. It performs a rather agressive search, which is the same in all
// systems. That may make it a bit slow, but it will only be run when creating a
// new ExecAllocator.
func findExecPath() string {
	for _, path := range [...]string{
		// Unix-like
		"headless_shell",
		"headless-shell",
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"google-chrome-beta",
		"google-chrome-unstable",
		"/usr/bin/google-chrome",

		// Windows
		"chrome",
		"chrome.exe", // in case PATHEXT is misconfigured
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,

		// Mac
		`/Applications/Google Chrome.app/Contents/MacOS/Google Chrome`,
	} {
		found, err := exec.LookPath(path)
		if err == nil {
			return found
		}
	}
	// Fall back to something simple and sensible, to give a useful error
	// message.
	return "google-chrome"
}

// Flag is a generic command line option to pass a flag to Chrome. If the value
// is a string, it will be passed as --name=value. If it's a boolean, it will be
// passed as --name if value is true.
func Flag(name string, value interface{}) ExecAllocatorOption {
	return func(p *ExecAllocator) {
		p.initFlags[name] = value
	}
}

// UserDataDir is the command line option to set the user data dir.
//
// Note: set this option to manually set the profile directory used by Chrome.
// When this is not set, then a default path will be created in the /tmp
// directory.
func UserDataDir(dir string) ExecAllocatorOption {
	return Flag("user-data-dir", dir)
}

// ProxyServer is the command line option to set the outbound proxy server.
func ProxyServer(proxy string) ExecAllocatorOption {
	return Flag("proxy-server", proxy)
}

// WindowSize is the command line option to set the initial window size.
func WindowSize(width, height int) ExecAllocatorOption {
	return Flag("window-size", fmt.Sprintf("%d,%d", width, height))
}

// UserAgent is the command line option to set the default User-Agent
// header.
func UserAgent(userAgent string) ExecAllocatorOption {
	return Flag("user-agent", userAgent)
}

// NoSandbox is the Chrome comamnd line option to disable the sandbox.
func NoSandbox(p *ExecAllocator) {
	Flag("no-sandbox", true)(p)
}

// NoFirstRun is the Chrome comamnd line option to disable the first run
// dialog.
func NoFirstRun(p *ExecAllocator) {
	Flag("no-first-run", true)(p)
}

// NoDefaultBrowserCheck is the Chrome comamnd line option to disable the
// default browser check.
func NoDefaultBrowserCheck(p *ExecAllocator) {
	Flag("no-default-browser-check", true)(p)
}

// Headless is the command line option to run in headless mode.
func Headless(p *ExecAllocator) {
	Flag("headless", true)(p)
}

// DisableGPU is the command line option to disable the GPU process.
func DisableGPU(p *ExecAllocator) {
	Flag("disable-gpu", true)(p)
}
