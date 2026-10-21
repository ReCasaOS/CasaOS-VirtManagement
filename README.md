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
- **Statistics**: CPU, memory, network and disk history per VM, and alerts when a VM is busy, short of memory or crashed (see below).
- **Images from a URL** the user gives, with an optional sha256. No built-in catalogue: the box only contacts addresses the user gives.
- **Images uploaded from the browser** into the same folder, listed as soon as they are written (see below).

## How it is built

The service listens on a random loopback port and registers `/v2/virt_management` (and its OpenAPI viewer under `/doc/v2/virt_management`) with CasaOS-Gateway. The API is `api/virt_management/openapi.yaml`; the Go server and types are generated from it (`go generate`) and are not committed.

| Path under `/v2/virt_management` | |
|---|---|
| `/host`, `/host/storage`, `/host/usb`, `/storage` | what the host offers, and the default disk folder |
| `/vms`, `/vms/{id}`, `/vms/{id}/{action}` | list, create, change, delete, start/stop/… |
| `/vms/{id}/cdrom`, `/disks`, `/snapshots`, `/usb`, `/ports` | a VM's devices, snapshots and port forwards |
| `/vms/{id}/console` | the VNC WebSocket (the only route that takes the token as `?token=`) |
| `/images`, `/images/downloads`, `/images/upload` | ISOs and disk images, downloads from a URL, and uploads from the browser |
| `/stats` | the VMs and their latest CPU and memory use |
| `/stats/{id}` | one VM's history over a range (`id` is `vm:<uuid>`, encoded as one path segment) |
| `/stats/{id}/alerts` | GET: a VM's alert rules and firing alerts; PUT: change the rules |

