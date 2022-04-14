package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sync/errgroup"
)

const hookCmdName = "qmexmut.hook"

func main() {
	cmdName := path.Base(flag.CommandLine.Name())
	if err := run(cmdName); err != nil {
		log.Fatal(err)
	}
}

// run provides command dispatch and flag parsing logic for main(),
// returning an error to log on failure.
func run(cmdName string) error {
	server := flag.String("ssh", "", "upload to and execute on remote host using ssh")
	rmSelf := flag.Bool("rm", false, "remove self executable once done")
	cmdFlag := flag.String("cmd", "", "overide argv[0] command name")
	flag.Parse()

	if *rmSelf {
		if selfExe, err := os.Executable(); err == nil {
			defer os.Remove(selfExe)
		}
	}

	if *server != "" {
		return runRemote(*server, flag.Args())
	}

	if *cmdFlag != "" {
		cmdName = *cmdFlag
	}

	switch cmdName {
	case hookCmdName:
		return runHook(cmdName, flag.Args())
	default:
		return runInit(flag.Args())
	}
}

// runRemote executes the currently ran executable on a remote ssh server with
// all positional args passed along.
func runRemote(server string, args []string) (rerr error) {
	log.Printf("running on remote %q", server)

	sshArgs := []string{
		server, "sh", "-c",
		"'self=`mktemp` && cat >$self && chmod +x $self && exec $self -rm \"$@\"'",
		"--",
	}
	for _, arg := range args {
		sshArgs = append(sshArgs, strconv.Quote(arg))
	}

	cmd := exec.Command("ssh", sshArgs...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to stdin pipe: %w", err)
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start ssh: %w", err)
	}

	defer func() {
		if err := in.Close(); rerr == nil && err != nil {
			rerr = fmt.Errorf("failed to close in: %w", err)
		}

		if err := cmd.Wait(); rerr == nil && err != nil {
			rerr = fmt.Errorf("remote self failed: %w", err)
		}
	}()

	return copySelfInto(in)
}

// runInit installs the current executable into proxmox snippets storage, and
// then sets that snippet as hookscript for any VMs that have host hardware
// passed through.
func runInit(args []string) error {
	snippetStore, storeDir, err := findSnippets()
	if err != nil {
		return err
	}

	hookScript := fmt.Sprintf("%s:snippets/%s", snippetStore, hookCmdName)
	hookDest := path.Join(storeDir, "snippets", hookCmdName)

	if dryRun {
		log.Printf("would copy self execuable to %q", hookDest)
	} else {
		if err := copySelfTo(hookDest); err != nil {
			return err
		}
		log.Printf("copied self execuable to %q", hookDest)
	}

	g := new(errgroup.Group)
	g.Go(func() error {
		cmm := matchCommand(exec.Command("qm", "list"), listPat)
		cmm.Scan() // skip first (header) line
		for cmm.Scan() {
			id := cmm.MatchText(1)
			g.Go(func() error {
				if should, err := shouldHook(id); err != nil || !should {
					return err
				}
				return maybeRun("qm", "set", id, "--hookscript", hookScript)
			})
		}
		return cmm.Err()
	})
	return g.Wait()
}

func findSnippets() (store, dir string, _ error) {
	var stores []struct {
		Name    string `json:"storage"`
		Content string `json:"content"`
		Path    string `json:"path"`
	}

	if err := decodeJSONCommand(
		&stores,
		exec.Command("pvesh", "get", "/storage", "--output-format", "json"),
	); err != nil {
		return "", "", err
	}

	for _, st := range stores {
		if st.Path == "" {
			continue
		}
		if !hasString("snippets", strings.Split(st.Content, ",")) {
			continue
		}

		store = st.Name
		dir = st.Path
		break
	}
	return store, dir, nil
}

func shouldHook(id string) (bool, error) {
	rec := recognizeCommand(exec.Command("qm", "config", id), keyValPat, labelHostResource)
	return rec.Scan(), rec.Err()
}

func copySelfTo(dest string) (rerr error) {
	f, err := os.OpenFile(dest, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0777)
	if err != nil {
		return fmt.Errorf("unable to create %q: %w", dest, err)
	}
	defer func() {
		if err := f.Close(); rerr == nil {
			rerr = err
		}
	}()
	return copySelfInto(f)
}

func copySelfInto(dst io.Writer) (rerr error) {
	selfExe, err := os.Executable()
	if err != err {
		return fmt.Errorf("unable to get self executable: %w", err)
	}

	self, err := os.Open(selfExe)
	if err != err {
		return fmt.Errorf("unable to open self executable: %w", err)
	}
	defer self.Close()

	if _, err := io.Copy(dst, self); err != nil {
		return fmt.Errorf("failed to copy self executable: %w", err)
	}
	return nil
}

