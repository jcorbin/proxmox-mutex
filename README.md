# Proxmox tool to manage mutually exclusive guest VMs

This repository contains a tool that helps to run a proxmox system with
alternate main guest VMs that have host hardware (like a PCIe GPU and USB
inputs) passed thru to them.

# Scenario

Let's say we have
- a headless proxmox server install
- a PCIe GPU and one or more USB devices that are used only by two or more main guest VMs
  - VM 101 is a Windows VM for gaming
  - VM 102 is a Linux VM for productivity

## Problem: hardward conflict between main guest VMs

Without any additional setup, what one must do to swith between VM 101 and 102
is: to first stop one, wait, and then start the other.

In other words, the best one can do is to issue a command like `qm shutdown 102
&& qm start 101` at a proxmox console using the [qm] management command.

## Solution: a proxmox qemu hookscript

We can start to do better by automating the "shutdown any conflicting VMs" part.

This can be achieved during the `pre-start` phase of a [qm] hookscript.

The `qmexmut.go` tool in this repository currently implemnts just that.
It does so by finding any overlap of `hostpciX: ...` or `usbX: host=...`
configuration in the VM that's trying to start, and any currently running VMs.
So there's no need for static rules to be configured like "stop X before
starting Y"; proxmox's existing config is sufficient.

To install qmexmut:
- clone this repository and build the binary
  - you'll need Go (tested on 1.18, but should work on 1.17)
  - just type `go build ./qmexmut.go`
- copy the `qmexmut` binary into your proxmox's snippet storage
  - you may need to first enable snippets on your local (`/var/lib/vz`) storage directory
  - the binary should end up at `/var/lib/vz/snippets/qmexmut` on your proxmox server(s)
- then set the hookscript on any relevant VMs
  - run commands like `qm set <vmid> --hookscript local:snippets/qmexmut` for
    all involved VMs (101 and 102 in our example here)

After this point, now you can simply start each VM, and it will first shutdown
any confliciting siblings. So the `qm shutdown 102 && qm start 101` above can
just be `qm start 101`.

# TODO

- implement automatic installation of `qmexmut` so that the above install
  advice can be boiled down to "just build it, upload it, and run it"
- implement automatic toggling of `onboot` config so that the last started
  guest VM is the one that will restart on host reboot; i.e. if you poweroff
  with the Linux guest booted, that should be what bets started on reboot, not
  the Windows guest
- if it's possible to detect "guest shutdown voluntarily", we could boot up the
  other/last/next sibling for fast swapping trigger from withing a guest
- alternatively, it'd be neat to leverage something like a macro pad attached
  to the host to trigger main guest start without access to a console or the
  proxmox webui

[qm]: https://pve.proxmox.com/pve-docs/qm.1.html
