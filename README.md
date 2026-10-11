# CasaOS-VirtManagement

> **Not affiliated with IceWhale.** A module for [ReCasaOS](https://github.com/ReCasaOS/CasaOS), an independent, community-maintained distribution of CasaOS. CASAOS is a trademark of Shanghai IceWhale Technology Limited, used here only to say what this is a module of.

Virtual machines for a ReCasaOS box: a service that drives libvirt and QEMU, and answers the dashboard's virtual machine page. It is an **opt-in module** ([ReCasaOS/CasaOS#18](https://github.com/ReCasaOS/CasaOS/issues/18)): a box without it downloads nothing of it and shows no VM card. Community module: its CI runs against libvirt's test driver, not real VMs.

## What it does

- **VMs**: create from an ISO or a disk image (qcow2, img/raw, vmdk, vdi, also .xz/.gz/.zip), start, shut down (ACPI), force off, pause, reset, delete (the disk is kept unless asked). vCPUs, memory, title and OS icon can be changed.
- **Console** in the browser: VNC over a WebSocket through the gateway.
- **Disks**: grow, add, remove; CD-ROM insert and eject; disks in `/DATA/VM` or a folder under `/DATA` or `/media`.
- **Snapshots** with the VM off (qemu-img, UEFI variables included).
- **Network**, per VM: the CasaOS NAT network (fixed or DHCP address, port forwards from the host, reachable from containers), an existing host bridge, or macvtap on a wired card. The service never creates or changes host bridges.
- **USB passthrough**, live on a running VM; devices the host needs (its network card, a mounted disk) are refused.
- **Images from a URL** the user gives, with an optional sha256. No built-in catalogue: the box only contacts addresses the user gives.

## How it is built

The service listens on a random loopback port and registers `/v2/virt_management` (and its OpenAPI viewer under `/doc/v2/virt_management`) with CasaOS-Gateway. The API is `api/virt_management/openapi.yaml`; the Go server and types are generated from it (`go generate`) and are not committed.

| Path under `/v2/virt_management` | |
|---|---|
| `/host`, `/host/storage`, `/host/usb`, `/storage` | what the host offers, and the default disk folder |
| `/vms`, `/vms/{id}`, `/vms/{id}/{action}` | list, create, change, delete, start/stop/… |
| `/vms/{id}/cdrom`, `/disks`, `/snapshots`, `/usb`, `/ports` | a VM's devices, snapshots and port forwards |
| `/vms/{id}/console` | the VNC WebSocket (the only route that takes the token as `?token=`) |
| `/images`, `/images/downloads` | ISOs and disk images, and downloads from a URL |

It talks to libvirtd with [go-libvirt](https://github.com/digitalocean/go-libvirt) (pure Go RPC), so it builds with `CGO_ENABLED=0`. libvirt is the only state: there is no database, and what CasaOS needs to remember goes in the domain's `<metadata>`. VMs made outside CasaOS are listed and can be started and stopped, but their disks are never touched. Without libvirt the service still runs and says so. Lifecycle events go to the MessageBus as `vm:*`.

The NAT network's firewall rules live in the service's own iptables chains, hooked from `DOCKER-USER` (or `FORWARD` without Docker), rewritten at start, after every change and every minute.

On a host:

- `/etc/casaos/virt-management.conf`, written from `build/sysroot/etc/casaos/virt-management.conf.sample` on first start
- `/DATA/VM/` (disks) and `/DATA/VM/iso/` (ISOs and images), the libvirt storage pool `casaos-vm`
- the libvirt network `casaos` (bridge `casaos0`, 192.168.123.0/24)
- `/var/log/casaos/virt-management.log`
- `casaos-virt-management.service`, after libvirtd

## Module

`module.json` says what the module is to the installer: its unit, gateway route, the packages it needs per architecture, the minimum ReCasaOS version and the systems it supports. It ships in the release archive next to `build/`; the installer copies it to `/etc/casaos/modules/virt.json`, where the core reads it too. `schema` is the manifest format version (1).

## Build and test

```sh
go generate ./...
go test ./...
```

The tests in `service/testdriver_test.go` run the service against libvirt's test driver, which keeps domains, networks and pools in memory and needs no KVM. They need a running libvirtd and are skipped unless `VIRT_TEST_LIBVIRT_URI` is set:

```sh
sudo VIRT_TEST_LIBVIRT_URI=test:///default go test ./service -run '^TestDriver'
```

`[libvirt] URI` in the config does the same for the whole service. Releases are built by GoReleaser from a `v*.*.*` tag: `linux-{amd64,arm64}-casaos-virt-management-v<version>.tar.gz`, like the other ReCasaOS components.

## Licence

Apache-2.0, see `LICENSE` and `NOTICE`.