func hasString(wanted string, ss []string) bool {
	for _, s := range ss {
		if s == wanted {
			return true
		}
	}
	return false
}

// runHook provides proxmox hookscript logic when dispatched by runHook based
// on the command name. returning an error to log on failure.
func runHook(progName string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: %s <vmid> <phase>", progName)
	}
	vmid := args[0]
	phase := args[1]

	switch phase {
	case "pre-start":
		return stopMutuals(vmid)

	case "post-start":
		// TODO update qm set -onboot

	case "pre-stop":

	case "post-stop":

	default:
		return fmt.Errorf("got unknown phase %q", phase)
	}

	return nil
}

// stopMutuals shuts down any running VMs that share host resources like
// passed-through PCI and USB devices.
func stopMutuals(vmid string) error {
	mutualRecs, err := mutuals(vmid)
	if err != nil {
		return err
	}
	g := new(errgroup.Group)
	for _, mutual := range mutualRecs {
		switch mutual.status {
		case "running":
			id := mutual.id
			g.Go(func() error {
				return maybeRun("qm", "shutdown", id)
			})
		case "stopped":
		default:
			log.Printf("not stopping mutual %q in unknown state %q", mutual.id, mutual.status)
		}
	}
	return g.Wait()
}

var (
	listPat    = regexp.MustCompile(`([^\s]+)\s+(.+?)\s+(.+?)\s+`)
	usbHostPat = regexp.MustCompile(`\bhost=([^,]+)`)
	statusPat  = regexp.MustCompile(`status:\s*(.+)`)
	keyValPat  = regexp.MustCompile(`(.+?):\s*(.+)`)
)

type listRec struct {
	id     string
	name   string
	status string
}

func mutuals(id string) (mutualIds []listRec, _ error) {
	res, err := hostResources(id)
	if err != nil {
		return nil, err
	}

	// TODO do we really need a better fixed-width scanner here?

	cmm := matchCommand(exec.Command("qm", "list"), listPat)
	cmm.Scan() // skip first (header) line

	for cmm.Scan() {

		otherId := cmm.MatchText(1)

		if otherId == id {
			continue
		}

		shares, err := sharesHostResources(otherId, res)
		if err != nil {
			return nil, err
		}

		otherName := cmm.MatchText(2)
		otherStatus := cmm.MatchText(3)

		if shares {
			mutualIds = append(mutualIds, listRec{otherId, otherName, otherStatus})
		}
	}

	if err = cmm.Err(); err != nil {
		return nil, err
	}

	return mutualIds, nil
}

func labelHostResource(cmm *cmdMatcher) string {
	name := cmm.MatchText(1)

	if strings.HasPrefix(name, "hostpci") {
		value := cmm.MatchText(2)
		if i := strings.IndexByte(value, ','); i >= 0 {
			value = value[:i]
		}
		return fmt.Sprintf("hostpci:%s", value)
	}

	if strings.HasPrefix(name, "usb") {
		if match := usbHostPat.FindSubmatch(cmm.Match(2)); len(match) > 0 {
			return fmt.Sprintf("hostusb:%s", match[1])
		}
	}

	return ""
}

func hostResources(id string) (_ map[string]struct{}, rerr error) {
	rec := recognizeCommand(exec.Command("qm", "config", id), keyValPat, labelHostResource)
	defer rec.Cleanup(&rerr)
	reses := make(map[string]struct{})
	for rec.Scan() {
		reses[rec.Label()] = struct{}{}
	}
	return reses, nil
}

func sharesHostResources(id string, reses map[string]struct{}) (hasAny bool, rerr error) {
	rec := recognizeCommand(exec.Command("qm", "config", id), keyValPat, labelHostResource)
	defer rec.Cleanup(&rerr)
	for rec.Scan() {
		if _, has := reses[rec.Label()]; has {
			return true, nil
		}
	}
	return false, nil
}

//// command running utilities

var dryRun = false

func init() {
	flag.BoolVar(&dryRun, "dry-run", false, "affect no change")
}

// maybeRun is used to run consequential commands like "qm shutodown <vmid>"
// unless -dry-run was given. It is not used for running interogative commands
// like "qm config <vmid>".
func maybeRun(args ...string) error {
	if dryRun {
		log.Printf("would run %q", args)
		return nil
	}
	log.Printf("run %q", args)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func decodeJSONCommand(val interface{}, cmd *exec.Cmd) error {
	rc, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start %q: %w", cmd.Args, err)
	}

	dec := json.NewDecoder(rc)
	err = dec.Decode(val)
	werr := cmd.Wait()

	if err != nil {
		return fmt.Errorf("failed to decode json from %q: %w", cmd.Args, err)
	}

	if werr != nil {
		return fmt.Errorf("%q failed: %w", cmd.Args, err)
	}

	return nil
}