It talks to libvirtd with [go-libvirt](https://github.com/digitalocean/go-libvirt) (pure Go RPC), so it builds with `CGO_ENABLED=0`. libvirt is the only state: there is no database, and what CasaOS needs to remember goes in the domain's `<metadata>`. VMs made outside CasaOS are listed and can be started and stopped, but their disks are never touched. Without libvirt the service still runs and says so. Lifecycle events go to the MessageBus as `vm:*`.

The NAT network's firewall rules live in the service's own iptables chains, hooked from `DOCKER-USER` (or `FORWARD` without Docker), rewritten at start, after every change and every minute.

On a host:

- `/etc/casaos/virt-management.conf`, written from `build/sysroot/etc/casaos/virt-management.conf.sample` on first start
- `/DATA/VM/` (disks) and `/DATA/VM/iso/` (ISOs and images), the libvirt storage pool `casaos-vm`
- the libvirt network `casaos` (bridge `casaos0`, 192.168.123.0/24)
- `[images] MaxUploadGiB` (default 64, at most 1048576): the largest image the browser can upload
- `/var/log/casaos/virt-management.log`
- `/var/lib/casaos/stats/virt_management/` (`[stats] Path`): the VMs' statistics history and alert settings
- `casaos-virt-management.service`, after libvirtd

## Uploading an image

`PUT /v2/virt_management/images/upload?name=<file name>[&sha256=<hex>]` with the file as the raw body (`application/octet-stream`, not multipart) and its `Content-Length`. The body is streamed to `/DATA/VM/iso/<name>.part` (the `iso` folder of the storage folder), hashed on the way, and renamed to `<name>` once complete; the answer (201) is the image as `GET /images` lists it, with its `sha256`. A failed upload deletes its `.part`, and a start deletes those a stop left behind, as for downloads.

- Refused before the body is read: a name that is not a plain image file name (400 `bad_name`: a path, a hidden or odd name, control or bidi characters, a Windows device name, over 250 bytes, an extension that is not an image's), a bad `sha256` (400 `bad_sha256`), no length (411 `length_required`), over `[images] MaxUploadGiB` (413 `too_large`), a name already there or being uploaded or downloaded (409 `exists`: nothing is overwritten), a third upload at once (503 `busy`), and a file larger than the free space less a reserve, the larger of 1 GiB and 2 % of the filesystem, and less what the running uploads and downloads will still write (507 `insufficient_storage`). A download of a name being uploaded is refused too (409).
- While it is read: a body that ends early or a client that goes (400 `incomplete`), a client silent for 2 minutes or slower than 128 KiB/s on average past 5 minutes of slack, 24 hours at most (408 `stalled`), a full disk (507 `insufficient_storage`).
- Once it is read: a checksum other than `sha256` (422 `checksum_mismatch`), a `.qcow2` without the qcow2 magic (422 `bad_image`). An `.iso` without an ISO 9660 or UDF signature at byte 32769 is kept, with `warning: iso_signature_missing`. The file is synced, then renamed without replacing anything (`RENAME_NOREPLACE`; on a filesystem without it, checked then renamed), mode 0644 like downloads.

Errors answer `{"message": "<code>"}`. A browser shown an error before it finished sending may report a network error instead, as the connection is closed: the page checks the name and the free space (`GET /images`, `GET /host`) first. File names are logged quoted; bodies never are.

## Statistics

A sampler reads every VM's counters from libvirt every 5 seconds (one `ConnectGetAllDomainStats` call, or one call per domain on a driver without it) and records them in `[stats] Path` (default `/var/lib/casaos/stats/virt_management`). The routes above serve them; the OpenAPI file has the details. The alerts `cpu_high`, `mem_high` and `vm_crashed` are announced on the MessageBus as `resource:alert-firing` and `resource:alert-resolved`. If the history cannot be opened, the stats routes answer 503 and the rest of the service is unaffected.

- **Memory used** needs the guest's balloon driver. Without it the series carries the note `vm_memory_used_unavailable` and the memory alert stays silent. A VM created by this version asks the balloon for statistics every few seconds. A VM created before it lacks that setting and shows the note until it is recreated (the setting is written only when a VM is created; stopping and starting it again does not add it).
- **Cost**: parsing, maths and recording for 10 running VMs (4 vCPUs, 2 NICs, 2 disks each) took about 3 to 3.7 ms per tick on a Windows 11 desktop with a Ryzen 9 5900X, a desktop CPU: slower boxes will differ (`go test ./pkg/vmstats -run xxx -bench Tick10VMs -benchmem`, 3 runs, about 125 KB and 473 allocations per tick). That excludes the libvirt calls themselves, which have not been measured.

Known limits:

- go-libvirt calls take no context: a `Snapshot` or `Describe` call that hangs (libvirtd stuck rather than gone) blocks the tick, the next ones and shutdown until libvirt answers or the connection drops.
- A VM crash is seen from libvirt's lifecycle events. Where only the periodic read is available, a crash that happens while the service is down is missed.
- The balloon's `last-update` timestamp is not parsed, so a guest that stopped reporting can show its last known memory figure.
- libvirt's test driver answers `ConnectGetAllDomainStats` with the state only; a running domain without a CPU time in that answer gets its CPU time, vCPUs and assigned memory from `DomainGetInfo` (no device counters).
- Not verified against real libvirt yet: `ConnectGetAllDomainStats` on the test driver, the no-wait flag on busy domains, a memory balloon being accepted on the domain XML, and reconnecting after libvirtd restarts. Only unit tests against fakes have run so far.

## Module

`module.json` says what the module is to the installer: its unit, gateway route, the packages it needs per architecture, the minimum ReCasaOS version and the systems it supports. It ships in the release archive next to `build/`; the installer copies it to `/etc/casaos/modules/virt.json`, where the core reads it too. `schema` is the manifest format version (1).

## Build and test

```sh
go generate ./...
go test ./...
```

The tests in `service/testdriver_test.go` and `service/testdriver_stats_test.go` run the service against libvirt's test driver, which keeps domains, networks and pools in memory and needs no KVM. They need a running libvirtd and are skipped unless `VIRT_TEST_LIBVIRT_URI` is set:

```sh
sudo VIRT_TEST_LIBVIRT_URI=test:///default go test ./service -run '^TestDriver'
```

`VIRT_TEST_LIBVIRT_SOCKET` is the daemon's socket (`/run/libvirt/libvirt-sock` when unset). `[libvirt] URI = test:///default` in the config does the same for the whole service, statistics included: that URI and `VIRT_TEST_LIBVIRT_URI=test:///default` are what a module proof sets. The test driver has no memory balloon, so a VM's series carry the note `vm_memory_used_unavailable`, and it cannot make a VM crash. Releases are built by GoReleaser from a `v*.*.*` tag: `linux-{amd64,arm64}-casaos-virt-management-v<version>.tar.gz`, like the other ReCasaOS components.

## Licence

Apache-2.0, see `LICENSE` and `NOTICE`.
