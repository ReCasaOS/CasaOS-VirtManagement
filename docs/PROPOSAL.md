# Proposal: virtual machines in ReCasaOS

> Posted as https://github.com/ReCasaOS/CasaOS/issues/18 on 2026-10-10.

**Title:** Proposal: virtual machines (a libvirt satellite and a dashboard page)

Hi @inkly,

I have been building virtual machine management for ReCasaOS on my fork, and before opening
any PR I would like to know whether you want it, and in what shape. It is a new satellite plus
a page in the dashboard, so it touches the installer too. That is why I am asking first.

Since it is a satellite of its own that talks to the rest only through the gateway and the
MessageBus, it could also serve the other forks discussed in #11.

## What it does

- **VMs**: create from an ISO or from a disk image (qcow2, img/raw, vmdk, vdi, also .xz/.gz/.zip),
  start, shut down (ACPI), force off, pause, reset, delete (the disk is kept unless asked).
  vCPUs, memory, title and OS icon editable.
- **Console** in the browser (noVNC over a WebSocket through the gateway), full screen,
  paste-as-keys.
- **Disks**: grow, add, remove; CD-ROM insert/eject; disks in `/DATA/VM` or a folder the user picks
  under `/DATA` or `/media`.
- **Snapshots** with the VM off (qemu-img, UEFI variables included).
- **Network**, per VM:
  - *CasaOS NAT* (a libvirt network of its own, 192.168.123.0/24): fixed or DHCP address, port
    forwards from the host, and **containers reach the VM** (ZimaOS cannot do this).
  - *An existing host bridge* (e.g. `br0`): the VM is on the LAN like any device. The service
    never creates or changes host bridges.
  - *macvtap* on a wired card, when there is no bridge.
- **USB passthrough**: one switch per host device, live on a running VM; devices the host needs
  (its network card, a mounted disk) are refused.
- **Images from a URL** the user pastes, with an optional sha256. No built-in catalogue, so the
  box only contacts addresses the user gives.

## How it is built

- **`casaos-virt-management`**, Go, same layout as AppManagement: `api/…/openapi.yaml`, oapi-codegen,
  echo + echo-jwt + the request validator, gateway route `/v2/virt_management`, events `vm:*`
  on the MessageBus, a systemd unit, an ini config in `/etc/casaos`.
- It talks to libvirtd with `digitalocean/go-libvirt` (pure Go RPC), so it builds with `CGO_ENABLED=0`
  and cross-compiles like the other components.
- **libvirt is the only state**: no database. What CasaOS needs to remember goes in the domain's
  `<metadata>`. VMs made outside CasaOS are listed and can be started and stopped, but their disks
  are never touched.
- Without libvirt the service still runs and says so, and the page tells what to install. Without the service, the dashboard shows no VM card.
- The firewall rules for the NAT network live in the service's own iptables chains (hooked from
  `DOCKER-USER`), rewritten at start and every minute, so they survive Docker restarts.
- Auth as in the other satellites: JWT from the dashboard, the internal secret from loopback.
  The console WebSocket is the only route that takes the token as `?token=`.
- About 7,900 lines of Go (1,300 of them tests), 3,900 lines of UI.

## What it needs from the rest of ReCasaOS

- **CasaOS-Install**: the component in `release/components.env`, `CASA_PACKAGES`, `CASA_SERVICES`,
  and the libvirt packages (`qemu-utils libvirt-daemon-system libvirt-clients`, plus
  `qemu-system-x86 ovmf` on amd64 or `qemu-system-arm qemu-efi-aarch64` on arm64). Default or
  opt-in is your call. They are a few hundred MB, and a failed install must not fail ReCasaOS.
- **CasaOS-UI**: the page is one self-contained folder (`src/components/VirtualMachines/`) plus
  `@novnc/novnc`. Outside that folder: the built-in card (`AppSection.vue`, `AppCard.vue`), the
  App launching entry and the `#/vms` route (`AppLaunchModal.vue`, `appLaunchPreference.js`,
  `route.js`), the strings in `en_US.json`, and one `vue.config.js` line for the dev proxy.
- Gateway, MessageBus, Common, LocalStorage, UserService: no changes.

## Tested

- Ubuntu 26.04, libvirt 12, QEMU 10.2 (nested, in a VM).
- Linux Mint 22.2 (Ubuntu 24.04), libvirt 10, QEMU 8.2, on metal: bridge, USB, snapshots, downloads.
- Not tested yet: arm64, `/DATA` on mergerfs (qcow2 works on FUSE, but without `cache=none`),
  ACPI shutdown of an installed guest, autostart (left out on purpose for now: a VM on a disk that is
  not mounted yet at boot would fail).

## Licence

The service is Apache-2.0, like the other components. My changes to CasaOS-UI, the VM page
included, are offered under Apache-2.0 too, in the spirit of #16.

## What I am asking

1. Do you want VMs in ReCasaOS at all?
2. If yes: a new repository `ReCasaOS/CasaOS-VirtManagement`, which you create and I open the
   first PR against? Or would you rather I publish it on my account first, for you to look at?
3. libvirt packages installed by default, or opt-in?
4. Then I would send it in reviewable pieces: the service first, then the installer change, then
   the UI.

Screenshots below. Happy to change whatever does not fit how you see ReCasaOS.

<!-- screenshots, in docs/img (drag them into the GitHub editor; gh cannot upload images):
     vm-list-detail.jpg  The list and a running VM
     vm-create.jpg       Creating a VM
     vm-console.jpg      The console
     vm-network.jpg      Network: CasaOS NAT, fixed address, a port forward
     vm-usb.jpg          USB passthrough -->

**The list and a running VM**

**Creating a VM**

**The console**

**Network: CasaOS NAT, fixed address, a port forward**

**USB passthrough**