// scanCommand creates a scanner bound to a running exec.Cmd.
// The command is auto started on first call to Scan().
// After Scan() returns false, Cleanup() should be deferred to cleanup and
// return any error encountered.
// Cleanup() may be called early if stopping once a "enough" input has been scanned.
func scanCommand(cmd *exec.Cmd) *cmdScanner {
	return &cmdScanner{cmd: cmd}
}

// matchCommand creates a scanner with a regular expression pattern added.
// Its Scan() method returns true only after an underlying Scan() whose Bytes()
// have matched the given pattern; it keeps calling underlying Scan() until
// such match, or underlying false is retruned.
func matchCommand(
	cmd *exec.Cmd,
	pat *regexp.Regexp,
) *cmdMatcher {
	return &cmdMatcher{
		cmdScanner: cmdScanner{cmd: cmd},
		pat:        pat,
	}
}

// matchCommandOnce returns any first match from running a command, along with
// any final error.
func matchCommandOnce(cmd *exec.Cmd, pat *regexp.Regexp) (_ string, rerr error) {
	cmm := cmdMatcher{
		cmdScanner: cmdScanner{cmd: cmd},
		pat:        pat,
	}
	defer cmm.Cleanup(&rerr)
	cmm.Scan()
	return cmm.MatchText(1), nil
}

// recognizeCommand creates a matcher with a recognition function that is
// called after every successful pattern match.
// Its Scan() method returns true only after an underlying Scan() where rec()
// has returned a non-empty label; it keeps calling underlying Scan() until
// such a label has been recognized.
func recognizeCommand(
	cmd *exec.Cmd,
	pat *regexp.Regexp,
	rec func(cmm *cmdMatcher) string,
) *cmdRecognizer {
	return &cmdRecognizer{
		cmdMatcher: cmdMatcher{
			cmdScanner: cmdScanner{cmd: cmd},
			pat:        pat,
		},
		rec: rec,
	}
}

type cmdScanner struct {
	cmd *exec.Cmd
	err error
	*bufio.Scanner
}

type cmdMatcher struct {
	cmdScanner
	pat   *regexp.Regexp
	match [][]byte
}

type cmdRecognizer struct {
	cmdMatcher
	rec   func(cmm *cmdMatcher) string
	label string
}

func (cmr *cmdRecognizer) Label() string {
	return cmr.label
}

func (cmr *cmdRecognizer) Scan() bool {
	cmr.label = ""
	for cmr.cmdMatcher.Scan() {
		cmr.label = cmr.rec(&cmr.cmdMatcher)
		if cmr.label != "" {
			return true
		}
	}
	return false
}

func (cmm *cmdMatcher) Match(i int) []byte {
	if i < len(cmm.match) {
		return cmm.match[i]
	}
	return nil
}

func (cmm *cmdMatcher) MatchText(i int) string {
	if i < len(cmm.match) {
		return string(cmm.match[i])
	}
	return ""
}

func (cmm *cmdMatcher) Scan() bool {
	cmm.match = nil
	for cmm.cmdScanner.Scan() {
		cmm.match = cmm.pat.FindSubmatch(cmm.Bytes())
		if cmm.match != nil {
			return true
		}
	}
	return false
}

func (csc *cmdScanner) Err() error {
	err := csc.err
	if err == nil && csc.Scanner != nil {
		if err = csc.Scanner.Err(); err != nil {
			err = fmt.Errorf("io error: %w", err)
			csc.err = err
		}
	}
	return err
}

func (csc *cmdScanner) Cleanup(errp *error) {
	if csc.cmd.Process != nil {
		_ = csc.cmd.Process.Kill()
		werr := csc.cmd.Wait()
		if isKillError(werr) {
			werr = nil // expected from Process.Kill() above
		}
		if err := csc.Err(); err == nil {
			csc.err = werr
		}
	}
	if err := csc.Err(); err != nil && errp != nil && *errp == nil {
		*errp = fmt.Errorf("command %q failed: %w", csc.cmd.Args, err)
	}
}

func isKillError(err error) bool {
	var xerr *exec.ExitError
	if errors.As(err, &xerr) {
		status, haveStatus := xerr.ProcessState.Sys().(syscall.WaitStatus)
		return haveStatus && status.Signaled() && status.Signal() == syscall.SIGKILL
	}
	return false
}

func (csc *cmdScanner) Scan() bool {
	if csc.err != nil {
		return false
	}
	if csc.cmd == nil {
		return false
	}

	if csc.cmd.Process == nil {
		if csc.Scanner == nil {
			rc, err := csc.cmd.StdoutPipe()
			if err != nil {
				csc.err = err
				return false
			}
			csc.Scanner = bufio.NewScanner(rc)
		}

		csc.err = csc.cmd.Start()

		if csc.err != nil {
			return false
		}
	}

	if csc.Scanner == nil {
		return false
	}
	return csc.Scanner.Scan()
}
