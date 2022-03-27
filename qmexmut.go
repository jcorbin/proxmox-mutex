package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
)

func main() {
	flag.Parse()
	if err := run(flag.CommandLine.Name(), flag.Args()); err != nil {
		log.Fatal(err)
	}
}

// run provides core logic for main() after flag parsing, providing an error to
// log on failure.
func run(progName string, args []string) error {
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

	// TODO better off as an x/sync/errgroup
	errch := make(chan error, 1)
	var wg sync.WaitGroup
	for _, mutual := range mutualRecs {
		switch mutual.status {
		case "running":
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				if err := maybeRun("qm", "shutdown", id); err != nil {
					select {
					case errch <- err:
					default:
					}
				}
			}(mutual.id)
		case "stopped":
		default:
			log.Printf("not stopping mutual %q in unknown state %q", mutual.id, mutual.status)
		}
	}
	wg.Wait()
	select {
	case err := <-errch:
		return err
	default:
	}

	return nil
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

func hostResources(id string) (map[string]struct{}, error) {
	rec := recognizeCommand(exec.Command("qm", "config", id), keyValPat, labelHostResource)
	reses := make(map[string]struct{})
	for rec.Scan() {
		reses[rec.Label()] = struct{}{}
	}
	if err := rec.Wait(); err != nil {
		return nil, err
	}
	return reses, nil
}

func sharesHostResources(id string, reses map[string]struct{}) (bool, error) {
	rec := recognizeCommand(exec.Command("qm", "config", id), keyValPat, labelHostResource)
	for rec.Scan() {
		if _, has := reses[rec.Label()]; has {
			return true, rec.Wait()
		}
	}
	return false, rec.Wait()
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
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// scanCommand creates a scanner bound to a running exec.Cmd.
// The command is auto started on first call to Scan().
// After Scan() returns false, Wait() should be called to cleanup and return
// any error encountered.
// Wait() may be called early if stopping once a "enough" input has been scanned.
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
func matchCommandOnce(cmd *exec.Cmd, pat *regexp.Regexp) (string, error) {
	cmm := cmdMatcher{
		cmdScanner: cmdScanner{cmd: cmd},
		pat:        pat,
	}
	cmm.Scan()
	return cmm.MatchText(1), cmm.Wait()
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

func (csc *cmdScanner) Wait() error {
	if csc.cmd.Process == nil {
		return nil
	}
	err := csc.Err()
	if werr := csc.cmd.Wait(); err == nil {
		err = werr
		csc.err = werr
	}
	if err != nil {
		err = fmt.Errorf("command %q failed: %w", csc.cmd.Args, err)
	}
	return err
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
